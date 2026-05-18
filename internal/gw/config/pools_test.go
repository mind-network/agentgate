package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadPoolsSuccess(t *testing.T) {
	path := writeTemp(t, "pools-valid.yaml", validPoolsYAML)
	cfg, err := LoadPools(path)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(cfg.ProviderEndpoints) != 4 {
		t.Errorf("expected 4 endpoints, got %d", len(cfg.ProviderEndpoints))
	}
	if len(cfg.Pools) != 4 {
		t.Errorf("expected 4 pools, got %d", len(cfg.Pools))
	}
}

func TestLoadPoolsEndpointNotInRegistry(t *testing.T) {
	path := writeTemp(t, "pools-bad.yaml", badEndpointPoolsYAML)
	_, err := LoadPools(path)
	if err == nil {
		t.Fatal("expected error for unknown endpoint_id, got nil")
	}
	if !strings.Contains(err.Error(), "not found in provider_endpoints registry") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadPoolsEmptyRegistry(t *testing.T) {
	path := writeTemp(t, "pools-empty-registry.yaml", `
provider_endpoints: {}
pools:
  cheap:
    members:
      - { endpoint_id: x, model: m, weight: 1 }
    fallback_pool: standard
    max_attempts: 2
    timeout_ms: 60000
`)
	_, err := LoadPools(path)
	if err == nil {
		t.Fatal("expected error for empty registry, got nil")
	}
}

func TestLoadPoolsZeroWeight(t *testing.T) {
	path := writeTemp(t, "pools-zero-weight.yaml", `
provider_endpoints:
  ep1:
    wire: test
    vendor: test
    url: "https://example.com"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
pools:
  cheap:
    members:
      - { endpoint_id: ep1, model: m, weight: 0 }
    fallback_pool: standard
    max_attempts: 2
    timeout_ms: 60000
`)
	_, err := LoadPools(path)
	if err == nil {
		t.Fatal("expected error for zero weight, got nil")
	}
}

func TestLoadPoolsFileNotFound(t *testing.T) {
	_, err := LoadPools("/nonexistent/pools.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := t.TempDir() + "/" + name
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

const validPoolsYAML = `
provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: "https://api.anthropic.com"
    data_residency: us
    trust_tier: vendor
    supports:
      streaming: true
  openai-prod:
    wire: openai
    vendor: openai
    url: "https://api.openai.com/v1"
    data_residency: us
    trust_tier: vendor
    supports:
      streaming: true
  ollama-cluster:
    wire: openai_compat
    vendor: ollama-local
    url: "https://ollama.internal:11434/v1"
    data_residency: on_prem
    trust_tier: private
    supports:
      streaming: true
  vllm-prod:
    wire: openai_compat
    vendor: vllm-local
    url: "https://vllm.internal:8000/v1"
    data_residency: on_prem
    trust_tier: private
    supports:
      streaming: true
pools:
  cheap:
    members:
      - { endpoint_id: ollama-cluster, model: "qwen2.5-coder:7b", weight: 100 }
    fallback_pool: standard
    max_attempts: 2
    timeout_ms: 60000
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 60 }
      - { endpoint_id: openai-prod, model: "gpt-4o", weight: 40 }
    fallback_pool: strong
    max_attempts: 3
    timeout_ms: 90000
  strong:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-opus-4-7", weight: 80 }
      - { endpoint_id: openai-prod, model: "o3", weight: 20 }
    fallback_pool: null
    max_attempts: 2
    timeout_ms: 180000
  private_strong:
    members:
      - { endpoint_id: vllm-prod, model: "llama3.1-405b", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 300000
`

const badEndpointPoolsYAML = `
provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: "https://api.anthropic.com"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
pools:
  cheap:
    members:
      - { endpoint_id: non-existent-endpoint, model: "gpt-4", weight: 100 }
    fallback_pool: standard
    max_attempts: 2
    timeout_ms: 60000
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 60000
`
