package policy

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func TestDetectCapabilitiesCacheControl(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Hello", "cache_control": {"type": "ephemeral"}}
				]
			}
		]
	}`)
	caps := DetectCapabilities(body)
	if !contains(caps, "cache_control") {
		t.Errorf("expected cache_control in caps, got %v", caps)
	}
}

func TestDetectCapabilitiesExtendedThinking(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"thinking": {"type": "enabled", "budget_tokens": 4000},
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello"}]}
		]
	}`)
	caps := DetectCapabilities(body)
	if !contains(caps, "extended_thinking") {
		t.Errorf("expected extended_thinking in caps, got %v", caps)
	}
}

func TestDetectCapabilitiesVision(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Describe this"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "abc"}}
				]
			}
		]
	}`)
	caps := DetectCapabilities(body)
	if !contains(caps, "vision") {
		t.Errorf("expected vision in caps, got %v", caps)
	}
}

func TestDetectCapabilitiesPlainText(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello"}]}
		]
	}`)
	caps := DetectCapabilities(body)
	if len(caps) != 0 {
		t.Errorf("expected no caps for plain text, got %v", caps)
	}
}

func TestDetectCapabilitiesSystemCacheControl(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"system": [
			{"type": "text", "text": "You are helpful", "cache_control": {"type": "ephemeral"}}
		],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello"}]}
		]
	}`)
	caps := DetectCapabilities(body)
	if !contains(caps, "cache_control") {
		t.Errorf("expected cache_control from system block, got %v", caps)
	}
}

func TestDetectCapabilitiesAllThree(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"thinking": {"type": "enabled", "budget_tokens": 4000},
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Describe this", "cache_control": {"type": "ephemeral"}},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "abc"}}
				]
			}
		]
	}`)
	caps := DetectCapabilities(body)
	if !containsAll(caps, []string{"cache_control", "extended_thinking", "vision"}) {
		t.Errorf("expected all three caps, got %v", caps)
	}
}

func TestDetectCapabilitiesInvalidJSON(t *testing.T) {
	body := json.RawMessage(`not json`)
	caps := DetectCapabilities(body)
	if len(caps) != 0 {
		t.Errorf("expected no caps for invalid JSON, got %v", caps)
	}
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func containsAll(slice, items []string) bool {
	for _, item := range items {
		if !contains(slice, item) {
			return false
		}
	}
	return true
}

func TestFilterDegradableEnabled(t *testing.T) {
	detected := []string{"cache_control", "extended_thinking"}
	allowed := []string{"cache_control", "extended_thinking"}
	hard := FilterDegradable(detected, allowed)
	if len(hard) != 0 {
		t.Errorf("expected empty hard requirements when all detected are degradable, got %v", hard)
	}
}

func TestFilterDegradableStrictMode(t *testing.T) {
	detected := []string{"cache_control", "extended_thinking", "vision"}
	hard := FilterDegradable(detected, nil)
	if !containsAll(hard, detected) {
		t.Errorf("strict mode should return all detected as hard, got %v", hard)
	}
	if len(hard) != len(detected) {
		t.Errorf("strict mode should not change length, got %d want %d", len(hard), len(detected))
	}
}

func TestFilterDegradableStrictModeEmpty(t *testing.T) {
	detected := []string{"cache_control", "extended_thinking"}
	hard := FilterDegradable(detected, []string{})
	if !containsAll(hard, detected) {
		t.Errorf("empty allowed list should return all as hard, got %v", hard)
	}
}

func TestFilterDegradableMixed(t *testing.T) {
	detected := []string{"cache_control", "extended_thinking", "vision"}
	allowed := []string{"cache_control", "extended_thinking"}
	hard := FilterDegradable(detected, allowed)
	if !containsAll(hard, []string{"vision"}) {
		t.Errorf("expected only vision as hard requirement, got %v", hard)
	}
	if len(hard) != 1 {
		t.Errorf("expected exactly 1 hard requirement, got %d", len(hard))
	}
}

func TestFilterDegradableNoneDetected(t *testing.T) {
	detected := []string{}
	allowed := []string{"cache_control", "extended_thinking"}
	hard := FilterDegradable(detected, allowed)
	if len(hard) != 0 {
		t.Errorf("expected no hard requirements, got %v", hard)
	}
}

func TestFilterDegradableOnlyVisionDetected(t *testing.T) {
	detected := []string{"vision"}
	allowed := []string{"cache_control", "extended_thinking"}
	hard := FilterDegradable(detected, allowed)
	if !containsAll(hard, []string{"vision"}) {
		t.Errorf("vision not in degradation list should remain hard, got %v", hard)
	}
}

func TestFilterDegradablePreservesOrder(t *testing.T) {
	detected := []string{"cache_control", "vision", "extended_thinking"}
	allowed := []string{"cache_control", "extended_thinking"}
	hard := FilterDegradable(detected, allowed)
	if len(hard) != 1 || hard[0] != "vision" {
		t.Errorf("expected [vision], got %v", hard)
	}
}

func TestFilterDegradableExtraAllowedIgnored(t *testing.T) {
	// Allowed list contains values not in detected — they are simply ignored.
	detected := []string{"cache_control"}
	allowed := []string{"cache_control", "extended_thinking"}
	hard := FilterDegradable(detected, allowed)
	if len(hard) != 0 {
		t.Errorf("expected empty hard requirements, got %v", hard)
	}
}

func TestCapabilitySliceNoDuplicates(t *testing.T) {
	// Two content blocks both with cache_control should still yield one entry.
	body := json.RawMessage(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "A", "cache_control": {"type": "ephemeral"}},
					{"type": "text", "text": "B", "cache_control": {"type": "ephemeral"}}
				]
			}
		]
	}`)
	caps := DetectCapabilities(body)
	sort.Strings(caps)
	expected := []string{"cache_control"}
	if !reflect.DeepEqual(caps, expected) {
		t.Errorf("expected %v, got %v", expected, caps)
	}
}
