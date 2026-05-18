package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/google/uuid"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/auth"
	"agentgate/internal/gw/budget"
	"agentgate/internal/gw/config"
	"agentgate/internal/gw/cost"
	"agentgate/internal/gw/dashboard"
	"agentgate/internal/gw/db"
	"agentgate/internal/gw/edge"
	"agentgate/internal/gw/policy"
	"agentgate/internal/gw/provider"
	"agentgate/internal/gw/rawstore"
	"agentgate/internal/gw/routing"
	"agentgate/internal/gw/server"
	"agentgate/internal/shared/version"
)

var (
	teamRegistryMu sync.RWMutex
	teamRegistry   = map[string]struct{}{}
)

// rebuildTeamRegistry refreshes the in-process team allowlist from a config
// snapshot. Called once at startup and again from the hot-reload callback.
func rebuildTeamRegistry(c *config.Config) {
	teamRegistryMu.Lock()
	defer teamRegistryMu.Unlock()
	next := make(map[string]struct{}, len(c.Identity.Teams))
	for _, t := range c.Identity.Teams {
		next[t.TeamID] = struct{}{}
	}
	teamRegistry = next
}

func main() {
	slog.Info("starting agentgate gateway", "version", version.String())
	publicBaseURL := envDefault("AICG_PUBLIC_BASE_URL", "https://localhost:8443")

	// --- Config (must load before DB so bare-container deploys fail fast
	//     with a clear "config not found" message) ---
	cfgLoader := &config.Loader{
		PoolsPath:   configPath("AICG_CONFIG_POOLS", "pools.yaml"),
		PolicyPath:  configPath("AICG_CONFIG_POLICY", "policies/main.yaml"),
		PricingPath: configPath("AICG_CONFIG_PRICING", "pricing.yaml"),
		UsersPath:   configPath("AICG_CONFIG_USERS", "identity/users.yaml"),
		TeamsPath:   configPath("AICG_CONFIG_TEAMS", "identity/teams.yaml"),
		ReposPath:   configPath("AICG_CONFIG_REPOS", "identity/repos.yaml"),
	}
	cfg, err := cfgLoader.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "pools", len(cfg.Pools.ProviderEndpoints), "rules", len(cfg.Policy.Rules))

	// --- DB ---
	dbDSN := buildDSN()
	ctx := context.Background()
	pool, err := db.Open(ctx, dbDSN)
	if err != nil {
		slog.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, db.Migrations, "migrations"); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}

	// --- Policy Engine ---
	pol, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		slog.Error("policy engine init failed", "error", err)
		os.Exit(1)
	}

	// --- Routing ---
	sel := routing.NewSelector(cfg.Pools, cfg.Pricing)

	// --- Hot Reload (pools + policy) ---
	// Updates pricing on the selector after a successful config reload.
	// Full policy engine hot-swap is a follow-up; the reload already validates
	// the new config atomically (retaining old on failure).
	hotReloader := config.NewHotReloader(cfg, cfgLoader)
	hotReloader.OnReload(func(newCfg *config.Config) {
		sel.SetPricing(newCfg.Pricing)
		slog.Info("hot reload: routing pricing updated")
	})
	if err := hotReloader.Start(); err != nil {
		slog.Warn("hot reload not available, continuing with static config", "error", err)
	} else {
		defer hotReloader.Stop()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGHUP)
		go func() {
			for range sigCh {
				slog.Info("SIGHUP received, triggering manual config reload")
				hotReloader.TriggerReload()
			}
		}()
	}

	// --- Provider Adapters ---
	anthropicAdapter := provider.NewAnthropicAdapter()
	openAIAdapter := provider.NewOpenAICompatAdapter()

	wireAdapters := map[string]provider.Adapter{
		"anthropic":        anthropicAdapter,
		"anthropic_compat": provider.NewAnthropicCompatAdapter(),
		"openai":           openAIAdapter,
		"openai_compat":    openAIAdapter,
	}

	// --- Pg Writers ---
	pgAuditW := audit.NewPgWriter(pool)
	pgCostW := cost.NewPgWriter(pool)
	pgRoutingW := routing.NewEventWriter(pool)
	pgRawstoreW := rawstore.NewMetadataWriter(pool)
	pgBudgetSvc := budget.NewPgService(pool)

	_ = pgCostW
	_ = pgRoutingW
	_ = pgRawstoreW

	// --- Egress ---
	costCalc := cost.NewCalculator(cfg.Pricing)
	auditW := audit.NewWriter() // in-memory for egress internal use
	egress := server.NewEgressPipeline(wireAdapters, costCalc, auditW, sel)

	// --- Ingress ---
	pipeline := server.NewPipeline(pol, sel, egress, auditW, costCalc, cfg)
	pipeline.PgAuditW = pgAuditW
	pipeline.PgCostW = pgCostW
	pipeline.PgRoutingW = pgRoutingW
	pipeline.PgRawstoreW = pgRawstoreW
	pipeline.PgBudgetSvc = pgBudgetSvc

	// --- Auth ---
	pgKeyStore := auth.NewPgKeyStore(pool)
	pgSetupTokens := auth.NewPgSetupTokenStore(pool)
	pgInvites := auth.NewPgInviteStore(pool)

	// Bootstrap: if no API keys exist in DB, mint a setup_token.
	var setupToken *auth.SetupToken
	if !pgKeyStore.HasAnyKey() && !pgSetupTokens.HasAnyToken(ctx) {
		token := auth.GenerateSetupToken()
		tokenHash := sha256Hex(token)
		if err := pgSetupTokens.InsertToken(ctx, tokenHash); err != nil {
			slog.Error("failed to insert setup token", "error", err)
			os.Exit(1)
		}
		setupToken = &auth.SetupToken{Token: token}

		fmt.Fprintf(os.Stderr, "\n===== AGENTGATE BOOTSTRAP =====\n")
		fmt.Fprintf(os.Stderr, "No API keys found. Use this setup_token once:\n\n")
		fmt.Fprintf(os.Stderr, "  %s\n\n", token)
		fmt.Fprintf(os.Stderr, "Run: aicg login --setup=%s --gateway=%s\n", token, publicBaseURL)
		fmt.Fprintf(os.Stderr, "================================\n\n")

		tokenPath := os.Getenv("AICG_SETUP_TOKEN_PATH")
		if tokenPath == "" {
			// /tmp is always writable in distroless containers.
			tokenPath = "/tmp/aicg-setup-token"
		}
		if err := os.MkdirAll(filepath.Dir(tokenPath), 0700); err != nil {
			slog.Warn("cannot create token directory", "error", err)
		} else if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
			slog.Warn("cannot write setup_token file", "error", err)
		} else {
			slog.Info("setup_token written", "path", tokenPath)
		}
	}

	// --- Budget Settler (Pg-backed) ---
	for _, team := range cfg.Identity.Teams {
		pgBudgetSvc.SetTeamCap(team.TeamID, team.BudgetMonthlyCapCents)
	}

	// Extend OnReload to also refresh team budget caps when identity YAML changes.
	hotReloader.OnReload(func(newCfg *config.Config) {
		sel.SetPricing(newCfg.Pricing)
		sel.SetPools(newCfg.Pools)
		costCalc.SetPricing(newCfg.Pricing)
		if err := pol.SetPolicy(newCfg.Policy); err != nil {
			slog.Warn("hot reload: policy recompile failed, retaining old rules", "error", err)
		}
		for _, team := range newCfg.Identity.Teams {
			pgBudgetSvc.SetTeamCap(team.TeamID, team.BudgetMonthlyCapCents)
		}
		rebuildTeamRegistry(newCfg)
		pipeline.SetConfig(newCfg)
		slog.Info("hot reload: live config snapshot updated",
			"pools", len(newCfg.Pools.ProviderEndpoints),
			"rules", len(newCfg.Policy.Rules),
			"teams", len(newCfg.Identity.Teams),
		)
	})

	pgBudgetSvc.OnAlert(func(teamID string, usedCents, capCents int) {
		_ = pgAuditW.AlertBudget(ctx, teamID, usedCents, capCents)
	})
	settler := budget.NewSettler(pgBudgetSvc)
	settler.OnAudit(func(ids []uuid.UUID) {
		strIDs := make([]string, len(ids))
		for i, id := range ids {
			strIDs[i] = id.String()
		}
		auditW.AlertReservationExpired(strIDs)
	})
	settler.Start(1*time.Minute, 10*time.Minute)
	slog.Info("budget settler started")

	// --- Server ---
	srv := edge.NewServer(envDefault("AICG_LISTEN_ADDR", ":8443"))
	if cert := os.Getenv("AICG_TLS_CERT"); cert != "" {
		srv.TLSCert = cert
	}
	if key := os.Getenv("AICG_TLS_KEY"); key != "" {
		srv.TLSKey = key
	}

	// Auth middleware — skips /healthz, /metrics, and /api/v1/lp/exchange-setup internally.
	srv.Router.Use(auth.AuthMiddleware(pgKeyStore))

	// Metrics endpoint — unauthenticated.
	srv.Router.Get("/metrics", func(w http.ResponseWriter, r *http.Request) {
		promhttp.Handler().ServeHTTP(w, r)
	})

	// Health check — after auth middleware (auth skips /healthz path).
	srv.SetHealthChecker(func(ctx context.Context) map[string]string {
		status := map[string]string{}
		if err := pool.Ping(ctx); err != nil {
			status["db"] = "fail"
		} else {
			status["db"] = "ok"
		}
		return status
	})

	// Handlers.
	handler := server.NewHandler(pipeline, pgKeyStore, setupToken)
	handler.PgKeyStore = pgKeyStore
	handler.PgSetupTokens = pgSetupTokens
	handler.PgInvites = pgInvites
	handler.PgAuditW = pgAuditW
	handler.TeamRegistered = func(teamID string) bool {
		// Closure reads the live identity snapshot maintained by the
		// hot-reload callback below. We seed it with the initial config
		// and refresh on each reload.
		teamRegistryMu.RLock()
		defer teamRegistryMu.RUnlock()
		_, ok := teamRegistry[teamID]
		return ok
	}
	rebuildTeamRegistry(cfg)

	// Route registration.
	srv.Router.Post("/v1/agent/forward", handler.HandleForward)
	srv.Router.Post("/api/v1/lp/exchange-setup", handler.HandleExchangeSetup)
	srv.Router.Post("/api/v1/lp/exchange-invite", handler.HandleExchangeInvite)
	srv.Router.With(auth.RequireRole(auth.RolePlatformAdmin)).
		Post("/api/v1/admin/invites", handler.HandleAdminCreateInvite)
	srv.Router.Post("/v1/repo/bind", handler.HandleBindRepo)

	// Dashboard routes — real DB queries.
	srv.Router.Get("/api/v1/cost/summary", dashboard.NewCostSummaryHandler(pool).ServeHTTP)

	srv.Router.Get("/api/v1/routing/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		traceID := r.URL.Query().Get("trace_id")
		if traceID == "" {
			_, _ = fmt.Fprint(w, "[]")
			return
		}
		rows, err := pool.Query(r.Context(),
			`SELECT trace_id::text, attempt_no, COALESCE(pool_selected,''), COALESCE(member_selected::text,'')
			 FROM routing_event WHERE trace_id::text = $1 LIMIT 50`, traceID)
		if err != nil {
			_, _ = fmt.Fprint(w, "[]")
			return
		}
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var tid string
			var attempt int
			var pool, member string
			if err := rows.Scan(&tid, &attempt, &pool, &member); err != nil {
				continue
			}
			out = append(out, map[string]any{
				"trace_id": tid, "attempt_no": attempt,
				"pool_selected": pool, "member_selected": member,
			})
		}
		if out == nil {
			_, _ = fmt.Fprint(w, "[]")
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	// --- Startup validation: every pool member's provider has a registered adapter ---
	adapterNames := make([]string, 0, len(wireAdapters))
	for k := range wireAdapters {
		adapterNames = append(adapterNames, k)
	}
	if err := sel.ValidatePoolAdapters(adapterNames); err != nil {
		slog.Error("pool adapter validation failed", "error", err)
		os.Exit(1)
	}

	slog.Info("listening", "addr", srv.Addr)
	if srv.TLSCert != "" {
		slog.Info("TLS 1.3 enabled")
	}
	if err := srv.Start(); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

func buildDSN() string {
	host := envDefault("AGENTGATE_DB_HOST", "localhost")
	port := envDefault("AGENTGATE_DB_PORT", "5432")
	user := envDefault("AGENTGATE_DB_USER", "agentgate")
	pass := envDefault("AGENTGATE_DB_PASSWORD", "agentgate")
	name := envDefault("AGENTGATE_DB_NAME", "agentgate")
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, pass, host, port, name)
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// configPath resolves a config file path with three-tier precedence:
//  1. Per-file env var (e.g. AICG_CONFIG_POOLS) — highest priority
//  2. AICG_CONFIG_DIR umbrella — if set and per-file var is unset
//  3. Hardcoded default (e.g. /etc/agentgate/pools.yaml) — lowest
func configPath(envKey, fileName string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if dir := os.Getenv("AICG_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, fileName)
	}
	return filepath.Join("/etc/agentgate", fileName)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
