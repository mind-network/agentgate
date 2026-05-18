// Package cli implements the `aicg` command-line interface.
package cli

import (
	"fmt"
	"os"
)

// Command represents a CLI subcommand.
type Command struct {
	Name     string
	Usage    string
	Run      func(args []string) error
}

// Registry holds all registered subcommands.
var Registry = map[string]*Command{}

func init() {
	Register("version", "Show LP version", runVersion)
	Register("login", "Authenticate to the gateway", runLogin)
	Register("start", "Start the LP daemon", runStart)
	Register("status", "Show local trace summary", runStatus)
	Register("bind-repo", "Bind current repo to a gateway repo_id", runBindRepo)
	Register("doctor", "Diagnose LP configuration and connectivity", runDoctor)
	Register("env", "Print shell environment snippet", runEnv)
	Register("stats", "Query cost summary from gateway", runStats)
	Register("policyctl", "Validate a policy file locally", runPolicyCtl)
	Register("routingctl", "Replay a routing decision", runRoutingCtl)
	Register("admin", "Platform-admin operations (invite ...)", runAdmin)
}

// Register adds a command to the registry.
func Register(name, usage string, run func(args []string) error) {
	Registry[name] = &Command{Name: name, Usage: usage, Run: run}
}

// Dispatch runs the named command with the given args.
func Dispatch(args []string) error {
	if len(args) < 2 {
		printUsage()
		return fmt.Errorf("no command specified")
	}
	name := args[1]
	cmd, ok := Registry[name]
	if !ok {
		printUsage()
		return fmt.Errorf("unknown command: %s", name)
	}
	return cmd.Run(args[2:])
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: aicg <command> [args...]\n\nCommands:\n")
	for name, cmd := range Registry {
		fmt.Fprintf(os.Stderr, "  %-12s %s\n", name, cmd.Usage)
	}
}
