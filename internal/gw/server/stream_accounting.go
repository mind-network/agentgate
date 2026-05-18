package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"agentgate/internal/gw/cost"
	"agentgate/internal/shared/ir"
)

// StreamAccountingResult holds the final accounting result after the stream is drained.
type StreamAccountingResult struct {
	Usage      *ir.Usage
	CostEvent  *cost.Event
	ErrorClass string
	Err        error
}

// AccountedStreamConfig carries the metadata needed for stream-level accounting.
// CostW is optional (best-effort persistence); when nil, DB writes are skipped.
type AccountedStreamConfig struct {
	Wire       string
	Calc       *cost.Calculator
	CostW      *cost.PgWriter
	VendorID   string
	EndpointID string
	Model      string
	Pool       string
	IsPrivate  bool
	TraceID    string
	TenantID   string
	UserID     string
	TeamID     string
	AttemptNo  int
	Degraded   []string
	LatencyMs  int
	// PostAccount, if set, is called from the background goroutine after EOF
	// accounting completes (cost_event written, aicg.usage injected). It receives
	// the final accounting result for post-processing such as budget settlement.
	PostAccount func(result *StreamAccountingResult)
}

// AccountedStream wraps an upstream SSE body reader, forwarding frames in real-time
// while accumulating usage from provider-specific SSE events. At normal EOF it
// produces one cost calculation, one cost_event database write (optional), and one
// aicg.usage tail event — all from the same cost.Event. On stream interruption it
// writes bounded failure accounting and an aicg.error event.
//
// Protocol-preserving relay: SSE frames are forwarded unchanged; unknown event types
// pass through. No early empty-usage cost_event is written before the stream is drained.
type AccountedStream struct {
	pr     *io.PipeReader
	result *StreamAccountingResult
	done   chan struct{}
}

// NewAccountedStream creates an AccountedStream. It starts a background goroutine
// that reads from body, forwards SSE frames to the returned reader via io.Pipe,
// and runs accounting at stream end.
func NewAccountedStream(body io.ReadCloser, cfg *AccountedStreamConfig) *AccountedStream {
	pr, pw := io.Pipe()
	as := &AccountedStream{
		pr:     pr,
		result: &StreamAccountingResult{},
		done:   make(chan struct{}),
	}
	go as.run(body, pw, cfg)
	return as
}

// Read implements io.Reader on the forwarded stream.
func (as *AccountedStream) Read(p []byte) (int, error) { return as.pr.Read(p) }

// Close implements io.Closer.
func (as *AccountedStream) Close() error { return as.pr.Close() }

// Result returns the accounting result, blocking until the underlying stream has been
// fully consumed and accounting has been performed.
func (as *AccountedStream) Result() *StreamAccountingResult {
	<-as.done
	return as.result
}

func (as *AccountedStream) run(body io.ReadCloser, pw *io.PipeWriter, cfg *AccountedStreamConfig) {
	defer func() {
		_ = body.Close()
		_ = pw.Close()
		close(as.done)
	}()

	acc := &usageAccumulator{}
	readErr := forwardFrames(body, pw, acc, cfg.Wire)

	if readErr != nil {
		// Stream interrupted by an upstream read error.
		as.result.Err = readErr
		as.result.ErrorClass = "stream_interrupted"
		as.result.Usage = acc.usage()
		ce := cost.BuildEvent(as.result.Usage, cfg.Wire, cfg.VendorID, cfg.EndpointID,
			cfg.Model, cfg.Pool, cfg.IsPrivate, cfg.Calc)
		ce.CostSource = "estimated" // interrupted stream data is inherently approximate
		as.result.CostEvent = ce

		if cfg.CostW != nil {
			if wErr := cfg.CostW.Write(context.Background(), ce,
				cfg.TraceID, cfg.TenantID, cfg.UserID, cfg.TeamID,
				"", "", "", cfg.AttemptNo, cfg.LatencyMs, false, "stream_interrupted"); wErr != nil {
				slog.Warn("cost_event write failed (stream interrupted)",
					"error", wErr, "trace_id", cfg.TraceID)
			}
		}

		if cfg.PostAccount != nil {
			cfg.PostAccount(as.result)
		}

		InjectError(pw, cfg.TraceID, "stream_interrupted", readErr.Error(), true, as.result.Usage.Output)
		return
	}

	// Normal EOF: compute and persist final cost from accumulated usage.
	as.result.Usage = acc.usage()
	if !acc.hasUsage {
		as.result.ErrorClass = "usage_missing"
	}

	ce := cost.BuildEvent(as.result.Usage, cfg.Wire, cfg.VendorID, cfg.EndpointID,
		cfg.Model, cfg.Pool, cfg.IsPrivate, cfg.Calc)
	if !acc.hasUsage {
		ce.CostSource = "estimated"
	}
	as.result.CostEvent = ce

	if cfg.CostW != nil {
		errorClass := ""
		if !acc.hasUsage {
			errorClass = "usage_missing"
		}
		if wErr := cfg.CostW.Write(context.Background(), ce,
			cfg.TraceID, cfg.TenantID, cfg.UserID, cfg.TeamID,
			"", "", "", cfg.AttemptNo, cfg.LatencyMs, true, errorClass); wErr != nil {
			slog.Warn("cost_event write failed",
				"error", wErr, "trace_id", cfg.TraceID)
		}
	}

	// Inject aicg.usage tail event before closing the pipe.
	InjectUsage(pw, cfg.TraceID, "", as.result.Usage, ce.CostCents, ce.CostSource,
		fmt.Sprintf("%s:%s", cfg.EndpointID, cfg.Model),
		cfg.AttemptNo, cfg.Degraded, cfg.LatencyMs)

	if cfg.PostAccount != nil {
		cfg.PostAccount(as.result)
	}
}

// usageAccumulator collects token counts from provider SSE events.
type usageAccumulator struct {
	input       int
	output      int
	cacheRead   int
	cacheCreate int
	hasUsage    bool
}

func (a *usageAccumulator) usage() *ir.Usage {
	return &ir.Usage{
		Input:       a.input,
		Output:      a.output,
		CacheRead:   a.cacheRead,
		CacheCreate: a.cacheCreate,
	}
}

// forwardFrames reads raw lines from src, writes complete SSE frames to dst
// (line-by-line, preserving the original structure), and accumulates usage from
// provider-specific SSE event fields. Uses bufio.Reader.ReadBytes to avoid
// bufio.Scanner's per-line token size limit, so data: lines of any length
// (e.g. oversized tool results) are handled correctly.
func forwardFrames(src io.Reader, dst io.Writer, acc *usageAccumulator, wire string) error {
	reader := bufio.NewReader(src)

	var currentEvent string

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			// Forward the raw line (ReadBytes includes the delimiter).
			if _, werr := dst.Write(line); werr != nil {
				return werr
			}

			// Strip trailing newline/carriage-return for parsing.
			text := strings.TrimRight(string(line), "\r\n")

			if text == "" {
				// Empty line marks the end of an SSE frame.
				currentEvent = ""
				continue
			}
			if strings.HasPrefix(text, "event: ") {
				currentEvent = strings.TrimPrefix(text, "event: ")
				continue
			}
			if strings.HasPrefix(text, "data: ") {
				dataStr := strings.TrimPrefix(text, "data: ")
				handleSSEData(currentEvent, dataStr, acc, wire)
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// handleSSEData dispatches provider-specific usage extraction per SSE data line.
func handleSSEData(event, data string, acc *usageAccumulator, wire string) {
	switch {
	case wire == "anthropic" || wire == "anthropic_compat":
		handleAnthropicData(event, data, acc)
	case wire == "openai" || wire == "openai_compat":
		handleOpenAIData(data, acc)
	}
}

// handleAnthropicData extracts usage from Anthropic message_start and message_delta events.
// Input/cache tokens come from message_start.message.usage; cumulative output tokens
// come from message_delta.usage. Non-usage events are silently ignored.
func handleAnthropicData(event, data string, acc *usageAccumulator) {
	switch event {
	case "message_start":
		var msg struct {
			Type    string `json:"type"`
			Message *struct {
				Usage *struct {
					InputTokens              int  `json:"input_tokens"`
					OutputTokens             int  `json:"output_tokens"`
					CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
					CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return
		}
		if msg.Message != nil && msg.Message.Usage != nil {
			u := msg.Message.Usage
			acc.hasUsage = true
			acc.input = u.InputTokens
			acc.output = u.OutputTokens
			if u.CacheReadInputTokens != nil {
				acc.cacheRead = *u.CacheReadInputTokens
			}
			if u.CacheCreationInputTokens != nil {
				acc.cacheCreate = *u.CacheCreationInputTokens
			}
		}

	case "message_delta":
		var delta struct {
			Type  string `json:"type"`
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &delta); err != nil {
			return
		}
		if delta.Usage != nil {
			acc.hasUsage = true
			acc.output = delta.Usage.OutputTokens // cumulative
		}
	}
}

// handleOpenAIData extracts usage from the final OpenAI-compatible streaming chunk.
// The chunk has the shape:
//
//	data: {"id":"...","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":N,"completion_tokens":N}}
//
// Non-usage chunks (most of the stream) are silently ignored.
func handleOpenAIData(data string, acc *usageAccumulator) {
	var chunk struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}
	if chunk.Usage != nil {
		acc.hasUsage = true
		acc.input = chunk.Usage.PromptTokens
		acc.output = chunk.Usage.CompletionTokens
	}
}
