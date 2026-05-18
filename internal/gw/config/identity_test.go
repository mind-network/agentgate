package config

import (
	"strings"
	"testing"
)

func TestLoadIdentitySuccess(t *testing.T) {
	usersPath := writeTemp(t, "users.yaml", validUsersYAML)
	teamsPath := writeTemp(t, "teams.yaml", validTeamsYAML)
	reposPath := writeTemp(t, "repos.yaml", validReposYAML)

	cfg, err := LoadIdentity(usersPath, teamsPath, reposPath)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(cfg.Users) != 2 {
		t.Errorf("expected 2 users, got %d", len(cfg.Users))
	}
	if len(cfg.Teams) != 2 {
		t.Errorf("expected 2 teams, got %d", len(cfg.Teams))
	}
	if len(cfg.Repos) != 2 {
		t.Errorf("expected 2 repos, got %d", len(cfg.Repos))
	}
}

func TestLoadIdentityUnknownTeam(t *testing.T) {
	usersPath := writeTemp(t, "users.yaml", `
users:
  - user_id: u1
    team_id: nonexistent-team
    role: developer
    api_key_hash: ""
`)
	teamsPath := writeTemp(t, "teams.yaml", `
teams:
  - team_id: platform
    name: Platform
    budget_monthly_cap_cents: 0
`)
	reposPath := writeTemp(t, "repos.yaml", "repos: []\n")

	_, err := LoadIdentity(usersPath, teamsPath, reposPath)
	if err == nil {
		t.Fatal("expected error for unknown team reference, got nil")
	}
	if !strings.Contains(err.Error(), "unknown team_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

const validUsersYAML = `
users:
  - user_id: admin
    team_id: platform
    role: platform_admin
    api_key_hash: ""
  - user_id: dev-1
    team_id: dogfood
    role: developer
    api_key_hash: ""
`

const validTeamsYAML = `
teams:
  - team_id: platform
    name: Platform Engineering
    budget_monthly_cap_cents: 0
  - team_id: dogfood
    name: Dogfood Team
    budget_monthly_cap_cents: 500000
`

const validReposYAML = `
repos:
  - repo_id: agentgate
    remote_url: "https://github.com/org/agentgate"
    default_team_id: platform
    restricted: false
  - repo_id: dogfood-service
    remote_url: "https://github.com/org/dogfood-service"
    default_team_id: dogfood
    restricted: false
`
