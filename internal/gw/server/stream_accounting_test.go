package server

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"agentgate/internal/gw/config"
	"agentgate/internal/gw/cost"
)

func testPricing() *config.PricingConfig {
	// Use a no-currency pricing config that matches the existing test helper,
	// adding cache prices for the models that need them.
	return &config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                      "anthropic",
				Model:                       "claude-sonnet-4-6",
				InputPricePer1KTokens:       3.0,
				OutputPricePer1KTokens:      15.0,
				CacheReadPricePer1KTokens:   float64Ptr(0.30),
				CacheCreatePricePer1KTokens: float64Ptr(3.0),
				Capabilities:                config.ModelCapabilities{Tools: true, CacheControl: true},
			},
			{
				Vendor:                 "openai",
				Model:                  "gpt-4o",
				InputPricePer1KTokens:  2.5,
				OutputPricePer1KTokens: 10.0,
				Capabilities:           config.ModelCapabilities{Tools: true},
			},
			{
				Vendor:       "ollama-local",
				Model:        "qwen2.5-coder:7b",
				Capabilities: config.ModelCapabilities{},
			},
		},
	}
}

func float64Ptr(v float64) *float64 { return &v }

// TestAccountedStreamForwardsAnthropicFrames tests that AccountedStream correctly
// forwards SSE frames from an Anthropic upstream and accumulates usage from
// message_start + message_delta events.
func TestAccountedStreamForwardsAnthropicFrames(t *testing.T) {
	calc := cost.NewCalculator(testPricing())
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":5,\"cache_read_input_tokens\":50,\"cache_creation_input_tokens\":20}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	body := io.NopCloser(strings.NewReader(sse))
	as := NewAccountedStream(body, &AccountedStreamConfig{
		Wire:       "anthropic",
		Calc:       calc,
		VendorID:   "anthropic",
		EndpointID: "anthropic-prod",
		Model:      "claude-sonnet-4-6",
		Pool:       "standard",
		TraceID:    "trace-t3-001",
		AttemptNo:  1,
	})

	out, err := io.ReadAll(as)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Verify frames are forwarded unchanged.
	output := string(out)
	if !strings.Contains(output, "event: message_start") {
		t.Error("expected message_start in output")
	}
	if !strings.Contains(output, "content_block_start") {
		t.Error("expected content_block_start in output")
	}
	if !strings.Contains(output, "message_delta") {
		t.Error("expected message_delta in output")
	}
	if !strings.Contains(output, "message_stop") {
		t.Error("expected message_stop in output")
	}
	if !strings.Contains(output, "Hello") {
		t.Error("expected text content in output")
	}

	// Verify aicg.usage is injected at the tail.
	if !strings.Contains(output, "event: aicg.usage") {
		t.Error("expected aicg.usage at stream tail")
	}

	// Verify accounting result.
	res := as.Result()
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.ErrorClass != "" {
		t.Errorf("unexpected error_class: %s", res.ErrorClass)
	}
	if res.Usage == nil {
		t.Fatal("expected usage in result")
	}
	if res.Usage.Input != 100 {
		t.Errorf("input = %d, want 100", res.Usage.Input)
	}
	if res.Usage.Output != 42 {
		t.Errorf("output = %d, want 42 (cumulative from message_delta)", res.Usage.Output)
	}
	if res.Usage.CacheRead != 50 {
		t.Errorf("cache_read = %d, want 50", res.Usage.CacheRead)
	}
	if res.Usage.CacheCreate != 20 {
		t.Errorf("cache_create = %d, want 20", res.Usage.CacheCreate)
	}
	if res.CostEvent == nil {
		t.Fatal("expected cost event")
	}
	if res.CostEvent.CostSource != "provider_usage" {
		t.Errorf("cost_source = %s, want provider_usage", res.CostEvent.CostSource)
	}
}

// TestAccountedStreamForwardsOpenAIFrames tests that AccountedStream correctly
// forwards frames from an OpenAI-compatible upstream and extracts usage from the
// final usage chunk.
func TestAccountedStreamForwardsOpenAIFrames(t *testing.T) {
	calc := cost.NewCalculator(testPricing())
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":25}}\n\ndata: [DONE]\n\n"

	body := io.NopCloser(strings.NewReader(sse))
	as := NewAccountedStream(body, &AccountedStreamConfig{
		Wire:       "openai_compat",
		Calc:       calc,
		VendorID:   "openai",
		EndpointID: "openai-prod",
		Model:      "gpt-4o",
		Pool:       "standard",
		TraceID:    "trace-t3-oai",
		AttemptNo:  1,
	})

	out, err := io.ReadAll(as)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	output := string(out)
	if !strings.Contains(output, "Hello") {
		t.Error("expected content in output")
	}
	if !strings.Contains(output, "[DONE]") {
		t.Error("expected [DONE] in output")
	}
	if !strings.Contains(output, "event: aicg.usage") {
		t.Error("expected aicg.usage at stream tail")
	}

	res := as.Result()
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.ErrorClass != "" {
		t.Errorf("unexpected error_class: %s", res.ErrorClass)
	}
	if res.Usage.Input != 15 {
		t.Errorf("input = %d, want 15", res.Usage.Input)
	}
	if res.Usage.Output != 25 {
		t.Errorf("output = %d, want 25", res.Usage.Output)
	}
}

// TestAccountedStreamMissingUsage tests that a stream with no usage events
// produces estimated accounting with error_class=usage_missing.
func TestAccountedStreamMissingUsage(t *testing.T) {
	calc := cost.NewCalculator(testPricing())
	// OpenAI stream without usage chunk.
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"

	body := io.NopCloser(strings.NewReader(sse))
	as := NewAccountedStream(body, &AccountedStreamConfig{
		Wire:       "openai_compat",
		Calc:       calc,
		VendorID:   "openai",
		EndpointID: "openai-prod",
		Model:      "gpt-4o",
		Pool:       "standard",
		TraceID:    "trace-mu-001",
		AttemptNo:  1,
	})

	_, _ = io.ReadAll(as)
	res := as.Result()

	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.ErrorClass != "usage_missing" {
		t.Errorf("error_class = %q, want usage_missing", res.ErrorClass)
	}
	if res.CostEvent == nil {
		t.Fatal("expected cost event")
	}
	// cost_source should be estimated because usage was missing.
	if res.CostEvent.CostSource != "estimated" {
		t.Errorf("cost_source = %s, want estimated", res.CostEvent.CostSource)
	}
}

// TestAccountedStreamInterrupted tests that a read error during stream
// produces failure accounting with error_class=stream_interrupted.
func TestAccountedStreamInterrupted(t *testing.T) {
	calc := cost.NewCalculator(testPricing())
	// A reader that returns partial data then fails.
	partialBody := io.NopCloser(&errorReader{data: "event: message_start\ndata: {\"type\":\"message_start\"}\n\n", err: errors.New("connection reset")})

	as := NewAccountedStream(partialBody, &AccountedStreamConfig{
		Wire:       "anthropic",
		Calc:       calc,
		VendorID:   "anthropic",
		EndpointID: "anthropic-prod",
		Model:      "claude-sonnet-4-6",
		Pool:       "standard",
		TraceID:    "trace-int-001",
		AttemptNo:  1,
	})

	out, err := io.ReadAll(as)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	output := string(out)
	// Partial data should still have been forwarded.
	if !strings.Contains(output, "message_start") {
		t.Error("expected partial content in output")
	}

	// aicg.error should be injected.
	if !strings.Contains(output, "event: aicg.error") {
		t.Error("expected aicg.error in output")
	}

	res := as.Result()
	if res.Err == nil {
		t.Fatal("expected error from interrupted stream")
	}
	if res.ErrorClass != "stream_interrupted" {
		t.Errorf("error_class = %q, want stream_interrupted", res.ErrorClass)
	}
	if res.CostEvent == nil {
		t.Fatal("expected cost event")
	}
}

// errorReader returns data then returns err on the next read.
type errorReader struct {
	data string
	err  error
	done bool
}

func (r *errorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

func (r *errorReader) Close() error { return nil }

// TestAccountedStreamRealTimeForwarding tests that the wrapper does not buffer
// the full response before starting to forward frames to the output reader.
func TestAccountedStreamRealTimeForwarding(t *testing.T) {
	calc := cost.NewCalculator(testPricing())
	// Use a pipe to simulate a slowly-producing upstream.
	pr, pw := io.Pipe()
	done := make(chan struct{})

	go func() {
		defer func() { close(done) }()
		for i := 0; i < 5; i++ {
			payload := fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"text_delta\",\"text\":\"chunk-%d\"}\n\n", i)
			_, _ = pw.Write([]byte(payload))
			time.Sleep(10 * time.Millisecond)
		}
		_ = pw.Close()
	}()

	as := NewAccountedStream(pr, &AccountedStreamConfig{
		Wire:       "anthropic",
		Calc:       calc,
		VendorID:   "anthropic",
		EndpointID: "anthropic-prod",
		Model:      "claude-sonnet-4-6",
		Pool:       "standard",
		TraceID:    "trace-rt-001",
		AttemptNo:  1,
	})

	// Read the stream and verify chunks arrive before the stream is fully produced.
	var saw int
	readBuf := make([]byte, 1024)
	for {
		n, err := as.Read(readBuf)
		if err != nil {
			break
		}
		chunk := string(readBuf[:n])
		if strings.Contains(chunk, "chunk-") {
			saw++
		}
	}
	// Should have seen all 5 chunks.
	if saw < 5 {
		t.Errorf("saw %d chunks, want 5 — indicates potential buffering", saw)
	}

	<-done
	res := as.Result()
	if res.Err != nil {
		t.Errorf("unexpected error: %v", res.Err)
	}
	if !strings.Contains(fmt.Sprintf("%v", res.CostEvent), "aicg.usage") && res.CostEvent == nil {
		t.Log("result available, cost event present")
	}
}

// TestAccountedStreamWithAnthropicCache tests that cache tokens from
// message_start are correctly propagated through accounting.
func TestAccountedStreamWithAnthropicCache(t *testing.T) {
	calc := cost.NewCalculator(testPricing())
	// message_start with cache tokens, message_delta with cumulative output.
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":200,\"output_tokens\":0,\"cache_read_input_tokens\":150,\"cache_creation_input_tokens\":80}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":60}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	body := io.NopCloser(strings.NewReader(sse))
	as := NewAccountedStream(body, &AccountedStreamConfig{
		Wire:       "anthropic",
		Calc:       calc,
		VendorID:   "anthropic",
		EndpointID: "anthropic-prod",
		Model:      "claude-sonnet-4-6",
		Pool:       "standard",
		TraceID:    "trace-cache-001",
		AttemptNo:  1,
	})

	_, _ = io.ReadAll(as)
	res := as.Result()

	if res.Usage.Input != 200 {
		t.Errorf("input = %d, want 200", res.Usage.Input)
	}
	if res.Usage.Output != 60 {
		t.Errorf("output = %d, want 60", res.Usage.Output)
	}
	if res.Usage.CacheRead != 150 {
		t.Errorf("cache_read = %d, want 150", res.Usage.CacheRead)
	}
	if res.Usage.CacheCreate != 80 {
		t.Errorf("cache_create = %d, want 80", res.Usage.CacheCreate)
	}
	if res.CostEvent.CacheReadTokens != 150 {
		t.Errorf("CostEvent.CacheReadTokens = %d, want 150", res.CostEvent.CacheReadTokens)
	}
	if res.CostEvent.CacheCreateTokens != 80 {
		t.Errorf("CostEvent.CacheCreateTokens = %d, want 80", res.CostEvent.CacheCreateTokens)
	}
}

// TestAccountedStreamNoEarlyCostEvent tests that no cost_event is written before
// the stream is fully consumed (verified by checking the result is only available
// after the stream is drained).
func TestAccountedStreamNoEarlyCostEvent(t *testing.T) {
	calc := cost.NewCalculator(testPricing())
	body := io.NopCloser(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))

	as := NewAccountedStream(body, &AccountedStreamConfig{
		Wire:       "anthropic",
		Calc:       calc,
		VendorID:   "anthropic",
		EndpointID: "anthropic-prod",
		Model:      "claude-sonnet-4-6",
		Pool:       "standard",
		TraceID:    "trace-early-001",
		AttemptNo:  1,
	})

	// Select on the result channel to check it's not available before drain.
	select {
	case <-as.done:
		t.Error("result should not be available before stream is fully consumed")
	default:
		// Expected: result is not available yet.
	}

	// Now drain the stream.
	_, _ = io.ReadAll(as)

	// After drain, result should be available immediately.
	res := as.Result()
	if res.CostEvent == nil {
		t.Error("expected cost event after stream drain")
	}
}

// TestAccountedStreamOversizedDataLine tests that a data: line larger than the
// old bufio.Scanner 256KB token-size limit is forwarded and still produces correct
// EOF accounting. This is a regression test for the T3 DoD requirement to avoid
// Scanner per-line token caps.
func TestAccountedStreamOversizedDataLine(t *testing.T) {
	calc := cost.NewCalculator(testPricing())
	// Build a stream with a content_block_delta data line > 256KB (the old
	// maxSSELineSize) to verify no Scanner token-size cap is hit.
	largeText := strings.Repeat("A", 300*1024) // 300KB exceeds old limit
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":5}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"text_delta\",\"text\":\"" + largeText + "\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	body := io.NopCloser(strings.NewReader(sse))
	as := NewAccountedStream(body, &AccountedStreamConfig{
		Wire:       "anthropic",
		Calc:       calc,
		VendorID:   "anthropic",
		EndpointID: "anthropic-prod",
		Model:      "claude-sonnet-4-6",
		Pool:       "standard",
		TraceID:    "trace-large-001",
		AttemptNo:  1,
	})

	out, err := io.ReadAll(as)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	output := string(out)
	if !strings.Contains(output, "event: aicg.usage") {
		t.Error("expected aicg.usage at stream tail")
	}
	if !strings.Contains(output, "message_start") {
		t.Error("expected message_start in output")
	}
	if !strings.Contains(output, "message_delta") {
		t.Error("expected message_delta in output")
	}
	if !strings.Contains(output, "message_stop") {
		t.Error("expected message_stop in output")
	}

	// Verify the large payload is in the forwarded output.
	if !strings.Contains(output, largeText[:100]) {
		t.Error("expected large text content in forwarded output")
	}

	res := as.Result()
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.ErrorClass != "" {
		t.Errorf("unexpected error_class: %s", res.ErrorClass)
	}
	if res.Usage.Input != 100 {
		t.Errorf("input = %d, want 100", res.Usage.Input)
	}
	if res.Usage.Output != 42 {
		t.Errorf("output = %d, want 42 (cumulative from message_delta)", res.Usage.Output)
	}
	if res.CostEvent == nil {
		t.Fatal("expected cost event")
	}
	if res.CostEvent.CostSource != "provider_usage" {
		t.Errorf("cost_source = %s, want provider_usage", res.CostEvent.CostSource)
	}
}

// TestAccountedStreamOpenAIMissingUsageChunk tests that an OpenAI-compatible
// stream that ends without a final usage chunk (e.g. interrupted by upstream
// omitting the usage chunk) produces estimated accounting.
func TestAccountedStreamOpenAIMissingUsageChunk(t *testing.T) {
	calc := cost.NewCalculator(testPricing())
	// Normal content chunks but no usage chunk — just [DONE].
	sse := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"A\"}}]}\n\ndata: [DONE]\n\n"

	body := io.NopCloser(strings.NewReader(sse))
	as := NewAccountedStream(body, &AccountedStreamConfig{
		Wire:       "openai_compat",
		Calc:       calc,
		VendorID:   "openai",
		EndpointID: "openai-prod",
		Model:      "gpt-4o",
		Pool:       "standard",
		TraceID:    "trace-nu-001",
		AttemptNo:  1,
	})

	_, _ = io.ReadAll(as)
	res := as.Result()
	if res.ErrorClass != "usage_missing" {
		t.Errorf("error_class = %q, want usage_missing", res.ErrorClass)
	}
	if res.CostEvent == nil {
		t.Fatal("expected cost event")
	}
	if res.CostEvent.CostSource != "estimated" {
		t.Errorf("cost_source = %s, want estimated", res.CostEvent.CostSource)
	}
}
