// Package config loads and validates all GW configuration from YAML files.
// It provides fsnotify-based hot reload with atomic double-buffering.
package config

// ProviderEndpoint describes a registered upstream endpoint (§9.1).
type ProviderEndpoint struct {
	Wire          string           `yaml:"wire"`
	Vendor        string           `yaml:"vendor"`
	URL           string           `yaml:"url"`
	DataResidency string           `yaml:"data_residency"`
	TrustTier     string           `yaml:"trust_tier"` // vendor, partner, private
	Deployments   []string         `yaml:"deployments,omitempty"`
	Supports      EndpointSupports `yaml:"supports"`
	KeyRef        string           `yaml:"key_ref,omitempty"`
}

// EndpointWithKey pairs a ProviderEndpoint with its resolved API key.
type EndpointWithKey struct {
	*ProviderEndpoint
	ResolvedKey string
}

// EndpointSupports declares optional capabilities of an endpoint.
type EndpointSupports struct {
	Streaming bool `yaml:"streaming"`
}

// ModelCapabilities declares model-level capability flags that vary by (vendor, model).
type ModelCapabilities struct {
	Tools            bool `yaml:"tools"`
	CacheControl     bool `yaml:"cache_control"`
	ExtendedThinking bool `yaml:"extended_thinking"`
	Vision           bool `yaml:"vision"`
}

// PoolMember is a weighted entry in a model pool.
type PoolMember struct {
	EndpointID string `yaml:"endpoint_id"`
	Model      string `yaml:"model"`
	Weight     int    `yaml:"weight"`
}

// Pool defines a model pool with members, fallback, and limits.
type Pool struct {
	Members      []PoolMember `yaml:"members"`
	FallbackPool string       `yaml:"fallback_pool"`
	MaxAttempts  int          `yaml:"max_attempts"`
	TimeoutMs    int          `yaml:"timeout_ms"`
}

// PoolsConfig is the top-level pools.yaml structure.
type PoolsConfig struct {
	ProviderEndpoints map[string]ProviderEndpoint `yaml:"provider_endpoints"`
	Pools             map[string]Pool             `yaml:"pools"`
}

// DegradationConfig declares which auto-detected Anthropic features may be
// silently degraded (stripped) when routing to non-anthropic wires.
type DegradationConfig struct {
	AllowedCapabilities []string `yaml:"allowed_capabilities"`
}

// PolicyConfig is the top-level policies/main.yaml structure.
type PolicyConfig struct {
	Version     string            `yaml:"version"`
	Defaults    Defaults          `yaml:"defaults"`
	Degradation DegradationConfig `yaml:"degradation"`
	Rules       []PolicyRule      `yaml:"rules"`
	Variables   map[string]any    `yaml:"variables,omitempty"`
}

// Defaults holds the default policy action.
type Defaults struct {
	OnNoMatch ActionSpec `yaml:"on_no_match"`
}

// ActionSpec describes what action to take and which pool to use.
type ActionSpec struct {
	Action    string   `yaml:"action"`
	ModelPool string   `yaml:"model_pool,omitempty"`
	Reasons   []string `yaml:"reasons,omitempty"`
}

// PolicyRule is a single CEL-based policy rule.
type PolicyRule struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	Priority    int      `yaml:"priority"`
	When        string   `yaml:"when"`
	Action      string   `yaml:"action"`
	ModelPool   string   `yaml:"model_pool,omitempty"`
	Reasons     []string `yaml:"reasons,omitempty"`
}

// PricingConfig is the top-level pricing.yaml structure.
type PricingConfig struct {
	Models []ModelPricing `yaml:"models"`
}

// ModelPricing defines input/output/cache pricing for a specific model.
type ModelPricing struct {
	Vendor                      string            `yaml:"vendor"`
	Model                       string            `yaml:"model"`
	Currency                    string            `yaml:"currency"`
	InputPricePer1KTokens       float64           `yaml:"input_price_per_1k_tokens"`
	OutputPricePer1KTokens      float64           `yaml:"output_price_per_1k_tokens"`
	CacheReadPricePer1KTokens   *float64          `yaml:"cache_read_price_per_1k_tokens,omitempty"`
	CacheCreatePricePer1KTokens *float64          `yaml:"cache_create_price_per_1k_tokens,omitempty"`
	Capabilities                ModelCapabilities `yaml:"capabilities"`
}

// IdentityConfig groups user, team, and repo identity definitions.
type IdentityConfig struct {
	Users []UserIdentity `yaml:"users"`
	Teams []TeamIdentity `yaml:"teams"`
	Repos []RepoIdentity `yaml:"repos"`
}

// UserIdentity maps a user to a team and role.
type UserIdentity struct {
	UserID     string `yaml:"user_id"`
	TeamID     string `yaml:"team_id"`
	Role       string `yaml:"role"`
	APIKeyHash string `yaml:"api_key_hash"`
}

// TeamIdentity describes a team and its budget cap.
type TeamIdentity struct {
	TeamID                string `yaml:"team_id"`
	Name                  string `yaml:"name"`
	BudgetMonthlyCapCents int    `yaml:"budget_monthly_cap_cents"`
}

// RepoIdentity maps a repository to a team.
type RepoIdentity struct {
	RepoID        string `yaml:"repo_id"`
	RemoteURL     string `yaml:"remote_url"`
	DefaultTeamID string `yaml:"default_team_id"`
	Restricted    bool   `yaml:"restricted"`
}

// Config bundles all loaded configuration sections.
type Config struct {
	Pools             *PoolsConfig
	Policy            *PolicyConfig
	Pricing           *PricingConfig
	Identity          *IdentityConfig
	ResolvedEndpoints map[string]EndpointWithKey // endpoint_id -> endpoint + resolved key
}
