package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentgate/internal/gw/edge"
)

func TestCostSummaryIDORGuard(t *testing.T) {
	h := &APIHandler{}
	mux := http.NewServeMux()
	h.Register(mux)

	// Developer trying to query another team.
	req := httptest.NewRequest("GET", "/api/v1/cost/summary?team_id=other-team", nil)
	req = req.WithContext(contextWithRole(req.Context(), "developer", "dev-user", "my-team"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Errorf("expected 403 for cross-team access, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCostSummaryOwnTeamPasses(t *testing.T) {
	h := &APIHandler{
		CostEvents: []CostSummaryRow{
			{Bucket: "2026-05", DimValue: "my-team", CostCents: 100, NRequests: 5},
		},
	}
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/v1/cost/summary?team_id=my-team", nil)
	req = req.WithContext(contextWithRole(req.Context(), "team_admin", "admin-user", "my-team"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200 for own team, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRoutingEventsRequiresTraceID(t *testing.T) {
	h := &APIHandler{}
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/v1/routing/events", nil)
	req = req.WithContext(contextWithRole(req.Context(), "platform_admin", "admin", "platform"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Errorf("expected 400 for missing trace_id, got %d", rec.Code)
	}
}

func TestRoutingEventsIDORGuard(t *testing.T) {
	h := &APIHandler{
		RoutingEvents: []RoutingEventRow{
			{TraceID: "trace-001", AttemptNo: 1, Pool: "standard", TenantID: "team-alpha", UserID: "user-a"},
			{TraceID: "trace-002", AttemptNo: 1, Pool: "standard", TenantID: "team-beta", UserID: "user-b"},
		},
	}
	mux := http.NewServeMux()
	h.Register(mux)

	// Developer user-a queries trace-001 (own) → 200.
	req := httptest.NewRequest("GET", "/api/v1/routing/events?trace_id=trace-001", nil)
	req = req.WithContext(contextWithRole(req.Context(), "developer", "user-a", "team-alpha"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("user-a querying own trace: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Developer user-a queries trace-002 (other user) → 403.
	req2 := httptest.NewRequest("GET", "/api/v1/routing/events?trace_id=trace-002", nil)
	req2 = req2.WithContext(contextWithRole(req2.Context(), "developer", "user-a", "team-alpha"))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != 403 {
		t.Errorf("user-a querying user-b trace: expected 403, got %d", rec2.Code)
	}
}

// Test helpers

func contextWithRole(parent context.Context, role, userID, teamID string) context.Context {
	return &testCtx{
		Context: parent,
		role:    role,
		userID:  userID,
		teamID:  teamID,
	}
}

type testCtx struct {
	context.Context
	role, userID, teamID string
}

func (c *testCtx) Value(key any) any {
	switch key {
	case edge.CtxRole:
		return c.role
	case edge.CtxUserID:
		return c.userID
	case edge.CtxTeamID:
		return c.teamID
	default:
		return c.Context.Value(key)
	}
}
