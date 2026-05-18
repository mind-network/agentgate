// Package server implements the LP HTTP server.
package server

import (
	"encoding/json"
	"net/http"

	"agentgate/internal/lp/localconfig"
	"agentgate/internal/shared/version"
)

// MetaHandler serves the /_aicg/* endpoints.
type MetaHandler struct {
	SessionID string
}

// Register registers all meta routes on the given mux.
func (h *MetaHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/_aicg/health", h.handleHealth)
	mux.HandleFunc("/_aicg/version", h.handleVersion)
	mux.HandleFunc("/_aicg/whoami", h.handleWhoami)
	mux.HandleFunc("/_aicg/sessions", h.handleSessions)
}

func (h *MetaHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *MetaHandler) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version":  version.String(),
		"semver":   version.Semver,
		"git_sha":  version.GitSHA,
	})
}

func (h *MetaHandler) handleWhoami(w http.ResponseWriter, r *http.Request) {
	creds, err := localconfig.LoadCredentials()
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in", "hint": "run `aicg login`"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"user_id":  creds.UserID,
		"team_id":  creds.TeamID,
		"gw_url":   creds.GwURL,
	})
}

func (h *MetaHandler) handleSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"session_id": h.SessionID,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
