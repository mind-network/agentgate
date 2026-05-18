//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestInviteOnboardingE2E exercises the full HANDOFF-023 invite chain:
// platform_admin bootstrap via setup_token, admin invite mint, user login --invite,
// streaming forward with per-user attribution, and double-redemption rejection.
// It satisfies HANDOFF-023 T6 — the dogfood / per-user attribution P0 exit
// criterion in §20.6 / R-18.
func TestInviteOnboardingE2E(t *testing.T) {
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

	// 2. Build binaries.
	repoRoot := findRepoRoot(t)
	gwBin := t.TempDir() + "/aicg-gw"
	lpBin := t.TempDir() + "/aicg-lp"
	runCmdDir(t, repoRoot, "go", "build", "-o", gwBin, "./cmd/aicg-gw")
	runCmdDir(t, repoRoot, "go", "build", "-o", lpBin, "./cmd/aicg-lp")

	// 3. Generate dev certs.
	certDir := t.TempDir()
	runCmdDir(t, repoRoot, "bash", "scripts/gen-dev-cert.sh")
	copyFile(t, repoRoot+"/certs/ca.crt", certDir+"/ca.crt")
	copyFile(t, repoRoot+"/certs/server.crt", certDir+"/server.crt")
	copyFile(t, repoRoot+"/certs/server.key", certDir+"/server.key")

	// 4. Mock Anthropic upstream — returns a streaming SSE response with a
	// usage frame so cost_event is populated and trace_id propagates.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"msg_invite","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":0}}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: message_delta\n")
		fmt.Fprint(w, `data: {"type":"message_delta","usage":{"output_tokens":5}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`)
		fmt.Fprint(w, "\n\n")
	}))
	defer upstream.Close()

	// 5. Configs — pools point at the mock upstream; teams.yaml must register
	// the dogfood team or HandleAdminCreateInvite returns 400 unknown_team.
	poolsPath := filepath.Join(t.TempDir(), "pools.yaml")
	writeE2EPools(t, poolsPath, upstream.URL)

	configDir := t.TempDir()
	writeInviteOnboardingConfigs(t, configDir)

	// 6. Start GW.
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

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}
	waitHealthy(t, httpClient, "https://"+gwAddr+"/healthz", 30*time.Second)

	setupToken := waitTokenFile(t, setupTokenPath, 10*time.Second)
	if setupToken == "" {
		t.Fatalf("no setup_token in GW output:\n%s\n%s", gwStdout.String(), gwStderr.String())
	}
	t.Logf("setup_token: %s...", setupToken[:16])

	// 7. Admin bootstrap via setup_token in adminHome.
	adminHome := t.TempDir()
	runCmdDirEnv(t, repoRoot, append(os.Environ(), "HOME="+adminHome),
		lpBin, "login", "--setup", setupToken, "--gateway", "https://"+gwAddr, "--ca", certDir+"/ca.crt")
	adminAPIKey := readAPIKeyFromCredentials(t, filepath.Join(adminHome, ".aicg", "credentials"))
	if adminAPIKey == "" {
		t.Fatal("admin bootstrap did not persist an api_key")
	}
	t.Logf("admin_api_key: %s...", adminAPIKey[:16])

	// 8. Mint a dogfood invite via `aicg-lp admin invite`. The token must be
	// returned exactly once and we capture it from stdout.
	adminInviteOut := mustCaptureStdout(t, repoRoot, append(os.Environ(), "HOME="+adminHome),
		lpBin, "admin", "invite", "--role", "developer", "--team", "dogfood",
		"--user", "dogfood-dev-9", "--ttl", "1h")
	t.Logf("admin invite stdout:\n%s", adminInviteOut)
	inviteToken := mustParseInviteToken(t, adminInviteOut)
	t.Logf("invite_token: %s...", inviteToken[:16])

	// 9. New user logs in with the invite token in userHome.
	userHome := t.TempDir()
	loginOut := mustCaptureStdout(t, repoRoot, append(os.Environ(), "HOME="+userHome),
		lpBin, "login", "--invite", inviteToken, "--gateway", "https://"+gwAddr, "--ca", certDir+"/ca.crt")
	t.Logf("user login stdout: %s", strings.TrimSpace(loginOut))
	if !strings.Contains(loginOut, "Login successful as dogfood-dev-9") {
		t.Fatalf("user login stdout missing dogfood-dev-9 user_id:\n%s", loginOut)
	}
	userAPIKey := readAPIKeyFromCredentials(t, filepath.Join(userHome, ".aicg", "credentials"))
	if userAPIKey == "" {
		t.Fatal("user login did not persist an api_key")
	}
	if userAPIKey == adminAPIKey {
		t.Fatal("user api_key must differ from admin api_key")
	}
	userUserID := readUserIDFromCredentials(t, filepath.Join(userHome, ".aicg", "credentials"))
	if userUserID != "dogfood-dev-9" {
		t.Fatalf("user credentials user_id = %q, want %q", userUserID, "dogfood-dev-9")
	}

	// 10. Start a user LP daemon and forward a streaming request through it.
	lpPort := freePort(t)
	lpCmd := exec.Command(lpBin, "start", "--port", fmt.Sprintf("%d", lpPort))
	lpCmd.Dir = repoRoot
	lpCmd.Env = append(os.Environ(), "HOME="+userHome)
	var lpStdout, lpStderr bytes.Buffer
	lpCmd.Stdout = &lpStdout
	lpCmd.Stderr = &lpStderr
	if err := lpCmd.Start(); err != nil {
		t.Fatalf("start user LP: %v", err)
	}
	defer lpCmd.Process.Kill()
	waitHTTPStatus(t, fmt.Sprintf("http://127.0.0.1:%d/_aicg/health", lpPort), http.StatusOK, 10*time.Second)

	forwardBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"invite onboarding e2e"}]}],"stream":true}`)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/anthropic/v1/messages", lpPort), "application/json", bytes.NewReader(forwardBody))
	if err != nil {
		t.Fatalf("LP forward: %v\nstdout:\n%s\nstderr:\n%s", err, lpStdout.String(), lpStderr.String())
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LP forward status %d, body:\n%s", resp.StatusCode, respBody)
	}
	traceID := resp.Header.Get("X-AICG-Trace-Id")
	if traceID == "" {
		t.Fatal("forward response missing X-AICG-Trace-Id")
	}
	t.Logf("forward trace_id: %s", traceID)

	// 11. psql verifications.
	pgDSN := fmt.Sprintf("postgres://agentgate:agentgate@%s:%s/agentgate?sslmode=disable", host, port)
	dbPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer dbPool.Close()

	// cost_event.user_id must reflect the dogfood user, not platform_admin.
	var ceUserID, ceTeamID string
	if err := dbPool.QueryRow(ctx,
		`SELECT user_id, team_id FROM cost_event WHERE trace_id = $1`, traceID,
	).Scan(&ceUserID, &ceTeamID); err != nil {
		t.Fatalf("query cost_event for trace %s: %v", traceID, err)
	}
	t.Logf("cost_event: user_id=%s team_id=%s", ceUserID, ceTeamID)
	if ceUserID != "dogfood-dev-9" {
		t.Errorf("cost_event.user_id = %q, want %q", ceUserID, "dogfood-dev-9")
	}
	if ceTeamID != "dogfood" {
		t.Errorf("cost_event.team_id = %q, want %q", ceTeamID, "dogfood")
	}

	// audit_event must record both invite lifecycle events.
	requireAuditEvent(t, dbPool, "invite.created", 1)
	requireAuditEvent(t, dbPool, "invite.exchanged", 1)

	// 12. stats --by user --from 7d must surface the dogfood-dev-9 row.
	// Run with the user's own credentials — /api/v1/cost/summary is
	// non-role-gated in P0.
	statsOut := mustCaptureStdout(t, repoRoot, append(os.Environ(), "HOME="+userHome),
		lpBin, "stats", "--by", "user", "--from", "7d")
	t.Logf("stats --by user --from 7d:\n%s", statsOut)
	if !strings.Contains(statsOut, "dogfood-dev-9") {
		t.Errorf("stats output missing dogfood-dev-9:\n%s", statsOut)
	}

	// 13. Double redemption: the same invite token must now fail with
	// token_consumed and the CLI must exit non-zero.
	secondHome := t.TempDir()
	stdout, stderr, exitCode := runCmdCaptureAll(t, repoRoot, append(os.Environ(), "HOME="+secondHome),
		lpBin, "login", "--invite", inviteToken, "--gateway", "https://"+gwAddr, "--ca", certDir+"/ca.crt")
	t.Logf("second login exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	if exitCode == 0 {
		t.Errorf("second `login --invite` should fail (token_consumed); got exit 0\nstdout:%s\nstderr:%s",
			stdout, stderr)
	}
	combined := stdout + stderr
	if !strings.Contains(combined, "token_consumed") {
		t.Errorf("second login error should mention token_consumed; got:\n%s", combined)
	}
}

// writeInviteOnboardingConfigs writes minimal pricing / policy / users / teams
// / repos YAML files. Unlike writeMinimalConfigs in full_stack_test.go this
// registers the `platform` and `dogfood` teams so /admin/invites accepts
// team_id=dogfood and exchange-setup still works for the bootstrap admin.
func writeInviteOnboardingConfigs(t *testing.T, dir string) {
	t.Helper()
	policyDir := filepath.Join(dir, "policies")
	identityDir := filepath.Join(dir, "identity")
	if err := os.MkdirAll(policyDir, 0755); err != nil {
		t.Fatalf("mkdir policies: %v", err)
	}
	if err := os.MkdirAll(identityDir, 0755); err != nil {
		t.Fatalf("mkdir identity: %v", err)
	}

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
  - user_id: admin
    team_id: platform
    role: platform_admin
    api_key_hash: ""
`)
	writeE2EFile(t, filepath.Join(identityDir, "teams.yaml"), `
teams:
  - team_id: platform
    name: Platform Engineering
    budget_monthly_cap_cents: 0
  - team_id: dogfood
    name: Dogfood Team
    budget_monthly_cap_cents: 500000
`)
	writeE2EFile(t, filepath.Join(identityDir, "repos.yaml"), `
repos:
  - repo_id: test-repo
    remote_url: "https://github.com/test/repo"
    default_team_id: dogfood
    restricted: false
`)
}

// mustCaptureStdout runs cmd and fatals on non-zero exit, returning stdout.
func mustCaptureStdout(t *testing.T, dir string, env []string, cmd string, args ...string) string {
	t.Helper()
	c := exec.Command(cmd, args...)
	c.Dir = dir
	c.Env = env
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		t.Fatalf("%s %v: %v\nstdout:%s\nstderr:%s", cmd, args, err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// runCmdCaptureAll runs cmd and returns stdout, stderr, and exit code without
// failing the test on non-zero exit. Caller decides how to assert.
func runCmdCaptureAll(t *testing.T, dir string, env []string, cmd string, args ...string) (string, string, int) {
	t.Helper()
	c := exec.Command(cmd, args...)
	c.Dir = dir
	c.Env = env
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("%s %v: non-exit error: %v", cmd, args, err)
		}
	}
	return stdout.String(), stderr.String(), exitCode
}

// mustParseInviteToken extracts a 64-character hex token from `aicg-lp admin
// invite` stdout. The CLI prints the token alongside an "expires" line —
// we match the first 64-hex run that follows the standard preamble.
var inviteTokenRE = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

func mustParseInviteToken(t *testing.T, stdout string) string {
	t.Helper()
	m := inviteTokenRE.FindString(stdout)
	if m == "" {
		t.Fatalf("could not find 64-hex invite_token in admin invite stdout:\n%s", stdout)
	}
	return m
}

// readUserIDFromCredentials parses the user_id field out of a credentials file.
func readUserIDFromCredentials(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "user_id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "user_id:"))
		}
	}
	return ""
}

// requireAuditEvent asserts the audit_event table has at least `min` rows
// matching the given event_type.
func requireAuditEvent(t *testing.T, pool *pgxpool.Pool, eventType string, min int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_event WHERE event_type = $1`, eventType,
	).Scan(&count); err != nil {
		t.Fatalf("query audit_event %s: %v", eventType, err)
	}
	t.Logf("audit_event %s rows: %d", eventType, count)
	if count < min {
		t.Errorf("audit_event %s rows = %d, want >= %d", eventType, count, min)
	}
}
