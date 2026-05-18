package db

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestOpenRetryExhaustion(t *testing.T) {
	// Bind a listener on a random port, then close it immediately so the
	// port is free but noone is listening — pgx will get a connection refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Set a short retry window so the test doesn't take 30s.
	orig := DefaultRetryMax
	DefaultRetryMax = 2 * time.Second
	defer func() { DefaultRetryMax = orig }()

	dsn := "postgres://nobody:nobody@" + addr + "/none?connect_timeout=1"
	_, err = Open(ctx, dsn)
	if err == nil {
		t.Fatal("expected error when connecting to closed port")
	}
	t.Logf("got expected error: %v", err)
}

func TestOpenBadDSN(t *testing.T) {
	ctx := context.Background()
	_, err := Open(ctx, "not a valid uri://")
	if err == nil {
		t.Fatal("expected error for invalid DSN")
	}
	t.Logf("got expected error: %v", err)
}
