// Package repobinding validates ed25519-signed repo binding tokens
// and links them to machine_id for per-repo identity.
package repobinding

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/http"
)

// Verifier checks repo binding tokens.
type Verifier struct {
	// P0: fixed key for development. P1+: key registry per repo.
	PublicKey ed25519.PublicKey
}

// NewVerifier creates a Verifier with the given hex-encoded public key.
func NewVerifier(pubKeyHex string) (*Verifier, error) {
	key, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid ed25519 public key size: %d, want %d", len(key), ed25519.PublicKeySize)
	}
	return &Verifier{PublicKey: ed25519.PublicKey(key)}, nil
}

// Verify checks the binding token signature.
func (v *Verifier) Verify(repoID, machineID, signature string) error {
	msg := repoID + ":" + machineID
	sig, err := hex.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(v.PublicKey, []byte(msg), sig) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}

// Middleware validates the X-AICG-Binding header.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// P0: binding is optional. If not present, pass through.
		binding := r.Header.Get("X-AICG-Binding")
		if binding == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Parse binding: "repo_id=<id>,machine_id=<id>,sig=<hex>"
		parsed := parseBinding(binding)
		repoID := parsed["repo_id"]
		machineID := parsed["machine_id"]
		sig := parsed["sig"]
		if repoID == "" || machineID == "" || sig == "" {
			writeBindingError(w, "incomplete binding header")
			return
		}
		if err := v.Verify(repoID, machineID, sig); err != nil {
			writeBindingError(w, "binding signature verification failed")
			return
		}
		ctx := context.WithValue(r.Context(), ctxBindingRepoID, repoID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type ctxKey int

const ctxBindingRepoID ctxKey = iota

// GetBindingRepoID extracts the verified repo_id from context.
func GetBindingRepoID(ctx context.Context) string {
	v, _ := ctx.Value(ctxBindingRepoID).(string)
	return v
}

func parseBinding(s string) map[string]string {
	m := make(map[string]string)
	for _, part := range splitComma(s) {
		kv := splitEqual(part)
		if len(kv) == 2 {
			m[kv[0]] = kv[1]
		}
	}
	return m
}

func splitComma(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

func splitEqual(s string) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

func writeBindingError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `{"code":"binding_error","message":%q}`+"\n", msg)
}
