package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPath(t *testing.T) {
	// Save and restore env.
	restore := func(keys ...string) func() {
		prev := make(map[string]string)
		for _, k := range keys {
			prev[k] = os.Getenv(k)
		}
		return func() {
			for k, v := range prev {
				if v == "" {
					_ = os.Unsetenv(k)
				} else {
					_ = os.Setenv(k, v)
				}
			}
		}
	}

	t.Run("per-file env set wins over everything", func(t *testing.T) {
		defer restore("AICG_CONFIG_POOLS", "AICG_CONFIG_DIR")()
		_ = os.Setenv("AICG_CONFIG_POOLS", "/custom/pools.yaml")
		_ = os.Setenv("AICG_CONFIG_DIR", "/should-not-be-used")
		got := configPath("AICG_CONFIG_POOLS", "pools.yaml")
		if got != "/custom/pools.yaml" {
			t.Errorf("got %q, want /custom/pools.yaml", got)
		}
	})

	t.Run("AICG_CONFIG_DIR umbrella when per-file unset", func(t *testing.T) {
		defer restore("AICG_CONFIG_POOLS", "AICG_CONFIG_DIR")()
		_ = os.Unsetenv("AICG_CONFIG_POOLS")
		_ = os.Setenv("AICG_CONFIG_DIR", "/opt/configs")
		got := configPath("AICG_CONFIG_POOLS", "pools.yaml")
		want := filepath.Join("/opt/configs", "pools.yaml")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("neither set falls back to /etc/agentgate", func(t *testing.T) {
		defer restore("AICG_CONFIG_POOLS", "AICG_CONFIG_DIR")()
		_ = os.Unsetenv("AICG_CONFIG_POOLS")
		_ = os.Unsetenv("AICG_CONFIG_DIR")
		got := configPath("AICG_CONFIG_POOLS", "pools.yaml")
		want := filepath.Join("/etc/agentgate", "pools.yaml")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("per-file env unset, AICG_CONFIG_DIR unset, uses default for all 6 files", func(t *testing.T) {
		defer restore(
			"AICG_CONFIG_POOLS", "AICG_CONFIG_PRICING", "AICG_CONFIG_POLICY",
			"AICG_CONFIG_USERS", "AICG_CONFIG_TEAMS", "AICG_CONFIG_REPOS",
			"AICG_CONFIG_DIR",
		)()
		_ = os.Unsetenv("AICG_CONFIG_POOLS")
		_ = os.Unsetenv("AICG_CONFIG_PRICING")
		_ = os.Unsetenv("AICG_CONFIG_POLICY")
		_ = os.Unsetenv("AICG_CONFIG_USERS")
		_ = os.Unsetenv("AICG_CONFIG_TEAMS")
		_ = os.Unsetenv("AICG_CONFIG_REPOS")
		_ = os.Unsetenv("AICG_CONFIG_DIR")

		cases := []struct {
			envKey, fileName string
		}{
			{"AICG_CONFIG_POOLS", "pools.yaml"},
			{"AICG_CONFIG_PRICING", "pricing.yaml"},
			{"AICG_CONFIG_POLICY", "policies/main.yaml"},
			{"AICG_CONFIG_USERS", "identity/users.yaml"},
			{"AICG_CONFIG_TEAMS", "identity/teams.yaml"},
			{"AICG_CONFIG_REPOS", "identity/repos.yaml"},
		}
		for _, c := range cases {
			got := configPath(c.envKey, c.fileName)
			want := filepath.Join("/etc/agentgate", c.fileName)
			if got != want {
				t.Errorf("configPath(%q, %q) = %q, want %q", c.envKey, c.fileName, got, want)
			}
		}
	})
}
