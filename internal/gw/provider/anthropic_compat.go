package provider

import (
	"context"
	"io"
	"net/http"
)

// AnthropicCompatAdapter implements Adapter for Anthropic-compatible upstreams
// (DeepSeek, GLM, etc.) that speak the Anthropic Messages API wire format.
type AnthropicCompatAdapter struct {
	client *http.Client
}

// NewAnthropicCompatAdapter creates a new Anthropic-compatible adapter.
func NewAnthropicCompatAdapter() *AnthropicCompatAdapter {
	return &AnthropicCompatAdapter{client: &http.Client{}}
}

// Name returns "anthropic_compat".
func (a *AnthropicCompatAdapter) Name() string { return "anthropic_compat" }

// Stream sends a streaming request.
func (a *AnthropicCompatAdapter) Stream(ctx context.Context, req *ProviderRequest) (io.ReadCloser, error) {
	return anthropicShapedDo(a.client, "anthropic_compat", ctx, req, true)
}

// NonStream sends a non-streaming request.
func (a *AnthropicCompatAdapter) NonStream(ctx context.Context, req *ProviderRequest) ([]byte, error) {
	rc, err := anthropicShapedDo(a.client, "anthropic_compat", ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// CountTokens returns 0 for P0 (tokenizer not integrated).
func (a *AnthropicCompatAdapter) CountTokens(_ context.Context, _ *ProviderRequest) (int, error) {
	return 0, nil
}
