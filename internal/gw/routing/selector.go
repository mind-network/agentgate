// Package routing implements the constraint-filtered, ChaCha8-seeded
// weighted member selection for model pool routing (§9).
package routing

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"agentgate/internal/gw/config"
	"agentgate/internal/gw/policy"
)

// ChaCha8RNG is a deterministic RNG seeded from trace_id + pool + attempt.
type ChaCha8RNG struct {
	rng *rand.Rand
}

// NewChaCha8RNG creates a deterministic RNG from a seed.
func NewChaCha8RNG(seed [32]byte) *ChaCha8RNG {
	return &ChaCha8RNG{rng: rand.New(rand.NewChaCha8(seed))}
}

// IntN returns a deterministic random integer in [0, n).
func (c *ChaCha8RNG) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	return c.rng.IntN(n)
}

// SeedBytes computes the routing seed = sha256(trace_id : pool : attempt_no)[:16].
// Returns a 16-byte seed suitable for writing to routing_event.seed (BYTEA) per Decision D-11.
func SeedBytes(traceID, pool string, attemptNo int) []byte {
	input := fmt.Sprintf("%s:%s:%d", traceID, pool, attemptNo)
	hash := sha256.Sum256([]byte(input))
	return hash[:16]
}

// Chacha8SeedFromStored derives the 32-byte ChaCha8 seed from the stored 16-byte
// seed (sha256[:16] per D-11). Both BuildChain and Replay use this single helper
// to guarantee deterministic shuffle order.
func Chacha8SeedFromStored(stored []byte) [32]byte {
	var seed [32]byte
	copy(seed[:], stored)
	// The stored seed is 16 bytes; expand to 32 via repetition.
	for i := len(stored); i < 32; i++ {
		seed[i] = seed[i%len(stored)]
	}
	return seed
}

// Member is a selected pool member with its weight-adjusted metadata.
type Member struct {
	EndpointID     string
	Wire           string
	VendorID       string
	Model          string
	URL            string
	TrustTier      string
	DataResidency  string
	Streaming      bool
	Private        bool
	OriginalWeight int
}

// Selector builds and filters candidate chains for a pool.
type Selector struct {
	mu       sync.RWMutex
	Registry map[string]config.ProviderEndpoint
	Pools    map[string]config.Pool
	pricing  atomic.Pointer[config.PricingConfig]
}

// NewSelector creates a Selector from a loaded config.
func NewSelector(poolsCfg *config.PoolsConfig, pricing *config.PricingConfig) *Selector {
	s := &Selector{
		Registry: poolsCfg.ProviderEndpoints,
		Pools:    poolsCfg.Pools,
	}
	s.pricing.Store(pricing)
	return s
}

// SetPricing atomically updates the pricing table used by capability checks.
// Safe for concurrent use with BuildChain; intended for hot-reload paths
// that swap in a freshly loaded PricingConfig without rebuilding the Selector.
func (s *Selector) SetPricing(pricing *config.PricingConfig) {
	s.pricing.Store(pricing)
}

// SetPools atomically updates the endpoint registry and pool definitions.
// Safe for concurrent use with BuildChain; intended for hot-reload paths.
func (s *Selector) SetPools(poolsCfg *config.PoolsConfig) {
	s.mu.Lock()
	s.Registry = poolsCfg.ProviderEndpoints
	s.Pools = poolsCfg.Pools
	s.mu.Unlock()
}

// BuildChain applies constraint filtering and produces an ordered list of
// candidate members using ChaCha8 seeded weighted selection.
// Returns ErrNoCandidate if no member satisfies all constraints.
func (s *Selector) BuildChain(poolName string, dec *policy.Decision, traceID string, attemptNo int) ([]Member, error) {
	s.mu.RLock()
	pool, ok := s.Pools[poolName]
	if !ok {
		s.mu.RUnlock()
		return nil, fmt.Errorf("pool %q not found", poolName)
	}

	var candidates []Member
	for _, m := range pool.Members {
		ep, ok := s.Registry[m.EndpointID]
		if !ok {
			continue // skip unregistered endpoints
		}
		candidate := Member{
			EndpointID:     m.EndpointID,
			Wire:           ep.Wire,
			VendorID:       ep.Vendor,
			Model:          m.Model,
			URL:            ep.URL,
			TrustTier:      ep.TrustTier,
			DataResidency:  ep.DataResidency,
			Streaming:      ep.Supports.Streaming,
			Private:        ep.TrustTier == "private",
			OriginalWeight: m.Weight,
		}
		candidates = append(candidates, candidate)
	}
	s.mu.RUnlock()

	// Apply capability constraints (does not access Registry/Pools).
	filtered := candidates[:0]
	for _, c := range candidates {
		if satisfiesConstraints(c, dec, s.pricing.Load()) {
			filtered = append(filtered, c)
		}
	}
	candidates = filtered

	if len(candidates) == 0 {
		return nil, &ErrNoCandidate{Pool: poolName, Constraints: constraintSummary(dec)}
	}

	// Shuffle with deterministic ChaCha8 RNG seeded from trace.
	stored := SeedBytes(traceID, poolName, attemptNo)
	chachaSeed := Chacha8SeedFromStored(stored)
	rng := NewChaCha8RNG(chachaSeed)

	// Weighted selection: each member is added weight times, then shuffled.
	var weighted []Member
	for _, c := range candidates {
		for i := 0; i < c.OriginalWeight; i++ {
			weighted = append(weighted, c)
		}
	}

	// Fisher-Yates shuffle with ChaCha8.
	for i := len(weighted) - 1; i > 0; i-- {
		j := rng.IntN(i + 1)
		weighted[i], weighted[j] = weighted[j], weighted[i]
	}

	// Deduplicate: keep first occurrence of each member.
	seen := make(map[string]bool)
	var chain []Member
	for _, m := range weighted {
		if seen[m.EndpointID] {
			continue
		}
		seen[m.EndpointID] = true
		chain = append(chain, m)
	}
	return chain, nil
}

func satisfiesConstraints(m Member, dec *policy.Decision, pricing *config.PricingConfig) bool {
	if dec == nil {
		return true
	}
	if dec.RequiredTrustTier != "" {
		if trustTierRank(m.TrustTier) < trustTierRank(dec.RequiredTrustTier) {
			return false
		}
	}
	if len(dec.RequiredDataResidency) > 0 {
		found := false
		for _, r := range dec.RequiredDataResidency {
			if m.DataResidency == r {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, cap := range dec.RequiredCapabilities {
		if !memberSupports(m, cap, pricing) {
			return false
		}
	}
	return true
}

func trustTierRank(tier string) int {
	switch tier {
	case "private":
		return 3
	case "partner":
		return 2
	case "vendor":
		return 1
	default:
		return 0
	}
}

// memberSupports checks whether a candidate member satisfies a capability constraint.
// Streaming is checked from the endpoint; all other capabilities are resolved from
// the (vendor, model) pricing row. Missing pricing rows or nil PricingConfig fail
// closed (D6) — non-streaming capabilities return false rather than panicking.
func memberSupports(m Member, cap string, pricing *config.PricingConfig) bool {
	switch cap {
	case "streaming":
		return m.Streaming
	case "tools", "cache_control", "extended_thinking", "vision":
		if pricing == nil {
			return false // D6: fail-closed on nil PricingConfig
		}
		row := pricing.Lookup(m.VendorID, m.Model)
		if row == nil {
			return false // D6: fail-closed
		}
		switch cap {
		case "tools":
			return row.Capabilities.Tools
		case "cache_control":
			return row.Capabilities.CacheControl
		case "extended_thinking":
			return row.Capabilities.ExtendedThinking
		case "vision":
			return row.Capabilities.Vision
		}
	}
	return false
}

func constraintSummary(dec *policy.Decision) string {
	return fmt.Sprintf("trust_tier>=%s residency∈%v caps=%v",
		dec.RequiredTrustTier, dec.RequiredDataResidency, dec.RequiredCapabilities)
}

// ErrNoCandidate is returned when no pool member satisfies the constraints.
type ErrNoCandidate struct {
	Pool        string
	Constraints string
}

func (e *ErrNoCandidate) Error() string {
	return fmt.Sprintf("routing: no candidate in pool %q satisfies constraints (%s)", e.Pool, e.Constraints)
}

// ValidatePoolAdapters checks that every pool member's endpoint wire
// has a registered adapter. Returns nil if all wires are covered,
// otherwise returns an error aggregating every mismatch.
func (s *Selector) ValidatePoolAdapters(adapterNames []string) error {
	have := make(map[string]bool, len(adapterNames))
	for _, n := range adapterNames {
		have[n] = true
	}

	var msgs []string
	for poolName, pool := range s.Pools {
		for _, m := range pool.Members {
			ep, ok := s.Registry[m.EndpointID]
			if !ok {
				msgs = append(msgs, fmt.Sprintf(
					`pool %q member endpoint %q: endpoint_id not found in provider_endpoints registry`,
					poolName, m.EndpointID))
				continue
			}
			if !have[ep.Wire] {
				msgs = append(msgs, fmt.Sprintf(
					`pool %q member endpoint %q wire %q has no registered adapter (registered: %v)`,
					poolName, m.EndpointID, ep.Wire, adapterNames))
			}
		}
	}
	if len(msgs) > 0 {
		return fmt.Errorf("routing: pool adapter validation failed:\n  - %s",
			joinValidationMsgs(msgs))
	}
	return nil
}

func joinValidationMsgs(msgs []string) string {
	if len(msgs) == 0 {
		return ""
	}
	s := msgs[0]
	for i := 1; i < len(msgs); i++ {
		s += "\n  - " + msgs[i]
	}
	return s
}
