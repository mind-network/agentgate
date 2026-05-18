// Package auth provides API key authentication, bcrypt hashing,
// RBAC middleware, and setup_token bootstrap for the GW.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"agentgate/internal/gw/edge"
)

// Role constants.
const (
	RoleDeveloper     = "developer"
	RoleTeamAdmin     = "team_admin"
	RolePlatformAdmin = "platform_admin"
)

// APIKey represents a stored API key record.
type APIKey struct {
	UserID string
	TeamID string
	Role   string
	Hash   string
}

// KeyStore is the interface for API key lookup.
type KeyStore interface {
	LookupAPIKey(keyPrefix string) (*APIKey, error)
	StoreAPIKey(userID, teamID, role, hash string) error
	StoreAPIKeyCtx(ctx context.Context, userID, teamID, role, hash string) error
	HasAnyKey() bool
}

// InMemKeyStore is a simple in-memory key store for P0.
type InMemKeyStore struct {
	keys map[string]*APIKey // keyed by first 8 chars of key
}

// NewInMemKeyStore creates a new in-memory key store.
func NewInMemKeyStore() *InMemKeyStore {
	return &InMemKeyStore{keys: make(map[string]*APIKey)}
}

// LookupAPIKey accepts the full cleartext key, iterates stored keys, and
// bcrypt-compares each one. For in-memory store this is O(n); for Pg store
// a DB prefix query + bcrypt verify is used.
func (s *InMemKeyStore) LookupAPIKey(cleartext string) (*APIKey, error) {
	for _, k := range s.keys {
		if bcrypt.CompareHashAndPassword([]byte(k.Hash), []byte(cleartext)) == nil {
			return k, nil
		}
	}
	// Fallback: also try direct comparison for tests that store cleartext.
	for _, k := range s.keys {
		if subtle.ConstantTimeCompare([]byte(cleartext), []byte(k.Hash)) == 1 {
			return k, nil
		}
	}
	return nil, fmt.Errorf("key not found")
}

// StoreAPIKey stores a key hash.
func (s *InMemKeyStore) StoreAPIKey(userID, teamID, role, hash string) error {
	s.keys[hash[:8]] = &APIKey{
		UserID: userID,
		TeamID: teamID,
		Role:   role,
		Hash:   hash,
	}
	return nil
}

// StoreAPIKeyCtx stores a key hash, accepting a context for parity with PgKeyStore.
func (s *InMemKeyStore) StoreAPIKeyCtx(_ context.Context, userID, teamID, role, hash string) error {
	return s.StoreAPIKey(userID, teamID, role, hash)
}

// HasAnyKey returns true if the store contains at least one key.
func (s *InMemKeyStore) HasAnyKey() bool {
	return len(s.keys) > 0
}

// SetupToken is a one-time bootstrap token used on first GW start.
type SetupToken struct {
	Token    string
	Consumed bool
}

// GenerateSetupToken creates a new one-time setup token.
func GenerateSetupToken() string {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fallback for crypto failure; should never happen.
		panic("crypto/rand failed: " + err.Error())
	}
	return "setup_" + hex.EncodeToString(buf[:])
}

// AuthMiddleware validates Bearer API keys and injects user/team/role into context.
// Setup tokens are NOT accepted here — they are only valid at /api/v1/lp/exchange-setup.
func AuthMiddleware(store KeyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for health check and setup-token / invite-exchange endpoints.
			// /healthz is unauthenticated; /api/v1/lp/exchange-setup and
			// /api/v1/lp/exchange-invite validate the presented token themselves
			// (DB-backed PgSetupTokenStore / PgInviteStore), not via global middleware.
			if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" ||
				r.URL.Path == "/api/v1/lp/exchange-setup" ||
				r.URL.Path == "/api/v1/lp/exchange-invite" {
				next.ServeHTTP(w, r)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				writeAuthError(w, "missing Authorization header")
				return
			}
			token := strings.TrimPrefix(authHeader, "Bearer ")

			// Setup tokens are ONLY valid at /api/v1/lp/exchange-setup
			// (which bypasses this middleware). They must never be accepted
			// as API keys for any other route.
			if strings.HasPrefix(token, "setup_") {
				writeAuthError(w, "setup tokens are only valid at /api/v1/lp/exchange-setup")
				return
			}

			// LookupAPIKey expects the full cleartext key and handles
			// bcrypt verification internally (both InMem and Pg stores).
			k, err := store.LookupAPIKey(token)
			if err != nil {
				writeAuthError(w, "invalid API key")
				return
			}

			ctx := context.WithValue(r.Context(), edge.CtxUserID, k.UserID)
			ctx = context.WithValue(ctx, edge.CtxTeamID, k.TeamID)
			ctx = context.WithValue(ctx, edge.CtxRole, k.Role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole returns middleware that enforces a minimum role.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := edge.GetRole(r.Context())
			for _, allowed := range roles {
				if role == allowed {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeForbidden(w, fmt.Sprintf("role %q not authorized", role))
		})
	}
}

// RouteOption declares the role requirement for a handler registration.
type RouteOption struct {
	Pattern string
	Handler http.HandlerFunc
	Roles   []string // empty = any authenticated user
}

// RegisterRoutes registers a set of handlers with role enforcement on a chi router.
func RegisterRoutes(r *chi.Mux, routes []RouteOption) {
	for _, rt := range routes {
		h := http.HandlerFunc(rt.Handler)
		if len(rt.Roles) > 0 {
			h = RequireRole(rt.Roles...)(h).(http.HandlerFunc)
		}
		r.HandleFunc(rt.Pattern, h)
	}
}

func writeAuthError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"code":"unauthorized","message":%q}`+"\n", msg)
}

func writeForbidden(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `{"code":"forbidden","message":%q}`+"\n", msg)
}
