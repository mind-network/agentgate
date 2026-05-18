// Package claude_code provides DB-backed conformance test for R-22.
//
// TestE2EStreamingCodeEditDBBacked is the formal R-22 evidence: it runs
// LP -> GW -> mock upstream end-to-end against a real testcontainer
// Postgres, then asserts each of the §20.5 R-22 acceptance points
// (a)..(e) using real DB rows and live subprocess output.
//
// The lighter-weight in-memory smoke test TestE2EStreamingCodeEdit
// (e2e_streaming_test.go) is intentionally kept so CI runners without
// Docker still exercise the wiring at unit speed.
package claude_code

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgate/internal/gw/config"
	"agentgate/internal/gw/policy"
	"agentgate/internal/gw/routing"
	"agentgate/internal/lp/gwclient"
	"agentgate/tests/conformance/upstream_mock"
)

// TestE2EStreamingCodeEditDBBacked covers R-22 acceptance points (a)..(e)
// against a testcontainer Postgres with aicg-gw and aicg-lp running as
// subprocesses. Docker unavailability causes a t.Skip rather than a
// failure (delegated to StartPostgres).
//
// T2 scope: wire LP -> GW -> mock upstream, issue one streaming code_edit
// request with tools, parse trace_id from the response header, log it,
// PASS. T3/T4 will append the DB and CLI assertions in subsequent tasks.
//
// trace_id source: GW writes X-AICG-Trace-Id (see
// internal/gw/edge/router.go:158-166 and
// internal/gw/server/ingress_pipeline.go:222); LP relayStream
// preserves response headers (internal/lp/server/anthropic.go:246-252)
// for streaming responses, so the LP client sees the same header.
func TestE2EStreamingCodeEditDBBacked(t *testing.T) {
	if os.Getenv("E2E_SKIP") != "" {
		t.Skip("E2E_SKIP set")
	}

	// ---- 1. Mock Anthropic upstream (reuse shared SSE fixture) ----
	mockUpstream := upstream_mock.NewServer()
	t.Cleanup(mockUpstream.Close)

	// ---- 2. Postgres testcontainer + migrations 0001..0006 ----
	// StartPostgres skips with t.Skipf("docker unavailable: %v", err)
	// when Docker is not reachable.
	pool, dsn := StartPostgres(t)

	// ---- 3. Generate dev certs and build binaries ----
	certDir := GenerateCerts(t)
	gwBin, lpBin := BuildBinaries(t)

	// ---- 4. Inline configs (pools points at mock upstream) ----
	configDir := t.TempDir()
	WriteMinimalConfigs(t, configDir)
	poolsPath := filepath.Join(t.TempDir(), "pools.yaml")
	writeDBBackedPools(t, poolsPath, mockUpstream.URL)

	// ---- 5. Start GW subprocess ----
	gwAddr := fmt.Sprintf("127.0.0.1:%d", FreePort(t))
	setupTokenPath := filepath.Join(t.TempDir(), "setup-token")
	gwURL := StartGW(t, GWStartOpts{
		BinPath:        gwBin,
		DSN:            dsn,
		PoolsPath:      poolsPath,
		ConfigDir:      configDir,
		CertDir:        certDir,
		ListenAddr:     gwAddr,
		SetupTokenPath: setupTokenPath,
	})

	// ---- 6. Bootstrap setup_token; bind a stable HOME for LP ----
	lpHome := t.TempDir()
	caPath := filepath.Join(certDir, "ca.crt")
	adminAPIKey := BootstrapSetupToken(t, lpBin, gwURL, setupTokenPath, caPath, lpHome)

	// ---- 7. Start LP subprocess sharing the same HOME ----
	lpPort := FreePort(t)
	lpURL := StartLP(t, LPStartOpts{
		BinPath: lpBin,
		HomeDir: lpHome,
		Port:    lpPort,
	})

	// ---- 8. Streaming code_edit request through LP ----
	reqBody := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"system": "You are a coding assistant.",
		"tools": [
			{
				"name": "write_file",
				"description": "Write content to a file",
				"input_schema": {
					"type": "object",
					"properties": {
						"path": {"type": "string"},
						"content": {"type": "string"}
					},
					"required": ["path", "content"]
				}
			},
			{
				"name": "edit_file",
				"description": "Edit an existing file",
				"input_schema": {
					"type": "object",
					"properties": {
						"path": {"type": "string"},
						"old_string": {"type": "string"},
						"new_string": {"type": "string"}
					},
					"required": ["path", "old_string", "new_string"]
				}
			}
		],
		"tool_choice": {"type": "auto"},
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Add logging to handler.go"}]}
		]
	}`)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Post(lpURL+"/anthropic/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("LP forward: %v", err)
	}
	streamBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LP forward status %d, body:\n%s", resp.StatusCode, streamBody)
	}
	if !bytes.Contains(streamBody, []byte("event: message_start")) {
		t.Fatalf("LP stream missing message_start; body:\n%s", streamBody)
	}
	if !bytes.Contains(streamBody, []byte("event: content_block_delta")) {
		t.Fatalf("LP stream missing content_block_delta; body:\n%s", streamBody)
	}
	if !bytes.Contains(streamBody, []byte("event: message_stop")) {
		t.Fatalf("LP stream missing message_stop; body:\n%s", streamBody)
	}

	// ---- 9. Extract trace_id from response header ----
	traceID := resp.Header.Get("X-AICG-Trace-Id")
	if traceID == "" {
		t.Fatalf("LP response missing X-AICG-Trace-Id header; headers=%v", resp.Header)
	}
	t.Logf("trace_id: %s", traceID)

	// ---- 10. Sanity: mock upstream received the request with tools ----
	if mockUpstream.RequestCount() != 1 {
		t.Errorf("expected 1 upstream request, got %d", mockUpstream.RequestCount())
	}
	if mockUpstream.RequestCount() > 0 {
		cap := mockUpstream.Requests[0]
		if len(cap.Tools) != 2 {
			t.Errorf("expected 2 tools forwarded upstream, got %d", len(cap.Tools))
		}
		if cap.ToolChoice == nil {
			t.Error("expected tool_choice forwarded upstream")
		}
		if cap.System == nil {
			t.Error("expected system forwarded upstream")
		}
	}

	// ---- T3: (a)(b)(c) DB assertions against trace_id ----
	ctx := context.Background()

	// (a) cost_event: cost_cents > 0, wire/endpoint_id/model non-empty
	var costCents int
	var wireStr, endpointID, modelStr string
	err = pool.QueryRow(ctx,
		"SELECT cost_cents, wire, endpoint_id, model FROM cost_event WHERE trace_id=$1", traceID).
		Scan(&costCents, &wireStr, &endpointID, &modelStr)
	if err != nil {
		t.Fatalf("(a) cost_event query: %v", err)
	}
	if costCents <= 0 {
		t.Errorf("(a) cost_event cost_cents=%d, want >0", costCents)
	}
	if wireStr == "" {
		t.Errorf("(a) cost_event wire empty")
	}
	if endpointID == "" {
		t.Errorf("(a) cost_event endpoint_id empty")
	}
	if modelStr == "" {
		t.Errorf("(a) cost_event model empty")
	}
	t.Logf("(a) cost_event: cost_cents=%d wire=%s endpoint_id=%s model=%s", costCents, wireStr, endpointID, modelStr)

	// (b) routing_event: seed len >= 16 and not all zeros
	var seed []byte
	err = pool.QueryRow(ctx,
		"SELECT seed FROM routing_event WHERE trace_id=$1", traceID).Scan(&seed)
	if err != nil {
		t.Fatalf("(b) routing_event query: %v", err)
	}
	if len(seed) < 16 {
		t.Errorf("(b) routing_event seed len=%d, want >=16", len(seed))
	}
	allZero := true
	for _, b := range seed {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Errorf("(b) routing_event seed is all zeros")
	}
	t.Logf("(b) routing_event: seed len=%d", len(seed))

	// (c) audit_event: count >= 1 and at least one relevant event type.
	// Handoff originally specified {route, allow} but actual enum from
	// internal/gw/audit/writer.go is {request_received, decision_made,
	// provider_call, request_completed, ...}. We assert against the
	// real event types produced by a successful request flow.
	rows, err := pool.Query(ctx,
		"SELECT event_type FROM audit_event WHERE trace_id=$1", traceID)
	if err != nil {
		t.Fatalf("(c) audit_event query: %v", err)
	}
	defer rows.Close()

	var eventTypes []string
	hasRelevant := false
	relevant := map[string]bool{
		"decision_made":     true,
		"provider_call":     true,
		"request_completed": true,
	}
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			t.Fatalf("(c) audit_event scan: %v", err)
		}
		eventTypes = append(eventTypes, et)
		if relevant[et] {
			hasRelevant = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("(c) audit_event rows error: %v", err)
	}
	if len(eventTypes) < 1 {
		t.Errorf("(c) audit_event count=%d, want >=1", len(eventTypes))
	}
	if !hasRelevant {
		t.Errorf("(c) audit_event event_types=%v, want at least one of decision_made, provider_call, request_completed", eventTypes)
	}
	t.Logf("(c) audit_event: count=%d types=%v", len(eventTypes), eventTypes)

	// ---- T4: (d) aicg-lp status + (e) replay consistency ----

	// (d) aicg-lp status stdout must contain trace_id
	statusCmd := exec.Command(lpBin, "status")
	statusCmd.Env = append(os.Environ(), "HOME="+lpHome)
	statusOut, err := statusCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("(d) aicg-lp status: %v\n%s", err, statusOut)
	}
	if !strings.Contains(string(statusOut), traceID) {
		t.Errorf("(d) aicg-lp status stdout missing trace_id=%s", traceID)
	}
	t.Logf("(d) aicg-lp status:\n%s", statusOut)

	// (e) replay consistency: gwclient.GetRoutingEvents vs local BuildChain
	client := gwclient.NewClient(gwURL, adminAPIKey, caPath)
	routingRows, err := client.GetRoutingEvents(traceID)
	if err != nil {
		t.Fatalf("(e) GetRoutingEvents: %v", err)
	}
	if len(routingRows) == 0 {
		t.Fatalf("(e) GetRoutingEvents: no rows for trace_id=%s", traceID)
	}
	storedRow := routingRows[0]

	// Parse stored member JSON to extract endpoint_id and model.
	// The JSON was marshaled from routing.Member which has no json tags,
	// so keys are PascalCase (EndpointID, Model, etc.).
	var storedMember routing.Member
	if err := json.Unmarshal([]byte(storedRow.MemberJSON), &storedMember); err != nil {
		t.Fatalf("(e) parse member JSON: %v", err)
	}

	// Load the same pools + pricing config the GW used.
	poolsCfg, err := config.LoadPools(poolsPath)
	if err != nil {
		t.Fatalf("(e) load pools: %v", err)
	}
	pricingCfg, err := config.LoadPricing(filepath.Join(configDir, "pricing.yaml"))
	if err != nil {
		t.Fatalf("(e) load pricing: %v", err)
	}
	selector := routing.NewSelector(poolsCfg, pricingCfg)

	// Reproduce the local decision with the same parameters as GW.
	// GW ingress pipeline uses attempt_no=1 (internal/gw/server/ingress_pipeline.go:129).
	dec := &policy.Decision{ModelPool: storedRow.Pool}
	localChain, err := selector.BuildChain(storedRow.Pool, dec, traceID, storedRow.AttemptNo)
	if err != nil {
		t.Fatalf("(e) local BuildChain: %v", err)
	}
	if len(localChain) == 0 {
		t.Fatalf("(e) local BuildChain: empty chain")
	}
	localMember := localChain[0]

	// Compare endpoint_id.
	if storedMember.EndpointID != localMember.EndpointID {
		t.Errorf("(e) endpoint_id mismatch: stored=%s local=%s", storedMember.EndpointID, localMember.EndpointID)
	}

	// Compare model.
	if storedMember.Model != localMember.Model {
		t.Errorf("(e) model mismatch: stored=%s local=%s", storedMember.Model, localMember.Model)
	}

	// Compare seed. gwclient.GetRoutingEvents does not expose seed (dashboard
	// API RoutingEventRow has no Seed field), so we query the DB directly.
	var dbSeed []byte
	err = pool.QueryRow(ctx,
		"SELECT seed FROM routing_event WHERE trace_id=$1", traceID).Scan(&dbSeed)
	if err != nil {
		t.Fatalf("(e) seed query: %v", err)
	}
	localSeed := routing.SeedBytes(traceID, storedRow.Pool, storedRow.AttemptNo)
	if !bytes.Equal(dbSeed, localSeed) {
		t.Errorf("(e) seed mismatch: db=%x local=%x", dbSeed, localSeed)
	}

	t.Logf("(e) replay: pool=%s endpoint_id=%s model=%s seed_match=true",
		storedRow.Pool, storedMember.EndpointID, storedMember.Model)
}

// writeDBBackedPools writes a pools.yaml that registers mock-ep pointing
// at the in-process Anthropic mock upstream and a pool "standard" that
// uses exactly one member, mock-ep / claude-sonnet-4-6.
func writeDBBackedPools(t *testing.T, path, upstreamURL string) {
	t.Helper()
	content := fmt.Sprintf(`provider_endpoints:
  mock-ep:
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
      extended_thinking: false

pools:
  standard:
    members:
      - { endpoint_id: mock-ep, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 90000
`, upstreamURL)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write pools.yaml: %v", err)
	}
}
