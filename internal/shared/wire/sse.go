// Package wire provides SSE (Server-Sent Events) reader/writer helpers
// used by both LP and GW for streaming Anthropic Messages API wire format.
package wire

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// SSEEvent represents a parsed SSE event.
type SSEEvent struct {
	Event string // event type (message_start, content_block_delta, etc.)
	ID    string // event id, if present
	Data  string // data payload
}

// SSEWriter writes SSE-formatted events to an io.Writer.
type SSEWriter struct {
	w *bufio.Writer
}

// NewSSEWriter creates a new SSEWriter.
func NewSSEWriter(w io.Writer) *SSEWriter {
	return &SSEWriter{w: bufio.NewWriter(w)}
}

// WriteEvent writes a single SSE event.
func (s *SSEWriter) WriteEvent(ev SSEEvent) error {
	if ev.Event != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", ev.Event); err != nil {
			return err
		}
	}
	if ev.ID != "" {
		if _, err := fmt.Fprintf(s.w, "id: %s\n", ev.ID); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", ev.Data); err != nil {
		return err
	}
	return s.w.Flush()
}

// WriteRaw writes a raw SSE-formatted line (used for passthrough).
func (s *SSEWriter) WriteRaw(line string) error {
	if _, err := s.w.WriteString(line); err != nil {
		return err
	}
	if _, err := s.w.WriteString("\n"); err != nil {
		return err
	}
	return s.w.Flush()
}

// SSEReader reads SSE events from an io.Reader.
type SSEReader struct {
	sc *bufio.Scanner
}

// NewSSEReader creates a new SSEReader.
func NewSSEReader(r io.Reader) *SSEReader {
	sc := bufio.NewScanner(r)
	sc.Split(bufio.ScanLines)
	return &SSEReader{sc: sc}
}

// ReadEvent reads the next SSE event. Returns io.EOF when done.
func (s *SSEReader) ReadEvent() (SSEEvent, error) {
	var ev SSEEvent
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			// Empty line signals end of event
			if ev.Data != "" || ev.Event != "" {
				return ev, nil
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			ev.Event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "id: ") {
			ev.ID = strings.TrimPrefix(line, "id: ")
		} else if strings.HasPrefix(line, "data: ") {
			ev.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := s.sc.Err(); err != nil {
		return SSEEvent{}, err
	}
	// Return final event if present
	if ev.Data != "" || ev.Event != "" {
		return ev, nil
	}
	return SSEEvent{}, io.EOF
}
