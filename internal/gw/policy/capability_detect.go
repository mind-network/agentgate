package policy

import "encoding/json"

// contentBlock is a single content block in an Anthropic message.
type contentBlock struct {
	Type         string          `json:"type"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// message is a single message in the Messages API body.
type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// messagesBody mirrors the top-level shape we need to inspect.
type messagesBody struct {
	Messages []message        `json:"messages"`
	System   json.RawMessage  `json:"system"`
	Thinking json.RawMessage  `json:"thinking"`
}

// FilterDegradable removes capabilities from the detected list that are
// configured as degradable. The returned slice contains only hard requirements —
// capabilities NOT present in allowed. When allowed is empty (strict mode),
// all detected capabilities are returned as hard requirements.
func FilterDegradable(detected, allowed []string) []string {
	if len(allowed) == 0 {
		return detected
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, c := range allowed {
		allowedSet[c] = true
	}
	out := make([]string, 0, len(detected))
	for _, c := range detected {
		if !allowedSet[c] {
			out = append(out, c)
		}
	}
	return out
}

// DetectCapabilities inspects the request body for features that require
// specific endpoint capabilities. Returns the set of required capability names
// (e.g. "cache_control", "extended_thinking", "vision").
func DetectCapabilities(body json.RawMessage) []string {
	var caps []string
	var mb messagesBody
	if err := json.Unmarshal(body, &mb); err != nil {
		return nil
	}

	hasCacheControl := false
	hasVision := false

	// Check system blocks for cache_control.
	if len(mb.System) > 0 {
		hasCacheControl = hasCacheControl || hasCacheControlInArray(mb.System)
	}

	// Check messages for cache_control and image content blocks.
	for _, msg := range mb.Messages {
		hasCacheControl = hasCacheControl || hasCacheControlInArray(msg.Content)
		hasVision = hasVision || hasVisionInArray(msg.Content)
	}

	if hasCacheControl {
		caps = append(caps, "cache_control")
	}
	if len(mb.Thinking) > 0 && string(mb.Thinking) != "null" {
		caps = append(caps, "extended_thinking")
	}
	if hasVision {
		caps = append(caps, "vision")
	}
	return caps
}

func hasCacheControlInArray(raw json.RawMessage) bool {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// Might be a single object instead of an array.
		var single contentBlock
		if err := json.Unmarshal(raw, &single); err != nil {
			return false
		}
		return len(single.CacheControl) > 0
	}
	for _, b := range blocks {
		if len(b.CacheControl) > 0 {
			return true
		}
	}
	return false
}

func hasVisionInArray(raw json.RawMessage) bool {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		var single contentBlock
		if err := json.Unmarshal(raw, &single); err != nil {
			return false
		}
		return single.Type == "image"
	}
	for _, b := range blocks {
		if b.Type == "image" {
			return true
		}
	}
	return false
}
