// Package server implements the GW HTTP handlers and ingress pipeline.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"golang.org/x/crypto/bcrypt"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/auth"
	"agentgate/internal/gw/edge"
	"agentgate/internal/gw/provider"
	"agentgate/internal/gw/routing"
)

// ForwardRequest is the body of POST /v1/agent/forward (§5.4.1).
type ForwardRequest struct {
	Envelope json.RawMessage `json:"envelope"`
	Wire     WirePayload     `json:"wire"`
}

// WirePayload carries the upstream protocol and body.
type WirePayload struct {
	Protocol string          `json:"protocol"`
	Stream   bool            `json:"stream"`
	Body     json.RawMessage `json:"body"`
}

// ErrorResponse is the standard GW error body.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id"`
}

// Handler holds dependencies for GW HTTP handlers.
type Handler struct {
	Pipeline      *Pipeline
	KeyStore      auth.KeyStore
	SetupToken    *auth.SetupToken
	PgKeyStore    *auth.PgKeyStore
	PgSetupTokens *auth.PgSetupTokenStore
	PgInvites     auth.InviteStore
	PgAuditW      auditWriter
	// TeamRegistered reports whether team_id is present in identity/teams.yaml.
	// Wired by main.go to a closure over the live identity config. Nil means
	// "no registry available" — handlers treat that as "reject all".
	TeamRegistered func(teamID string) bool
}

// auditWriter is the minimal audit-event sink used by invite handlers.
// Decoupled from a concrete pg writer so tests can substitute an in-memory
// implementation without spinning up Postgres.
type auditWriter interface {
	Write(ev audit.Event)
}

// NewHandler creates a new Handler.
func NewHandler(pipeline *Pipeline, store auth.KeyStore, st *auth.SetupToken) *Handler {
	return &Handler{Pipeline: pipeline, KeyStore: store, SetupToken: st}
}

// HandleForward handles POST /v1/agent/forward.
func (h *Handler) HandleForward(w http.ResponseWriter, r *http.Request) {
	traceID := edge.GetTraceID(r.Context())

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported", traceID)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read body", traceID)
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req ForwardRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error(), traceID)
		return
	}
	if len(req.Envelope) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "envelope is required", traceID)
		return
	}
	if req.Wire.Protocol == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "wire.protocol is required", traceID)
		return
	}

	result, err := h.Pipeline.Run(r.Context(), &req, traceID)
	if err != nil {
		var upErr *provider.UpstreamError
		if errors.As(err, &upErr) {
			code := fmt.Sprintf("provider_%d", upErr.Status)
			slog.Error("ingress pipeline upstream error", "trace_id", traceID, "upstream_status", upErr.Status, "error", err)
			writeError(w, upErr.Status, code, upErr.Body, traceID)
			return
		}
		var noCand *routing.ErrNoCandidate
		if errors.As(err, &noCand) {
			slog.Warn("ingress pipeline no eligible endpoint", "trace_id", traceID, "error", err)
			writeError(w, http.StatusBadRequest, "no_eligible_endpoint", noCand.Error(), traceID)
			return
		}
		var badReq *BadRequestError
		if errors.As(err, &badReq) {
			slog.Warn("ingress pipeline bad request", "trace_id", traceID, "error", err)
			writeError(w, http.StatusBadRequest, "bad_request", badReq.Error(), traceID)
			return
		}
		slog.Error("ingress pipeline error", "trace_id", traceID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), traceID)
		return
	}

	for k, vs := range result.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(result.StatusCode)

	if result.Body != nil {
		buf := make([]byte, 4096)
		for {
			n, err := result.Body.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if err != nil {
				if err != io.EOF {
					slog.Warn("response body read error", "trace_id", traceID, "error", err)
				}
				return
			}
		}
	}
}

// HandleExchangeSetup exchanges a setup_token for a long-lived platform_admin API key.
// This endpoint is mounted behind the global auth middleware, but the middleware
// skips this path — validation happens here with DB-backed PgSetupTokenStore.
// POST /api/v1/lp/exchange-setup
func (h *Handler) HandleExchangeSetup(w http.ResponseWriter, r *http.Request) {
	traceID := edge.MintTraceID()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported", traceID)
		return
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || len(authHeader) < 8 || authHeader[:7] != "Bearer " {
		writeError(w, http.StatusUnauthorized, "unauthorized", "setup_token required in Authorization header", traceID)
		return
	}
	presented := authHeader[7:]

	// Validate and consume the setup token.
	tokenConsumed := false
	tokenAvailable := false

	// Prefer DB-backed setup token store (PgSetupTokenStore).
	if h.PgSetupTokens != nil {
		tokenHash := sha256Hex(presented)
		consumed, err := h.PgSetupTokens.ConsumeToken(context.Background(), tokenHash)
		if err != nil {
			slog.Error("setup token consume failed", "trace_id", traceID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "token validation failed", traceID)
			return
		}
		if consumed {
			tokenConsumed = true
		}
		// Whether the token was found but already consumed, or never existed.
		if !tokenConsumed {
			// Check if any unconsumed token exists at all.
			if h.PgSetupTokens.HasAnyToken(context.Background()) {
				writeError(w, http.StatusForbidden, "bad_token", "invalid setup_token", traceID)
			} else {
				writeError(w, http.StatusGone, "token_expired", "no setup_token available", traceID)
			}
			return
		}
	} else if h.SetupToken != nil {
		// Fallback to in-memory setup token for test/backward compat.
		if h.SetupToken.Consumed {
			writeError(w, http.StatusGone, "token_expired", "setup_token already consumed", traceID)
			return
		}
		tokenAvailable = true
		if presented == h.SetupToken.Token {
			tokenConsumed = true
			h.SetupToken.Consumed = true
		}
	}

	if !tokenConsumed {
		if tokenAvailable {
			writeError(w, http.StatusForbidden, "bad_token", "invalid setup_token", traceID)
		} else {
			writeError(w, http.StatusGone, "token_expired", "setup_token not available or already consumed", traceID)
		}
		return
	}

	// Generate a long-lived API key (32 random bytes, hex-encoded).
	var keyBytes [32]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to generate API key", traceID)
		return
	}
	cleartext := hex.EncodeToString(keyBytes[:])

	// Bcrypt hash the key for storage.
	hash, err := bcrypt.GenerateFromPassword([]byte(cleartext), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to hash API key", traceID)
		return
	}

	// Persist as platform_admin.
	if err := h.KeyStore.StoreAPIKey("platform_admin", "platform", auth.RolePlatformAdmin, string(hash)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to store API key", traceID)
		return
	}

	slog.Info("setup_token exchanged: platform_admin API key minted", "trace_id", traceID)

	writeJSON(w, http.StatusOK, map[string]string{
		"api_key":  cleartext,
		"user_id":  "platform_admin",
		"team_id":  "platform",
		"role":     auth.RolePlatformAdmin,
		"trace_id": traceID,
		"warning":  "store this key securely; it will not be shown again",
	})
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func writeError(w http.ResponseWriter, status int, code, message, traceID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Code:    code,
		Message: message,
		TraceID: traceID,
	})
}

// HandleBindRepo handles POST /v1/repo/bind.
func (h *Handler) HandleBindRepo(w http.ResponseWriter, r *http.Request) {
	traceID := edge.GetTraceID(r.Context())

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported", traceID)
		return
	}

	var req struct {
		RepoID    string `json:"repo_id"`
		RemoteURL string `json:"remote_url"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON", traceID)
		return
	}

	bindingToken := edge.MintTraceID()[:16]

	writeJSON(w, http.StatusOK, map[string]string{
		"status":        "bound",
		"repo_id":       req.RepoID,
		"binding_token": bindingToken,
		"trace_id":      traceID,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
