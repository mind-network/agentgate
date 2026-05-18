// Package session generates and manages LP session identity.
package session

import (
	"crypto/sha256"
	"fmt"
	"os"
	"time"
)

// Session holds the stable session identity for one LP process lifetime.
type Session struct {
	SessionID string
	PID       int
	StartedAt time.Time
}

// New creates a new Session. SessionID = sha256(agent_pid + start_ts + repo).
// repo may be empty if not in a git working tree.
func New(repo string) *Session {
	pid := os.Getpid()
	startedAt := time.Now()
	input := fmt.Sprintf("%d:%d:%s", pid, startedAt.Unix(), repo)
	hash := sha256.Sum256([]byte(input))
	return &Session{
		SessionID: fmt.Sprintf("%x", hash),
		PID:       pid,
		StartedAt: startedAt,
	}
}
