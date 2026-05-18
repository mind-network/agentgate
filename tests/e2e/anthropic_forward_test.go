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
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestAnthropicForwardE2E(t *testing.T) {
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

	// 3. Generate certs.
	certDir := t.TempDir()
	runCmdDir(t, repoRoot, "bash", "scripts/gen-dev-cert.sh")
	copyFile(t, repoRoot+"/certs/ca.crt", certDir+"/ca.crt")
	copyFile(t, repoRoot+"/certs/server.crt", certDir+"/server.crt")
	copyFile(t, repoRoot+"/certs/server.key", certDir+"/server.key")

	// 4. Mock Anthropic upstream with body capture and configurable assertions.
	var mu sync.Mutex
	var upstreamBodies [][]byte

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBodies = append(upstreamBodies, body)
		mu.Unlock()

		key := r.Header.Get("x-api-key")
		if key == "" {
			http.Error(w, "missing x-api-key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"msg_e2e","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":5,"output_tokens":0}}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`)
		fmt.Fprint(w, "\n\n")
	}))
	defer upstream.Close()

	poolsPath := filepath.Join(t.TempDir(), "pools.yaml")
	writeE2EPools(t, poolsPath, upstream.URL)
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
	gwCmd.Stdout = io.Discard
	gwCmd.Stderr = io.Discard
	if err := gwCmd.Start(); err != nil {
		t.Fatalf("start GW: %v", err)
	}
	defer gwCmd.Process.Kill()

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	waitHealthy(t, httpClient, "https://"+gwAddr+"/healthz", 30*time.Second)

	// 6. Start LP.
	setupToken := waitTokenFile(t, setupTokenPath, 10*time.Second)
	if setupToken == "" {
		t.Fatal("no setup_token")
	}
	lpHome := t.TempDir()
	runCmdDirEnv(t, repoRoot, append(os.Environ(), "HOME="+lpHome),
		lpBin, "login", "--setup", setupToken, "--gateway", "https://"+gwAddr, "--ca", certDir+"/ca.crt")

	lpPort := freePort(t)
	lpCmd := exec.Command(lpBin, "start", "--port", fmt.Sprintf("%d", lpPort))
	lpCmd.Dir = repoRoot
	lpCmd.Env = append(os.Environ(), "HOME="+lpHome)
	lpCmd.Stdout = io.Discard
	lpCmd.Stderr = io.Discard
	if err := lpCmd.Start(); err != nil {
		t.Fatalf("start LP: %v", err)
	}
	defer lpCmd.Process.Kill()
	waitHTTPStatus(t, "http://127.0.0.1:"+fmt.Sprint(lpPort)+"/_aicg/health", http.StatusOK, 10*time.Second)

	lpURL := "http://127.0.0.1:" + fmt.Sprint(lpPort) + "/anthropic/v1/messages"

	// --- Sub-case: string_content ---
	t.Run("string_content", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"Say hello"}],"stream":true}`)
		resp, err := http.Post(lpURL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("LP forward: %v", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("string_content: status %d, body: %s", resp.StatusCode, respBody)
		}

		// Assert upstream received messages[0].content as array with one text block.
		mu.Lock()
		idx := len(upstreamBodies) - 1
		upBody := make([]byte, len(upstreamBodies[idx]))
		copy(upBody, upstreamBodies[idx])
		mu.Unlock()

		var parsed struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(upBody, &parsed); err != nil {
			t.Fatalf("parse upstream body: %v (body: %s)", err, string(upBody))
		}
		if len(parsed.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(parsed.Messages))
		}
		if len(parsed.Messages[0].Content) != 1 || parsed.Messages[0].Content[0].Type != "text" || parsed.Messages[0].Content[0].Text != "Say hello" {
			t.Errorf("content = %+v, want [{type:text text:Say hello}]", parsed.Messages[0].Content)
		}
	})

	// --- Sub-case: array_content ---
	t.Run("array_content", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"Hello from array"}]}],"stream":true}`)
		resp, err := http.Post(lpURL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("LP forward: %v", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("array_content: status %d, body: %s", resp.StatusCode, respBody)
		}

		mu.Lock()
		idx := len(upstreamBodies) - 1
		upBody := make([]byte, len(upstreamBodies[idx]))
		copy(upBody, upstreamBodies[idx])
		mu.Unlock()

		var parsed struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(upBody, &parsed); err != nil {
			t.Fatalf("parse upstream body: %v (body: %s)", err, string(upBody))
		}
		if len(parsed.Messages) != 1 || len(parsed.Messages[0].Content) != 1 || parsed.Messages[0].Content[0].Text != "Hello from array" {
			t.Errorf("content = %+v, want [{type:text text:Hello from array}]", parsed.Messages[0].Content)
		}
	})

	// --- Sub-case: malformed_body ---
	t.Run("malformed_body", func(t *testing.T) {
		// Record upstream call count before the malformed request.
		mu.Lock()
		callsBefore := len(upstreamBodies)
		mu.Unlock()

		body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":123}],"stream":true}`)
		resp, err := http.Post(lpURL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("LP forward: %v", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("malformed_body: expected 4xx, got %d. body: %s", resp.StatusCode, respBody)
		}

		// The response must not contain the old generic "failed to parse request" message.
		bodyStr := string(respBody)
		if strings.Contains(bodyStr, "failed to parse request") {
			t.Errorf("response must not contain the old generic 'failed to parse request' message: %s", bodyStr)
		}

		// The upstream stub must not have been called for this malformed request.
		mu.Lock()
		callsAfter := len(upstreamBodies)
		mu.Unlock()
		if callsAfter != callsBefore {
			t.Errorf("upstream stub was called %d times for malformed request, expected 0", callsAfter-callsBefore)
		}

		// The response must carry code "bad_request" with a trace_id and a
		// parse-cause substring from the underlying JSON error, proving the
		// GW typed bad-request boundary survives the LP->GW->LP path and
		// reaches the client (HANDOFF-015).
		var errResp struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			TraceID string `json:"trace_id"`
		}
		if err := json.Unmarshal(respBody, &errResp); err != nil {
			t.Fatalf("parse error response JSON: %v (body: %s)", err, bodyStr)
		}
		if errResp.Code != "bad_request" {
			t.Errorf("code = %q, want bad_request", errResp.Code)
		}
		if errResp.TraceID == "" {
			t.Error("trace_id must not be empty")
		}
		if !strings.Contains(errResp.Message, "cannot unmarshal") && !strings.Contains(errResp.Message, "json:") {
			t.Errorf("message must contain parse cause substring, got: %s", errResp.Message)
		}
		})
			// --- Sub-case: nested_tool_result_string_content ---
		t.Run("nested_tool_result_string_content", func(t *testing.T) {
			body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_001","content":"file contents here"}]}],"stream":true}`)
			resp, err := http.Post(lpURL, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("LP forward: %v", err)
			}
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("nested_tool_result_string_content: status %d, body: %s", resp.StatusCode, respBody)
			}

			mu.Lock()
			idx := len(upstreamBodies) - 1
			upBody := make([]byte, len(upstreamBodies[idx]))
			copy(upBody, upstreamBodies[idx])
			mu.Unlock()

			// Verify upstream received nested content as an array of blocks, not a string.
			var parsed struct {
				Messages []struct {
					Content []struct {
						Type      string          `json:"type"`
						ToolUseID string          `json:"tool_use_id"`
						Content   json.RawMessage `json:"content"`
					} `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(upBody, &parsed); err != nil {
				t.Fatalf("parse upstream body: %v (body: %s)", err, string(upBody))
			}
			if len(parsed.Messages) != 1 {
				t.Fatalf("expected 1 message, got %d", len(parsed.Messages))
			}
			if len(parsed.Messages[0].Content) != 1 {
				t.Fatalf("expected 1 top-level block, got %d", len(parsed.Messages[0].Content))
			}
			trBlock := parsed.Messages[0].Content[0]
			if trBlock.Type != "tool_result" || trBlock.ToolUseID != "tool_001" {
				t.Errorf("top-level block = {type:%q tool_use_id:%q}, want {type:tool_result tool_use_id:tool_001}", trBlock.Type, trBlock.ToolUseID)
			}

			// Nested content must be a JSON array of blocks.
			var nestedBlocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(trBlock.Content, &nestedBlocks); err != nil {
				t.Fatalf("nested content is not a JSON array: %v (raw: %s)", err, string(trBlock.Content))
			}
			if len(nestedBlocks) != 1 || nestedBlocks[0].Type != "text" || nestedBlocks[0].Text != "file contents here" {
				t.Errorf("nested blocks = %+v, want [{type:text text:file contents here}]", nestedBlocks)
			}
		})

		// --- Sub-case: nested_tool_result_malformed_content ---
		t.Run("nested_tool_result_malformed_content", func(t *testing.T) {
			mu.Lock()
			callsBefore := len(upstreamBodies)
			mu.Unlock()

			body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_001","content":123}]}],"stream":true}`)
			resp, err := http.Post(lpURL, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("LP forward: %v", err)
			}
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 400 || resp.StatusCode >= 500 {
				t.Fatalf("nested_tool_result_malformed_content: expected 4xx, got %d. body: %s", resp.StatusCode, respBody)
			}

			// Upstream must not have been called for this malformed request.
			mu.Lock()
			callsAfter := len(upstreamBodies)
			mu.Unlock()
			if callsAfter != callsBefore {
				t.Errorf("upstream stub was called %d times for malformed nested request, expected 0", callsAfter-callsBefore)
			}

			var errResp struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				TraceID string `json:"trace_id"`
			}
			if err := json.Unmarshal(respBody, &errResp); err != nil {
				t.Fatalf("parse error response JSON: %v (body: %s)", err, string(respBody))
			}
			if errResp.Code != "bad_request" {
				t.Errorf("code = %q, want bad_request", errResp.Code)
			}
			if errResp.TraceID == "" {
				t.Error("trace_id must not be empty")
			}
			if !strings.Contains(errResp.Message, "cannot unmarshal") && !strings.Contains(errResp.Message, "json:") {
				t.Errorf("message must contain parse cause substring, got: %s", errResp.Message)
			}
		})

// --- Sub-case: max_tokens and temperature propagation ---
	t.Run("max_tokens_and_temperature", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":50,"temperature":0.4,"messages":[{"role":"user","content":"Test max_tokens and temperature"}],"stream":true}`)
		resp, err := http.Post(lpURL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("LP forward: %v", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("max_tokens_and_temperature: status %d, body: %s", resp.StatusCode, respBody)
		}

		mu.Lock()
		idx := len(upstreamBodies) - 1
		upBody := make([]byte, len(upstreamBodies[idx]))
		copy(upBody, upstreamBodies[idx])
		mu.Unlock()

		var parsed struct {
			MaxTokens   int      `json:"max_tokens"`
			Temperature *float64 `json:"temperature"`
		}
		if err := json.Unmarshal(upBody, &parsed); err != nil {
			t.Fatalf("parse upstream body: %v (body: %s)", err, string(upBody))
		}
		if parsed.MaxTokens != 50 {
			t.Errorf("upstream max_tokens = %d, want 50", parsed.MaxTokens)
		}
		if parsed.Temperature == nil {
			t.Error("upstream temperature is nil, want 0.4")
		} else if *parsed.Temperature != 0.4 {
			t.Errorf("upstream temperature = %f, want 0.4", *parsed.Temperature)
		}
	})

	// --- Sub-case: temperature omitted ---
	t.Run("temperature_omitted", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":50,"messages":[{"role":"user","content":"Test temperature omitted"}],"stream":true}`)
		resp, err := http.Post(lpURL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("LP forward: %v", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("temperature_omitted: status %d, body: %s", resp.StatusCode, respBody)
		}

		mu.Lock()
		idx := len(upstreamBodies) - 1
		upBody := make([]byte, len(upstreamBodies[idx]))
		copy(upBody, upstreamBodies[idx])
		mu.Unlock()

		var parsed struct {
			MaxTokens   int      `json:"max_tokens"`
			Temperature *float64 `json:"temperature"`
		}
		if err := json.Unmarshal(upBody, &parsed); err != nil {
			t.Fatalf("parse upstream body: %v (body: %s)", err, string(upBody))
		}
		if parsed.MaxTokens != 50 {
			t.Errorf("upstream max_tokens = %d, want 50", parsed.MaxTokens)
		}
		if parsed.Temperature != nil {
			t.Errorf("upstream temperature = %v, want nil (omitted)", *parsed.Temperature)
		}
	})

	t.Log("Anthropic forward E2E: string_content, array_content, malformed_body, max_tokens_and_temperature, temperature_omitted all passed")
}
