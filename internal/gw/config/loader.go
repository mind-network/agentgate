package config

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"agentgate/internal/gw/metrics"
	"agentgate/internal/gw/secretref"
)

// Loader performs one-shot config loading and validates all sections.
type Loader struct {
	PoolsPath   string
	PolicyPath  string
	PricingPath string
	UsersPath   string
	TeamsPath   string
	ReposPath   string
}

// Load reads and validates all config files.
func (l *Loader) Load() (*Config, error) {
	pools, err := LoadPools(l.PoolsPath)
	if err != nil {
		return nil, err
	}
	policy, err := LoadPolicy(l.PolicyPath)
	if err != nil {
		return nil, err
	}
	pricing, err := LoadPricing(l.PricingPath)
	if err != nil {
		return nil, err
	}
	pricing.DefaultCurrencies()
	identity, err := LoadIdentity(l.UsersPath, l.TeamsPath, l.ReposPath)
	if err != nil {
		return nil, err
	}
	// Cross-config consistency: policy model_pool refs must exist in pools.
	if err := ValidatePolicyAgainstPools(policy, pools); err != nil {
		return nil, err
	}

	// Cross-config consistency: endpoints declare vendors that have pricing rows.
	if err := ValidateEndpointsAgainstPricing(pools, pricing); err != nil {
		return nil, err
	}

	// Resolve provider endpoint key_refs.
	resolved, err := resolveKeys(pools)
	if err != nil {
		return nil, err
	}

	return &Config{
		Pools:             pools,
		Policy:            policy,
		Pricing:           pricing,
		Identity:          identity,
		ResolvedEndpoints: resolved,
	}, nil
}

// resolveKeys resolves all provider_endpoints[*].key_ref into EndpointWithKey.
func resolveKeys(pools *PoolsConfig) (map[string]EndpointWithKey, error) {
	out := make(map[string]EndpointWithKey, len(pools.ProviderEndpoints))
	for id, ep := range pools.ProviderEndpoints {
		epCopy := ep
		if epCopy.KeyRef == "" {
			out[id] = EndpointWithKey{ProviderEndpoint: &epCopy}
			continue
		}
		key, err := secretref.Resolve(epCopy.KeyRef)
		if err != nil {
			return nil, fmt.Errorf("provider_endpoints.%s: %w", id, err)
		}
		out[id] = EndpointWithKey{ProviderEndpoint: &epCopy, ResolvedKey: key}
	}
	return out, nil
}

// HotReloader watches config and secret files for changes and atomically
// swaps to a new Config via the provided callback. Reload failure retains
// the old configuration.
type HotReloader struct {
	mu            sync.RWMutex
	current       *Config
	loader        *Loader
	onReload      func(*Config)
	watcher       *fsnotify.Watcher
	secretPaths   map[string]bool // file:// paths currently watched
	wantedFiles   map[string]bool // config files to accept events for (abs path -> true)
	pendingFiles  map[string]bool // files that triggered the current reload batch
	pendingMu     sync.Mutex
	manualTrigger chan struct{} // buffered (1); signals loop() to reload
	done          chan struct{}
}

// NewHotReloader creates a HotReloader with an initial config.
func NewHotReloader(initial *Config, loader *Loader) *HotReloader {
	return &HotReloader{
		current:       initial,
		loader:        loader,
		secretPaths:   make(map[string]bool),
		pendingFiles:  make(map[string]bool),
		manualTrigger: make(chan struct{}, 1),
		done:          make(chan struct{}),
	}
}

// fileRefPaths collects unique absolute file paths from file:// key_refs.
func fileRefPaths(endpoints map[string]ProviderEndpoint) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, ep := range endpoints {
		if !strings.HasPrefix(ep.KeyRef, "file://") {
			continue
		}
		p := strings.TrimPrefix(ep.KeyRef, "file://")
		if seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	return paths
}

// OnReload registers a callback invoked after each successful reload.
func (h *HotReloader) OnReload(fn func(*Config)) {
	h.onReload = fn
}

// Start begins watching config directories for changes.
func (h *HotReloader) Start() error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	h.watcher = w

	files := []string{
		h.loader.PoolsPath,
		h.loader.PolicyPath,
		h.loader.PricingPath,
		h.loader.UsersPath,
		h.loader.TeamsPath,
		h.loader.ReposPath,
	}

	// Build wantedFiles index keyed by clean absolute path.
	h.wantedFiles = make(map[string]bool, len(files))
	dirs := make(map[string]bool)
	for _, f := range files {
		h.wantedFiles[filepath.Clean(f)] = true
		dirs[filepath.Dir(f)] = true
	}

	// Watch parent directories instead of individual files so that
	// atomic rename-over edits (vim, sed -i, mv tmp target) are visible.
	for d := range dirs {
		if err := w.Add(d); err != nil {
			_ = w.Close()
			return fmt.Errorf("watch dir %s: %w", d, err)
		}
	}

	// Watch initial file:// secret paths (per-file, unchanged).
	for _, p := range fileRefPaths(h.current.Pools.ProviderEndpoints) {
		if err := w.Add(p); err != nil {
			slog.Warn("cannot watch secret file, will retry on next reload", "path", p, "error", err)
			continue
		}
		h.secretPaths[p] = true
	}

	go h.loop()
	return nil
}

// Stop shuts down the hot reload watcher.
func (h *HotReloader) Stop() {
	select {
	case <-h.done:
		// Already stopped
	default:
		close(h.done)
	}
	if h.watcher != nil {
		_ = h.watcher.Close()
	}
}

// TriggerReload queues a manual reload via the <sighup> sentinel so the
// caller (typically a SIGHUP handler) can request a config refresh that
// flows through the same single-goroutine reload path as file events.
func (h *HotReloader) TriggerReload() {
	h.pendingMu.Lock()
	h.pendingFiles["<sighup>"] = true
	h.pendingMu.Unlock()

	select {
	case h.manualTrigger <- struct{}{}:
	default:
		// A manual trigger is already pending; debounce will coalesce.
	}
}

// Current returns the current config (atomic read).
func (h *HotReloader) Current() *Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.current
}

func (h *HotReloader) loop() {
	// Debounce: batch rapid successive events within 200ms.
	var debounce *time.Timer
	for {
		select {
		case <-h.done:
			if debounce != nil {
				debounce.Stop()
			}
			return
		case event, ok := <-h.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			// Filter: only accept events for configured config files
			// or per-file watched secret paths.
			if !h.wantedFiles[filepath.Clean(event.Name)] && !h.secretPaths[event.Name] {
				continue
			}
			// Track which file triggered this batch.
			h.pendingMu.Lock()
			h.pendingFiles[event.Name] = true
			h.pendingMu.Unlock()

			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(200*time.Millisecond, func() {
				h.reload()
			})
		case <-h.manualTrigger:
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(200*time.Millisecond, func() {
				h.reload()
			})
		case err, ok := <-h.watcher.Errors:
			if !ok {
				return
			}
			slog.Warn("config hot reload watch error", "error", err)
		}
	}
}

func (h *HotReloader) reload() {
	// Snapshot and clear pending files.
	h.pendingMu.Lock()
	pending := h.pendingFiles
	h.pendingFiles = make(map[string]bool)
	h.pendingMu.Unlock()

	start := time.Now()
	newCfg, err := h.loader.Load()
	durationMs := time.Since(start).Milliseconds()

	if err != nil {
		errStr := err.Error()
		result := "parse_error"
		if strings.Contains(errStr, "validate ") {
			result = "validate_error"
		}
		// Emit per-file counters for each file in the pending batch.
		// If no pending files tracked (e.g. manual reload), use "all".
		if len(pending) == 0 {
			metrics.ConfigReloadTotal.WithLabelValues("all", result).Inc()
			slog.Warn("config reload failed, retaining old config",
				"event", "config_reload",
				"file", "all",
				"result", result,
				"error", errStr,
				"duration_ms", durationMs,
			)
		} else {
			for f := range pending {
				base := filepath.Base(f)
				metrics.ConfigReloadTotal.WithLabelValues(base, result).Inc()
				slog.Warn("config reload failed, retaining old config",
					"event", "config_reload",
					"file", base,
					"result", result,
					"error", errStr,
					"duration_ms", durationMs,
				)
			}
		}

		// If the failure is a secret resolution error, emit resolve_error
		// per the handoff contract: secret_reload_total{ref=<scheme>,result=resolve_error}.
		if strings.Contains(errStr, "secret_ref") {
			metrics.SecretReloadTotal.WithLabelValues("file", "resolve_error").Inc()
			slog.Warn("secret reload failed",
				"event", "secret_reload",
				"ref", "file",
				"result", "resolve_error",
				"error", errStr,
			)
		}
		return
	}

	h.mu.Lock()
	h.current = newCfg
	h.mu.Unlock()

	// Per-file success counters.
	if len(pending) == 0 {
		pending = map[string]bool{"all": true}
	}
	for f := range pending {
		base := filepath.Base(f)
		metrics.ConfigReloadTotal.WithLabelValues(base, "success").Inc()
	}
	slog.Info("config reload succeeded",
		"event", "config_reload",
		"result", "success",
		"duration_ms", durationMs,
	)

	// Sync file:// secret watches: add new paths, drop removed ones.
	h.syncSecretWatches(newCfg)

	// Emit secret reload counter per the handoff contract:
	//   secret_reload_total{ref=<scheme>,result=success}.
	secretRefs := fileRefPaths(newCfg.Pools.ProviderEndpoints)
	if len(secretRefs) > 0 {
		metrics.SecretReloadTotal.WithLabelValues("file", "success").Inc()
		slog.Info("secret reload succeeded",
			"event", "secret_reload",
			"ref", "file",
			"result", "success",
		)
	}

	if h.onReload != nil {
		h.onReload(newCfg)
	}
}

// syncSecretWatches updates the fsnotify watcher to match the file:// paths
// in the current config snapshot. Paths no longer referenced are removed;
// new paths are added. Failure to add a path is logged but not fatal.
func (h *HotReloader) syncSecretWatches(cfg *Config) {
	if h.watcher == nil {
		return
	}
	wanted := make(map[string]bool)
	for _, p := range fileRefPaths(cfg.Pools.ProviderEndpoints) {
		wanted[p] = true
		if h.secretPaths[p] {
			continue
		}
		if err := h.watcher.Add(p); err != nil {
			slog.Warn("cannot watch secret file", "path", p, "error", err)
			continue
		}
		h.secretPaths[p] = true
	}
	for p := range h.secretPaths {
		if wanted[p] {
			continue
		}
		_ = h.watcher.Remove(p)
		delete(h.secretPaths, p)
	}
}
