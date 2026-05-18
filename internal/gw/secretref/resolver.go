// Package secretref resolves provider API key references (env://, file://, vault://).
// It must never log or otherwise expose cleartext keys.
package secretref

import (
	"fmt"
	"os"
	"strings"
)

// Resolve resolves a key_ref string to a cleartext API key.
// Supported schemes:
//   - env://VARNAME — read from environment variable
//   - file:///absolute/path — read first line from file (must be absolute)
//   - vault:// — returns error (P2+)
func Resolve(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}

	switch {
	case strings.HasPrefix(ref, "env://"):
		varName := strings.TrimPrefix(ref, "env://")
		if varName == "" {
			return "", fmt.Errorf("secret_ref: empty env var name in %q", ref)
		}
		val, ok := os.LookupEnv(varName)
		if !ok || val == "" {
			return "", fmt.Errorf("secret_ref unresolved: %s (set the env var or remove the endpoint)", ref)
		}
		return val, nil

	case strings.HasPrefix(ref, "file://"):
		path := strings.TrimPrefix(ref, "file://")
		if !strings.HasPrefix(path, "/") {
			return "", fmt.Errorf("secret_ref: file:// requires absolute path, got %q", ref)
		}
		if strings.Contains(path, "..") {
			return "", fmt.Errorf("secret_ref: file:// path must not contain '..': %q", ref)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("secret_ref: read %s: %w", ref, err)
		}
		// Take first line, trim whitespace.
		line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
		return line, nil

	case strings.HasPrefix(ref, "vault://"):
		return "", fmt.Errorf("secret_ref: vault:// is not supported in P0 (planned P2+); use env:// or file:// instead")

	default:
		return "", fmt.Errorf("secret_ref: unsupported scheme in %q (supported: env://, file://)", ref)
	}
}
