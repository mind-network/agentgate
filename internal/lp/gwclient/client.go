// Package gwclient implements the LP→GW HTTP client with the first-byte
// may-retry state machine (§5.4.4).
package gwclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// ForwardRequest is the body of POST /v1/agent/forward.
type ForwardRequest struct {
	Envelope json.RawMessage `json:"envelope"`
	Wire     WirePayload     `json:"wire"`
}

// WirePayload carries the upstream protocol and body.
type WirePayload struct {
	Protocol string          `json:"protocol"`
	Stream   bool            `json:"stream"`
	Body     json.RawMessage `json:"body"`
}

// Client handles LP→GW communication.
type Client struct {
	HTTPClient *http.Client
	GwURL      string
	APIKey     string
}

// NewClient creates a new GW client. If caPath is non-empty, it is loaded
// and added to the TLS root CA pool so self-signed GW certs are trusted.
func NewClient(gwURL, apiKey, caPath string) *Client {
	transport := &http.Transport{}
	if caPath != "" {
		caCert, err := os.ReadFile(caPath)
		if err == nil {
			pool, err := x509.SystemCertPool()
			if err != nil {
				pool = x509.NewCertPool()
			}
			if pool.AppendCertsFromPEM(caCert) {
				transport.TLSClientConfig = &tls.Config{RootCAs: pool}
			}
		}
	}
	return &Client{
		HTTPClient: &http.Client{Timeout: 5 * time.Minute, Transport: transport},
		GwURL:      gwURL,
		APIKey:     apiKey,
	}
}

// Forward sends a forward request to the GW and returns the streaming response.
func (c *Client) Forward(ctx context.Context, req *ForwardRequest) (*ForwardResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal forward request: %w", err)
	}

	resp, err := c.doWithRetry(ctx, body)
	if err != nil {
		return nil, err
	}
	return &ForwardResponse{Response: resp}, nil
}

// doWithRetry sends the request; on pre-first-byte 5xx, retries once.
// body is the pre-serialized JSON so each attempt gets a fresh reader.
func (c *Client) doWithRetry(ctx context.Context, body []byte) (*http.Response, error) {
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.GwURL+"/v1/agent/forward", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		return req, nil
	}

	req, err := build()
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gw request: %w", err)
	}
	if resp.StatusCode < 500 || resp.StatusCode >= 600 {
		return resp, nil
	}
	_ = resp.Body.Close()

	req2, err := build()
	if err != nil {
		return nil, fmt.Errorf("gw retry request: %w", err)
	}
	resp2, err2 := c.HTTPClient.Do(req2)
	if err2 != nil {
		return nil, fmt.Errorf("gw retry request: %w", err2)
	}
	if resp2.StatusCode >= 500 && resp2.StatusCode < 600 {
		bodySnippet := readErrorSnippet(resp2.Body)
		_ = resp2.Body.Close()
		return nil, fmt.Errorf("gw returned %d after retry: %s", resp2.StatusCode, bodySnippet)
	}
	return resp2, nil
}

// readErrorSnippet reads up to 1KB of the GW error response body and extracts
// code and trace_id for operator diagnostics. It never leaks raw request content
// or credentials into the returned string.
func readErrorSnippet(body io.ReadCloser) string {
	buf := make([]byte, 1024)
	n, err := io.ReadFull(body, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "(unreadable)"
	}
	snippet := buf[:n]

	var parsed struct {
		Code    string `json:"code"`
		TraceID string `json:"trace_id"`
	}
	if json.Unmarshal(snippet, &parsed) == nil && (parsed.Code != "" || parsed.TraceID != "") {
		return fmt.Sprintf("code=%s trace_id=%s", parsed.Code, parsed.TraceID)
	}
	return "(no error details)"
}

// ForwardResponse wraps the HTTP response for streaming consumption.
type ForwardResponse struct {
	*http.Response
}

// FirstByteStateMachine tracks the may-retry flag during streaming.
// Before the first byte is flushed to the agent, a 5xx from GW can be retried.
// After the first byte, the HTTP status is fixed and errors go via aicg.error SSE events.
type FirstByteStateMachine struct {
	mu        sync.Mutex
	firstByte bool
	mayRetry  bool
}

// NewFirstByteStateMachine creates a state machine in may-retry state.
func NewFirstByteStateMachine() *FirstByteStateMachine {
	return &FirstByteStateMachine{mayRetry: true}
}

// MayRetry returns true if no byte has been flushed to the agent yet.
func (sm *FirstByteStateMachine) MayRetry() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.mayRetry
}

// FlushFirstByte clears the may-retry flag. Call on first successful byte to agent.
func (sm *FirstByteStateMachine) FlushFirstByte() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.firstByte = true
	sm.mayRetry = false
}

// HasFlushed returns true if at least one byte has been sent to the agent.
func (sm *FirstByteStateMachine) HasFlushed() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.firstByte
}

// ExchangeSetup exchanges a setup_token for a long-lived API key.
func (c *Client) ExchangeSetup(setupToken string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, c.GwURL+"/api/v1/lp/exchange-setup", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+setupToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange setup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exchange failed: status %d", resp.StatusCode)
	}

	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("parse exchange response: %w", err)
	}
	return body.APIKey, nil
}

// CostSummaryRow is a row from GET /api/v1/cost/summary.
type CostSummaryRow struct {
	Bucket       string `json:"bucket"`
	DimValue     string `json:"dim_value"`
	CostCents    int    `json:"cost_cents"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	NRequests    int    `json:"n_requests"`
	NFailed      int    `json:"n_failed"`
}

// GetCostSummary fetches cost summary from the GW dashboard API.
// dim, from, and to are appended as query parameters only when non-empty.
func (c *Client) GetCostSummary(dim, from, to string) ([]CostSummaryRow, error) {
	url := c.GwURL + "/api/v1/cost/summary"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	if dim != "" {
		q.Set("dim", dim)
	}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get cost summary: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var rows []CostSummaryRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("parse cost summary: %w", err)
	}
	return rows, nil
}

// RoutingEventRow is a row from GET /api/v1/routing/events.
type RoutingEventRow struct {
	TraceID    string `json:"trace_id"`
	AttemptNo  int    `json:"attempt_no"`
	Pool       string `json:"pool_selected"`
	MemberJSON string `json:"member_selected"`
}

// GetRoutingEvents fetches routing events for a trace_id.
func (c *Client) GetRoutingEvents(traceID string) ([]RoutingEventRow, error) {
	url := fmt.Sprintf("%s/api/v1/routing/events?trace_id=%s", c.GwURL, traceID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get routing events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var rows []RoutingEventRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("parse routing events: %w", err)
	}
	return rows, nil
}

// BindRepoResponse is the response from POST /v1/repo/bind.
type BindRepoResponse struct {
	RepoID       string `json:"repo_id"`
	BindingToken string `json:"binding_token"`
}

// BindRepo sends a repo binding request to the GW.
func (c *Client) BindRepo(repoID, remoteURL, signature string) (*BindRepoResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"repo_id":    repoID,
		"remote_url": remoteURL,
		"signature":  signature,
	})
	req, err := http.NewRequest(http.MethodPost, c.GwURL+"/v1/repo/bind", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bind repo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var r BindRepoResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("parse bind repo response: %w", err)
	}
	return &r, nil
}

// ExchangeInvite consumes a one-shot invite_token at the GW and returns the
// long-lived user API key plus the user_id / role / team_id that the admin
// pre-assigned when minting the invite. The call is anonymous — no
// Authorization header is sent — matching the GW's middleware skip semantics
// for /api/v1/lp/exchange-invite. Non-2xx responses surface the GW's `code`
// field via readErrorSnippet so the CLI / caller can distinguish
// bad_token / token_consumed / token_expired without exposing the raw body.
func (c *Client) ExchangeInvite(token, machineID string) (apiKey, userID, role, teamID string, err error) {
	body, _ := json.Marshal(map[string]string{
		"invite_token": token,
		"machine_id":   machineID,
	})
	req, err := http.NewRequest(http.MethodPost, c.GwURL+"/api/v1/lp/exchange-invite", bytes.NewReader(body))
	if err != nil {
		return "", "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", "", "", "", fmt.Errorf("exchange invite: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet := readErrorSnippet(resp.Body)
		return "", "", "", "", fmt.Errorf("exchange invite failed: status %d: %s", resp.StatusCode, snippet)
	}

	var parsed struct {
		APIKey string `json:"api_key"`
		UserID string `json:"user_id"`
		Role   string `json:"role"`
		TeamID string `json:"team_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", "", "", "", fmt.Errorf("parse exchange invite response: %w", err)
	}
	return parsed.APIKey, parsed.UserID, parsed.Role, parsed.TeamID, nil
}

// CreateInvite mints a one-shot invite_token at the GW for the given target
// user_id / team_id / role with the supplied TTL. The caller must already be
// authenticated as a platform_admin via c.APIKey — GW enforces RBAC and
// returns 403 forbidden otherwise, which is surfaced in the error via
// readErrorSnippet. The plaintext token is returned exactly once; subsequent
// reads are impossible because only its sha256 hash is persisted.
//
// ttl is rounded down to whole hours and must be in (0h, 720h] after rounding;
// the GW also enforces this range and will return bad_request otherwise.
func (c *Client) CreateInvite(userID, teamID, role string, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	ttlHours := int(ttl / time.Hour)
	body, _ := json.Marshal(map[string]any{
		"user_id":   userID,
		"team_id":   teamID,
		"role":      role,
		"ttl_hours": ttlHours,
	})
	req, err := http.NewRequest(http.MethodPost, c.GwURL+"/api/v1/admin/invites", bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create invite: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		snippet := readErrorSnippet(resp.Body)
		return "", time.Time{}, fmt.Errorf("create invite failed: status %d: %s", resp.StatusCode, snippet)
	}

	var parsed struct {
		InviteToken string    `json:"invite_token"`
		ExpiresAt   time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("parse create invite response: %w", err)
	}
	return parsed.InviteToken, parsed.ExpiresAt, nil
}
