// Package provider defines the adapter interface and implementations for
// upstream AI providers (Anthropic, OpenAI-compatible).
package provider

import (
	"context"
	"io"
)

// Adapter abstracts communication with an upstream provider.
type Adapter interface {
	// Name returns the provider name (e.g. "anthropic", "openai_compat").
	Name() string

	// Stream sends a streaming request and returns a body that emits SSE events.
	Stream(ctx context.Context, req *ProviderRequest) (io.ReadCloser, error)

	// NonStream sends a non-streaming request and returns the response body bytes.
	NonStream(ctx context.Context, req *ProviderRequest) ([]byte, error)

	// CountTokens estimates token count for the given messages.
	CountTokens(ctx context.Context, req *ProviderRequest) (int, error)
}

// ProviderRequest is the adapter-level request.
type ProviderRequest struct {
	Model       string
	Messages    []byte // serialized IR messages
	MaxTokens   int
	Temperature *float64
	Stream      bool
	APIKey      string
	URL         string
	// Optional Anthropic top-level fields pre-serialized by the transformer.
	System     []byte            `json:"-"`
	Tools      []byte            `json:"-"`
	ToolChoice []byte            `json:"-"`
	Metadata   map[string]string `json:"-"`
	Thinking   []byte            `json:"-"`
}
