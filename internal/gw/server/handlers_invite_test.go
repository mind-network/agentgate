package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/auth"
	"agentgate/internal/gw/db"
	"agentgate/internal/gw/edge"
)

// stubAuditWriter is a minimal in-memory audit sink for handler tests.
type stubAuditWriter struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *stubAuditWriter) Write(ev audit.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *stubAuditWriter) snapshot() []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]audit.Event, len(s.events))
	copy(out, s.events)
	return out
}

func newInviteHandler(t *testing.T, teams []string) (*Handler, *stubAuditWriter) {
	t.Helper()
	teamSet := make(map[string]struct{}, len(teams))
	for _, tm := range teams {
		teamSet[tm] = struct{}{}
	}
	keyStore := auth.NewInMemKeyStore()
	invites := auth.NewInMemInviteStore()
	// Wire the in-memory key store into the invite store so
	// ConsumeAndIssueKey commits the api_key insert atomically with the
	// invite consume — mirroring the Pg transaction's all-or-nothing
	// behavior.
	invites.SetKeyStore(keyStore)
	auditW := &stubAuditWriter{}
	return &Handler{
		KeyStore:  keyStore,
		PgInvites: invites,
		PgAuditW:  auditW,
		TeamRegistered: func(teamID string) bool {
			_, ok := teamSet[teamID]
			return ok
		},
	}, auditW
}

func contextWithAdmin() context.Context {
	ctx := context.Background()
	ctx = context.WithValue(ctx, edge.CtxUserID, "admin")
	ctx = context.WithValue(ctx, edge.CtxRole, auth.RolePlatformAdmin)
	return ctx
}

func TestHandleAdminCreateInviteSuccess(t *testing.T) {
	h, auditW := newInviteHandler(t, []string{"dogfood", "platform"})

	body := `{"user_id":"dogfood-dev-3","team_id":"dogfood","role":"developer","ttl_hours":24}`
	req := httptest.NewRequest("POST", "/api/v1/admin/invites", strings.NewReader(body)).
		WithContext(contextWithAdmin())
	rec := httptest.NewRecorder()
	h.HandleAdminCreateInvite(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp createInviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.InviteToken) != 64 {
		t.Errorf("token len=%d, want 64", len(resp.InviteToken))
	}
	if resp.UserID != "dogfood-dev-3" {
		t.Errorf("user_id=%q", resp.UserID)
	}
	if resp.Role != "developer" {
		t.Errorf("role=%q", resp.Role)
	}
	if resp.TeamID != "dogfood" {
		t.Errorf("team_id=%q", resp.TeamID)
	}
	if !resp.ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at=%v not in future", resp.ExpiresAt)
	}

	events := auditW.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events=%d, want 1", len(events))
	}
	if events[0].EventType != audit.EventInviteCreated {
		t.Errorf("event_type=%q, want invite.created", events[0].EventType)
	}
	if events[0].UserID != "admin" {
		t.Errorf("audit user_id=%q, want admin", events[0].UserID)
	}
}

func TestHandleAdminCreateInviteRoleInvalid(t *testing.T) {
	h, _ := newInviteHandler(t, []string{"dogfood"})
	body := `{"user_id":"u","team_id":"dogfood","role":"superuser","ttl_hours":24}`
	req := httptest.NewRequest("POST", "/api/v1/admin/invites", strings.NewReader(body)).
		WithContext(contextWithAdmin())
	rec := httptest.NewRecorder()
	h.HandleAdminCreateInvite(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var er ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &er)
	if er.Code != "bad_request" {
		t.Errorf("code=%q, want bad_request", er.Code)
	}
}

func TestHandleAdminCreateInviteUnknownTeam(t *testing.T) {
	h, _ := newInviteHandler(t, []string{"platform"})
	body := `{"user_id":"u","team_id":"ghost","role":"developer","ttl_hours":24}`
	req := httptest.NewRequest("POST", "/api/v1/admin/invites", strings.NewReader(body)).
		WithContext(contextWithAdmin())
	rec := httptest.NewRecorder()
	h.HandleAdminCreateInvite(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var er ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &er)
	if er.Code != "unknown_team" {
		t.Errorf("code=%q, want unknown_team", er.Code)
	}
}

func TestHandleAdminCreateInviteTTLBounds(t *testing.T) {
	h, _ := newInviteHandler(t, []string{"dogfood"})
	cases := []struct {
		name string
		ttl  int
	}{
		{"zero", 0},
		{"negative", -5},
		{"over_max", 721},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"user_id":"u","team_id":"dogfood","role":"developer","ttl_hours":` + fmt.Sprint(tc.ttl) + `}`
			req := httptest.NewRequest("POST", "/api/v1/admin/invites", strings.NewReader(body)).
				WithContext(contextWithAdmin())
			rec := httptest.NewRecorder()
			h.HandleAdminCreateInvite(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", rec.Code)
			}
		})
	}
}

func TestHandleAdminCreateInviteMissingFields(t *testing.T) {
	h, _ := newInviteHandler(t, []string{"dogfood"})
	cases := []struct {
		name string
		body string
	}{
		{"missing_user_id", `{"team_id":"dogfood","role":"developer","ttl_hours":24}`},
		{"missing_team_id", `{"user_id":"u","role":"developer","ttl_hours":24}`},
		{"bad_json", `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/admin/invites", strings.NewReader(tc.body)).
				WithContext(contextWithAdmin())
			rec := httptest.NewRecorder()
			h.HandleAdminCreateInvite(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", rec.Code)
			}
		})
	}
}

func TestAdminInviteRouteRequiresPlatformAdmin(t *testing.T) {
	// Wire the route the way main.go does: AuthMiddleware + RequireRole.
	store := auth.NewInMemKeyStore()
	devKey := "dev-" + strings.Repeat("a", 60)
	adminKey := "adm-" + strings.Repeat("b", 60)
	_ = store.StoreAPIKey("dev-user", "dogfood", auth.RoleDeveloper, devKey)
	_ = store.StoreAPIKey("admin-user", "platform", auth.RolePlatformAdmin, adminKey)

	h, _ := newInviteHandler(t, []string{"dogfood", "platform"})
	h.KeyStore = store

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Use(auth.AuthMiddleware(store))
	r.With(auth.RequireRole(auth.RolePlatformAdmin)).
		Post("/api/v1/admin/invites", h.HandleAdminCreateInvite)

	body := `{"user_id":"target","team_id":"dogfood","role":"developer","ttl_hours":1}`

	// Developer must be rejected with 403.
	reqDev := httptest.NewRequest("POST", "/api/v1/admin/invites", strings.NewReader(body))
	reqDev.Header.Set("Authorization", "Bearer "+devKey)
	recDev := httptest.NewRecorder()
	r.ServeHTTP(recDev, reqDev)
	if recDev.Code != http.StatusForbidden {
		t.Errorf("developer status=%d, want 403; body=%s", recDev.Code, recDev.Body.String())
	}

	// No auth header → 401.
	reqAnon := httptest.NewRequest("POST", "/api/v1/admin/invites", strings.NewReader(body))
	recAnon := httptest.NewRecorder()
	r.ServeHTTP(recAnon, reqAnon)
	if recAnon.Code != http.StatusUnauthorized {
		t.Errorf("anonymous status=%d, want 401", recAnon.Code)
	}

	// Platform admin succeeds.
	reqAdm := httptest.NewRequest("POST", "/api/v1/admin/invites", strings.NewReader(body))
	reqAdm.Header.Set("Authorization", "Bearer "+adminKey)
	recAdm := httptest.NewRecorder()
	r.ServeHTTP(recAdm, reqAdm)
	if recAdm.Code != http.StatusCreated {
		t.Errorf("admin status=%d, want 201; body=%s", recAdm.Code, recAdm.Body.String())
	}
}

func TestHandleExchangeInviteSuccess(t *testing.T) {
	h, auditW := newInviteHandler(t, []string{"dogfood"})

	// Seed an invite directly into the store.
	token := "tok-success-" + strings.Repeat("c", 52)
	tokenHash := sha256Hex(token)
	if err := h.PgInvites.Create(context.Background(), auth.Invite{
		TokenHash: tokenHash,
		Role:      auth.RoleDeveloper,
		TeamID:    "dogfood",
		UserID:    "dogfood-dev-7",
		CreatedBy: "admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"invite_token":%q,"machine_id":"mid-12345"}`, token)
	req := httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleExchangeInvite(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp exchangeInviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.UserID != "dogfood-dev-7" {
		t.Errorf("user_id=%q", resp.UserID)
	}
	if resp.Role != auth.RoleDeveloper {
		t.Errorf("role=%q", resp.Role)
	}
	if resp.TeamID != "dogfood" {
		t.Errorf("team_id=%q", resp.TeamID)
	}
	if len(resp.APIKey) != 64 {
		t.Errorf("api_key len=%d, want 64", len(resp.APIKey))
	}

	// Verify the key was stored and a non-admin role was issued.
	stored, err := h.KeyStore.LookupAPIKey(resp.APIKey)
	if err != nil {
		t.Fatalf("lookup new key: %v", err)
	}
	if stored.UserID != "dogfood-dev-7" || stored.Role != auth.RoleDeveloper {
		t.Errorf("stored=%+v want user_id=dogfood-dev-7 role=developer", stored)
	}

	events := auditW.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events=%d, want 1", len(events))
	}
	if events[0].EventType != audit.EventInviteExchanged {
		t.Errorf("event=%q, want invite.exchanged", events[0].EventType)
	}
	if events[0].UserID != "dogfood-dev-7" {
		t.Errorf("audit user_id=%q", events[0].UserID)
	}
}

func TestHandleExchangeInviteDoubleRedemption(t *testing.T) {
	h, _ := newInviteHandler(t, []string{"dogfood"})

	token := "tok-double-" + strings.Repeat("d", 53)
	tokenHash := sha256Hex(token)
	if err := h.PgInvites.Create(context.Background(), auth.Invite{
		TokenHash: tokenHash,
		Role:      auth.RoleDeveloper,
		TeamID:    "dogfood",
		UserID:    "dogfood-dev-8",
		CreatedBy: "admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"invite_token":%q,"machine_id":"mid"}`, token)

	rec1 := httptest.NewRecorder()
	h.HandleExchangeInvite(rec1, httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body)))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first exchange status=%d, want 200; body=%s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.HandleExchangeInvite(rec2, httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body)))
	if rec2.Code != http.StatusGone {
		t.Fatalf("second exchange status=%d, want 410; body=%s", rec2.Code, rec2.Body.String())
	}
	var er ErrorResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &er)
	if er.Code != "token_consumed" {
		t.Errorf("code=%q, want token_consumed", er.Code)
	}
}

func TestHandleExchangeInviteExpired(t *testing.T) {
	h, _ := newInviteHandler(t, []string{"dogfood"})

	token := "tok-expired-" + strings.Repeat("e", 53)
	tokenHash := sha256Hex(token)
	if err := h.PgInvites.Create(context.Background(), auth.Invite{
		TokenHash: tokenHash,
		Role:      auth.RoleDeveloper,
		TeamID:    "dogfood",
		UserID:    "dogfood-dev-9",
		CreatedBy: "admin",
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"invite_token":%q,"machine_id":"mid"}`, token)
	rec := httptest.NewRecorder()
	h.HandleExchangeInvite(rec, httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body)))
	if rec.Code != http.StatusGone {
		t.Fatalf("status=%d, want 410; body=%s", rec.Code, rec.Body.String())
	}
	var er ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &er)
	if er.Code != "token_expired" {
		t.Errorf("code=%q, want token_expired", er.Code)
	}
}

func TestHandleExchangeInviteUnknownToken(t *testing.T) {
	h, _ := newInviteHandler(t, []string{"dogfood"})
	body := `{"invite_token":"deadbeef","machine_id":"mid"}`
	rec := httptest.NewRecorder()
	h.HandleExchangeInvite(rec, httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	var er ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &er)
	if er.Code != "bad_token" {
		t.Errorf("code=%q, want bad_token", er.Code)
	}
}

func TestHandleExchangeInviteBadBody(t *testing.T) {
	h, _ := newInviteHandler(t, []string{"dogfood"})
	cases := []struct {
		name string
		body string
	}{
		{"bad_json", `nope`},
		{"missing_token", `{"machine_id":"mid"}`},
		{"missing_machine_id", `{"invite_token":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.HandleExchangeInvite(rec, httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", rec.Code)
			}
		})
	}
}

func TestExchangeInviteAnonymousSkipsAuthMiddleware(t *testing.T) {
	// The endpoint must be reachable without an Authorization header, mirroring
	// /api/v1/lp/exchange-setup. Otherwise normal callers would see 401.
	store := auth.NewInMemKeyStore()
	h, _ := newInviteHandler(t, []string{"dogfood"})
	h.KeyStore = store

	token := "tok-anon-" + strings.Repeat("f", 55)
	tokenHash := sha256Hex(token)
	if err := h.PgInvites.Create(context.Background(), auth.Invite{
		TokenHash: tokenHash,
		Role:      auth.RoleDeveloper,
		TeamID:    "dogfood",
		UserID:    "u-anon",
		CreatedBy: "admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Use(auth.AuthMiddleware(store))
	r.Post("/api/v1/lp/exchange-invite", h.HandleExchangeInvite)

	body := fmt.Sprintf(`{"invite_token":%q,"machine_id":"m"}`, token)
	req := httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (skip list misconfigured?); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleExchangeInviteConcurrentRedemption(t *testing.T) {
	h, _ := newInviteHandler(t, []string{"dogfood"})

	token := "tok-race-" + strings.Repeat("g", 55)
	tokenHash := sha256Hex(token)
	if err := h.PgInvites.Create(context.Background(), auth.Invite{
		TokenHash: tokenHash,
		Role:      auth.RoleDeveloper,
		TeamID:    "dogfood",
		UserID:    "racer",
		CreatedBy: "admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"invite_token":%q,"machine_id":"m"}`, token)

	const goroutines = 8
	var (
		wg    sync.WaitGroup
		ok410 atomic.Int32
		ok200 atomic.Int32
		other atomic.Int32
		start = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			h.HandleExchangeInvite(rec, httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body)))
			switch rec.Code {
			case http.StatusOK:
				ok200.Add(1)
			case http.StatusGone:
				ok410.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok200.Load() != 1 {
		t.Errorf("200 count=%d, want exactly 1", ok200.Load())
	}
	if ok410.Load() != goroutines-1 {
		t.Errorf("410 count=%d, want %d", ok410.Load(), goroutines-1)
	}
	if other.Load() != 0 {
		t.Errorf("unexpected statuses count=%d", other.Load())
	}
}

// TestHandleExchangeInviteAPIKeyStoreFailureLeavesInviteRedeemable is the
// regression guard for the T3 revision. It forces the api_key insert step
// to fail on the first exchange attempt and then proves the invite is
// still redeemable on a second attempt — meaning the consume + insert
// commits all-or-nothing. Under the old "consume invite, then insert
// api_key" ordering, the second attempt would return 410 token_consumed
// because the invite would have been burned by the first call.
func TestHandleExchangeInviteAPIKeyStoreFailureLeavesInviteRedeemable(t *testing.T) {
	h, auditW := newInviteHandler(t, []string{"dogfood"})

	token := "tok-regress-" + strings.Repeat("h", 51)
	tokenHash := sha256Hex(token)
	if err := h.PgInvites.Create(context.Background(), auth.Invite{
		TokenHash: tokenHash,
		Role:      auth.RoleDeveloper,
		TeamID:    "dogfood",
		UserID:    "dogfood-dev-regress",
		CreatedBy: "admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Inject a one-shot api_key-store failure into the in-memory invite
	// store. The first ConsumeAndIssueKey call sees an error from the hook
	// and rolls back; subsequent calls run normally.
	invites := h.PgInvites.(*auth.InMemInviteStore)
	var failureCalls atomic.Int32
	invites.SetFailIssue(func() error {
		if failureCalls.Add(1) == 1 {
			return fmt.Errorf("simulated api_keys insert failure")
		}
		return nil
	})

	body := fmt.Sprintf(`{"invite_token":%q,"machine_id":"m"}`, token)

	rec1 := httptest.NewRecorder()
	h.HandleExchangeInvite(rec1, httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body)))
	if rec1.Code != http.StatusInternalServerError {
		t.Fatalf("first exchange status=%d, want 500; body=%s", rec1.Code, rec1.Body.String())
	}
	// No audit event must be emitted on a rolled-back exchange. This is
	// the audit-boundary half of the contract: the transaction covers
	// invites + api_keys only, but audit only fires on commit.
	if events := auditW.snapshot(); len(events) != 0 {
		t.Errorf("audit events after failed exchange=%d, want 0", len(events))
	}

	// The second exchange must succeed against the same token, proving
	// the first attempt did NOT consume the invite. This is the
	// transactional-atomicity assertion.
	rec2 := httptest.NewRecorder()
	h.HandleExchangeInvite(rec2, httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second exchange status=%d, want 200 (invite must remain redeemable after first failure); body=%s",
			rec2.Code, rec2.Body.String())
	}
	var resp exchangeInviteResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.UserID != "dogfood-dev-regress" || resp.Role != auth.RoleDeveloper {
		t.Errorf("second-exchange resp=%+v want user_id=dogfood-dev-regress role=developer", resp)
	}
	if _, err := h.KeyStore.LookupAPIKey(resp.APIKey); err != nil {
		t.Errorf("issued api key not found in keystore: %v", err)
	}
}

// TestHandleExchangeInvitePgConcurrent uses a real Postgres testcontainer to
// validate that the UPDATE...WHERE used_at IS NULL fence is enforced by the
// database, not just by our in-memory store.
func TestHandleExchangeInvitePgConcurrent(t *testing.T) {
	ctx := context.Background()
	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available: %v", err)
		return
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, db.Migrations, "migrations"); err != nil {
		t.Fatal(err)
	}

	invites := auth.NewPgInviteStore(pool)
	keys := auth.NewPgKeyStore(pool)

	token := strings.Repeat("a", 64)
	tokenHash := sha256Hex(token)
	if err := invites.Create(ctx, auth.Invite{
		TokenHash: tokenHash,
		Role:      auth.RoleDeveloper,
		TeamID:    "dogfood",
		UserID:    "pg-racer",
		CreatedBy: "admin",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		KeyStore:       keys,
		PgInvites:      invites,
		PgAuditW:       &stubAuditWriter{},
		TeamRegistered: func(string) bool { return true },
	}

	body := fmt.Sprintf(`{"invite_token":%q,"machine_id":"m"}`, token)

	const goroutines = 6
	var (
		wg    sync.WaitGroup
		ok410 atomic.Int32
		ok200 atomic.Int32
		other atomic.Int32
		start = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/api/v1/lp/exchange-invite", strings.NewReader(body))
			h.HandleExchangeInvite(rec, req)
			switch rec.Code {
			case http.StatusOK:
				ok200.Add(1)
			case http.StatusGone:
				ok410.Add(1)
			default:
				other.Add(1)
				t.Logf("unexpected status=%d body=%s", rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok200.Load() != 1 {
		t.Errorf("200 count=%d, want 1", ok200.Load())
	}
	if ok410.Load() != goroutines-1 {
		t.Errorf("410 count=%d, want %d", ok410.Load(), goroutines-1)
	}
	if other.Load() != 0 {
		t.Errorf("unexpected statuses=%d", other.Load())
	}
}
