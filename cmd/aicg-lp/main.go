package main

import (
	"fmt"
	"os"

	"agentgate/internal/lp/cli"
)

func main() {
	if err := cli.Dispatch(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "aicg: %v\n", err)
		os.Exit(1)
	}
}
