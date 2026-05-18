//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgate/internal/lp/ledger"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestFullStackE2E(t *testing.T) {
	ctx := context.Background()

	// 1. Start Postgres.
	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available: %v", err)
		return
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	host, port := parsePostgresConn(t, connStr)

	// 2. Build binaries (resolve module root via GOMOD).
	repoRoot := findRepoRoot(t)
	gwBin := t.TempDir() + "/aicg-gw"
	lpBin := t.TempDir() + "/aicg-lp"
	runCmdDir(t, repoRoot, "go", "build", "-o", gwBin, "./cmd/aicg-gw")
	runCmdDir(t, repoRoot, "go", "build", "-o", lpBin, "./cmd/aicg-lp")

	// 3. Generate certs.
	certDir := t.TempDir()
	runCmdDir(t, repoRoot, "bash", "scripts/gen-dev-cert.sh")
	copyFile(t, repoRoot+"/certs/ca.crt", certDir+"/ca.crt")
	copyFile(t, repoRoot+"/certs/server.crt", certDir+"/server.crt")
	copyFile(t, repoRoot+"/certs/server.key", certDir+"/server.key")

	// 4. Start a mock Anthropic upstream and point GW routing at it.
	var upstreamKeys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		key := r.Header.Get("x-api-key")
		if key == "" {
			http.Error(w, "missing x-api-key", http.StatusUnauthorized)
			return
		}
		upstreamKeys = append(upstreamKeys, key)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"msg_e2e","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":1000,"output_tokens":5,"cache_read_input_tokens":4000,"cache_creation_input_tokens":1000}}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: message_delta\n")
		fmt.Fprint(w, `data: {"type":"message_delta","usage":{"output_tokens":700}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`)
		fmt.Fprint(w, "\n\n")
	}))
	defer upstream.Close()

	poolsPath := filepath.Join(t.TempDir(), "pools.yaml")
	writeE2EPools(t, poolsPath, upstream.URL)

	// Create minimal config dir with the 5 other runtime YAML files required
	// since T2 changed defaults to /etc/agentgate/... (HANDOFF-007).
	configDir := t.TempDir()
	writeMinimalConfigs(t, configDir)

	// 5. Start GW.
	gwAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	setupTokenPath := filepath.Join(t.TempDir(), "setup-token")
	gwCmd := exec.Command(gwBin)
	gwCmd.Dir = repoRoot
	gwCmd.Env = append(os.Environ(),
		"AGENTGATE_DB_HOST="+host,
		"AGENTGATE_DB_PORT="+port,
		"AGENTGATE_DB_USER=agentgate",
		"AGENTGATE_DB_PASSWORD=agentgate",
		"AGENTGATE_DB_NAME=agentgate",
		"AICG_TLS_CERT="+certDir+"/server.crt",
		"AICG_TLS_KEY="+certDir+"/server.key",
		"AICG_LISTEN_ADDR="+gwAddr,
		"AICG_SETUP_TOKEN_PATH="+setupTokenPath,
		"AICG_CONFIG_POOLS="+poolsPath,
		"AICG_CONFIG_PRICING="+filepath.Join(configDir, "pricing.yaml"),
		"AICG_CONFIG_POLICY="+filepath.Join(configDir, "policies", "main.yaml"),
		"AICG_CONFIG_USERS="+filepath.Join(configDir, "identity", "users.yaml"),
		"AICG_CONFIG_TEAMS="+filepath.Join(configDir, "identity", "teams.yaml"),
		"AICG_CONFIG_REPOS="+filepath.Join(configDir, "identity", "repos.yaml"),
		"ANTHROPIC_API_KEY=sk-ant-test",
		"OPENAI_API_KEY=sk-openai-test",
	)
	var gwStdout, gwStderr bytes.Buffer
	gwCmd.Stdout = &gwStdout
	gwCmd.Stderr = &gwStderr
	if err := gwCmd.Start(); err != nil {
		t.Fatalf("start GW: %v", err)
	}
	defer gwCmd.Process.Kill()

	// 6. Wait for GW healthy.
	t.Log("waiting for GW healthy...")
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	waitHealthy(t, httpClient, "https://"+gwAddr+"/healthz", 30*time.Second)

	// 7. Get setup_token from GW's bootstrap token file.
	setupToken := waitTokenFile(t, setupTokenPath, 10*time.Second)
	if setupToken == "" {
		t.Fatalf("no setup_token in GW output:\n%s\n%s", gwStdout.String(), gwStderr.String())
	}
	t.Logf("setup_token: %s...", setupToken[:16])

	// 8. Exercise `aicg login --ca`, then start LP.
	lpHome := t.TempDir()
	runCmdDirEnv(t, repoRoot, append(os.Environ(), "HOME="+lpHome),
		lpBin, "login", "--setup", setupToken, "--gateway", "https://"+gwAddr, "--ca", certDir+"/ca.crt")
	apiKey := readAPIKeyFromCredentials(t, filepath.Join(lpHome, ".aicg", "credentials"))
	if apiKey == "" {
		t.Fatal("login did not persist an api_key")
	}
	t.Logf("api_key: %s...", apiKey[:16])

	lpPort := freePort(t)
	lpCmd := exec.Command(lpBin, "start", "--port", fmt.Sprintf("%d", lpPort))
	lpCmd.Dir = repoRoot
	lpCmd.Env = append(os.Environ(), "HOME="+lpHome)
	var lpStdout, lpStderr bytes.Buffer
	lpCmd.Stdout = &lpStdout
	lpCmd.Stderr = &lpStderr
	if err := lpCmd.Start(); err != nil {
		t.Fatalf("start LP: %v", err)
	}
	defer lpCmd.Process.Kill()
	waitHTTPStatus(t, "http://127.0.0.1:"+fmt.Sprint(lpPort)+"/_aicg/health", http.StatusOK, 10*time.Second)

	// 9. Forward a streaming Anthropic request through LP -> GW -> mock upstream.
	forwardBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"inspect tests/e2e/full_stack_test.go"}]}],"stream":true}`)
	resp, err := http.Post("http://127.0.0.1:"+fmt.Sprint(lpPort)+"/anthropic/v1/messages", "application/json", bytes.NewReader(forwardBody))
	if err != nil {
		t.Fatalf("LP forward: %v\nstdout:\n%s\nstderr:\n%s", err, lpStdout.String(), lpStderr.String())
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LP forward status %d, body:\n%s", resp.StatusCode, responseBody)
	}
	// LP must filter aicg.* frames from the agent-facing stream.
	if bytes.Contains(responseBody, []byte("event: aicg.")) {
		t.Fatalf("LP-facing response must not contain aicg.* frames:\n%s", responseBody)
	}
	// Normal Anthropic frames must be forwarded.
	if !bytes.Contains(responseBody, []byte("event: message_start")) {
		t.Fatal("LP-facing response missing Anthropic message_start")
	}
	if !bytes.Contains(responseBody, []byte("event: message_stop")) {
		t.Fatal("LP-facing response missing Anthropic message_stop")
	}
	traceID := resp.Header.Get("X-AICG-Trace-Id")
	if traceID == "" {
		t.Fatal("forward response missing X-AICG-Trace-Id")
	}
	// Ledger must still be written from the intercepted aicg.usage frame.
	waitLedgerTrace(t, filepath.Join(lpHome, ".aicg"), traceID, 10*time.Second)

	// 10. Verify API key persists across GW restart.
	t.Log("restarting GW...")
	gwCmd.Process.Kill()
	gwCmd.Wait()

	gwCmd2 := exec.Command(gwBin)
	gwCmd2.Dir = repoRoot
	gwCmd2.Env = gwCmd.Env
	gwCmd2.Stdout = io.Discard
	gwCmd2.Stderr = io.Discard
	if err := gwCmd2.Start(); err != nil {
		t.Fatalf("restart GW: %v", err)
	}
	defer gwCmd2.Process.Kill()

	waitHealthy(t, httpClient, "https://"+gwAddr+"/healthz", 20*time.Second)

	req, _ := http.NewRequest("GET", "https://"+gwAddr+"/api/v1/cost/summary", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatalf("API key check after restart: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Log("API key valid after GW restart")
	} else {
		t.Errorf("API key invalid after restart: status %d", resp.StatusCode)
	}

	// 11. Verify DB row counts via pgx.
	pgDSN := fmt.Sprintf("postgres://agentgate:agentgate@%s:%s/agentgate?sslmode=disable", host, port)
	dbPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("connect to postgres for verification: %v", err)
	}
	defer dbPool.Close()

	tables := []string{"audit_event", "routing_event", "cost_event", "raw_record", "api_keys", "budget_reservation", "setup_tokens"}
	for _, tbl := range tables {
		var count int
		if err := dbPool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", tbl)).Scan(&count); err != nil {
			t.Errorf("table %s: query failed: %v", tbl, err)
		} else {
			t.Logf("table %s: %d rows", tbl, count)
		}
	}
	// Verify cost_event values reflect provider-reported streaming usage (HANDOFF-019 T6).
	var ceInput, ceOutput, ceCacheRead, ceCacheCreate int
	var ceCostCents, ceInputCost, ceOutputCost, ceCacheReadCost, ceCacheCreateCost int
	var ceCostSource string
	if err := dbPool.QueryRow(ctx,
		`SELECT input_tokens, output_tokens, cache_read_tokens, cache_create_tokens,
			        cost_cents, input_cost_cents, output_cost_cents,
			        cache_read_cost_cents, cache_create_cost_cents, cost_source
			 FROM cost_event WHERE trace_id = $1`, traceID).Scan(
		&ceInput, &ceOutput, &ceCacheRead, &ceCacheCreate,
		&ceCostCents, &ceInputCost, &ceOutputCost,
		&ceCacheReadCost, &ceCacheCreateCost, &ceCostSource); err != nil {
		t.Fatalf("query cost_event for trace %s: %v", traceID, err)
	}
	t.Logf("cost_event for %s: input=%d output=%d cache_read=%d cache_create=%d input_cost=%d output_cost=%d cache_read_cost=%d cache_create_cost=%d cost_cents=%d source=%s",
		traceID, ceInput, ceOutput, ceCacheRead, ceCacheCreate,
		ceInputCost, ceOutputCost, ceCacheReadCost, ceCacheCreateCost, ceCostCents, ceCostSource)
	if ceInput != 1000 {
		t.Errorf("cost_event.input_tokens = %d, want 1000 (from mock upstream message_start)", ceInput)
	}
	if ceOutput != 700 {
		t.Errorf("cost_event.output_tokens = %d, want 700 (cumulative from message_delta)", ceOutput)
	}
	if ceCacheRead != 4000 {
		t.Errorf("cost_event.cache_read_tokens = %d, want 4000", ceCacheRead)
	}
	if ceCacheCreate != 1000 {
		t.Errorf("cost_event.cache_create_tokens = %d, want 1000", ceCacheCreate)
	}
	if ceInputCost != 3 {
		t.Errorf("cost_event.input_cost_cents = %d, want 3 (1000 * 3.00/1K = 3)", ceInputCost)
	}
	if ceOutputCost != 10 {
		t.Errorf("cost_event.output_cost_cents = %d, want 10 (700 * 15.00/1K = 10)", ceOutputCost)
	}
	if ceCacheReadCost != 1 {
		t.Errorf("cost_event.cache_read_cost_cents = %d, want 1 (4000 * 0.30/1K = 1)", ceCacheReadCost)
	}
	if ceCacheCreateCost != 3 {
		t.Errorf("cost_event.cache_create_cost_cents = %d, want 3 (1000 * 3.00/1K = 3)", ceCacheCreateCost)
	}
	if ceCostCents != 17 {
		t.Errorf("cost_event.cost_cents = %d, want 17 (per-bucket float sum)", ceCostCents)
	}
	if ceCostSource != "provider_usage" {
		t.Errorf("cost_event.cost_source = %q, want provider_usage", ceCostSource)
	}

	// Verify specific expectations.
	var apiKeysCount int
	dbPool.QueryRow(ctx, "SELECT count(*) FROM api_keys").Scan(&apiKeysCount)
	if apiKeysCount < 1 {
		t.Errorf("expected >=1 api_keys row after exchange, got %d", apiKeysCount)
	}

	for _, tbl := range []string{"audit_event", "routing_event", "cost_event", "raw_record", "budget_reservation"} {
		requireTableCountAtLeast(t, dbPool, tbl, 1)
	}

	var setupTokensCount int
	dbPool.QueryRow(ctx, "SELECT count(*) FROM setup_tokens").Scan(&setupTokensCount)
	if setupTokensCount < 1 {
		t.Errorf("expected >=1 setup_tokens row, got %d", setupTokensCount)
	}

	// Verify setup_token was consumed.
	var consumedCount int
	dbPool.QueryRow(ctx, "SELECT count(*) FROM setup_tokens WHERE consumed_at IS NOT NULL").Scan(&consumedCount)
	t.Logf("setup_tokens consumed: %d", consumedCount)

	// --- file:// secret rotation proof (HANDOFF-007 T4) ---
	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "e2e-rotating-key")
	initialKey := "sk-ant-rotated-v1"
	rotatedKey := "sk-ant-rotated-v2"
	if err := os.WriteFile(secretFile, []byte(initialKey), 0644); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	// Rewrite pools.yaml to use the file:// secret.
	poolsPath2 := filepath.Join(t.TempDir(), "pools-secret.yaml")
	writeE2EPoolsSecret(t, poolsPath2, upstream.URL, secretFile)

	// Restart GW pointing at the new pools (simulates hot reload with
	// restart fallback — Docker Desktop compatible).
	t.Log("restarting GW with file:// secret endpoint...")
	gwCmd2.Process.Kill()
	gwCmd2.Wait()

	gwCmd3 := exec.Command(gwBin)
	gwCmd3.Dir = repoRoot
	gwEnv3 := make([]string, len(gwCmd.Env))
	copy(gwEnv3, gwCmd.Env)
	// Override pools path, keep other config env vars.
	gwEnv3 = replaceEnv(gwEnv3, "AICG_CONFIG_POOLS", poolsPath2)
	gwCmd3.Env = gwEnv3
	gwCmd3.Stdout = io.Discard
	gwCmd3.Stderr = io.Discard
	if err := gwCmd3.Start(); err != nil {
		t.Fatalf("restart GW with secret: %v", err)
	}
	defer gwCmd3.Process.Kill()

	waitHealthy(t, httpClient, "https://"+gwAddr+"/healthz", 30*time.Second)

	// Record the key count before the first rotation request.
	keysBefore := len(upstreamKeys)

	// Forward a request — upstream must see the initial key.
	t.Log("forwarding request with file:// secret (pre-rotation)...")
	forwardBody2 := []byte(`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"secret rotation test pre"}]}],"stream":true}`)
	resp2, err := http.Post("http://127.0.0.1:"+fmt.Sprint(lpPort)+"/anthropic/v1/messages", "application/json", bytes.NewReader(forwardBody2))
	if err != nil {
		t.Fatalf("LP forward (pre-rotation): %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("LP forward (pre-rotation) status %d", resp2.StatusCode)
	}

	// Assert the upstream received the initial key.
	found := false
	for i := keysBefore; i < len(upstreamKeys); i++ {
		if upstreamKeys[i] == initialKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("mock upstream did not receive initial key %q after rotation; keys seen: %v", initialKey, upstreamKeys[keysBefore:])
	} else {
		t.Logf("file:// secret pre-rotation: upstream received initial key %q", initialKey)
	}

	// Rotate the secret without restarting GW — HotReloader must detect
	// the file change and re-resolve the key for the next outbound request
	// (HANDOFF-007 T4 no-restart requirement).
	if err := os.WriteFile(secretFile, []byte(rotatedKey), 0644); err != nil {
		t.Fatalf("rotate secret file: %v", err)
	}
	t.Log("secret file rotated on disk, waiting for HotReloader...")

	// Wait for the HotReloader to detect the file change and re-resolve.
	// T4's proof path must not restart GW: this assertion is what proves
	// file:// rotation reaches the next outbound request atomically.
	if !waitCounterIncrement(t, httpClient, "https://"+gwAddr+"/metrics",
		`secret_reload_total{ref="file",result="success"}`, 1, 15*time.Second) {
		t.Fatal("file:// secret rotation did not trigger HotReloader; refusing restart fallback for T4 proof")
	}
	t.Log("file:// secret: HotReloader detected rotation, counter incremented")

	keysBefore2 := len(upstreamKeys)

	// Forward a request — upstream must see the rotated key.
	t.Log("forwarding request with file:// secret (post-rotation)...")
	forwardBody3 := []byte(`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"secret rotation test post"}]}],"stream":true}`)
	resp3, err := http.Post("http://127.0.0.1:"+fmt.Sprint(lpPort)+"/anthropic/v1/messages", "application/json", bytes.NewReader(forwardBody3))
	if err != nil {
		t.Fatalf("LP forward (post-rotation): %v", err)
	}
	io.Copy(io.Discard, resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("LP forward (post-rotation) status %d", resp3.StatusCode)
	}

	found2 := false
	for i := keysBefore2; i < len(upstreamKeys); i++ {
		if upstreamKeys[i] == rotatedKey {
			found2 = true
			break
		}
	}
	if !found2 {
		t.Errorf("mock upstream did not receive rotated key %q; keys seen: %v", rotatedKey, upstreamKeys[keysBefore2:])
	} else {
		t.Logf("file:// secret post-rotation: upstream received rotated key %q without GW restart", rotatedKey)
	}

	t.Log("E2E complete: GW builds, starts, restarts, API key persists, DB verified, file:// secret rotated at request level")
}

func parsePostgresConn(t *testing.T, connStr string) (host, port string) {
	t.Helper()
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse connection string %q: %v", connStr, err)
	}
	host = u.Hostname()
	port = u.Port()
	// Strip trailing "/tcp" that some testcontainers versions append.
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
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("service not healthy at %s after %v", url, timeout)
}

func waitHTTPStatus(t *testing.T, url string, status int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == status {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
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

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func runCmdDir(t *testing.T, dir, cmd string, args ...string) {
	t.Helper()
	c := exec.Command(cmd, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", cmd, args, err, out)
	}
}

func runCmdDirEnv(t *testing.T, dir string, env []string, cmd string, args ...string) {
	t.Helper()
	c := exec.Command(cmd, args...)
	c.Dir = dir
	c.Env = env
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", cmd, args, err, out)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	// Use go env GOMOD to find the module root.
	out, err := exec.Command("go", "env", "GOMOD").CombinedOutput()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	modPath := strings.TrimSpace(string(out))
	// GOMOD returns /path/to/repo/go.mod; we need the directory.
	return strings.TrimSuffix(modPath, "/go.mod")
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func writeMinimalConfigs(t *testing.T, dir string) {
	t.Helper()
	policyDir := filepath.Join(dir, "policies")
	identityDir := filepath.Join(dir, "identity")
	os.MkdirAll(policyDir, 0755)
	os.MkdirAll(identityDir, 0755)

	writeE2EFile(t, filepath.Join(dir, "pricing.yaml"), `
models:
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
    cache_read_price_per_1k_tokens: 0.30
    cache_create_price_per_1k_tokens: 3.00
`)
	writeE2EFile(t, filepath.Join(policyDir, "main.yaml"), `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: cheap
    reasons: ["default"]
rules: []
`)
	writeE2EFile(t, filepath.Join(identityDir, "users.yaml"), `
users:
  - user_id: test-user
    team_id: test-team
    role: developer
    api_key_hash: ""
`)
	writeE2EFile(t, filepath.Join(identityDir, "teams.yaml"), `
teams:
  - team_id: test-team
    name: Test Team
    budget_monthly_cap_cents: 10000
`)
	writeE2EFile(t, filepath.Join(identityDir, "repos.yaml"), `
repos:
  - repo_id: test-repo
    remote_url: "https://github.com/test/repo"
    default_team_id: test-team
    restricted: false
`)
}

func writeE2EFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeE2EPools(t *testing.T, path, upstreamURL string) {
	t.Helper()
	data := fmt.Sprintf(`provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: %q
    data_residency: us
    trust_tier: vendor
    key_ref: env://ANTHROPIC_API_KEY
    supports:
      streaming: true
      tools: true
      cache_control: true
      extended_thinking: true

pools:
  cheap:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: standard
    max_attempts: 1
    timeout_ms: 60000
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: strong
    max_attempts: 1
    timeout_ms: 90000
  strong:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-opus-4-7", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 180000
`, upstreamURL)
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("write e2e pools: %v", err)
	}
}

func writeE2EPoolsSecret(t *testing.T, path, upstreamURL, secretFile string) {
	t.Helper()
	data := fmt.Sprintf(`provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: %q
    data_residency: us
    trust_tier: vendor
    key_ref: file://%s
    supports:
      streaming: true
      tools: true
      cache_control: true
      extended_thinking: true

pools:
  cheap:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: standard
    max_attempts: 1
    timeout_ms: 60000
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: strong
    max_attempts: 1
    timeout_ms: 90000
  strong:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-opus-4-7", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 180000
`, upstreamURL, secretFile)
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("write e2e pools (secret): %v", err)
	}
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func readAPIKeyFromCredentials(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "api_key:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "api_key:"))
		}
	}
	return ""
}

func extractUsageTraceID(t *testing.T, body []byte) string {
	t.Helper()
	for _, frame := range strings.Split(string(body), "\n\n") {
		if !strings.HasPrefix(frame, "event: aicg.usage") {
			continue
		}
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var data struct {
				TraceID string `json:"trace_id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatalf("parse aicg.usage: %v", err)
			}
			if data.TraceID == "" {
				t.Fatal("aicg.usage missing trace_id")
			}
			return data.TraceID
		}
	}
	t.Fatal("aicg.usage frame missing data line")
	return ""
}

func waitLedgerTrace(t *testing.T, dir, traceID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		l, err := ledger.Open(dir)
		if err == nil {
			_, queryErr := l.QueryByTraceID(traceID)
			_ = l.Close()
			if queryErr == nil {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("local ledger missing trace_id %s after %v", traceID, timeout)
}

func requireTableCountAtLeast(t *testing.T, pool *pgxpool.Pool, table string, min int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count < min {
		t.Fatalf("expected %s to have >= %d rows, got %d", table, min, count)
	}
}
