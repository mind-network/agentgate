package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func writeConfigFiles(t *testing.T, dir, poolsContent, policyContent string) {
	t.Helper()
	if poolsContent != "" {
		writeFile(t, filepath.Join(dir, "pools.yaml"), poolsContent)
	}
	if policyContent != "" {
		writeFile(t, filepath.Join(dir, "policies", "main.yaml"), policyContent)
	}
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

func setupHotReloader(t *testing.T) (*HotReloader, string) {
	t.Helper()
	dir := t.TempDir()
	writeConfigFiles(t, dir, validPoolsYAML, validPolicyYAML)

	pricingDir := t.TempDir()
	writeFile(t, filepath.Join(pricingDir, "pricing.yaml"), validPricingYAML)

	identityDir := t.TempDir()
	writeFile(t, filepath.Join(identityDir, "users.yaml"), validUsersYAML)
	writeFile(t, filepath.Join(identityDir, "teams.yaml"), validTeamsYAML)
	writeFile(t, filepath.Join(identityDir, "repos.yaml"), validReposYAML)

	loader := &Loader{
		PoolsPath:   filepath.Join(dir, "pools.yaml"),
		PolicyPath:  filepath.Join(dir, "policies", "main.yaml"),
		PricingPath: filepath.Join(pricingDir, "pricing.yaml"),
		UsersPath:   filepath.Join(identityDir, "users.yaml"),
		TeamsPath:   filepath.Join(identityDir, "teams.yaml"),
		ReposPath:   filepath.Join(identityDir, "repos.yaml"),
	}

	cfg, err := loader.Load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}

	hr := NewHotReloader(cfg, loader)
	if err := hr.Start(); err != nil {
		t.Fatalf("start hot reloader: %v", err)
	}
	t.Cleanup(hr.Stop)

	return hr, dir
}

func TestHotReloaderSuccessSwap(t *testing.T) {
	hr, dir := setupHotReloader(t)

	orig := hr.Current()
	if len(orig.Pools.ProviderEndpoints) != 4 {
		t.Fatalf("initial endpoints count = %d, want 4", len(orig.Pools.ProviderEndpoints))
	}

	// Write modified pools.yaml with an extra endpoint + a new pool that uses it.
	modified := `
provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: "https://api.anthropic.com"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
  openai-prod:
    wire: openai
    vendor: openai
    url: "https://api.openai.com/v1"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
  ollama-cluster:
    wire: openai_compat
    vendor: ollama-local
    url: "https://ollama.internal:11434/v1"
    data_residency: on_prem
    trust_tier: private
    supports: { streaming: true }
  vllm-prod:
    wire: openai_compat
    vendor: vllm-local
    url: "https://vllm.internal:8000/v1"
    data_residency: on_prem
    trust_tier: private
    supports: { streaming: true }
  new-endpoint:
    wire: openai_compat
    vendor: vllm-local
    url: "https://new.internal:8000/v1"
    data_residency: on_prem
    trust_tier: private
    supports: { streaming: true }
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

	done := make(chan struct{})
	hr.OnReload(func(cfg *Config) {
		close(done)
	})

	writeFile(t, filepath.Join(dir, "pools.yaml"), modified)

	select {
	case <-done:
		// reload callback fired
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for hot reload after valid edit")
	}

	current := hr.Current()
	if len(current.Pools.ProviderEndpoints) != 5 {
		t.Errorf("after reload endpoints count = %d, want 5", len(current.Pools.ProviderEndpoints))
	}
}

func TestHotReloaderFailureRetainsOld(t *testing.T) {
	hr, dir := setupHotReloader(t)

	orig := hr.Current()
	origCount := len(orig.Pools.ProviderEndpoints)

	// Write invalid YAML that references a non-existent endpoint_id.
	invalid := `
provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: "https://api.anthropic.com"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
  openai-prod:
    wire: openai
    vendor: openai
    url: "https://api.openai.com/v1"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
pools:
  cheap:
    members:
      - { endpoint_id: NONEXISTENT, model: "m", weight: 100 }
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

	reloadAttempted := make(chan struct{})
	hr.OnReload(func(cfg *Config) {
		close(reloadAttempted)
	})

	writeFile(t, filepath.Join(dir, "pools.yaml"), invalid)

	// Wait long enough for fsnotify + debounce + reload to complete.
	// The reload should fail, so OnReload callback should NOT fire.
	select {
	case <-reloadAttempted:
		t.Fatal("OnReload fired unexpectedly — invalid config should not trigger successful reload")
	case <-time.After(2 * time.Second):
		// Expected: reload failed, callback never invoked.
	}

	current := hr.Current()
	if len(current.Pools.ProviderEndpoints) != origCount {
		t.Errorf("after failed reload endpoints count = %d, want %d (old config should be retained)", len(current.Pools.ProviderEndpoints), origCount)
	}
}

func TestHotReloaderPolicyEditTriggersReload(t *testing.T) {
	hr, dir := setupHotReloader(t)

	orig := hr.Current()
	origRuleCount := len(orig.Policy.Rules)

	// Write a modified policy with an extra rule.
	modified := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["default-fallthrough"]
rules:
  - id: P-ROUTE-001
    description: "summary / test_output → cheap"
    priority: 500
    when: 'server_class.task_type in ["summary","test_output"]'
    action: route
    model_pool: cheap
    reasons: ["low-complexity-to-cheap"]
  - id: P-ROUTE-002
    description: "code_edit → standard"
    priority: 500
    when: 'server_class.task_type in ["code_edit","simple_edit"]'
    action: route
    model_pool: standard
    reasons: ["standard-work-to-standard"]
  - id: P-ROUTE-003
    description: "debug → strong"
    priority: 500
    when: 'server_class.task_type in ["architecture","debug"]'
    action: route
    model_pool: strong
    reasons: ["complex-work-to-strong"]
  - id: P-NEW-001
    description: "new rule"
    priority: 100
    when: "true"
    action: allow
    reasons: ["test"]
`

	done := make(chan struct{})
	hr.OnReload(func(cfg *Config) {
		close(done)
	})

	writeFile(t, filepath.Join(dir, "policies", "main.yaml"), modified)

	select {
	case <-done:
		// reload callback fired
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for hot reload after policy edit")
	}

	current := hr.Current()
	if len(current.Policy.Rules) != origRuleCount+1 {
		t.Errorf("after reload policy rules = %d, want %d", len(current.Policy.Rules), origRuleCount+1)
	}
}

func TestHotReloaderIdentityCallback(t *testing.T) {
	hr, _ := setupHotReloader(t)

	orig := hr.Current()
	if len(orig.Identity.Teams) != 2 {
		t.Fatalf("initial teams count = %d, want 2", len(orig.Identity.Teams))
	}

	findTeam := func(cfg *Config, teamID string) *TeamIdentity {
		for i := range cfg.Identity.Teams {
			if cfg.Identity.Teams[i].TeamID == teamID {
				return &cfg.Identity.Teams[i]
			}
		}
		return nil
	}
	dogfood := findTeam(orig, "dogfood")
	if dogfood == nil || dogfood.BudgetMonthlyCapCents != 500000 {
		t.Fatalf("initial dogfood cap = %d, want 500000", dogfood.BudgetMonthlyCapCents)
	}

	// Modify teams.yaml: bump dogfood cap from 500000 to 750000.
	modified := `
teams:
  - team_id: platform
    name: Platform Engineering
    budget_monthly_cap_cents: 0
  - team_id: dogfood
    name: Dogfood Team
    budget_monthly_cap_cents: 750000
`
	var reloadedCfg *Config
	done := make(chan struct{})
	hr.OnReload(func(cfg *Config) {
		reloadedCfg = cfg
		close(done)
	})

	writeFile(t, hr.loader.TeamsPath, modified)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for hot reload after teams.yaml edit")
	}

	if reloadedCfg == nil {
		t.Fatal("reloadedCfg is nil")
	}
	dogfood = findTeam(reloadedCfg, "dogfood")
	if dogfood == nil {
		t.Fatal("dogfood team missing after reload")
	}
	if dogfood.BudgetMonthlyCapCents != 750000 {
		t.Errorf("reloaded dogfood cap = %d, want 750000", dogfood.BudgetMonthlyCapCents)
	}
}

func TestFileRefPaths(t *testing.T) {
	eps := map[string]ProviderEndpoint{
		"ep1": {KeyRef: "file:///run/secrets/key1"},
		"ep2": {KeyRef: "env://KEY2"},
		"ep3": {KeyRef: ""},
		"ep4": {KeyRef: "file:///run/secrets/key1"}, // duplicate
		"ep5": {KeyRef: "file:///run/secrets/key2"},
	}
	paths := fileRefPaths(eps)
	if len(paths) != 2 {
		t.Fatalf("got %d unique file paths, want 2: %v", len(paths), paths)
	}
	seen := make(map[string]bool)
	for _, p := range paths {
		seen[p] = true
	}
	if !seen["/run/secrets/key1"] || !seen["/run/secrets/key2"] {
		t.Errorf("missing expected paths in %v", paths)
	}
}

func TestHotReloaderSecretFileRotation(t *testing.T) {
	dir := t.TempDir()

	// Create a pools.yaml with a file:// key_ref.
	poolsWithSecret := `
provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: "https://api.anthropic.com"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
    key_ref: "env://ANTHROPIC_KEY"
  openai-prod:
    wire: openai
    vendor: openai
    url: "https://api.openai.com/v1"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
pools:
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 60000
`
	writeFile(t, filepath.Join(dir, "pools.yaml"), poolsWithSecret)
	minimalPolicy := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["default-fallthrough"]
rules:
  - id: P-ROUTE-001
    description: "everything to standard"
    priority: 500
    when: "true"
    action: route
    model_pool: standard
    reasons: ["all-to-standard"]
`
	writeFile(t, filepath.Join(dir, "policies", "main.yaml"), minimalPolicy)

	pricingDir := t.TempDir()
	writeFile(t, filepath.Join(pricingDir, "pricing.yaml"), validPricingYAML)

	identityDir := t.TempDir()
	writeFile(t, filepath.Join(identityDir, "users.yaml"), validUsersYAML)
	writeFile(t, filepath.Join(identityDir, "teams.yaml"), validTeamsYAML)
	writeFile(t, filepath.Join(identityDir, "repos.yaml"), validReposYAML)

	loader := &Loader{
		PoolsPath:   filepath.Join(dir, "pools.yaml"),
		PolicyPath:  filepath.Join(dir, "policies", "main.yaml"),
		PricingPath: filepath.Join(pricingDir, "pricing.yaml"),
		UsersPath:   filepath.Join(identityDir, "users.yaml"),
		TeamsPath:   filepath.Join(identityDir, "teams.yaml"),
		ReposPath:   filepath.Join(identityDir, "repos.yaml"),
	}

	// Set the env:// key so initial load succeeds.
	t.Setenv("ANTHROPIC_KEY", "initial-key")

	cfg, err := loader.Load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}

	// Verify the env:// key was resolved.
	if key := cfg.ResolvedEndpoints["anthropic-prod"].ResolvedKey; key != "initial-key" {
		t.Fatalf("initial resolved key = %q, want initial-key", key)
	}

	hr := NewHotReloader(cfg, loader)
	if err := hr.Start(); err != nil {
		t.Fatalf("start hot reloader: %v", err)
	}
	t.Cleanup(hr.Stop)

	// Rotate the env var and trigger a reload by touching pools.yaml.
	t.Setenv("ANTHROPIC_KEY", "rotated-key")

	done := make(chan struct{})
	hr.OnReload(func(cfg *Config) {
		close(done)
	})

	// Touch pools.yaml to trigger a reload.
	writeFile(t, filepath.Join(dir, "pools.yaml"), poolsWithSecret)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for reload after pools edit")
	}

	current := hr.Current()
	if key := current.ResolvedEndpoints["anthropic-prod"].ResolvedKey; key != "rotated-key" {
		t.Errorf("resolved key after env rotation = %q, want rotated-key", key)
	}
}

func TestHotReloaderFileSecretRotation(t *testing.T) {
	dir := t.TempDir()
	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "api-key")

	// Write the initial secret file.
	writeFile(t, secretPath, "secret-v1")

	poolsWithFileSecret := `
provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: "https://api.anthropic.com"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
    key_ref: "file://` + secretPath + `"
pools:
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 60000
`
	writeFile(t, filepath.Join(dir, "pools.yaml"), poolsWithFileSecret)

	minimalPolicy := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["default-fallthrough"]
rules:
  - id: P-ROUTE-001
    description: "everything to standard"
    priority: 500
    when: "true"
    action: route
    model_pool: standard
    reasons: ["all-to-standard"]
`
	writeFile(t, filepath.Join(dir, "policies", "main.yaml"), minimalPolicy)

	pricingDir := t.TempDir()
	writeFile(t, filepath.Join(pricingDir, "pricing.yaml"), validPricingYAML)

	identityDir := t.TempDir()
	writeFile(t, filepath.Join(identityDir, "users.yaml"), validUsersYAML)
	writeFile(t, filepath.Join(identityDir, "teams.yaml"), validTeamsYAML)
	writeFile(t, filepath.Join(identityDir, "repos.yaml"), validReposYAML)

	loader := &Loader{
		PoolsPath:   filepath.Join(dir, "pools.yaml"),
		PolicyPath:  filepath.Join(dir, "policies", "main.yaml"),
		PricingPath: filepath.Join(pricingDir, "pricing.yaml"),
		UsersPath:   filepath.Join(identityDir, "users.yaml"),
		TeamsPath:   filepath.Join(identityDir, "teams.yaml"),
		ReposPath:   filepath.Join(identityDir, "repos.yaml"),
	}

	cfg, err := loader.Load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if key := cfg.ResolvedEndpoints["anthropic-prod"].ResolvedKey; key != "secret-v1" {
		t.Fatalf("initial resolved file:// key = %q, want secret-v1", key)
	}

	hr := NewHotReloader(cfg, loader)
	if err := hr.Start(); err != nil {
		t.Fatalf("start hot reloader: %v", err)
	}
	t.Cleanup(hr.Stop)

	// Verify the secret file is being watched.
	if !hr.secretPaths[secretPath] {
		t.Errorf("secret path %q not found in watched paths: %v", secretPath, hr.secretPaths)
	}

	// Rotate the file:// secret on disk.
	done := make(chan struct{})
	hr.OnReload(func(cfg *Config) {
		close(done)
	})

	writeFile(t, secretPath, "secret-v2")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for hot reload after file:// secret rotation")
	}

	current := hr.Current()
	if key := current.ResolvedEndpoints["anthropic-prod"].ResolvedKey; key != "secret-v2" {
		t.Errorf("resolved key after file:// rotation = %q, want secret-v2", key)
	}
}

func TestSecretReloadLogOmitsPlaintext(t *testing.T) {
	dir := t.TempDir()
	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "api-key")
	secretValue := "sk-ant-secret-v1-abcdef"

	writeFile(t, secretPath, secretValue)

	poolsWithSecret := `
provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: "https://api.anthropic.com"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
    key_ref: "file://` + secretPath + `"
pools:
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 60000
`
	minimalPolicy := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["default-fallthrough"]
rules:
  - id: P-ROUTE-001
    description: "everything to standard"
    priority: 500
    when: "true"
    action: route
    model_pool: standard
    reasons: ["all-to-standard"]
`
	writeFile(t, filepath.Join(dir, "pools.yaml"), poolsWithSecret)
	writeFile(t, filepath.Join(dir, "policies", "main.yaml"), minimalPolicy)

	pricingDir := t.TempDir()
	writeFile(t, filepath.Join(pricingDir, "pricing.yaml"), validPricingYAML)

	identityDir := t.TempDir()
	writeFile(t, filepath.Join(identityDir, "users.yaml"), validUsersYAML)
	writeFile(t, filepath.Join(identityDir, "teams.yaml"), validTeamsYAML)
	writeFile(t, filepath.Join(identityDir, "repos.yaml"), validReposYAML)

	loader := &Loader{
		PoolsPath:   filepath.Join(dir, "pools.yaml"),
		PolicyPath:  filepath.Join(dir, "policies", "main.yaml"),
		PricingPath: filepath.Join(pricingDir, "pricing.yaml"),
		UsersPath:   filepath.Join(identityDir, "users.yaml"),
		TeamsPath:   filepath.Join(identityDir, "teams.yaml"),
		ReposPath:   filepath.Join(identityDir, "repos.yaml"),
	}

	cfg, err := loader.Load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if key := cfg.ResolvedEndpoints["anthropic-prod"].ResolvedKey; key != secretValue {
		t.Fatalf("initial resolved key = %q, want %q", key, secretValue)
	}

	// Capture log output.
	var logBuf strings.Builder
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(origLogger)

	hr := NewHotReloader(cfg, loader)
	if err := hr.Start(); err != nil {
		t.Fatalf("start hot reloader: %v", err)
	}
	defer hr.Stop()

	// Trigger a reload by rotating the secret.
	done := make(chan struct{})
	hr.OnReload(func(cfg *Config) {
		close(done)
	})

	writeFile(t, secretPath, "sk-ant-secret-v2-123456")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for hot reload after secret rotation")
	}

	logOutput := logBuf.String()
	// The log must not contain the plaintext secret value.
	if strings.Contains(logOutput, secretValue) {
		t.Errorf("log output contains plaintext secret value %q", secretValue)
	}
	if strings.Contains(logOutput, "sk-ant-secret-v2-123456") {
		t.Errorf("log output contains rotated plaintext secret value")
	}
	// The log must contain the secret_reload event.
	if !strings.Contains(logOutput, "secret_reload") {
		t.Errorf("log output missing secret_reload event")
	}
	// The log must mention the ref scheme, not the value.
	if !strings.Contains(logOutput, "ref") {
		t.Errorf("log output missing ref field")
	}
}

// TestSecretReloadResolveError verifies that when a file:// secret cannot be
// resolved during HotReloader.reload, the Prometheus counter
// secret_reload_total{ref="file",result="resolve_error"} is incremented and a
// structured log line is emitted per HANDOFF-007 T5.
func TestSecretReloadResolveError(t *testing.T) {
	dir := t.TempDir()
	secretDir := t.TempDir()
	missingPath := filepath.Join(secretDir, "does-not-exist")

	okPools := `
provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: "https://api.anthropic.com"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
pools:
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 60000
`
	writeFile(t, filepath.Join(dir, "pools.yaml"), okPools)

	badPools := `
provider_endpoints:
  anthropic-prod:
    wire: anthropic
    vendor: anthropic
    url: "https://api.anthropic.com"
    data_residency: us
    trust_tier: vendor
    supports: { streaming: true }
    key_ref: "file://` + missingPath + `"
pools:
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 60000
`

	minimalPolicy := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["default-fallthrough"]
rules:
  - id: P-ROUTE-001
    description: "everything to standard"
    priority: 500
    when: "true"
    action: route
    model_pool: standard
    reasons: ["all-to-standard"]
`
	writeFile(t, filepath.Join(dir, "policies", "main.yaml"), minimalPolicy)

	pricingDir := t.TempDir()
	writeFile(t, filepath.Join(pricingDir, "pricing.yaml"), validPricingYAML)

	identityDir := t.TempDir()
	writeFile(t, filepath.Join(identityDir, "users.yaml"), validUsersYAML)
	writeFile(t, filepath.Join(identityDir, "teams.yaml"), validTeamsYAML)
	writeFile(t, filepath.Join(identityDir, "repos.yaml"), validReposYAML)

	loader := &Loader{
		PoolsPath:   filepath.Join(dir, "pools.yaml"),
		PolicyPath:  filepath.Join(dir, "policies", "main.yaml"),
		PricingPath: filepath.Join(pricingDir, "pricing.yaml"),
		UsersPath:   filepath.Join(identityDir, "users.yaml"),
		TeamsPath:   filepath.Join(identityDir, "teams.yaml"),
		ReposPath:   filepath.Join(identityDir, "repos.yaml"),
	}

	cfg, err := loader.Load()
	if err != nil {
		t.Fatalf("initial Load: %v", err)
	}

	// Capture structured log output.
	var logBuf bytes.Buffer
	prevHandler := swapLogHandler(t, slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defer swapLogHandler(t, prevHandler)

	hr := NewHotReloader(cfg, loader)
	if err := hr.Start(); err != nil {
		t.Fatalf("start hot reloader: %v", err)
	}
	defer hr.Stop()

	// Read the current value of the resolve_error counter.
	getResolveErrorCount := func() float64 {
		metrics := prometheusGather(t)
		for _, mf := range metrics {
			if mf.GetName() == "secret_reload_total" {
				for _, m := range mf.GetMetric() {
					labels := m.GetLabel()
					ref := ""
					result := ""
					for _, l := range labels {
						if l.GetName() == "ref" {
							ref = l.GetValue()
						}
						if l.GetName() == "result" {
							result = l.GetValue()
						}
					}
					if ref == "file" && result == "resolve_error" {
						return m.GetCounter().GetValue()
					}
				}
			}
		}
		return 0
	}

	prevCount := getResolveErrorCount()

	// Write bad pools — triggers a reload that fails on the missing file:// secret.
	writeFile(t, filepath.Join(dir, "pools.yaml"), badPools)

	// Wait for fsnotify + debounce + reload.
	time.Sleep(2 * time.Second)

	// The reload must have failed and incremented the resolve_error counter.
	newCount := getResolveErrorCount()
	if newCount <= prevCount {
		t.Errorf("secret_reload_total{ref=\"file\",result=\"resolve_error\"} did not increment: prev=%v, new=%v", prevCount, newCount)
	} else {
		t.Logf("resolve_error counter incremented: prev=%v, new=%v", prevCount, newCount)
	}

	// The structured log must contain a secret_reload resolve_error event.
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "secret_reload") {
		t.Error("structured log missing secret_reload event after resolve_error")
	}
	if !strings.Contains(logOutput, "resolve_error") {
		t.Error("structured log missing result=resolve_error after secret resolution failure")
	}
	t.Logf("resolve_error log captured")
}

func TestHotReloaderTriggerReload(t *testing.T) {
	hr, _ := setupHotReloader(t)

	getSighupSuccess := func() float64 {
		gathered, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatalf("prometheus gather: %v", err)
		}
		for _, mf := range gathered {
			if mf.GetName() == "config_reload_total" {
				for _, m := range mf.GetMetric() {
					var file, result string
					for _, l := range m.GetLabel() {
						if l.GetName() == "file" {
							file = l.GetValue()
						}
						if l.GetName() == "result" {
							result = l.GetValue()
						}
					}
					if file == "<sighup>" && result == "success" {
						return m.GetCounter().GetValue()
					}
				}
			}
		}
		return 0
	}

	before := getSighupSuccess()
	hr.TriggerReload()

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if getSighupSuccess() >= before+1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	after := getSighupSuccess()
	if after < before+1 {
		t.Errorf("config_reload_total{file=\"<sighup>\",result=\"success\"} did not increment: before=%v after=%v", before, after)
	}

	if cur := hr.Current(); cur == nil {
		t.Error("Current() returned nil after TriggerReload")
	} else if len(cur.Pools.ProviderEndpoints) == 0 {
		t.Error("Current() config has no provider endpoints after TriggerReload")
	}
}

func swapLogHandler(t *testing.T, h slog.Handler) slog.Handler {
	t.Helper()
	prev := slog.Default().Handler()
	slog.SetDefault(slog.New(h))
	return prev
}

func prometheusGather(t *testing.T) []*dto.MetricFamily {
	t.Helper()
	gathered, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("prometheus gather: %v", err)
	}
	return gathered
}
