package cli

import "testing"

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello world", 8, "hello..."},
		{"abc", 3, "abc"},
		{"abcd", 3, "..."},
		{"", 5, ""},
	}
	for _, tc := range tests {
		got := truncate(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestParseMemberJSON(t *testing.T) {
	tests := []struct {
		raw         string
		wantEpID    string
		wantModel   string
	}{
		{`{"endpoint_id":"anthropic-prod","model":"claude-sonnet-4-6"}`, "anthropic-prod", "claude-sonnet-4-6"},
		{`{"pool":"standard","endpoint_id":"deepseek","model":"deepseek-chat"}`, "deepseek", "deepseek-chat"},
		{`{"endpoint_id":"","model":""}`, "", ""},
		{`not json at all`, "not json at all", ""},
		{`{"other":"value"}`, `{"other":"value"}`, ""},
		{`{"endpoint_id":"multi-word-ep","model":"claude-opus-4-7","weight":100}`, "multi-word-ep", "claude-opus-4-7"},
	}

	for _, tc := range tests {
		epID, model := parseMemberJSON(tc.raw)
		if epID != tc.wantEpID {
			t.Errorf("parseMemberJSON(%q) endpoint_id = %q, want %q", tc.raw, epID, tc.wantEpID)
		}
		if model != tc.wantModel {
			t.Errorf("parseMemberJSON(%q) model = %q, want %q", tc.raw, model, tc.wantModel)
		}
	}
}
