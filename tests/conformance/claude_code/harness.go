// Package claude_code provides shared test harness helpers for conformance tests.
package claude_code

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgate/internal/gw/db"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// StartPostgres starts a testcontainer Postgres 16, runs migrations 0001..0006,
// and returns the pool and DSN. Skips with t.Skipf if Docker is unavailable.
func StartPostgres(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if err := db.Migrate(ctx, pool, db.Migrations, "migrations"); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	return pool, dsn
}

// BuildBinaries builds aicg-gw and aicg-lp to t.TempDir() and returns the paths.
func BuildBinaries(t *testing.T) (gwBin, lpBin string) {
	t.Helper()
	repoRoot := findRepoRoot(t)
	tmp := t.TempDir()
	gwBin = filepath.Join(tmp, "aicg-gw")
	lpBin = filepath.Join(tmp, "aicg-lp")
	runCmd(t, repoRoot, nil, "go", "build", "-o", gwBin, "./cmd/aicg-gw")
	runCmd(t, repoRoot, nil, "go", "build", "-o", lpBin, "./cmd/aicg-lp")
	return
}

// GenerateCerts runs scripts/gen-dev-cert.sh and returns the cert directory.
func GenerateCerts(t *testing.T) string {
	t.Helper()
	repoRoot := findRepoRoot(t)
	runCmd(t, repoRoot, nil, "bash", filepath.Join(repoRoot, "scripts/gen-dev-cert.sh"))
	certDir := t.TempDir()
	copyFile(t, filepath.Join(repoRoot, "certs", "ca.crt"), filepath.Join(certDir, "ca.crt"))
	copyFile(t, filepath.Join(repoRoot, "certs", "server.crt"), filepath.Join(certDir, "server.crt"))
	copyFile(t, filepath.Join(repoRoot, "certs", "server.key"), filepath.Join(certDir, "server.key"))
	return certDir
}

// GWStartOpts controls GW subprocess startup.
type GWStartOpts struct {
	BinPath        string
	DSN            string
	PoolsPath      string
	ConfigDir      string
	CertDir        string
	ListenAddr     string
	SetupTokenPath string
	ExtraEnv       []string
}

// StartGW starts the GW subprocess and waits for it to be healthy.
// Returns the GW base URL (https://<addr>).
func StartGW(t *testing.T, opts GWStartOpts) string {
	t.Helper()

	host, port := parsePostgresConn(t, opts.DSN)

	cmd := exec.Command(opts.BinPath)
	cmd.Dir = findRepoRoot(t)
	cmd.Env = append(os.Environ(),
		"AGENTGATE_DB_HOST="+host,
		"AGENTGATE_DB_PORT="+port,
		"AGENTGATE_DB_USER=agentgate",
		"AGENTGATE_DB_PASSWORD=agentgate",
		"AGENTGATE_DB_NAME=agentgate",
		"AICG_TLS_CERT="+filepath.Join(opts.CertDir, "server.crt"),
		"AICG_TLS_KEY="+filepath.Join(opts.CertDir, "server.key"),
		"AICG_LISTEN_ADDR="+opts.ListenAddr,
		"AICG_SETUP_TOKEN_PATH="+opts.SetupTokenPath,
		"AICG_CONFIG_POOLS="+opts.PoolsPath,
		"AICG_CONFIG_PRICING="+filepath.Join(opts.ConfigDir, "pricing.yaml"),
		"AICG_CONFIG_POLICY="+filepath.Join(opts.ConfigDir, "policies", "main.yaml"),
		"AICG_CONFIG_USERS="+filepath.Join(opts.ConfigDir, "identity", "users.yaml"),
		"AICG_CONFIG_TEAMS="+filepath.Join(opts.ConfigDir, "identity", "teams.yaml"),
		"AICG_CONFIG_REPOS="+filepath.Join(opts.ConfigDir, "identity", "repos.yaml"),
		"ANTHROPIC_API_KEY=sk-ant-test",
		"OPENAI_API_KEY=sk-openai-test",
	)
	cmd.Env = append(cmd.Env, opts.ExtraEnv...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start GW: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if t.Failed() {
			t.Logf("GW stdout:\n%s", stdout.String())
			t.Logf("GW stderr:\n%s", stderr.String())
		}
	})

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	gwURL := "https://" + opts.ListenAddr
	waitHealthy(t, client, gwURL+"/healthz", 30*time.Second)
	return gwURL
}

// LPStartOpts controls LP subprocess startup.
type LPStartOpts struct {
	BinPath  string
	HomeDir  string
	Port     int
	ExtraEnv []string
}

// StartLP starts the LP subprocess and waits for it to be healthy.
// Returns the LP base URL (http://127.0.0.1:<port>).
func StartLP(t *testing.T, opts LPStartOpts) string {
	t.Helper()

	cmd := exec.Command(opts.BinPath, "start", "--port", fmt.Sprintf("%d", opts.Port))
	cmd.Dir = findRepoRoot(t)
	cmd.Env = append(os.Environ(), "HOME="+opts.HomeDir)
	cmd.Env = append(cmd.Env, opts.ExtraEnv...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start LP: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if t.Failed() {
			t.Logf("LP stdout:\n%s", stdout.String())
			t.Logf("LP stderr:\n%s", stderr.String())
		}
	})

	lpURL := fmt.Sprintf("http://127.0.0.1:%d", opts.Port)
	waitHTTPStatus(t, lpURL+"/_aicg/health", http.StatusOK, 10*time.Second)
	return lpURL
}

// BootstrapSetupToken reads the setup token from the file GW wrote it to,
// runs aicg-lp login --setup with homeDir as HOME, and returns the admin API key.
// The caller must pass the same homeDir to StartLP and any subsequent aicg-lp
// CLI calls (e.g. status) so that credentials and ledger state are shared.
func BootstrapSetupToken(t *testing.T, lpBin, gwURL, setupTokenPath, caPath, homeDir string) string {
	t.Helper()

	token := waitTokenFile(t, setupTokenPath, 10*time.Second)
	if token == "" {
		t.Fatalf("no setup token found in %s", setupTokenPath)
	}
	t.Logf("setup_token: %s...", token[:16])

	cmd := exec.Command(lpBin, "login", "--setup", token, "--gateway", gwURL, "--ca", caPath)
	cmd.Dir = findRepoRoot(t)
	cmd.Env = append(os.Environ(), "HOME="+homeDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("login --setup: %v\n%s", err, out)
	}

	// The login command persists credentials to ~/.aicg/credentials.
	// Read back the API key from the same homeDir.
	credPath := filepath.Join(homeDir, ".aicg", "credentials")
	apiKey := readAPIKeyFromCredentials(t, credPath)
	if apiKey == "" {
		t.Fatalf("login did not persist an api_key")
	}
	t.Logf("api_key: %s...", apiKey[:16])
	return apiKey
}

// FreePort finds a free TCP port.
func FreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	_ = ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// WriteMinimalConfigs writes the required minimal YAML configs for GW startup.
func WriteMinimalConfigs(t *testing.T, dir string) {
	t.Helper()
	policyDir := filepath.Join(dir, "policies")
	identityDir := filepath.Join(dir, "identity")
	mkDir(t, policyDir)
	mkDir(t, identityDir)

	writeFile(t, filepath.Join(dir, "pricing.yaml"), `models:
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
    cache_read_price_per_1k_tokens: 0.30
    cache_create_price_per_1k_tokens: 3.00
`)
	writeFile(t, filepath.Join(policyDir, "main.yaml"), `version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["default"]
rules: []
`)
	writeFile(t, filepath.Join(identityDir, "users.yaml"), `users:
  - user_id: test-user
    team_id: test-team
    role: developer
    api_key_hash: ""
`)
	writeFile(t, filepath.Join(identityDir, "teams.yaml"), `teams:
  - team_id: test-team
    name: Test Team
    budget_monthly_cap_cents: 10000
`)
	writeFile(t, filepath.Join(identityDir, "repos.yaml"), `repos:
  - repo_id: test-repo
    remote_url: "https://github.com/test/repo"
    default_team_id: test-team
    restricted: false
`)
}

// --- internal helpers ---

func findRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").CombinedOutput()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	modPath := strings.TrimSpace(string(out))
	return strings.TrimSuffix(modPath, "/go.mod")
}

func runCmd(t *testing.T, dir string, env []string, cmd string, args ...string) {
	t.Helper()
	c := exec.Command(cmd, args...)
	c.Dir = dir
	if env != nil {
		c.Env = env
	}
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", cmd, args, err, out)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func mkDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func parsePostgresConn(t *testing.T, connStr string) (host, port string) {
	t.Helper()
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse connection string %q: %v", connStr, err)
	}
	host = u.Hostname()
	port = u.Port()
	port = strings.TrimSuffix(port, "/tcp")
	if port == "" {
		port = "5432"
	}
	return
}

func waitHealthy(t *testing.T, client *http.Client, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == 200 {
			_ = resp.Body.Close()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("service not healthy at %s after %v", url, timeout)
}

func waitHTTPStatus(t *testing.T, url string, status int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == status {
			_ = resp.Body.Close()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("service did not return %d at %s after %v", status, url, timeout)
}

func waitTokenFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			token := strings.TrimSpace(string(data))
			if strings.HasPrefix(token, "setup_") {
				return token
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return ""
}

func readAPIKeyFromCredentials(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "api_key:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "api_key:"))
		}
	}
	return ""
}
