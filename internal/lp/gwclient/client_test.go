package gwclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFirstByteStateMachineMayRetryInitial(t *testing.T) {
	sm := NewFirstByteStateMachine()
	if !sm.MayRetry() {
		t.Fatal("expected MayRetry() = true initially")
	}
}

func TestFirstByteStateMachineFlushClearsRetry(t *testing.T) {
	sm := NewFirstByteStateMachine()
	sm.FlushFirstByte()
	if sm.MayRetry() {
		t.Fatal("expected MayRetry() = false after FlushFirstByte")
	}
	if !sm.HasFlushed() {
		t.Fatal("expected HasFlushed() = true after FlushFirstByte")
	}
}

func TestFirstByteStateMachinePreFirstByte5xxRetry(t *testing.T) {
	sm := NewFirstByteStateMachine()
	// Simulate: GW 5xx before first byte → we may retry.
	if !sm.MayRetry() {
		t.Fatal("pre-first-byte 5xx should allow retry")
	}
	// FlushFirstByte is NOT called because no data was sent to agent.
	// GW retried, second attempt succeeds → FlushFirstByte on first data.
	sm.FlushFirstByte()
	if sm.MayRetry() {
		t.Fatal("after flush, retry must be cleared")
	}
}

func TestFirstByteStateMachinePostFirstByteNoRetry(t *testing.T) {
	sm := NewFirstByteStateMachine()
	// Simulate: flushed first byte to agent, then GW returns 5xx.
	sm.FlushFirstByte()
	if sm.MayRetry() {
		t.Fatal("post-first-byte must not allow retry — status is fixed")
	}
	// Agent sees aicg.error SSE event instead.
}

// retryTestServer returns an httptest.Server that responds with the given
// status codes in order. It records each request body for inspection.
func retryTestServer(codes []int) (*httptest.Server, *[]*retryRecord) {
	var records []*retryRecord
	var mu sync.Mutex
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		mu.Lock()
		records = append(records, &retryRecord{
			body:          bodyBytes,
			contentLength: r.ContentLength,
			headers:       r.Header,
		})
		mu.Unlock()
		idx := int(count.Add(1)) - 1
		if idx < len(codes) {
			w.WriteHeader(codes[idx])
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	return srv, &records
}

type retryRecord struct {
	body          []byte
	contentLength int64
	headers       http.Header
}

func TestForwardRetry5xxThen200(t *testing.T) {
	codes := []int{http.StatusInternalServerError, http.StatusOK}
	srv, records := retryTestServer(codes)
	defer srv.Close()

	client := &Client{
		HTTPClient: srv.Client(),
		GwURL:      srv.URL,
		APIKey:     "test-key",
	}

	req := &ForwardRequest{
		Envelope: json.RawMessage(`{"trace_id":"t1"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     json.RawMessage(`{"model":"claude-sonnet-4-6"}`),
		},
	}

	resp, err := client.Forward(t.Context(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()

	if len(*records) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(*records))
	}

	first := (*records)[0]
	second := (*records)[1]

	if first.contentLength != int64(len(first.body)) {
		t.Fatalf("first request ContentLength=%d, body len=%d", first.contentLength, len(first.body))
	}
	if second.contentLength != int64(len(second.body)) {
		t.Fatalf("second request ContentLength=%d, body len=%d", second.contentLength, len(second.body))
	}

	if !bytes.Equal(first.body, second.body) {
		t.Fatal("first and second request bodies differ")
	}

	if second.contentLength == 0 {
		t.Fatal("second request ContentLength is 0 — body was not reset")
	}

	// Verify headers are set on both requests.
	if ct := first.headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("first request Content-Type = %q", ct)
	}
	if ct := second.headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("second request Content-Type = %q", ct)
	}
	if auth := second.headers.Get("Authorization"); auth != "Bearer test-key" {
		t.Fatalf("second request Authorization = %q", auth)
	}
}

func TestForwardRetry5xxThen5xx(t *testing.T) {
	codes := []int{http.StatusInternalServerError, http.StatusInternalServerError}
	srv, records := retryTestServer(codes)
	defer srv.Close()

	client := &Client{
		HTTPClient: srv.Client(),
		GwURL:      srv.URL,
		APIKey:     "test-key",
	}

	req := &ForwardRequest{
		Envelope: json.RawMessage(`{"trace_id":"t2"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     json.RawMessage(`{"model":"claude-sonnet-4-6"}`),
		},
	}

	_, err := client.Forward(t.Context(), req)
	if err == nil {
		t.Fatal("expected error after both attempts 5xx")
	}
	if !strings.Contains(err.Error(), "after retry") {
		t.Fatalf("expected error to contain 'after retry', got: %v", err)
	}
	if len(*records) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(*records))
	}
}

func TestForward200SingleAttempt(t *testing.T) {
	codes := []int{http.StatusOK}
	srv, records := retryTestServer(codes)
	defer srv.Close()

	client := &Client{
		HTTPClient: srv.Client(),
		GwURL:      srv.URL,
		APIKey:     "test-key",
	}

	req := &ForwardRequest{
		Envelope: json.RawMessage(`{"trace_id":"t3"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     json.RawMessage(`{"model":"claude-sonnet-4-6"}`),
		},
	}

	resp, err := client.Forward(t.Context(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()

	if len(*records) != 1 {
		t.Fatalf("expected 1 request, got %d", len(*records))
	}
}

func TestForward4xxNoRetry(t *testing.T) {
	codes := []int{http.StatusBadRequest}
	srv, records := retryTestServer(codes)
	defer srv.Close()

	client := &Client{
		HTTPClient: srv.Client(),
		GwURL:      srv.URL,
		APIKey:     "test-key",
	}

	req := &ForwardRequest{
		Envelope: json.RawMessage(`{"trace_id":"t4"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     json.RawMessage(`{"model":"claude-sonnet-4-6"}`),
		},
	}

	resp, err := client.Forward(t.Context(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 status, got %d", resp.StatusCode)
	}
	if len(*records) != 1 {
		t.Fatalf("expected 1 request, got %d", len(*records))
	}
}

func TestForwardRetry5xxBoundedDiagnostics(t *testing.T) {
	// Return 500 for both attempts with a JSON error body containing code and trace_id.
	var mu sync.Mutex
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		mu.Lock()
		callCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error","message":"something broke","trace_id":"trace-diag-001"}`))
	}))
	defer srv.Close()

	client := &Client{
		HTTPClient: srv.Client(),
		GwURL:      srv.URL,
		APIKey:     "test-key",
	}

	req := &ForwardRequest{
		Envelope: json.RawMessage(`{"trace_id":"t5"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     json.RawMessage(`{"model":"claude-sonnet-4-6"}`),
		},
	}

	_, err := client.Forward(t.Context(), req)
	if err == nil {
		t.Fatal("expected error after both attempts 5xx")
	}
	if !strings.Contains(err.Error(), "after retry") {
		t.Fatalf("expected error to contain 'after retry', got: %v", err)
	}
	// The error must include bounded GW error metadata (code and trace_id).
	if !strings.Contains(err.Error(), "code=internal_error") {
		t.Errorf("expected error to contain 'code=internal_error', got: %v", err)
	}
	if !strings.Contains(err.Error(), "trace_id=trace-diag-001") {
		t.Errorf("expected error to contain 'trace_id=trace-diag-001', got: %v", err)
	}
	// Must not contain the full message body.
	if strings.Contains(err.Error(), "something broke") {
		t.Errorf("error must not leak the full error message body: %v", err)
	}
}

func TestGetCostSummaryEncodesParams(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	client := &Client{
		HTTPClient: srv.Client(),
		GwURL:      srv.URL,
		APIKey:     "test-key",
	}

	_, err := client.GetCostSummary("team", "2026-05-01", "2026-06-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(capturedURL, "dim=team") {
		t.Errorf("expected dim=team in URL, got: %s", capturedURL)
	}
	if !strings.Contains(capturedURL, "from=2026-05-01") {
		t.Errorf("expected from=2026-05-01 in URL, got: %s", capturedURL)
	}
	if !strings.Contains(capturedURL, "to=2026-06-01") {
		t.Errorf("expected to=2026-06-01 in URL, got: %s", capturedURL)
	}
}

func TestGetCostSummaryEmptyParams(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	client := &Client{
		HTTPClient: srv.Client(),
		GwURL:      srv.URL,
		APIKey:     "test-key",
	}

	_, err := client.GetCostSummary("", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(capturedURL, "dim=") {
		t.Errorf("expected no dim param when empty, got: %s", capturedURL)
	}
	if strings.Contains(capturedURL, "from=") {
		t.Errorf("expected no from param when empty, got: %s", capturedURL)
	}
}

func TestGetCostSummaryPartialParams(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	client := &Client{
		HTTPClient: srv.Client(),
		GwURL:      srv.URL,
		APIKey:     "test-key",
	}

	_, err := client.GetCostSummary("model", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(capturedURL, "dim=model") {
		t.Errorf("expected dim=model in URL, got: %s", capturedURL)
	}
	if strings.Contains(capturedURL, "from=") {
		t.Errorf("expected no from param, got: %s", capturedURL)
	}

	_, err = client.GetCostSummary("", "", "2026-06-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(capturedURL, "to=2026-06-01") {
		t.Errorf("expected to=2026-06-01 in URL, got: %s", capturedURL)
	}
}

// recordedRequest captures what a fake GW server saw for one request.
type recordedRequest struct {
	method  string
	path    string
	headers http.Header
	body    []byte
}

// inviteFakeServer returns an httptest.Server that records the first request
// and replies with the given status code and body. It is used by ExchangeInvite
// and CreateInvite tests; behavior is intentionally minimal — see the request
// captured in `*captured` after the call.
func inviteFakeServer(status int, respBody string) (*httptest.Server, *recordedRequest) {
	captured := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.headers = r.Header.Clone()
		captured.body = bodyBytes
		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, respBody)
	}))
	return srv, captured
}

func TestExchangeInviteSuccess200(t *testing.T) {
	respBody := `{"api_key":"u-key-deadbeef","user_id":"dogfood-dev-9","role":"developer","team_id":"dogfood","trace_id":"trace-1"}`
	srv, captured := inviteFakeServer(http.StatusOK, respBody)
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), GwURL: srv.URL, APIKey: "should-not-be-sent"}
	apiKey, userID, role, teamID, err := c.ExchangeInvite("token-abc", "machine-xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if apiKey != "u-key-deadbeef" {
		t.Errorf("apiKey = %q, want u-key-deadbeef", apiKey)
	}
	if userID != "dogfood-dev-9" {
		t.Errorf("userID = %q, want dogfood-dev-9", userID)
	}
	if role != "developer" {
		t.Errorf("role = %q, want developer", role)
	}
	if teamID != "dogfood" {
		t.Errorf("teamID = %q, want dogfood", teamID)
	}

	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.path != "/api/v1/lp/exchange-invite" {
		t.Errorf("path = %q, want /api/v1/lp/exchange-invite", captured.path)
	}
	// Anonymous endpoint — Authorization header must NOT be sent even when
	// the client has an APIKey stored.
	if auth := captured.headers.Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q, want empty (anonymous endpoint)", auth)
	}
	if ct := captured.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var sent struct {
		InviteToken string `json:"invite_token"`
		MachineID   string `json:"machine_id"`
	}
	if err := json.Unmarshal(captured.body, &sent); err != nil {
		t.Fatalf("decode sent body: %v (body=%s)", err, string(captured.body))
	}
	if sent.InviteToken != "token-abc" || sent.MachineID != "machine-xyz" {
		t.Errorf("sent body = %+v, want invite_token=token-abc machine_id=machine-xyz", sent)
	}
}

func TestExchangeInvite4xxSurfacesGwCode(t *testing.T) {
	// GW returns 410 token_consumed; the client error message must include
	// `code=token_consumed` so the CLI / caller can distinguish failure modes
	// without exposing the raw error body to logs.
	respBody := `{"code":"token_consumed","message":"invite_token already consumed","trace_id":"trace-410"}`
	srv, _ := inviteFakeServer(http.StatusGone, respBody)
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), GwURL: srv.URL}
	_, _, _, _, err := c.ExchangeInvite("burned-token", "machine-xyz")
	if err == nil {
		t.Fatal("expected error on 410, got nil")
	}
	if !strings.Contains(err.Error(), "status 410") {
		t.Errorf("err = %v, want it to mention status 410", err)
	}
	if !strings.Contains(err.Error(), "code=token_consumed") {
		t.Errorf("err = %v, want it to surface code=token_consumed", err)
	}
	if !strings.Contains(err.Error(), "trace_id=trace-410") {
		t.Errorf("err = %v, want it to surface trace_id=trace-410", err)
	}
}

func TestExchangeInvite5xx(t *testing.T) {
	respBody := `{"code":"internal_error","trace_id":"trace-500"}`
	srv, _ := inviteFakeServer(http.StatusInternalServerError, respBody)
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), GwURL: srv.URL}
	_, _, _, _, err := c.ExchangeInvite("token-x", "machine-y")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("err = %v, want status 500 in message", err)
	}
	if !strings.Contains(err.Error(), "code=internal_error") {
		t.Errorf("err = %v, want code=internal_error in message", err)
	}
}

func TestCreateInviteSuccess201(t *testing.T) {
	expiresAt := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	respBody, _ := json.Marshal(map[string]any{
		"invite_token": "raw-hex-token-001",
		"user_id":      "dogfood-dev-9",
		"team_id":      "dogfood",
		"role":         "developer",
		"expires_at":   expiresAt,
		"trace_id":     "trace-create",
	})
	srv, captured := inviteFakeServer(http.StatusCreated, string(respBody))
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), GwURL: srv.URL, APIKey: "admin-bearer"}
	token, gotExpires, err := c.CreateInvite("dogfood-dev-9", "dogfood", "developer", 24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "raw-hex-token-001" {
		t.Errorf("token = %q, want raw-hex-token-001", token)
	}
	if !gotExpires.Equal(expiresAt) {
		t.Errorf("expiresAt = %v, want %v", gotExpires, expiresAt)
	}

	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.path != "/api/v1/admin/invites" {
		t.Errorf("path = %q, want /api/v1/admin/invites", captured.path)
	}
	if auth := captured.headers.Get("Authorization"); auth != "Bearer admin-bearer" {
		t.Errorf("Authorization = %q, want Bearer admin-bearer", auth)
	}
	if ct := captured.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var sent map[string]any
	if err := json.Unmarshal(captured.body, &sent); err != nil {
		t.Fatalf("decode sent body: %v (body=%s)", err, string(captured.body))
	}
	if sent["user_id"] != "dogfood-dev-9" {
		t.Errorf("sent user_id = %v, want dogfood-dev-9", sent["user_id"])
	}
	if sent["team_id"] != "dogfood" {
		t.Errorf("sent team_id = %v, want dogfood", sent["team_id"])
	}
	if sent["role"] != "developer" {
		t.Errorf("sent role = %v, want developer", sent["role"])
	}
	// json numeric -> float64; compare via conversion.
	if v, ok := sent["ttl_hours"].(float64); !ok || int(v) != 24 {
		t.Errorf("sent ttl_hours = %v, want 24", sent["ttl_hours"])
	}
}

func TestCreateInvite4xxSurfacesGwCode(t *testing.T) {
	respBody := `{"code":"forbidden","message":"caller is not platform_admin","trace_id":"trace-403"}`
	srv, _ := inviteFakeServer(http.StatusForbidden, respBody)
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), GwURL: srv.URL, APIKey: "not-admin"}
	_, _, err := c.CreateInvite("u", "t", "developer", 24*time.Hour)
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "status 403") {
		t.Errorf("err = %v, want status 403 in message", err)
	}
	if !strings.Contains(err.Error(), "code=forbidden") {
		t.Errorf("err = %v, want code=forbidden in message", err)
	}
}

func TestCreateInvite5xx(t *testing.T) {
	respBody := `{"code":"internal_error","trace_id":"trace-x500"}`
	srv, _ := inviteFakeServer(http.StatusInternalServerError, respBody)
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), GwURL: srv.URL, APIKey: "admin"}
	_, _, err := c.CreateInvite("u", "t", "developer", 24*time.Hour)
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("err = %v, want status 500 in message", err)
	}
	if !strings.Contains(err.Error(), "code=internal_error") {
		t.Errorf("err = %v, want code=internal_error in message", err)
	}
}

func TestCreateInviteTTLConversion(t *testing.T) {
	// Verify that time.Duration is rounded down to whole hours when encoding
	// ttl_hours in the request body. The GW enforces (0, 720] on the resulting
	// integer; this test only checks the conversion the client performs.
	cases := []struct {
		name   string
		input  time.Duration
		expect int
	}{
		{"exactly 1h", time.Hour, 1},
		{"1h30m rounds down", 90 * time.Minute, 1},
		{"24h", 24 * time.Hour, 24},
		{"max 720h", 720 * time.Hour, 720},
		{"sub-hour rounds to zero", 30 * time.Minute, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, captured := inviteFakeServer(http.StatusCreated, `{"invite_token":"t","expires_at":"2026-05-13T00:00:00Z"}`)
			defer srv.Close()
			c := &Client{HTTPClient: srv.Client(), GwURL: srv.URL, APIKey: "admin"}
			_, _, err := c.CreateInvite("u", "t", "developer", tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var sent map[string]any
			if err := json.Unmarshal(captured.body, &sent); err != nil {
				t.Fatalf("decode sent body: %v", err)
			}
			if v, ok := sent["ttl_hours"].(float64); !ok || int(v) != tc.expect {
				t.Errorf("ttl_hours = %v, want %d", sent["ttl_hours"], tc.expect)
			}
		})
	}
}
