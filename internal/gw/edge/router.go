// Package edge provides the GW HTTP server: chi router, TLS termination,
// global rate-limiting, and UUIDv7 trace_id minting.
package edge

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Context keys for request-scoped values.
type ctxKey int

const (
	CtxTraceID ctxKey = iota
	CtxUserID
	CtxTeamID
	CtxRole
)

// HealthChecker is called by the /healthz handler to report component status.
type HealthChecker func(ctx context.Context) map[string]string

// Server wraps the chi router and TLS configuration.
type Server struct {
	Router        *chi.Mux
	Addr          string
	TLSCert       string
	TLSKey        string
	healthChecker HealthChecker
}

// NewServer creates a new edge Server.
func NewServer(addr string) *Server {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(structuredLoggerMiddleware)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(5 * time.Minute))
	r.Use(TraceIDMiddleware)

	return &Server{
		Router: r,
		Addr:   addr,
	}
}

// SetHealthChecker registers a health check function that /healthz will call.
func (s *Server) SetHealthChecker(hc HealthChecker) {
	s.healthChecker = hc
	s.Router.Get("/healthz", s.handleHealthz)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := map[string]any{
		"status": "ok",
	}
	if s.healthChecker != nil {
		components := s.healthChecker(r.Context())
		status["components"] = components
		for _, v := range components {
			if v == "fail" {
				// Return 503 if any component is failing.
				w.WriteHeader(http.StatusServiceUnavailable)
				status["status"] = "degraded"
				_ = json.NewEncoder(w).Encode(status)
				return
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(status)
}

// structuredLoggerMiddleware replaces chi's default Logger with slog.
func structuredLoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

// Start begins listening. When TLS cert/key are configured, enforces TLS 1.3-only.
func (s *Server) Start() error {
	srv := &http.Server{
		Addr:    s.Addr,
		Handler: s.Router,
	}
	if s.TLSCert != "" && s.TLSKey != "" {
		slog.Info("starting TLS 1.3 server", "addr", s.Addr)
		srv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS13,
		}
		return srv.ListenAndServeTLS(s.TLSCert, s.TLSKey)
	}
	slog.Info("starting plain HTTP server", "addr", s.Addr)
	return srv.ListenAndServe()
}

// MintTraceID generates a UUIDv7 trace_id at the edge.
func MintTraceID() string {
	var buf [16]byte
	// UUIDv7: 48-bit timestamp (ms) + 12-bit rand_a + 62-bit rand_b
	ts := uint64(time.Now().UnixMilli())
	buf[0] = byte(ts >> 40)
	buf[1] = byte(ts >> 32)
	buf[2] = byte(ts >> 24)
	buf[3] = byte(ts >> 16)
	buf[4] = byte(ts >> 8)
	buf[5] = byte(ts)
	// 4-bit version (7) + 12-bit rand_a
	if _, err := rand.Read(buf[6:8]); err != nil {
		// fallback
		buf[6] = 0x70
		buf[7] = 0x00
	}
	// 2-bit variant (10) + 62-bit rand_b
	if _, err := rand.Read(buf[8:]); err != nil {
		buf[8] = 0x80
	}

	buf[6] = (buf[6] & 0x0f) | 0x70 // version 7
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10

	dst := make([]byte, 36)
	hex.Encode(dst[:8], buf[:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], buf[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], buf[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], buf[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:], buf[10:])
	return string(dst)
}

// TraceIDMiddleware injects a UUIDv7 trace_id into every request context
// and sets the X-AICG-Trace-Id response header.
func TraceIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.Header.Get("X-AICG-Trace-Id")
		if traceID == "" {
			traceID = MintTraceID()
		}
		ctx := context.WithValue(r.Context(), CtxTraceID, traceID)
		w.Header().Set("X-AICG-Trace-Id", traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetTraceID extracts the trace_id from context.
func GetTraceID(ctx context.Context) string {
	v, _ := ctx.Value(CtxTraceID).(string)
	return v
}

// GetUserID extracts the user_id from context.
func GetUserID(ctx context.Context) string {
	v, _ := ctx.Value(CtxUserID).(string)
	return v
}

// GetTeamID extracts the team_id from context.
func GetTeamID(ctx context.Context) string {
	v, _ := ctx.Value(CtxTeamID).(string)
	return v
}

// GetRole extracts the role from context.
func GetRole(ctx context.Context) string {
	v, _ := ctx.Value(CtxRole).(string)
	return v
}
