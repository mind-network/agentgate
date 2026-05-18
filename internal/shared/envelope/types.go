// Package envelope defines the AICGEnvelope metadata schema (§6).
// The envelope carries client-side hints; the server reclassifier recomputes
// task_type / complexity / data_sensitivity independently.
package envelope

// AICGEnvelope is the metadata wrapper sent from LP to GW in
// POST /v1/agent/forward alongside the upstream wire body.
type AICGEnvelope struct {
	SchemaVersion  string          `json:"schema_version"`
	Client         ClientInfo      `json:"client"`
	Identity       IdentityHint    `json:"identity"`
	Agent          AgentInfo       `json:"agent"`
	Session        SessionInfo     `json:"session"`
	Repo           RepoInfo        `json:"repo"`
	ContextSignals ContextSignals  `json:"context_signals"`
	TaskHints      TaskHints       `json:"task_hints"`
	Scan           *ScanInfo       `json:"scan,omitempty"`
	CustomTags     map[string]any  `json:"custom_tags,omitempty"`
}

type ClientInfo struct {
	LPVersion string `json:"lp_version"`
	OS        string `json:"os"`   // darwin, linux, windows
	Arch      string `json:"arch"` // amd64, arm64
}

type IdentityHint struct {
	UserID    string `json:"user_id"`
	TeamID    string `json:"team_id"`
	MachineID string `json:"machine_id"`
}

type AgentInfo struct {
	Tool         string `json:"tool"`          // claude_code, cursor, aider, codex_cli, continue, custom_oai
	ToolVersion  string `json:"tool_version,omitempty"`
	WireProtocol string `json:"wire_protocol"` // anthropic_messages, openai_chat_completions
}

type SessionInfo struct {
	SessionID      string `json:"session_id"`
	TurnIndex      int    `json:"turn_index"`
	IsContinuation bool   `json:"is_continuation"`
}

type RepoInfo struct {
	RepoID    string `json:"repo_id,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	Branch    string `json:"branch,omitempty"`
	HeadSHA   string `json:"head_sha,omitempty"`
	IsDirty   bool   `json:"is_dirty,omitempty"`
}

type ContextSignals struct {
	FilePaths        []string          `json:"file_paths"`
	FileFingerprints []FileFingerprint `json:"file_fingerprints"`
	DiffSummary      DiffSummary       `json:"diff_summary"`
	PrimaryLanguage  string            `json:"primary_language,omitempty"`
}

type FileFingerprint struct {
	Path      string `json:"path"`
	SizeBytes int    `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Language  string `json:"language,omitempty"`
}

type DiffSummary struct {
	LinesAdded                int  `json:"lines_added"`
	LinesRemoved              int  `json:"lines_removed"`
	ContainsTestFailureKW     bool `json:"contains_test_failure_keyword"`
	ContainsStackTrace        bool `json:"contains_stack_trace"`
}

type TaskHints struct {
	TaskType                string   `json:"task_type"`
	Complexity              string   `json:"complexity"`
	DataSensitivity         string   `json:"data_sensitivity"`
	AgenticLoop             bool     `json:"agentic_loop"`
	ContainsSecretLike      bool     `json:"contains_secret_like_pattern"`
	ContainsPIILike         bool     `json:"contains_pii_like_pattern"`
	SecuritySensitiveArea   bool     `json:"security_sensitive_area"`
	RoutingHints            []string `json:"routing_hints"`
	Confidence              Confidence `json:"confidence"`
}

type Confidence struct {
	TaskType        float64 `json:"task_type"`
	Complexity      float64 `json:"complexity"`
	DataSensitivity float64 `json:"data_sensitivity"`
}

type ScanInfo struct {
	PreScanEngine  string        `json:"pre_scan_engine"`
	PreScanFindings []ScanFinding `json:"pre_scan_findings"`
}

type ScanFinding struct {
	RuleID   string `json:"rule_id"`
	Severity string `json:"severity"` // info, warn, critical
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
}
