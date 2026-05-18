package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadIdentity loads all identity YAML files.
func LoadIdentity(usersPath, teamsPath, reposPath string) (*IdentityConfig, error) {
	cfg := &IdentityConfig{}

	users, err := loadUsers(usersPath)
	if err != nil {
		return nil, err
	}
	cfg.Users = users

	teams, err := loadTeams(teamsPath)
	if err != nil {
		return nil, err
	}
	cfg.Teams = teams

	repos, err := loadRepos(reposPath)
	if err != nil {
		return nil, err
	}
	cfg.Repos = repos

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate identity: %w", err)
	}
	return cfg, nil
}

func loadUsers(path string) ([]UserIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read users.yaml: %w", err)
	}
	var wrapper struct {
		Users []UserIdentity `yaml:"users"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parse users.yaml: %w", err)
	}
	return wrapper.Users, nil
}

func loadTeams(path string) ([]TeamIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read teams.yaml: %w", err)
	}
	var wrapper struct {
		Teams []TeamIdentity `yaml:"teams"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parse teams.yaml: %w", err)
	}
	return wrapper.Teams, nil
}

func loadRepos(path string) ([]RepoIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read repos.yaml: %w", err)
	}
	var wrapper struct {
		Repos []RepoIdentity `yaml:"repos"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parse repos.yaml: %w", err)
	}
	return wrapper.Repos, nil
}

// Validate checks identity configuration consistency.
func (c *IdentityConfig) Validate() error {
	teamIDs := make(map[string]bool)
	for _, t := range c.Teams {
		teamIDs[t.TeamID] = true
	}
	for _, u := range c.Users {
		if u.UserID == "" {
			return fmt.Errorf("user entry missing user_id")
		}
		if u.Role == "" {
			return fmt.Errorf("user %q missing role", u.UserID)
		}
		if u.TeamID != "" && !teamIDs[u.TeamID] {
			return fmt.Errorf("user %q references unknown team_id %q", u.UserID, u.TeamID)
		}
	}
	for _, r := range c.Repos {
		if r.RepoID == "" {
			return fmt.Errorf("repo entry missing repo_id")
		}
	}
	return nil
}
