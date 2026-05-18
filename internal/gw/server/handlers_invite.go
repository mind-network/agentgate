package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/auth"
	"agentgate/internal/gw/edge"
)

// Invite handler config — error codes mirror exchange-setup conventions.
// See HANDOFF-023 § Context for the canonical HTTP/code table.

const (
	maxInviteTTLHours = 720
)

// createInviteRequest is the body of POST /api/v1/admin/invites.
type createInviteRequest struct {
	UserID   string `json:"user_id"`
	TeamID   string `json:"team_id"`
	Role     string `json:"role"`
	TTLHours int    `json:"ttl_hours"`
}

// createInviteResponse is the response of POST /api/v1/admin/invites.
type createInviteResponse struct {
	InviteToken string    `json:"invite_token"`
	UserID      string    `json:"user_id"`
	TeamID      string    `json:"team_id"`
	Role        string    `json:"role"`
	ExpiresAt   time.Time `json:"expires_at"`
	TraceID     string    `json:"trace_id"`
}

// HandleAdminCreateInvite mints a one-shot invite token for a target user_id.
// Caller must already have role=platform_admin (enforced by RequireRole at
// the route level). The plaintext token is returned exactly once in the
// response body; only its sha256 hash is persisted.
func (h *Handler) HandleAdminCreateInvite(w http.ResponseWriter, r *http.Request) {
	traceID := edge.GetTraceID(r.Context())
	if traceID == "" {
		traceID = edge.MintTraceID()
	}

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

	var req createInviteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error(), traceID)
		return
	}

	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "user_id is required", traceID)
		return
	}
	if req.TeamID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "team_id is required", traceID)
		return
	}
	if !isAllowedInviteRole(req.Role) {
		writeError(w, http.StatusBadRequest, "bad_request", "role must be one of developer, team_admin, platform_admin", traceID)
		return
	}
	if req.TTLHours <= 0 || req.TTLHours > maxInviteTTLHours {
		writeError(w, http.StatusBadRequest, "bad_request", "ttl_hours must be in (0, 720]", traceID)
		return
	}
	if h.TeamRegistered == nil || !h.TeamRegistered(req.TeamID) {
		writeError(w, http.StatusBadRequest, "unknown_team", "team_id is not registered in teams.yaml", traceID)
		return
	}
	if h.PgInvites == nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "invite store not configured", traceID)
		return
	}

	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to generate token", traceID)
		return
	}
	token := hex.EncodeToString(buf[:])
	tokenHash := sha256Hex(token)
	expiresAt := time.Now().Add(time.Duration(req.TTLHours) * time.Hour)

	createdBy := edge.GetUserID(r.Context())
	if createdBy == "" {
		createdBy = "platform_admin"
	}

	inv := auth.Invite{
		TokenHash: tokenHash,
		Role:      req.Role,
		TeamID:    req.TeamID,
		UserID:    req.UserID,
		CreatedBy: createdBy,
		ExpiresAt: expiresAt,
	}
	if err := h.PgInvites.Create(r.Context(), inv); err != nil {
		slog.Error("invite create failed", "trace_id", traceID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to persist invite", traceID)
		return
	}

	h.writeInviteAudit(r.Context(), audit.EventInviteCreated, traceID, createdBy,
		"platform", req.TeamID, map[string]any{
			"target_user_id": req.UserID,
			"role":           req.Role,
			"ttl_hours":      req.TTLHours,
			"expires_at":     expiresAt.UTC(),
			"token_hash_pfx": tokenHash[:16],
		})

	slog.Info("invite created", "trace_id", traceID, "created_by", createdBy,
		"target_user_id", req.UserID, "role", req.Role, "team_id", req.TeamID)

	writeJSON(w, http.StatusCreated, createInviteResponse{
		InviteToken: token,
		UserID:      req.UserID,
		TeamID:      req.TeamID,
		Role:        req.Role,
		ExpiresAt:   expiresAt.UTC(),
		TraceID:     traceID,
	})
}

// exchangeInviteRequest is the body of POST /api/v1/lp/exchange-invite.
type exchangeInviteRequest struct {
	InviteToken string `json:"invite_token"`
	MachineID   string `json:"machine_id"`
}

// exchangeInviteResponse is the response of POST /api/v1/lp/exchange-invite.
type exchangeInviteResponse struct {
	APIKey  string `json:"api_key"`
	UserID  string `json:"user_id"`
	Role    string `json:"role"`
	TeamID  string `json:"team_id"`
	TraceID string `json:"trace_id"`
}

// HandleExchangeInvite consumes an invite token and issues a long-lived user
// API key in a single database transaction. The endpoint is anonymous
// (middleware skip list); validation is entirely token-based here.
//
// Transaction boundary: invite consume + api_keys insert commit or roll back
// together. If api_key issuance fails for any reason, the invite remains
// redeemable rather than being burned without a usable key. Audit writes
// happen AFTER the transaction commits — audit failures must not roll back
// user-visible key issuance, matching exchange-setup semantics and the
// handoff's audit boundary requirement.
func (h *Handler) HandleExchangeInvite(w http.ResponseWriter, r *http.Request) {
	traceID := edge.MintTraceID()

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

	var req exchangeInviteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error(), traceID)
		return
	}
	if req.InviteToken == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "invite_token is required", traceID)
		return
	}
	if req.MachineID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "machine_id is required", traceID)
		return
	}
	if h.PgInvites == nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "invite store not configured", traceID)
		return
	}

	// Generate the user API key (32 random bytes → hex → bcrypt hash) before
	// opening the transaction. Bcrypt is intentionally outside the tx: it is
	// slow (~100ms), and the constant-time delay applies to bad-token
	// requests too, a small hedge against timing-based token probing.
	var keyBuf [32]byte
	if _, err := rand.Read(keyBuf[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to generate API key", traceID)
		return
	}
	cleartext := hex.EncodeToString(keyBuf[:])
	hashBytes, err := bcrypt.GenerateFromPassword([]byte(cleartext), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to hash API key", traceID)
		return
	}

	tokenHash := sha256Hex(req.InviteToken)
	inv, err := h.PgInvites.ConsumeAndIssueKey(r.Context(), tokenHash, req.MachineID, string(hashBytes))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInviteNotFound):
			writeError(w, http.StatusForbidden, "bad_token", "invalid invite_token", traceID)
		case errors.Is(err, auth.ErrInviteConsumed):
			writeError(w, http.StatusGone, "token_consumed", "invite_token already consumed", traceID)
		case errors.Is(err, auth.ErrInviteExpired):
			writeError(w, http.StatusGone, "token_expired", "invite_token expired", traceID)
		default:
			slog.Error("invite exchange failed", "trace_id", traceID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "invite exchange failed", traceID)
		}
		return
	}

	// Audit is written outside the database transaction. Failure to log must
	// not roll back the key issuance — matching exchange-setup semantics.
	h.writeInviteAudit(r.Context(), audit.EventInviteExchanged, traceID, inv.UserID,
		"platform", inv.TeamID, map[string]any{
			"role":           inv.Role,
			"machine_id":     req.MachineID,
			"created_by":     inv.CreatedBy,
			"token_hash_pfx": tokenHash[:16],
		})

	slog.Info("invite exchanged", "trace_id", traceID, "user_id", inv.UserID,
		"role", inv.Role, "team_id", inv.TeamID)

	writeJSON(w, http.StatusOK, exchangeInviteResponse{
		APIKey:  cleartext,
		UserID:  inv.UserID,
		Role:    inv.Role,
		TeamID:  inv.TeamID,
		TraceID: traceID,
	})
}

func isAllowedInviteRole(role string) bool {
	switch role {
	case auth.RoleDeveloper, auth.RoleTeamAdmin, auth.RolePlatformAdmin:
		return true
	default:
		return false
	}
}

func (h *Handler) writeInviteAudit(ctx context.Context, et audit.EventType, traceID, userID, tenantID, teamID string, detail map[string]any) {
	if h.PgAuditW == nil {
		return
	}
	ev := audit.NewEvent(et, traceID, tenantID, userID, teamID)
	if detail != nil {
		raw, err := json.Marshal(detail)
		if err == nil {
			ev.Detail = raw
		}
	}
	h.PgAuditW.Write(ev)
}
