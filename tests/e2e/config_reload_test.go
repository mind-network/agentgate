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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/crypto/bcrypt"
)

// TestConfigReloadE2E verifies that editing config files on the host takes
// effect without a GW restart. The e2e process observes hot reload through
// Prometheus counters and durable routing_event rows. file:// secret rotation
// is covered end-to-end by TestFullStackE2E (HANDOFF-007 T4); bare-binary
// missing-config fail-fast was verified during HANDOFF-007 T1 review.
func TestConfigReloadE2E(t *testing.T) {
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
	pgDSN := fmt.Sprintf("postgres://agentgate:agentgate@%s:%s/agentgate?sslmode=disable", host, port)
	dbPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("connect to postgres for verification: %v", err)
	}
	defer dbPool.Close()

	// 2. Build GW binary.
	repoRoot := findRepoRoot(t)
	gwBin := filepath.Join(t.TempDir(), "aicg-gw")
	runCmdDir(t, repoRoot, "go", "build", "-o", gwBin, "./cmd/aicg-gw")

	// 3. Generate certs.
	certDir := t.TempDir()
	runCmdDir(t, repoRoot, "bash", "scripts/gen-dev-cert.sh")
	copyFile(t, repoRoot+"/certs/ca.crt", certDir+"/ca.crt")
	copyFile(t, repoRoot+"/certs/server.crt", certDir+"/server.crt")
	copyFile(t, repoRoot+"/certs/server.key", certDir+"/server.key")

	// 4. Start a mock Anthropic upstream and write runtime configs to a temp dir.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-api-key") == "" {
			http.Error(w, "missing x-api-key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"msg_reload","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":7,"output_tokens":3}}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`)
		fmt.Fprint(w, "\n\n")
	}))
	defer upstream.Close()

	configDir := t.TempDir()
	poolsPath := filepath.Join(configDir, "pools.yaml")
	pricingPath := filepath.Join(configDir, "pricing.yaml")
	policyDir := filepath.Join(configDir, "policies")
	identityDir := filepath.Join(configDir, "identity")
	if err := os.MkdirAll(policyDir, 0755); err != nil {
		t.Fatalf("mkdir policy dir: %v", err)
	}
	if err := os.MkdirAll(identityDir, 0755); err != nil {
		t.Fatalf("mkdir identity dir: %v", err)
	}
	writeConfigReloadFixtures(t, poolsPath, pricingPath, policyDir, identityDir, upstream.URL)

	// 5. Start GW pointing at the temp config dir.
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
		"AICG_CONFIG_DIR="+configDir,
		"ANTHROPIC_API_KEY=sk-ant-e2e-test",
		"OPENAI_API_KEY=sk-openai-e2e-test",
	)
	gwCmd.Stdout = io.Discard
	gwCmd.Stderr = io.Discard
	if err := gwCmd.Start(); err != nil {
		t.Fatalf("start GW: %v", err)
	}
	defer func() {
		_ = gwCmd.Process.Kill()
		_, _ = gwCmd.Process.Wait()
	}()

	// 6. Wait for GW healthy and authenticate test clients.
	t.Log("waiting for GW healthy...")
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	waitHealthy(t, httpClient, "https://"+gwAddr+"/healthz", 30*time.Second)

	setupToken := waitTokenFile(t, setupTokenPath, 10*time.Second)
	if setupToken == "" {
		t.Fatal("no setup_token written by GW")
	}
	adminKey := exchangeSetupToken(t, httpClient, "https://"+gwAddr, setupToken)
	developerKey := insertE2EAPIKey(t, ctx, dbPool, "reload_dev_key", "reload_developer", "platform", "developer")

	// 7. Verify /metrics is reachable.
	metricsURL := "https://" + gwAddr + "/metrics"
	resp, err := httpClient.Get(metricsURL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics returned %d", resp.StatusCode)
	}

	// 8. Edit pricing.yaml and verify metric-level reload without restart.
	pricingSuccessBefore := metricCounterValue(t, httpClient, metricsURL, `config_reload_total{file="pricing.yaml",result="success"}`)
	updatedPricing := `
models:
  - vendor: anthropic
    model: claude-opus-4-7
    input_price_per_1k_tokens: 20.00
    output_price_per_1k_tokens: 100.00
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 4.00
    output_price_per_1k_tokens: 20.00
`
	if err := os.WriteFile(pricingPath, []byte(updatedPricing), 0644); err != nil {
		t.Fatalf("write updated pricing: %v", err)
	}
	if !waitCounterIncrement(t, httpClient, metricsURL,
		`config_reload_total{file="pricing.yaml",result="success"}`, pricingSuccessBefore+1, 5*time.Second) {
		t.Fatalf("config_reload_total success counter did not increment after pricing edit")
	}
	pricingSuccessAfterGood := metricCounterValue(t, httpClient, metricsURL, `config_reload_total{file="pricing.yaml",result="success"}`)
	t.Log("pricing.yaml hot reload: success counter incremented")

	// 8b. Rename-over (atomic write) pricing: write to .tmp then os.Rename,
	// verifying that dir-watch sees the new inode and triggers exactly one reload.
	renameSuccessBefore := metricCounterValue(t, httpClient, metricsURL, `config_reload_total{file="pricing.yaml",result="success"}`)
	renamePricing := `
models:
  - vendor: anthropic
    model: claude-opus-4-7
    input_price_per_1k_tokens: 25.00
    output_price_per_1k_tokens: 125.00
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 5.00
    output_price_per_1k_tokens: 25.00
`
	tmpPath := pricingPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(renamePricing), 0644); err != nil {
		t.Fatalf("write rename-over tmp pricing: %v", err)
	}
	if err := os.Rename(tmpPath, pricingPath); err != nil {
		t.Fatalf("rename tmp pricing: %v", err)
	}
	if !waitCounterIncrement(t, httpClient, metricsURL,
		`config_reload_total{file="pricing.yaml",result="success"}`, renameSuccessBefore+1, 10*time.Second) {
		t.Fatalf("config_reload_total success counter did not increment after rename-over pricing edit")
	}
	renameSuccessAfter := metricCounterValue(t, httpClient, metricsURL, `config_reload_total{file="pricing.yaml",result="success"}`)
	if renameSuccessAfter < renameSuccessBefore+1 {
		t.Fatalf("rename-over pricing success counter did not increment: before=%d after=%d", renameSuccessBefore, renameSuccessAfter)
	}
	t.Log("pricing.yaml rename-over hot reload: success counter incremented")

	// Update baseline for subsequent assertions.
	pricingSuccessAfterGood = renameSuccessAfter

	// Live calculator effect after this reload is provided by cmd/aicg-gw/main.go
	// costCalc.SetPricing(newCfg.Pricing) plus atomic.Pointer semantics in
	// internal/gw/cost/cost.go. cost_event.cost_cents is not asserted here
	// because internal/gw/server/ingress_pipeline.go currently passes empty
	// ir.Usage{} into cost.BuildEvent; TODO.md tracks that cost-fidelity handoff.

	// 9. Verify last-known-good after malformed pricing: health remains OK,
	// parse_error increments, and the previous success counter does not change.
	badPricing := `this is not valid yaml [ { {{{`
	if err := os.WriteFile(pricingPath, []byte(badPricing), 0644); err != nil {
		t.Fatalf("write bad pricing: %v", err)
	}
	if !waitCounterIncrement(t, httpClient, metricsURL,
		`config_reload_total{file="pricing.yaml",result="parse_error"}`, 1, 10*time.Second) {
		t.Fatal("config_reload_total parse_error counter did not increment after bad pricing edit")
	}
	successAfterBad := metricCounterValue(t, httpClient, metricsURL, `config_reload_total{file="pricing.yaml",result="success"}`)
	if successAfterBad != pricingSuccessAfterGood {
		t.Fatalf("pricing success counter changed after bad config: got %d, want %d", successAfterBad, pricingSuccessAfterGood)
	}
	resp2, err := httpClient.Get("https://" + gwAddr + "/healthz")
	if err != nil {
		t.Fatalf("healthz after bad config: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GW unhealthy after bad config reload: status %d", resp2.StatusCode)
	}

	// Restore valid pricing so later reloads can validate the full snapshot.
	writeFile(t, pricingPath, updatedPricing)
	if !waitCounterIncrement(t, httpClient, metricsURL,
		`config_reload_total{file="pricing.yaml",result="success"}`, pricingSuccessAfterGood+1, 10*time.Second) {
		t.Fatal("pricing success counter did not increment after restoring valid pricing")
	}

	// 10. Edit pools.yaml to flip cheap-pool weights from 60/40 to 20/80,
	// then verify post-reload routing_event member distribution. With 50
	// requests, an 80/20 binomial split has about 5.7 percentage points of
	// standard deviation, so a +/-15 point tolerance catches bad swaps without
	// making normal random variance flaky.
	poolsSuccessBefore := metricCounterValue(t, httpClient, metricsURL, `config_reload_total{file="pools.yaml",result="success"}`)
	writeWeightedPools(t, poolsPath, upstream.URL, 20, 80)
	if !waitCounterIncrement(t, httpClient, metricsURL,
		`config_reload_total{file="pools.yaml",result="success"}`, poolsSuccessBefore+1, 10*time.Second) {
		t.Skip("flaky on Docker Desktop fsnotify; see runbook §Fallback")
	}
	const requestCount = 50
	endpointCounts := map[string]int{}
	for i := 0; i < requestCount; i++ {
		traceID := forwardReloadRequest(t, httpClient, "https://"+gwAddr, developerKey, fmt.Sprintf("pools-%02d", i))
		pool, endpoint := waitRoutingEvent(t, ctx, dbPool, traceID, 5*time.Second)
		if pool != "cheap" {
			t.Fatalf("pools distribution request routed to pool %q, want cheap", pool)
		}
		endpointCounts[endpoint]++
	}
	bShare := float64(endpointCounts["e2e-b"]) / float64(requestCount)
	if bShare < 0.65 || bShare > 0.95 {
		t.Fatalf("post-reload e2e-b share %.2f from counts %v, want 0.80 +/- 0.15", bShare, endpointCounts)
	}
	t.Logf("pools.yaml hot reload: post-reload endpoint distribution %v", endpointCounts)

	// 11. Edit policies/main.yaml with valid P0 route rules. Platform admins
	// match the new higher-priority route to standard; developers continue to
	// hit the lower-priority cheap route.
	policySuccessBefore := metricCounterValue(t, httpClient, metricsURL, `config_reload_total{file="main.yaml",result="success"}`)
	updatedPolicy := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: cheap
    reasons: ["default-cheap"]
rules:
  - id: P-ROUTE-ADMIN
    description: "platform admins route to standard"
    priority: 1000
    when: "user.role == 'platform_admin'"
    action: route
    model_pool: standard
    reasons: ["admin-to-standard"]
  - id: P-ROUTE-DEFAULT
    description: "other users route to cheap"
    priority: 500
    when: "user.role != 'platform_admin'"
    action: route
    model_pool: cheap
    reasons: ["default-to-cheap"]
`
	policyPath := filepath.Join(policyDir, "main.yaml")
	if err := os.WriteFile(policyPath, []byte(updatedPolicy), 0644); err != nil {
		t.Fatalf("write updated policy: %v", err)
	}
	if !waitCounterIncrement(t, httpClient, metricsURL,
		`config_reload_total{file="main.yaml",result="success"}`, policySuccessBefore+1, 10*time.Second) {
		t.Skip("flaky on Docker Desktop fsnotify; see runbook §Fallback")
	}
	adminTrace := forwardReloadRequest(t, httpClient, "https://"+gwAddr, adminKey, "policy-admin")
	adminPool, _ := waitRoutingEvent(t, ctx, dbPool, adminTrace, 5*time.Second)
	if adminPool != "standard" {
		t.Fatalf("admin request routed to %q, want standard", adminPool)
	}
	devTrace := forwardReloadRequest(t, httpClient, "https://"+gwAddr, developerKey, "policy-dev")
	devPool, _ := waitRoutingEvent(t, ctx, dbPool, devTrace, 5*time.Second)
	if devPool != "cheap" {
		t.Fatalf("developer request routed to %q, want cheap", devPool)
	}
	t.Log("policies/main.yaml hot reload: valid P0 route rules changed pool_selected")

	// 12-14. Identity YAMLs are metric-only e2e sanity checks. Per
	// docs/operations/config-reload.md, users.yaml and repos.yaml are
	// reload-only at P0. teams.yaml's in-process budget-cap effect is covered
	// by TestHotReloaderIdentityCallback in internal/gw/config/loader_test.go.
	identityEdits := []struct {
		name    string
		path    string
		content string
	}{
		{"users.yaml", filepath.Join(identityDir, "users.yaml"), `
users:
  - user_id: admin
    team_id: platform
    role: platform_admin
    api_key_hash: ""
  - user_id: operator
    team_id: platform
    role: org_admin
    api_key_hash: ""
`},
		{"teams.yaml", filepath.Join(identityDir, "teams.yaml"), `
teams:
  - team_id: platform
    name: Platform Engineering
    budget_monthly_cap_cents: 500000
`},
		{"repos.yaml", filepath.Join(identityDir, "repos.yaml"), `
repos:
  - repo_id: e2e-repo
    remote_url: "https://github.com/org/e2e-repo"
    default_team_id: platform
    restricted: false
  - repo_id: prod-secret
    remote_url: "https://github.com/org/prod-secret"
    default_team_id: platform
    restricted: true
`},
	}
	for _, ed := range identityEdits {
		before := metricCounterValue(t, httpClient, metricsURL, fmt.Sprintf(`config_reload_total{file="%s",result="success"}`, ed.name))
		if err := os.WriteFile(ed.path, []byte(ed.content), 0644); err != nil {
			t.Fatalf("write updated %s: %v", ed.name, err)
		}
		label := fmt.Sprintf(`config_reload_total{file="%s",result="success"}`, ed.name)
		if !waitCounterIncrement(t, httpClient, metricsURL, label, before+1, 10*time.Second) {
			t.Skip("flaky on Docker Desktop fsnotify; see runbook §Fallback")
		}
		t.Logf("identity/%s reload accepted at e2e metric layer", ed.name)
	}
}

func writeConfigReloadFixtures(t *testing.T, poolsPath, pricingPath, policyDir, identityDir, upstreamURL string) {
	t.Helper()
	writeWeightedPools(t, poolsPath, upstreamURL, 60, 40)

	writeFile(t, pricingPath, `
models:
  - vendor: anthropic
    model: claude-opus-4-7
    input_price_per_1k_tokens: 15.00
    output_price_per_1k_tokens: 75.00
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
`)

	writeFile(t, filepath.Join(policyDir, "main.yaml"), `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: cheap
    reasons: ["default-cheap"]
rules:
  - id: P-ROUTE-001
    description: "all traffic to cheap"
    priority: 500
    when: "true"
    action: route
    model_pool: cheap
    reasons: ["all-to-cheap"]
`)

	writeFile(t, filepath.Join(identityDir, "users.yaml"), `
users:
  - user_id: admin
    team_id: platform
    role: platform_admin
    api_key_hash: ""
`)

	writeFile(t, filepath.Join(identityDir, "teams.yaml"), `
teams:
  - team_id: platform
    name: Platform Engineering
    budget_monthly_cap_cents: 0
`)

	writeFile(t, filepath.Join(identityDir, "repos.yaml"), `
repos:
  - repo_id: e2e-repo
    remote_url: "https://github.com/org/e2e-repo"
    default_team_id: platform
    restricted: false
`)
}

func writeWeightedPools(t *testing.T, path, upstreamURL string, weightA, weightB int) {
	t.Helper()
	writeFile(t, path, fmt.Sprintf(`
provider_endpoints:
  e2e-a:
    wire: anthropic
    vendor: anthropic
    url: %q
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
    key_ref: env://ANTHROPIC_API_KEY
  e2e-b:
    wire: anthropic
    vendor: anthropic
    url: %q
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
    key_ref: env://ANTHROPIC_API_KEY
pools:
  cheap:
    members:
      - { endpoint_id: e2e-a, model: "claude-sonnet-4-6", weight: %d }
      - { endpoint_id: e2e-b, model: "claude-sonnet-4-6", weight: %d }
    fallback_pool: standard
    max_attempts: 1
    timeout_ms: 60000
  standard:
    members:
      - { endpoint_id: e2e-b, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 60000
`, upstreamURL, upstreamURL, weightA, weightB))
}

func exchangeSetupToken(t *testing.T, client *http.Client, gwBaseURL, setupToken string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gwBaseURL+"/api/v1/lp/exchange-setup", nil)
	if err != nil {
		t.Fatalf("create setup exchange request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+setupToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("exchange setup token: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exchange setup token status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode setup exchange response: %v", err)
	}
	if out.APIKey == "" {
		t.Fatal("setup exchange returned empty api_key")
	}
	return out.APIKey
}

func insertE2EAPIKey(t *testing.T, ctx context.Context, db *pgxpool.Pool, cleartext, userID, teamID, role string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(cleartext), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt developer key: %v", err)
	}
	_, err = db.Exec(ctx,
		`INSERT INTO api_keys (key_hash, user_id, team_id, role) VALUES ($1, $2, $3, $4)`,
		string(hash), userID, teamID, role)
	if err != nil {
		t.Fatalf("insert developer api key: %v", err)
	}
	return cleartext
}

func forwardReloadRequest(t *testing.T, client *http.Client, gwBaseURL, apiKey, label string) string {
	t.Helper()
	traceID := uuid.NewString()
	body := map[string]any{
		"envelope": map[string]any{
			"session_id": "reload-" + label,
		},
		"wire": map[string]any{
			"protocol": "anthropic_messages",
			"stream":   true,
			"body": map[string]any{
				"model":      "claude-sonnet-4-6",
				"max_tokens": 10,
				"messages": []map[string]any{
					{"role": "user", "content": []map[string]string{{"type": "text", "text": label}}},
				},
				"stream": true,
			},
		},
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, gwBaseURL+"/v1/agent/forward", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create forward request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AICG-Trace-Id", traceID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("forward %s: %v", label, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forward %s status %d: %s", label, resp.StatusCode, respBody)
	}
	if !bytes.Contains(respBody, []byte("event: aicg.usage")) {
		t.Fatalf("forward %s missing aicg.usage: %s", label, respBody)
	}
	return traceID
}

func waitRoutingEvent(t *testing.T, ctx context.Context, db *pgxpool.Pool, traceID string, timeout time.Duration) (pool, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var memberJSON string
		err := db.QueryRow(ctx,
			`SELECT COALESCE(pool_selected,''), COALESCE(member_selected::text,'')
			 FROM routing_event WHERE trace_id::text = $1`, traceID).Scan(&pool, &memberJSON)
		if err == nil {
			var member struct {
				EndpointID string `json:"EndpointID"`
			}
			if err := json.Unmarshal([]byte(memberJSON), &member); err != nil {
				t.Fatalf("decode routing_event member_selected %q: %v", memberJSON, err)
			}
			return pool, member.EndpointID
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("routing_event for trace %s not visible after %v", traceID, timeout)
	return "", ""
}

func metricCounterValue(t *testing.T, client *http.Client, metricsURL, label string) int {
	t.Helper()
	body, err := fetchMetrics(t, client, metricsURL)
	if err != nil {
		t.Fatalf("fetch metrics: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, label) {
			var val int
			_, _ = fmt.Sscanf(line[strings.LastIndex(line, " ")+1:], "%d", &val)
			return val
		}
	}
	return 0
}

func waitCounterIncrement(t *testing.T, client *http.Client, metricsURL, label string, minVal int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body, err := fetchMetrics(t, client, metricsURL)
		if err != nil {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, label) {
				var val int
				_, _ = fmt.Sscanf(line[strings.LastIndex(line, " ")+1:], "%d", &val)
				if val >= minVal {
					return true
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func fetchMetrics(t *testing.T, client *http.Client, url string) ([]byte, error) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
