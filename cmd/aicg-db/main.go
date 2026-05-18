package main

import (
	"context"
	"fmt"
	"os"

	"agentgate/internal/gw/db"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: aicg-db <up|down> [dsn]\n")
		os.Exit(1)
	}

	action := os.Args[1]
	dsn := os.Getenv("AGENTGATE_DSN")
	if dsn == "" {
		dsn = "postgres://agentgate:agentgate@localhost:5432/agentgate?sslmode=disable"
	}
	if len(os.Args) > 2 {
		dsn = os.Args[2]
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db open: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	switch action {
	case "up":
		if err := db.Migrate(ctx, pool, db.Migrations, "migrations"); err != nil {
			fmt.Fprintf(os.Stderr, "migrate up: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("migrate up: ok")
	case "down":
		steps := 0
		if err := db.MigrateDown(ctx, pool, db.Migrations, "migrations", steps); err != nil {
			fmt.Fprintf(os.Stderr, "migrate down: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("migrate down: ok")
	default:
		fmt.Fprintf(os.Stderr, "unknown action: %s\n", action)
		os.Exit(1)
	}
}
