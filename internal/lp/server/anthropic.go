package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"runtime"
	"strings"

	"agentgate/internal/lp/gwclient"
	"agentgate/internal/lp/ledger"
	"agentgate/internal/lp/localconfig"
	"agentgate/internal/lp/repobinder"
	"agentgate/internal/lp/session"
	"agentgate/internal/lp/tagger"
	"agentgate/internal/shared/version"
)

// AnthropicHandler proxies /anthropic/v1/messages to the GW.
type AnthropicHandler struct {
	GwClient *gwclient.Client
	Session  *session.Session
	RepoRoot string
}

// Register registers Anthropic-compatible routes.
func (h *AnthropicHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/anthropic/v1/messages", h.handleMessages)
	mux.HandleFunc("/anthropic/v1/messages/count_tokens", h.handleCountTokens)
}

func (h *AnthropicHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	defer func() { _ = r.Body.Close() }()

	// Build envelope
	envelope := h.buildEnvelope(body)
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to marshal envelope"})
		return
	}

	stream := isStreamRequest(body)

	fwd := &gwclient.ForwardRequest{
		Envelope: envelopeJSON,
		Wire: gwclient.WirePayload{
			Protocol: "anthropic_messages",
			Stream:   stream,
			Body:     json.RawMessage(body),
		},
	}

	ctx := r.Context()
	resp, err := h.GwClient.Forward(ctx, fwd)
	if err != nil {
		log.Printf("gw forward error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "gateway unreachable",
			"hint":  "ensure aicg-gw is running and credentials are valid",
		})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// For successful streaming Anthropic responses, use the SSE frame relay
	// to filter AgentGate metadata frames from the client-facing stream.
	// relayStream handles its own headers and WriteHeader — do not copy
	// headers before the relay path.
	if resp.StatusCode == http.StatusOK && isStreamContentType(resp.Header.Get("Content-Type")) {
		h.relayStream(w, resp, envelope)
		return
	}

	// Non-streaming and non-200 responses keep the existing direct copy behavior.
	// Copy response headers
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	sm := gwclient.NewFirstByteStateMachine()
	var captured bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			captured.Write(buf[:n])
			if !sm.HasFlushed() {
				sm.FlushFirstByte()
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("gw response body read error: %v", err)
			}
			break
		}
	}

	// Parse aicg.usage from captured SSE stream and write to local ledger.
	if err := writeUsageToLedger(captured.Bytes(), envelope["session"].(map[string]any)["session_id"].(string)); err != nil {
		log.Printf("ledger write error: %v", err)
	}
}

func (h *AnthropicHandler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	// P0: forward count_tokens to GW for local estimation.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	defer func() { _ = r.Body.Close() }()

	envelope := h.buildEnvelope(body)
	envelopeJSON, _ := json.Marshal(envelope)

	fwd := &gwclient.ForwardRequest{
		Envelope: envelopeJSON,
		Wire: gwclient.WirePayload{
			Protocol: "anthropic_messages",
			Stream:   false,
			Body:     json.RawMessage(body),
		},
	}

	resp, err := h.GwClient.Forward(r.Context(), fwd)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "gateway unreachable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

var filePathRE = regexp.MustCompile(`[\w/.\-]+\.\w{1,5}`)

func (h *AnthropicHandler) buildEnvelope(body []byte) map[string]any {
	tag := tagger.Classify(nil, "", 0, 0, false, false)

	// Load identity from credentials.
	identity := map[string]any{
		"user_id":    "",
		"team_id":    "",
		"machine_id": "",
	}
	if creds, err := localconfig.LoadCredentials(); err == nil {
		identity["user_id"] = creds.UserID
		identity["team_id"] = creds.TeamID
	}

	// Load repo binding.
	repo := map[string]any{}
	if binding, err := repobinder.Load(h.RepoRoot); err == nil {
		repo["repo_id"] = binding.RepoID
		repo["binding"] = binding.Token
	}

	// Extract file paths from message body content.
	var filePaths []string
	var bodyJSON struct {
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &bodyJSON) == nil {
		seen := make(map[string]bool)
		for _, msg := range bodyJSON.Messages {
			for _, c := range msg.Content {
				for _, m := range filePathRE.FindAllString(c.Text, -1) {
					if !seen[m] && len(filePaths) < 20 {
						seen[m] = true
						filePaths = append(filePaths, m)
					}
				}
			}
		}
	}
	if filePaths == nil {
		filePaths = []string{}
	}

	return map[string]any{
		"schema_version": "1.0",
		"client": map[string]any{
			"lp_version": version.String(),
			"os":         runtime.GOOS,
			"arch":       runtime.GOARCH,
		},
		"identity": identity,
		"agent": map[string]any{
			"tool":          "claude_code",
			"wire_protocol": "anthropic_messages",
		},
		"session": map[string]any{
			"session_id":      h.Session.SessionID,
			"turn_index":      0,
			"is_continuation": false,
		},
		"repo": repo,
		"context_signals": map[string]any{
			"file_paths":        filePaths,
			"file_fingerprints": nil,
			"diff_summary":      nil,
		},
		"task_hints": map[string]any{
			"task_type":        tag.TaskType,
			"complexity":       tag.Complexity,
			"data_sensitivity": "unknown",
			"agentic_loop":     false,
		},
	}
}

func (h *AnthropicHandler) relayStream(w http.ResponseWriter, resp *gwclient.ForwardResponse, envelope map[string]any) {
	filterStreamHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	sessionID := envelope["session"].(map[string]any)["session_id"].(string)
	relay := &sseReader{r: resp.Body}
	for {
		frame, err := relay.NextFrame()
		if err != nil {
			if err != io.EOF {
				log.Printf("sse relay read error: %v", err)
			}
			break
		}
		if !isAgentGateFrame(frame) {
			if _, writeErr := w.Write(frame); writeErr != nil {
				return
			}
			if _, writeErr := w.Write([]byte("\n\n")); writeErr != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			continue
		}
		if bytes.HasPrefix(frame, []byte("event: aicg.usage")) {
			if err := processUsageFrame(frame, sessionID); err != nil {
				log.Printf("ledger write error: %v", err)
			}
			continue
		}
		if bytes.HasPrefix(frame, []byte("event: aicg.error")) {
			errFrame := buildAnthropicErrorFrame(frame)
			if _, writeErr := w.Write(errFrame); writeErr != nil {
				return
			}
			if _, writeErr := w.Write([]byte("\n\n")); writeErr != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}
		// Unknown aicg.* frame: suppress.
	}
}

func isStreamContentType(ct string) bool {
	return strings.HasPrefix(ct, "text/event-stream")
}

func isAgentGateFrame(frame []byte) bool {
	return bytes.HasPrefix(frame, []byte("event: aicg."))
}

func isStreamRequest(body []byte) bool {
	var v struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return false
	}
	return v.Stream
}

// unsafeStreamHeaders are headers that become stale when the streaming body
// is transformed (frames removed or replaced), so they must not be forwarded.
var unsafeStreamHeaders = map[string]bool{
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Connection":        true,
	"Trailer":           true,
}

func filterStreamHeaders(h http.Header) {
	for k := range unsafeStreamHeaders {
		h.Del(k)
	}
}

func processUsageFrame(frame []byte, sessionID string) error {
	lines := bytes.Split(frame, []byte("\n"))
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		return processUsageData(string(bytes.TrimPrefix(line, []byte("data: "))), sessionID)
	}
	return nil
}

func processUsageData(usageJSON string, sessionID string) error {
	var usage struct {
		TraceID   string `json:"trace_id"`
		CostCents int    `json:"cost_cents"`
		Tokens    struct {
			Input  int `json:"input"`
			Output int `json:"output"`
		} `json:"tokens"`
		RoutedTo string `json:"routed_to"`
	}
	if err := json.Unmarshal([]byte(usageJSON), &usage); err != nil {
		return err
	}
	dir, err := localconfig.HomeDir()
	if err != nil {
		return err
	}
	l, err := ledger.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()
	routedParts := strings.SplitN(usage.RoutedTo, ":", 2)
	provider := ""
	model := ""
	if len(routedParts) == 2 {
		provider = routedParts[0]
		model = routedParts[1]
	}
	return l.Insert(&ledger.TraceRecord{
		TraceID:   usage.TraceID,
		SessionID: sessionID,
		CostCents: usage.CostCents,
		TokensIn:  usage.Tokens.Input,
		TokensOut: usage.Tokens.Output,
		Model:     model,
		Provider:  provider,
	})
}

// writeUsageToLedger scans captured SSE bytes for aicg.usage frames and writes
// trace data to the local SQLite ledger. Used by the non-streaming copy path.
func writeUsageToLedger(data []byte, sessionID string) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Split(sseSplitFunc)

	for scanner.Scan() {
		frame := scanner.Text()
		if !strings.HasPrefix(frame, "event: aicg.usage") {
			continue
		}
		lines := strings.Split(frame, "\n")
		for _, line := range lines {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			return processUsageData(strings.TrimPrefix(line, "data: "), sessionID)
		}
	}
	return scanner.Err()
}

// buildAnthropicErrorFrame translates an aicg.error SSE frame into an
// Anthropic-compatible event: error frame.
func buildAnthropicErrorFrame(frame []byte) []byte {
	var aicgMsg, traceID string

	lines := bytes.Split(frame, []byte("\n"))
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		var data struct {
			Message string `json:"message"`
			TraceID string `json:"trace_id"`
		}
		if err := json.Unmarshal(bytes.TrimPrefix(line, []byte("data: ")), &data); err != nil {
			continue
		}
		aicgMsg = data.Message
		traceID = data.TraceID
	}

	msg := aicgMsg
	if traceID != "" {
		if msg != "" {
			msg = msg + " (trace_id: " + traceID + ")"
		} else {
			msg = "trace_id: " + traceID
		}
	}
	if msg == "" {
		msg = "gateway stream error"
	}

	errData := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": msg,
		},
	}
	dataBytes, _ := json.Marshal(errData)
	return append([]byte("event: error\ndata: "), dataBytes...)
}

// sseReader reads complete SSE frames delimited by "\n\n" from a stream.
// It handles frames larger than 64 KiB and frames split across arbitrary
// read boundaries without using bufio.Scanner (whose default token limit
// is only 64 KiB).
type sseReader struct {
	r    io.Reader
	buf  []byte
	done bool
}

// NextFrame returns the next complete SSE frame (without the trailing "\n\n").
func (r *sseReader) NextFrame() ([]byte, error) {
	for {
		if i := bytes.Index(r.buf, []byte("\n\n")); i >= 0 {
			frame := make([]byte, i)
			copy(frame, r.buf[:i])
			r.buf = r.buf[i+2:]
			return frame, nil
		}
		if r.done {
			if len(r.buf) > 0 {
				frame := r.buf
				r.buf = nil
				return frame, nil
			}
			return nil, io.EOF
		}
		tmp := make([]byte, 4096)
		n, err := r.r.Read(tmp)
		if n > 0 {
			r.buf = append(r.buf, tmp[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				r.done = true
			} else {
				return nil, err
			}
		}
	}
}

// sseSplitFunc splits SSE frames by double newlines.
func sseSplitFunc(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
