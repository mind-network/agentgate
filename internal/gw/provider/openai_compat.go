package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// OpenAICompatAdapter implements Adapter for OpenAI-compatible endpoints
// (Ollama, vLLM, TGI, etc.). Uses raw HTTP, no SDK dependency.
type OpenAICompatAdapter struct {
	client *http.Client
}

// NewOpenAICompatAdapter creates a new OpenAI-compatible adapter.
func NewOpenAICompatAdapter() *OpenAICompatAdapter {
	return &OpenAICompatAdapter{client: &http.Client{}}
}

// Name returns "openai_compat".
func (a *OpenAICompatAdapter) Name() string { return "openai_compat" }

// Stream sends a streaming request.
func (a *OpenAICompatAdapter) Stream(ctx context.Context, req *ProviderRequest) (io.ReadCloser, error) {
	return a.do(ctx, req, true)
}

// NonStream sends a non-streaming request.
func (a *OpenAICompatAdapter) NonStream(ctx context.Context, req *ProviderRequest) ([]byte, error) {
	rc, err := a.do(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// CountTokens returns 0 for P0 (local models have no tokenizer integration).
func (a *OpenAICompatAdapter) CountTokens(_ context.Context, _ *ProviderRequest) (int, error) {
	return 0, nil
}

func (a *OpenAICompatAdapter) do(ctx context.Context, req *ProviderRequest, stream bool) (io.ReadCloser, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		req.URL+"/chat/completions", bytes.NewReader(req.Messages))
	if err != nil {
		return nil, fmt.Errorf("openai_compat: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai_compat: %w", err)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("openai_compat %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}
