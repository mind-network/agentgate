package routing

import (
	"encoding/json"
	"fmt"

	"agentgate/internal/gw/config"
)

// RoutingEventRow mirrors the routing_event table for replay.
type RoutingEventRow struct {
	TraceID        string          `json:"trace_id"`
	AttemptNo      int             `json:"attempt_no"`
	PoolSelected   string          `json:"pool_selected"`
	Seed           []byte          `json:"seed"`
	MemberSelected json.RawMessage `json:"member_selected"`
}

// Replay reconstructs the routing chain from a stored routing_event row.
// It uses the same ChaCha8 RNG with the same seed and endpoint registry,
// and returns a member sequence matching the original member_selected.
func Replay(row RoutingEventRow, registry map[string]config.ProviderEndpoint, pools map[string]config.Pool) ([]Member, error) {
	pool, ok := pools[row.PoolSelected]
	if !ok {
		return nil, fmt.Errorf("replay: pool %q not found", row.PoolSelected)
	}

	// Collect all candidates (P0: no constraints filter on replay — we
	// re-derive the full weighted list as it was at selection time).
	var candidates []Member
	for _, m := range pool.Members {
		ep, ok := registry[m.EndpointID]
		if !ok {
			continue
		}
		candidates = append(candidates, Member{
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
		})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("replay: no members in pool %q", row.PoolSelected)
	}

	// Derive the 32-byte ChaCha8 seed from the stored 16-byte seed
	// using the same helper as BuildChain, guaranteeing determinism.
	chachaSeed := Chacha8SeedFromStored(row.Seed)
	rng := NewChaCha8RNG(chachaSeed)

	var weighted []Member
	for _, c := range candidates {
		for i := 0; i < c.OriginalWeight; i++ {
			weighted = append(weighted, c)
		}
	}
	for i := len(weighted) - 1; i > 0; i-- {
		j := rng.IntN(i + 1)
		weighted[i], weighted[j] = weighted[j], weighted[i]
	}

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
