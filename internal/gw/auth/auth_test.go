package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"agentgate/internal/gw/edge"
)

func TestSetupTokenRejectedByMiddleware(t *testing.T) {
	// Setup tokens must NOT be accepted as API keys by the global auth middleware.
	// They are only valid at /api/v1/lp/exchange-setup (which bypasses this middleware).
	store := NewInMemKeyStore()

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Use(AuthMiddleware(store))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer setup_test-token-12345678")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("expected 401 for setup token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyAuth(t *testing.T) {
	store := NewInMemKeyStore()
	apiKey := "my-api-key-12345678-abcdefgh"
	_ = store.StoreAPIKey("user-1", "team-1", RoleDeveloper, apiKey)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Use(AuthMiddleware(store))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		userID := edge.GetUserID(r.Context())
		if userID != "user-1" {
			t.Errorf("expected user-1, got %q", userID)
		}
		w.WriteHeader(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestInvalidAPIKey(t *testing.T) {
	store := NewInMemKeyStore()
	_ = store.StoreAPIKey("user-1", "team-1", RoleDeveloper, "correct-key-12345678")

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Use(AuthMiddleware(store))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong-key-abcdefgh")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("expected 401 for wrong key, got %d", rec.Code)
	}
}

func TestMissingAuthHeader(t *testing.T) {
	store := NewInMemKeyStore()

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Use(AuthMiddleware(store))
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("expected 401 for missing auth, got %d", rec.Code)
	}
}

func TestRequireRoleMiddleware(t *testing.T) {
	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), edge.CtxRole, RoleDeveloper)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	r.With(RequireRole(RolePlatformAdmin)).Get("/admin", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	req := httptest.NewRequest("GET", "/admin", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Errorf("expected 403 for insufficient role, got %d", rec.Code)
	}
}

func traceIDStub(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), edge.CtxTraceID, "stub-trace-id")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
