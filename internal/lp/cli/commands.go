package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agentgate/internal/gw/config"
	"agentgate/internal/lp/gwclient"
	"agentgate/internal/lp/ledger"
	"agentgate/internal/lp/localconfig"
	"agentgate/internal/lp/server"
	"agentgate/internal/lp/session"
	"agentgate/internal/shared/version"
)

func runVersion(_ []string) error {
	fmt.Println(version.String())
	return nil
}

// loginFlags holds the parsed flags for `aicg-lp login`.
type loginFlags struct {
	setupToken  string
	inviteToken string
	gwURL       string
	caPath      string
}

// parseLoginFlags parses login args. Exactly one of --setup / --invite is
// required; specifying both is an error.
func parseLoginFlags(args []string) (loginFlags, error) {
	f := loginFlags{
		gwURL: os.Getenv("AICG_PUBLIC_BASE_URL"),
	}
	if f.gwURL == "" {
		f.gwURL = "https://localhost:8443"
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--setup":
			if i+1 < len(args) {
				f.setupToken = args[i+1]
				i++
			}
		case "--invite":
			if i+1 < len(args) {
				f.inviteToken = args[i+1]
				i++
			}
		case "--gateway":
			if i+1 < len(args) {
				f.gwURL = args[i+1]
				i++
			}
		case "--ca":
			if i+1 < len(args) {
				f.caPath = args[i+1]
				i++
			}
		}
	}
	if f.setupToken != "" && f.inviteToken != "" {
		return loginFlags{}, fmt.Errorf("--setup and --invite are mutually exclusive")
	}
	if f.setupToken == "" && f.inviteToken == "" {
		return loginFlags{}, fmt.Errorf("one of --setup <token> or --invite <token> is required")
	}
	return f, nil
}

// inviteExchanger is swapped by tests so runLogin can be exercised without a
// live GW. Production binds it to a real gwclient.Client at call time.
var inviteExchanger = func(gwURL, caPath, token, machineID string) (apiKey, userID, role, teamID string, err error) {
	c := gwclient.NewClient(gwURL, "", caPath)
	return c.ExchangeInvite(token, machineID)
}

// setupExchanger is swapped by tests so runLogin can be exercised without a
// live GW.
var setupExchanger = func(gwURL, caPath, token string) (string, error) {
	c := gwclient.NewClient(gwURL, "", caPath)
	return c.ExchangeSetup(token)
}

// computeMachineID returns a stable per-host identifier as sha256(hostname).
// The GW persists this verbatim in invites.machine_id at exchange time.
func computeMachineID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:])
}

func runLogin(args []string) error {
	f, err := parseLoginFlags(args)
	if err != nil {
		return err
	}

	var creds *localconfig.Credentials
	var successLine string

	switch {
	case f.inviteToken != "":
		machineID := computeMachineID()
		apiKey, userID, role, teamID, err := inviteExchanger(f.gwURL, f.caPath, f.inviteToken, machineID)
		if err != nil {
			return fmt.Errorf("exchange invite: %w", err)
		}
		creds = &localconfig.Credentials{
			GwURL:  f.gwURL,
			UserID: userID,
			APIKey: apiKey,
			TeamID: teamID,
		}
		successLine = fmt.Sprintf("Login successful as %s (role=%s, team=%s)", userID, role, teamID)
	default: // --setup path
		apiKey, err := setupExchanger(f.gwURL, f.caPath, f.setupToken)
		if err != nil {
			return fmt.Errorf("exchange setup token: %w", err)
		}
		creds = &localconfig.Credentials{
			GwURL:  f.gwURL,
			UserID: "platform_admin",
			APIKey: apiKey,
		}
		successLine = "Login successful. Credentials saved to ~/.aicg/credentials"
	}

	if err := localconfig.SaveCredentials(creds); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}

	if f.caPath != "" {
		cfg, _ := localconfig.LoadConfig()
		if cfg != nil {
			cfg.CAPath = f.caPath
			_ = localconfig.SaveConfig(cfg)
		}
	}

	fmt.Println(successLine)
	return nil
}

func runStart(args []string) error {
	cfg, err := localconfig.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	creds, err := localconfig.LoadCredentials()
	if err != nil {
		return fmt.Errorf("load credentials: %w (run `aicg login` first)", err)
	}

	port := cfg.Port
	for i := 0; i < len(args); i++ {
		if args[i] == "--port" && i+1 < len(args) {
			_, _ = fmt.Sscanf(args[i+1], "%d", &port)
		}
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	gw := gwclient.NewClient(creds.GwURL, creds.APIKey, loadCAPath())
	repoRoot, _ := os.Getwd()
	sess := session.New(repoRoot)

	handler := &server.AnthropicHandler{
		GwClient: gw,
		Session:  sess,
		RepoRoot: repoRoot,
	}

	mux := http.NewServeMux()
	handler.Register(mux)
	meta := &server.MetaHandler{SessionID: sess.SessionID}
	meta.Register(mux)

	srv := &http.Server{Addr: addr, Handler: mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	fmt.Printf("LP daemon listening on http://%s\n", addr)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func runStatus(args []string) error {
	dir, err := localconfig.HomeDir()
	if err != nil {
		return err
	}
	l, err := ledger.Open(dir)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer func() { _ = l.Close() }()

	fmt.Printf("Local Proxy\n")
	fmt.Printf("  Ledger: %s/traces.db\n\n", dir)

	recs, err := l.ListRecent(5)
	if err != nil {
		return fmt.Errorf("list recent: %w", err)
	}

	fmt.Println("Recent traces")
	if len(recs) == 0 {
		fmt.Println("  (no local traces yet)")
		return nil
	}
	fmt.Printf("  %-36s %-20s %-12s %8s %8s %8s %s\n",
		"trace_id", "model", "provider", "in", "out", "cost", "recorded_at")
	for _, r := range recs {
		model := r.Model
		if model == "" {
			model = "-"
		}
		prov := r.Provider
		if prov == "" {
			prov = "-"
		}
		fmt.Printf("  %-36s %-20s %-12s %8d %8d %7d¢ %s\n",
			truncate(r.TraceID, 36), truncate(model, 20), truncate(prov, 12),
			r.TokensIn, r.TokensOut, r.CostCents, r.RecordedAt.Format("2006-01-02 15:04:05"))
	}
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func runBindRepo(args []string) error {
	creds, err := localconfig.LoadCredentials()
	if err != nil {
		return err
	}
	gw := gwclient.NewClient(creds.GwURL, creds.APIKey, loadCAPath())
	resp, err := gw.BindRepo("local", "", "")
	if err != nil {
		return fmt.Errorf("bind repo: %w", err)
	}
	_ = resp
	fmt.Println("bind-repo: ok")
	return nil
}

func runDoctor(args []string) error {
	issues := 0
	if _, err := localconfig.LoadCredentials(); err != nil {
		fmt.Printf("[FAIL] credentials: %v\n", err)
		issues++
	} else {
		fmt.Println("[ OK ] credentials: ~/.aicg/credentials exists")
	}
	if _, err := localconfig.LoadConfig(); err != nil {
		fmt.Printf("[FAIL] config: %v\n", err)
		issues++
	} else {
		fmt.Println("[ OK ] config: ~/.aicg/config.yaml ok")
	}
	fmt.Printf("[INFO] version: %s\n", version.String())
	if issues > 0 {
		return fmt.Errorf("%d issue(s) found", issues)
	}
	fmt.Println("All checks passed.")
	return nil
}

func runEnv(args []string) error {
	cfg, err := localconfig.LoadConfig()
	if err != nil {
		return err
	}
	fmt.Printf("export ANTHROPIC_BASE_URL=http://127.0.0.1:%d\n", cfg.Port)
	fmt.Printf("export ANTHROPIC_API_KEY=lp-noop\n")
	return nil
}

// statsFetcher is swapped by tests to avoid requiring live GW credentials.
var statsFetcher = func(creds *localconfig.Credentials, dim, from, to string) ([]gwclient.CostSummaryRow, error) {
	gw := gwclient.NewClient(creds.GwURL, creds.APIKey, loadCAPath())
	return gw.GetCostSummary(dim, from, to)
}

func runStats(args []string) error {
	opts, err := ParseStatsOptions(args)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}

	creds, err := localconfig.LoadCredentials()
	if err != nil {
		return err
	}
	summary, err := statsFetcher(creds, opts.Dim, opts.From, opts.To)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}

	fmt.Print(RenderStatsTable(opts, summary))
	return nil
}

func runPolicyCtl(args []string) error {
	if len(args) < 1 || (len(args) > 0 && args[0] == "validate") {
		filePath := "configs/policies/main.yaml"
		if len(args) > 1 {
			filePath = args[1]
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("read policy file: %w", err)
		}
		_, err = config.LoadAndValidate(filePath, data)
		if err != nil {
			fmt.Printf("Validation FAILED: %v\n", err)
			return err
		}
		fmt.Printf("Policy %s: valid\n", filePath)
		return nil
	}
	return fmt.Errorf("usage: aicg policyctl validate [file]")
}

func runRoutingCtl(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: aicg routingctl replay <trace_id>")
	}
	traceID := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "replay":
			if i+1 < len(args) {
				traceID = args[i+1]
				i++
			}
		}
	}
	if traceID == "" {
		return fmt.Errorf("trace_id required")
	}

	creds, err := localconfig.LoadCredentials()
	if err != nil {
		return err
	}
	gw := gwclient.NewClient(creds.GwURL, creds.APIKey, loadCAPath())
	events, err := gw.GetRoutingEvents(traceID)
	if err != nil {
		return fmt.Errorf("routingctl: %w", err)
	}

	fmt.Printf("Routing Replay\n")
	fmt.Printf("  trace_id: %s\n\n", traceID)
	if len(events) == 0 {
		fmt.Println("  (no events found)")
		return nil
	}
	fmt.Printf("  %-8s %-12s %-24s %s\n", "attempt", "pool", "endpoint_id", "model")
	for _, ev := range events {
		epID, model := parseMemberJSON(ev.MemberJSON)
		fmt.Printf("  %-8d %-12s %-24s %s\n", ev.AttemptNo, truncate(ev.Pool, 12), truncate(epID, 24), truncate(model, 30))
	}
	return nil
}

// parseMemberJSON extracts endpoint_id and model from a routing event's member JSON.
// Falls back to the raw JSON if parsing fails.
func parseMemberJSON(raw string) (endpointID, model string) {
	epID, okEP := extractJSONString(raw, "endpoint_id")
	if !okEP {
		epID, okEP = extractJSONString(raw, "EndpointID")
	}
	m, okModel := extractJSONString(raw, "model")
	if !okModel {
		m, okModel = extractJSONString(raw, "Model")
	}
	if !okEP && !okModel {
		return raw, ""
	}
	return epID, m
}

func extractJSONString(raw, key string) (string, bool) {
	search := `"` + key + `"`
	idx := -1
	for i := 0; i <= len(raw)-len(search); i++ {
		if raw[i:i+len(search)] == search {
			idx = i + len(search)
			break
		}
	}
	if idx < 0 {
		return "", false
	}
	// Skip whitespace and colon.
	for idx < len(raw) && (raw[idx] == ' ' || raw[idx] == ':' || raw[idx] == '\t') {
		idx++
	}
	if idx >= len(raw) || raw[idx] != '"' {
		return "", false
	}
	idx++ // skip opening quote
	end := idx
	for end < len(raw) && raw[end] != '"' {
		if raw[end] == '\\' {
			end += 2
		} else {
			end++
		}
	}
	return raw[idx:end], true
}

func loadCAPath() string {
	cfg, err := localconfig.LoadConfig()
	if err != nil {
		return ""
	}
	return cfg.CAPath
}

// adminInviteFlags holds the parsed flags for `aicg-lp admin invite`.
type adminInviteFlags struct {
	role string
	team string
	user string
	ttl  time.Duration
}

// parseAdminInviteFlags parses admin-invite args. --role, --team, --user are
// required. --ttl defaults to 24h and must be in (0, 720h].
func parseAdminInviteFlags(args []string) (adminInviteFlags, error) {
	f := adminInviteFlags{ttl: 24 * time.Hour}
	ttlSet := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--role":
			if i+1 >= len(args) {
				return adminInviteFlags{}, fmt.Errorf("--role requires a value")
			}
			f.role = args[i+1]
			i++
		case "--team":
			if i+1 >= len(args) {
				return adminInviteFlags{}, fmt.Errorf("--team requires a value")
			}
			f.team = args[i+1]
			i++
		case "--user":
			if i+1 >= len(args) {
				return adminInviteFlags{}, fmt.Errorf("--user requires a value")
			}
			f.user = args[i+1]
			i++
		case "--ttl":
			if i+1 >= len(args) {
				return adminInviteFlags{}, fmt.Errorf("--ttl requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return adminInviteFlags{}, fmt.Errorf("--ttl: %w", err)
			}
			f.ttl = d
			ttlSet = true
			i++
		default:
			return adminInviteFlags{}, fmt.Errorf("unknown flag: %s", args[i])
		}
	}
	if f.role == "" {
		return adminInviteFlags{}, fmt.Errorf("--role is required")
	}
	if f.team == "" {
		return adminInviteFlags{}, fmt.Errorf("--team is required")
	}
	if f.user == "" {
		return adminInviteFlags{}, fmt.Errorf("--user is required")
	}
	if ttlSet {
		if f.ttl <= 0 {
			return adminInviteFlags{}, fmt.Errorf("--ttl must be > 0")
		}
		if f.ttl > 720*time.Hour {
			return adminInviteFlags{}, fmt.Errorf("--ttl must be <= 720h")
		}
	}
	return f, nil
}

// inviteCreator is swapped by tests so runAdminInvite can be exercised without
// a live GW. Production binds it to a real gwclient.Client at call time.
var inviteCreator = func(gwURL, apiKey, caPath, userID, teamID, role string, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	c := gwclient.NewClient(gwURL, apiKey, caPath)
	return c.CreateInvite(userID, teamID, role, ttl)
}

func runAdminInvite(args []string) error {
	f, err := parseAdminInviteFlags(args)
	if err != nil {
		return err
	}
	creds, err := localconfig.LoadCredentials()
	if err != nil {
		return fmt.Errorf("load credentials: %w (run `aicg login` first)", err)
	}
	token, expiresAt, err := inviteCreator(creds.GwURL, creds.APIKey, loadCAPath(), f.user, f.team, f.role, f.ttl)
	if err != nil {
		return fmt.Errorf("create invite: %w", err)
	}
	fmt.Printf("Invite created. Token (single use, expires %s):\n%s\n", expiresAt.Format(time.RFC3339), token)
	return nil
}

// runAdmin dispatches `aicg-lp admin <subcommand> [args...]` to the matching
// admin-namespaced handler. Subcommands live alongside top-level commands in
// the registry but are addressed via the `admin` prefix to keep the operator
// surface narrow.
func runAdmin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aicg admin <subcommand> [args...]\n  invite  Mint a one-shot invite token (platform_admin only)")
	}
	switch args[0] {
	case "invite":
		return runAdminInvite(args[1:])
	default:
		return fmt.Errorf("unknown admin subcommand: %s", args[0])
	}
}
