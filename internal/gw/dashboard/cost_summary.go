// Package dashboard provides the P0 dashboard read APIs.
package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CostSummaryHandler serves the DB-backed /api/v1/cost/summary endpoint.
type CostSummaryHandler struct {
	Pool *pgxpool.Pool
}

// NewCostSummaryHandler creates a handler bound to the given pool.
func NewCostSummaryHandler(pool *pgxpool.Pool) *CostSummaryHandler {
	return &CostSummaryHandler{Pool: pool}
}

// ServeHTTP handles GET /api/v1/cost/summary with dim, from, and to.
func (h *CostSummaryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	dim := r.URL.Query().Get("dim")
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")

	// Validate and resolve dimension column.
	var dimCol string
	if dim == "" {
		dimCol = "team_id"
	} else {
		switch dim {
		case "team":
			dimCol = "team_id"
		case "user":
			dimCol = "user_id"
		case "repo":
			dimCol = "repo_id"
		case "model":
			dimCol = "model"
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid dim: must be team, user, repo, or model"})
			return
		}
	}

	// Parse date strings into typed UTC time.Time boundaries so date-only filters
	// do not depend on the Postgres session timezone.
	var fromTime, toTime time.Time
	if from != "" {
		parsed, err := time.Parse("2006-01-02", from)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid from: must be YYYY-MM-DD"})
			return
		}
		fromTime = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC)
	}
	if to != "" {
		parsed, err := time.Parse("2006-01-02", to)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid to: must be YYYY-MM-DD"})
			return
		}
		toTime = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC)
	}

	query := fmt.Sprintf(`SELECT COALESCE(%[1]s,''), COALESCE(%[1]s,''),
		sum(cost_cents), sum(input_tokens), sum(output_tokens), count(*),
		count(*) FILTER (WHERE NOT success)
		FROM cost_event`, dimCol)

	var args []any
	argN := 1
	if from != "" && to != "" {
		query += fmt.Sprintf(" WHERE event_at >= $%d AND event_at < $%d", argN, argN+1)
		args = append(args, fromTime, toTime)
	} else if from != "" {
		query += fmt.Sprintf(" WHERE event_at >= $%d", argN)
		args = append(args, fromTime)
	} else if to != "" {
		query += fmt.Sprintf(" WHERE event_at < $%d", argN)
		args = append(args, toTime)
	}

	query += fmt.Sprintf(" GROUP BY %s ORDER BY sum(cost_cents) DESC, COALESCE(%s, '') ASC LIMIT 100", dimCol, dimCol)

	rows, err := h.Pool.Query(r.Context(), query, args...)
	if err != nil {
		_, _ = fmt.Fprint(w, "[]")
		return
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var dimVal string
		var costCents, inTok, outTok, nReq, nFail int
		if err := rows.Scan(&dimVal, &dimVal, &costCents, &inTok, &outTok, &nReq, &nFail); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"bucket": dimVal, "dim_value": dimVal,
			"cost_cents": costCents, "input_tokens": inTok, "output_tokens": outTok,
			"n_requests": nReq, "n_failed": nFail,
		})
	}
	if out == nil {
		_, _ = fmt.Fprint(w, "[]")
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}
