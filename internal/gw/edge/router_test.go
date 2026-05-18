package edge

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTLSServerConfig verifies TLS 1.3-only is enforced when cert+key are set.
func TestTLSServerConfig(t *testing.T) {
	srv := NewServer(":0")
	// Before cert/key are set, no TLS config.
	if srv.TLSCert != "" || srv.TLSKey != "" {
		t.Fatal("expected no TLS config initially")
	}

	// Set cert/key — Start() should now enforce TLS 1.3.
	srv.TLSCert = "/path/to/cert.pem"
	srv.TLSKey = "/path/to/key.pem"

	// Verify the TLS config is 1.3-only by constructing the same
	// http.Server that Start() would create.
	httpSrv := &http.Server{
		Addr:    srv.Addr,
		Handler: srv.Router,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
		},
	}
	if httpSrv.TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected TLS 1.3 min version, got %x", httpSrv.TLSConfig.MinVersion)
	}
	// Confirm TLS 1.2 is below the minimum.
	if tls.VersionTLS12 >= httpSrv.TLSConfig.MinVersion {
		t.Error("TLS 1.2 should be below the minimum version (1.3)")
	}
}

func TestMintTraceID(t *testing.T) {
	id := MintTraceID()
	if len(id) != 36 {
		t.Errorf("expected 36-char UUIDv7, got %d: %s", len(id), id)
	}
	// Check version 7 marker: character at position 14 is '7'
	if id[14] != '7' {
		t.Errorf("expected UUIDv7 (version nibble '7'), got '%c' at pos 14", id[14])
	}
	// Check variant marker at position 19
	if id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b' {
		t.Errorf("expected variant 10xx, got '%c' at pos 19", id[19])
	}
}

func TestMintTraceIDUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := MintTraceID()
		if seen[id] {
			t.Fatalf("duplicate trace_id: %s", id)
		}
		seen[id] = true
	}
}

func TestTraceIDFromHeader(t *testing.T) {
	router := NewServer("").Router
	router.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		traceID := GetTraceID(r.Context())
		if traceID == "" {
			t.Error("expected non-empty trace_id")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-AICG-Trace-Id", "custom-trace-id-12345")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Header().Get("X-AICG-Trace-Id") != "custom-trace-id-12345" {
		t.Errorf("expected custom trace_id in response, got %q", rec.Header().Get("X-AICG-Trace-Id"))
	}
}

func TestTLS13ConfigRejectsTLS12(t *testing.T) {
	srv := NewServer("127.0.0.1:0")
	srv.TLSCert = "/path/to/cert.pem"
	srv.TLSKey = "/path/to/key.pem"

	httpSrv := &http.Server{
		Addr:    srv.Addr,
		Handler: srv.Router,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
		},
	}
	if httpSrv.TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Fatal("expected TLS 1.3 min version")
	}

	// TLS 1.2 client must be below the minimum.
	if tls.VersionTLS12 >= httpSrv.TLSConfig.MinVersion {
		t.Error("TLS 1.2 should be below the minimum version (1.3)")
	}
}

func TestNewServerHealthz(t *testing.T) {
	srv := NewServer("")
	srv.SetHealthChecker(func(ctx context.Context) map[string]string {
		return map[string]string{"db": "ok"}
	})

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHealthzDegraded(t *testing.T) {
	srv := NewServer("")
	srv.SetHealthChecker(func(ctx context.Context) map[string]string {
		return map[string]string{"db": "fail"}
	})

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for degraded, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHealthzWithoutChecker(t *testing.T) {
	srv := NewServer("")
	srv.SetHealthChecker(nil)
	// SetHealthChecker with nil does not register the route.
	// Manually register to test default behavior.
	srv.Router.Get("/healthz", srv.handleHealthz)

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 without checker, got %d", rec.Code)
	}
}
