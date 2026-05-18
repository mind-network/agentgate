// Package dashboard provides the P0 dashboard read APIs.
package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"

	"agentgate/internal/gw/edge"
)

// APIHandler serves dashboard read endpoints.
// P0: in-memory store. P1+: Postgres-backed.
type APIHandler struct {
	CostEvents    []CostSummaryRow
	RoutingEvents []RoutingEventRow
}

// CostSummaryRow is a response row for GET /api/v1/cost/summary.
type CostSummaryRow struct {
	Bucket      string `json:"bucket"`
	DimValue    string `json:"dim_value"`
	CostCents   int    `json:"cost_cents"`
	InputTokens int    `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	NRequests   int    `json:"n_requests"`
	NFailed     int    `json:"n_failed"`
}

// RoutingEventRow is a response row for GET /api/v1/routing/events.
type RoutingEventRow struct {
	TraceID    string `json:"trace_id"`
	AttemptNo  int    `json:"attempt_no"`
	Pool       string `json:"pool_selected"`
	MemberJSON string `json:"member_selected"`
	TenantID   string `json:"-"`
	UserID     string `json:"-"`
}

// Register registers dashboard routes.
func (h *APIHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/cost/summary", h.handleCostSummary)
	mux.HandleFunc("/api/v1/routing/events", h.handleRoutingEvents)
}

func (h *APIHandler) handleCostSummary(w http.ResponseWriter, r *http.Request) {
	userID := edge.GetUserID(r.Context())
	teamID := edge.GetTeamID(r.Context())
	role := edge.GetRole(r.Context())
	_ = userID

	queryTeam := r.URL.Query().Get("team_id")
	queryUser := r.URL.Query().Get("user_id")

	// IDOR guard: developer only sees own data.
	if role == "developer" {
		if queryUser != "" && queryUser != userID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-user access denied"})
			return
		}
		if queryTeam != "" && queryTeam != teamID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-team access denied"})
			return
		}
	}

	var rows []CostSummaryRow
	for _, row := range h.CostEvents {
		if queryTeam != "" && !strings.Contains(row.DimValue, queryTeam) {
			continue
		}
		rows = append(rows, row)
	}
	if rows == nil {
		rows = []CostSummaryRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *APIHandler) handleRoutingEvents(w http.ResponseWriter, r *http.Request) {
	userID := edge.GetUserID(r.Context())
	teamID := edge.GetTeamID(r.Context())
	role := edge.GetRole(r.Context())

	traceID := r.URL.Query().Get("trace_id")
	if traceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "trace_id is required"})
		return
	}

	var rows []RoutingEventRow
	for _, row := range h.RoutingEvents {
		if row.TraceID != traceID {
			continue
		}
		// IDOR guard: developer only sees own traces.
		if role == "developer" && row.UserID != "" && row.UserID != userID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-user access denied"})
			return
		}
		// team_admin only sees own team's traces.
		if role == "team_admin" && row.TenantID != "" && row.TenantID != teamID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-team access denied"})
			return
		}
		rows = append(rows, row)
	}
	if rows == nil {
		rows = []RoutingEventRow{}
	}
	_ = userID
	writeJSON(w, http.StatusOK, rows)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
