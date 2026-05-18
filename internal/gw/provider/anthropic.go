package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AnthropicAdapter implements Adapter for the Anthropic Messages API.
// This is the ONLY file in the repo that imports anthropic SDK types — but
// we avoid the SDK to prevent type leakage; instead we use raw HTTP + JSON.
type AnthropicAdapter struct {
	client *http.Client
}

// NewAnthropicAdapter creates a new Anthropic adapter.
func NewAnthropicAdapter() *AnthropicAdapter {
	return &AnthropicAdapter{client: &http.Client{}}
}

// Name returns "anthropic".
func (a *AnthropicAdapter) Name() string { return "anthropic" }

// Stream sends a streaming request to the Anthropic Messages API.
func (a *AnthropicAdapter) Stream(ctx context.Context, req *ProviderRequest) (io.ReadCloser, error) {
	return a.do(ctx, req, true)
}

// NonStream sends a non-streaming request.
func (a *AnthropicAdapter) NonStream(ctx context.Context, req *ProviderRequest) ([]byte, error) {
	rc, err := a.do(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// CountTokens returns a token count estimate. P0: fixed 0 (routed to anthropic-tokenizer in T5 proper).
func (a *AnthropicAdapter) CountTokens(_ context.Context, _ *ProviderRequest) (int, error) {
	return 0, nil
}

func (a *AnthropicAdapter) do(ctx context.Context, req *ProviderRequest, stream bool) (io.ReadCloser, error) {
	return anthropicShapedDo(a.client, "anthropic", ctx, req, stream)
}

// anthropicShapedDo is the shared helper for Anthropic-shaped upstreams
// (official Anthropic and anthropic_compat). Both use the same wire format:
// POST <base>/v1/messages, x-api-key, anthropic-version: 2023-06-01.
func anthropicShapedDo(client *http.Client, providerName string, ctx context.Context, req *ProviderRequest, stream bool) (io.ReadCloser, error) {
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     stream,
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	var msgs []json.RawMessage
	_ = json.Unmarshal(req.Messages, &msgs)
	if len(msgs) > 0 {
		body["messages"] = msgs
	}
	if len(req.System) > 0 {
		var sys json.RawMessage
		if json.Unmarshal(req.System, &sys) == nil && len(sys) > 0 {
			body["system"] = sys
		}
	}
	if len(req.Tools) > 0 {
		var tools json.RawMessage
		if json.Unmarshal(req.Tools, &tools) == nil && len(tools) > 0 {
			body["tools"] = tools
		}
	}
	if len(req.ToolChoice) > 0 {
		var tc json.RawMessage
		if json.Unmarshal(req.ToolChoice, &tc) == nil && len(tc) > 0 {
			body["tool_choice"] = tc
		}
	}
	if len(req.Metadata) > 0 {
		body["metadata"] = req.Metadata
	}
	if len(req.Thinking) > 0 {
		var th json.RawMessage
		if json.Unmarshal(req.Thinking, &th) == nil && len(th) > 0 {
			body["thinking"] = th
		}
	}

	b, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(req.URL, "/")+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: create request: %w", providerName, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", req.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", providerName, err)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		if resp.StatusCode < 500 {
			return nil, &UpstreamError{
				Status:   resp.StatusCode,
				Body:     string(body),
				Provider: providerName,
			}
		}
		return nil, fmt.Errorf("%s %d: %s", providerName, resp.StatusCode, string(body))
	}
	return resp.Body, nil
}
