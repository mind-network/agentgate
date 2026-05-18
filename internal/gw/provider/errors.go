package provider

import "fmt"

// UpstreamError is an error from an upstream provider that carries the HTTP
// status code. 4xx UpstreamErrors are not retried and are propagated to the
// downstream caller with the original status code preserved.
type UpstreamError struct {
	Status   int
	Body     string
	Provider string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("%s %d: %s", e.Provider, e.Status, e.Body)
}
