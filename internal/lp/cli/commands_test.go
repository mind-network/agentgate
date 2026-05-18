package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"agentgate/internal/lp/localconfig"
)

// withTempHome redirects $HOME to a temporary directory for the duration of t
// so that localconfig.SaveCredentials / LoadCredentials operate on a sandbox.
func withTempHome(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
}

// captureStdout returns whatever fn writes to os.Stdout while it runs.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("capture stdout: %v", err)
	}
	return buf.String()
}

func TestParseLoginFlagsSetup(t *testing.T) {
	f, err := parseLoginFlags([]string{"--setup", "tok-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.setupToken != "tok-123" {
		t.Errorf("setupToken = %q, want tok-123", f.setupToken)
	}
	if f.inviteToken != "" {
		t.Errorf("inviteToken should be empty, got %q", f.inviteToken)
	}
}

func TestParseLoginFlagsInvite(t *testing.T) {
	f, err := parseLoginFlags([]string{"--invite", "inv-abc", "--gateway", "https://gw.test", "--ca", "/etc/ca.pem"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.inviteToken != "inv-abc" {
		t.Errorf("inviteToken = %q, want inv-abc", f.inviteToken)
	}
	if f.gwURL != "https://gw.test" {
		t.Errorf("gwURL = %q, want https://gw.test", f.gwURL)
	}
	if f.caPath != "/etc/ca.pem" {
		t.Errorf("caPath = %q, want /etc/ca.pem", f.caPath)
	}
}

func TestParseLoginFlagsMutexBothPresent(t *testing.T) {
	_, err := parseLoginFlags([]string{"--setup", "s", "--invite", "i"})
	if err == nil {
		t.Fatal("expected error when --setup and --invite both given")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention 'mutually exclusive', got: %v", err)
	}
}

func TestParseLoginFlagsMutexBothPresentReverse(t *testing.T) {
	_, err := parseLoginFlags([]string{"--invite", "i", "--setup", "s"})
	if err == nil {
		t.Fatal("expected error when --invite and --setup both given (reverse order)")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention 'mutually exclusive', got: %v", err)
	}
}

func TestParseLoginFlagsRequireOne(t *testing.T) {
	_, err := parseLoginFlags(nil)
	if err == nil {
		t.Fatal("expected error when neither --setup nor --invite given")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error should mention 'required', got: %v", err)
	}
}

func TestParseLoginFlagsGatewayDefault(t *testing.T) {
	t.Setenv("AICG_PUBLIC_BASE_URL", "")
	f, err := parseLoginFlags([]string{"--setup", "tok"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.gwURL != "https://localhost:8443" {
		t.Errorf("default gwURL = %q, want https://localhost:8443", f.gwURL)
	}
}

func TestParseLoginFlagsGatewayFromEnv(t *testing.T) {
	t.Setenv("AICG_PUBLIC_BASE_URL", "https://env.test")
	f, err := parseLoginFlags([]string{"--setup", "tok"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.gwURL != "https://env.test" {
		t.Errorf("env gwURL = %q, want https://env.test", f.gwURL)
	}
}

func TestRunLoginInvitePersistsUserID(t *testing.T) {
	withTempHome(t)
	origExchanger := inviteExchanger
	defer func() { inviteExchanger = origExchanger }()

	var capturedGwURL, capturedToken, capturedMachine string
	inviteExchanger = func(gwURL, caPath, token, machineID string) (string, string, string, string, error) {
		capturedGwURL = gwURL
		capturedToken = token
		capturedMachine = machineID
		return "u-key-xyz", "dogfood-dev-9", "developer", "dogfood", nil
	}

	out := captureStdout(t, func() {
		if err := runLogin([]string{"--invite", "tok-abc", "--gateway", "https://gw.test"}); err != nil {
			t.Fatalf("runLogin: %v", err)
		}
	})

	if capturedGwURL != "https://gw.test" {
		t.Errorf("exchanger gwURL = %q, want https://gw.test", capturedGwURL)
	}
	if capturedToken != "tok-abc" {
		t.Errorf("exchanger token = %q, want tok-abc", capturedToken)
	}
	if capturedMachine == "" || len(capturedMachine) != 64 {
		t.Errorf("machine_id should be a 64-char sha256 hex, got %q (len=%d)", capturedMachine, len(capturedMachine))
	}

	creds, err := localconfig.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.UserID != "dogfood-dev-9" {
		t.Errorf("creds.UserID = %q, want dogfood-dev-9 (non-hardcoded)", creds.UserID)
	}
	if creds.APIKey != "u-key-xyz" {
		t.Errorf("creds.APIKey = %q, want u-key-xyz", creds.APIKey)
	}
	if creds.TeamID != "dogfood" {
		t.Errorf("creds.TeamID = %q, want dogfood", creds.TeamID)
	}
	if creds.UserID == "platform_admin" {
		t.Error("creds.UserID must not be hardcoded to platform_admin on the invite path")
	}

	if !strings.Contains(out, "Login successful as dogfood-dev-9") {
		t.Errorf("stdout = %q, want it to mention 'Login successful as dogfood-dev-9'", out)
	}
	if !strings.Contains(out, "role=developer") || !strings.Contains(out, "team=dogfood") {
		t.Errorf("stdout = %q, want it to mention role=developer and team=dogfood", out)
	}
}

func TestRunLoginInviteSurfacesError(t *testing.T) {
	withTempHome(t)
	origExchanger := inviteExchanger
	defer func() { inviteExchanger = origExchanger }()
	inviteExchanger = func(_, _, _, _ string) (string, string, string, string, error) {
		return "", "", "", "", &gwError{msg: "status 410: code=token_consumed"}
	}
	err := runLogin([]string{"--invite", "burned-token"})
	if err == nil {
		t.Fatal("expected error when exchanger fails")
	}
	if !strings.Contains(err.Error(), "exchange invite") {
		t.Errorf("err = %v, want it to wrap with 'exchange invite'", err)
	}
	if !strings.Contains(err.Error(), "token_consumed") {
		t.Errorf("err = %v, want GW code surfaced", err)
	}
}

// gwError is a minimal error type used to assert error wrapping in tests.
type gwError struct{ msg string }

func (e *gwError) Error() string { return e.msg }

func TestParseAdminInviteFlagsDefaults(t *testing.T) {
	f, err := parseAdminInviteFlags([]string{"--role", "developer", "--team", "dogfood", "--user", "dogfood-dev-9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.role != "developer" {
		t.Errorf("role = %q, want developer", f.role)
	}
	if f.team != "dogfood" {
		t.Errorf("team = %q, want dogfood", f.team)
	}
	if f.user != "dogfood-dev-9" {
		t.Errorf("user = %q, want dogfood-dev-9", f.user)
	}
	if f.ttl != 24*time.Hour {
		t.Errorf("default ttl = %v, want 24h", f.ttl)
	}
}

func TestParseAdminInviteFlagsCustomTTL(t *testing.T) {
	cases := []struct {
		raw    string
		expect time.Duration
	}{
		{"1h", time.Hour},
		{"24h", 24 * time.Hour},
		{"72h", 72 * time.Hour},
		{"720h", 720 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			f, err := parseAdminInviteFlags([]string{
				"--role", "developer", "--team", "dogfood", "--user", "u", "--ttl", tc.raw,
			})
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.raw, err)
			}
			if f.ttl != tc.expect {
				t.Errorf("ttl = %v, want %v", f.ttl, tc.expect)
			}
		})
	}
}

func TestParseAdminInviteFlagsTTLOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"zero", []string{"--role", "developer", "--team", "t", "--user", "u", "--ttl", "0s"}, "--ttl must be > 0"},
		{"negative", []string{"--role", "developer", "--team", "t", "--user", "u", "--ttl", "-1h"}, "--ttl must be > 0"},
		{"too large", []string{"--role", "developer", "--team", "t", "--user", "u", "--ttl", "721h"}, "--ttl must be <= 720h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAdminInviteFlags(tc.args)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseAdminInviteFlagsTTLMalformed(t *testing.T) {
	_, err := parseAdminInviteFlags([]string{"--role", "developer", "--team", "t", "--user", "u", "--ttl", "tomorrow"})
	if err == nil {
		t.Fatal("expected error for malformed --ttl value")
	}
}

func TestParseAdminInviteFlagsMissingRequired(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing role", []string{"--team", "t", "--user", "u"}, "--role is required"},
		{"missing team", []string{"--role", "developer", "--user", "u"}, "--team is required"},
		{"missing user", []string{"--role", "developer", "--team", "t"}, "--user is required"},
		{"empty args", nil, "--role is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAdminInviteFlags(tc.args)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseAdminInviteFlagsUnknown(t *testing.T) {
	_, err := parseAdminInviteFlags([]string{"--role", "developer", "--bogus", "x"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("err = %v, want it to mention 'unknown flag'", err)
	}
}

func TestParseAdminInviteFlagsValueRequired(t *testing.T) {
	cases := [][]string{
		{"--role"},
		{"--role", "developer", "--team"},
		{"--role", "developer", "--team", "t", "--user"},
		{"--role", "developer", "--team", "t", "--user", "u", "--ttl"},
	}
	for _, args := range cases {
		_, err := parseAdminInviteFlags(args)
		if err == nil {
			t.Errorf("expected error when value missing for %v", args)
		}
	}
}

func TestRunAdminInvitePrintsToken(t *testing.T) {
	withTempHome(t)
	// Pre-populate admin credentials so LoadCredentials succeeds.
	creds := &localconfig.Credentials{GwURL: "https://gw.test", UserID: "platform_admin", APIKey: "admin-key"}
	if err := localconfig.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	origCreator := inviteCreator
	defer func() { inviteCreator = origCreator }()

	expectedExpiry := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	var capturedGwURL, capturedAPIKey, capturedUser, capturedTeam, capturedRole string
	var capturedTTL time.Duration
	inviteCreator = func(gwURL, apiKey, _, userID, teamID, role string, ttl time.Duration) (string, time.Time, error) {
		capturedGwURL = gwURL
		capturedAPIKey = apiKey
		capturedUser = userID
		capturedTeam = teamID
		capturedRole = role
		capturedTTL = ttl
		return "raw-token-hex", expectedExpiry, nil
	}

	out := captureStdout(t, func() {
		if err := runAdminInvite([]string{"--role", "developer", "--team", "dogfood", "--user", "dogfood-dev-9", "--ttl", "1h"}); err != nil {
			t.Fatalf("runAdminInvite: %v", err)
		}
	})

	if capturedGwURL != "https://gw.test" {
		t.Errorf("gwURL = %q, want https://gw.test", capturedGwURL)
	}
	if capturedAPIKey != "admin-key" {
		t.Errorf("apiKey = %q, want admin-key", capturedAPIKey)
	}
	if capturedUser != "dogfood-dev-9" || capturedTeam != "dogfood" || capturedRole != "developer" {
		t.Errorf("captured args = (%q, %q, %q), want (dogfood-dev-9, dogfood, developer)",
			capturedUser, capturedTeam, capturedRole)
	}
	if capturedTTL != time.Hour {
		t.Errorf("captured ttl = %v, want 1h", capturedTTL)
	}

	if !strings.Contains(out, "raw-token-hex") {
		t.Errorf("stdout = %q, want it to include the plaintext token", out)
	}
	if !strings.Contains(out, "single use") {
		t.Errorf("stdout = %q, want it to mention 'single use'", out)
	}
	if !strings.Contains(out, expectedExpiry.Format(time.RFC3339)) {
		t.Errorf("stdout = %q, want it to mention the RFC3339 expires_at", out)
	}
}

func TestRunAdminInviteMissingCredentials(t *testing.T) {
	withTempHome(t)
	// no SaveCredentials, so LoadCredentials should fail.
	err := runAdminInvite([]string{"--role", "developer", "--team", "dogfood", "--user", "u"})
	if err == nil {
		t.Fatal("expected error when credentials missing")
	}
	if !strings.Contains(err.Error(), "load credentials") {
		t.Errorf("err = %v, want it to mention 'load credentials'", err)
	}
}

func TestRunAdminInviteSurfacesCreatorError(t *testing.T) {
	withTempHome(t)
	creds := &localconfig.Credentials{GwURL: "https://gw.test", UserID: "platform_admin", APIKey: "admin-key"}
	if err := localconfig.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	origCreator := inviteCreator
	defer func() { inviteCreator = origCreator }()
	inviteCreator = func(_, _, _, _, _, _ string, _ time.Duration) (string, time.Time, error) {
		return "", time.Time{}, &gwError{msg: "status 403: code=forbidden"}
	}
	err := runAdminInvite([]string{"--role", "developer", "--team", "t", "--user", "u"})
	if err == nil {
		t.Fatal("expected error when creator fails")
	}
	if !strings.Contains(err.Error(), "create invite") {
		t.Errorf("err = %v, want 'create invite' wrap", err)
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v, want underlying code surfaced", err)
	}
}

func TestRunAdminDispatchInvite(t *testing.T) {
	withTempHome(t)
	creds := &localconfig.Credentials{GwURL: "https://gw.test", UserID: "platform_admin", APIKey: "admin-key"}
	if err := localconfig.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	origCreator := inviteCreator
	defer func() { inviteCreator = origCreator }()
	called := false
	inviteCreator = func(_, _, _, _, _, _ string, _ time.Duration) (string, time.Time, error) {
		called = true
		return "tok", time.Now().UTC(), nil
	}

	_ = captureStdout(t, func() {
		if err := runAdmin([]string{"invite", "--role", "developer", "--team", "dogfood", "--user", "u"}); err != nil {
			t.Fatalf("runAdmin: %v", err)
		}
	})
	if !called {
		t.Error("runAdmin did not route to runAdminInvite for 'invite' subcommand")
	}
}

func TestRunAdminUnknownSubcommand(t *testing.T) {
	err := runAdmin([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown admin subcommand")
	}
	if !strings.Contains(err.Error(), "unknown admin subcommand") {
		t.Errorf("err = %v, want 'unknown admin subcommand'", err)
	}
}

func TestRunAdminMissingSubcommand(t *testing.T) {
	err := runAdmin(nil)
	if err == nil {
		t.Fatal("expected error when no admin subcommand given")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("err = %v, want it to print usage", err)
	}
}

func TestDispatchAdminInvite(t *testing.T) {
	withTempHome(t)
	creds := &localconfig.Credentials{GwURL: "https://gw.test", UserID: "platform_admin", APIKey: "admin-key"}
	if err := localconfig.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	origCreator := inviteCreator
	defer func() { inviteCreator = origCreator }()
	called := false
	inviteCreator = func(_, _, _, _, _, _ string, _ time.Duration) (string, time.Time, error) {
		called = true
		return "tok", time.Now().UTC(), nil
	}
	_ = captureStdout(t, func() {
		if err := Dispatch([]string{"aicg-lp", "admin", "invite", "--role", "developer", "--team", "dogfood", "--user", "u"}); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
	})
	if !called {
		t.Error("Dispatch did not reach inviteCreator via admin/invite chain")
	}
}

func TestComputeMachineIDStable(t *testing.T) {
	a := computeMachineID()
	b := computeMachineID()
	if a == "" {
		t.Fatal("computeMachineID returned empty string")
	}
	if a != b {
		t.Errorf("computeMachineID not stable across calls: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("computeMachineID len = %d, want 64 (sha256 hex)", len(a))
	}
}
