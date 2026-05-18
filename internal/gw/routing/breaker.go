package routing

// CircuitBreaker is a placeholder stub for P0. Full breaker logic arrives in P1.
type CircuitBreaker struct{}

// NewCircuitBreaker creates a stub breaker.
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{}
}

// State returns the breaker state for an endpoint. Always "closed" in P0.
func (cb *CircuitBreaker) State(endpointID string) string {
	return "closed"
}
