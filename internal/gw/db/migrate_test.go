package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestMigrateUpIdempotent(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available (need Docker): %v", err)
		return
	}
	defer func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate: %v", err)
		}
	}()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Run migrate up 3 times — must be idempotent.
	for i := 0; i < 3; i++ {
		if err := Migrate(ctx, pool, Migrations, "migrations"); err != nil {
			t.Fatalf("migrate up attempt %d: %v", i+1, err)
		}
	}

	// Verify all 10 tables exist (8 from 0001 + 2 from 0002).
	rows, err := pool.Query(ctx,
		`SELECT tablename FROM pg_catalog.pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}

	expected := []string{
		"api_keys", "approval_request", "audit_chain_root", "audit_event",
		"budget_reservation", "cost_event", "invites", "raw_access_audit",
		"raw_record", "routing_event", "setup_tokens",
	}

	if len(tables) < 11 {
		t.Errorf("expected at least 11 tables, got %d: %v", len(tables), tables)
	}

	for _, exp := range expected {
		found := false
		for _, tbl := range tables {
			if tbl == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing table %q", exp)
		}
	}
}

func TestCostEventSchemaPost0004(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available (need Docker): %v", err)
		return
	}
	defer func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate: %v", err)
		}
	}()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := Migrate(ctx, pool, Migrations, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// Verify cost_event columns (post-0005: 30 cols: 26 from 0004 + 4 cost breakdown columns).
	rows, err := pool.Query(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'cost_event' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}

	hasWire := false
	hasVendorID := false
	hasProvider := false
	hasCurrency := false
	for _, c := range cols {
		switch c {
		case "wire":
			hasWire = true
		case "vendor_id":
			hasVendorID = true
		case "provider":
			hasProvider = true
		case "currency":
			hasCurrency = true
		}
	}
	if !hasWire {
		t.Error("post-0004: missing wire column")
	}
	if !hasVendorID {
		t.Error("post-0004: missing vendor_id column")
	}
	if hasProvider {
		t.Error("post-0004: provider column should not exist (renamed to wire)")
	}
	if !hasCurrency {
		t.Error("post-0004: missing currency column (should survive from 0003)")
	}
	// 26 from 0004 + 4 from 0005 = 30 columns.
	if len(cols) != 30 {
		t.Errorf("post-0005: expected 30 columns, got %d: %v", len(cols), cols)
	}

	// Verify index on (vendor_id, model, event_at DESC) exists.
	idxRows, err := pool.Query(ctx,
		`SELECT indexname FROM pg_indexes WHERE tablename = 'cost_event' AND indexdef LIKE '%vendor_id%' AND indexdef LIKE '%model%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer idxRows.Close()
	var indexes []string
	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, name)
	}
	if len(indexes) == 0 {
		t.Error("post-0004: missing index on (vendor_id, model, event_at DESC)")
	}

	// Verify no stale provider index.
	provIdxRows, err := pool.Query(ctx,
		`SELECT indexname FROM pg_indexes WHERE tablename = 'cost_event' AND indexdef LIKE '%provider%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer provIdxRows.Close()
	if provIdxRows.Next() {
		t.Error("post-0004: stale provider index should not exist")
	}
}

func TestCostEventSchemaPost0005(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available (need Docker): %v", err)
		return
	}
	defer func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate: %v", err)
		}
	}()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := Migrate(ctx, pool, Migrations, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// Verify all 30 columns including the four cost breakdown columns from 0005.
	rows, err := pool.Query(ctx,
		`SELECT column_name, data_type FROM information_schema.columns WHERE table_name = 'cost_event' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	cols := make(map[string]string)
	for rows.Next() {
		var name, dtype string
		if err := rows.Scan(&name, &dtype); err != nil {
			t.Fatal(err)
		}
		cols[name] = dtype
	}

	// Existing columns must survive.
	for _, col := range []string{"event_at", "trace_id", "cost_cents", "wire", "vendor_id", "currency"} {
		if _, ok := cols[col]; !ok {
			t.Errorf("post-0005: missing existing column %q", col)
		}
	}

	// New 0005 columns must exist with INTEGER type.
	newCols := map[string]string{
		"input_cost_cents":        "integer",
		"output_cost_cents":       "integer",
		"cache_read_cost_cents":   "integer",
		"cache_create_cost_cents": "integer",
	}
	for col, expectedType := range newCols {
		dtype, ok := cols[col]
		if !ok {
			t.Errorf("post-0005: missing new column %q", col)
			continue
		}
		if dtype != expectedType {
			t.Errorf("post-0005: column %q has type %q, expected %q", col, dtype, expectedType)
		}
	}

	// Total column count must be 30 (26 from post-0004 + 4 from 0005).
	if len(cols) != 30 {
		t.Errorf("post-0005: expected 30 columns, got %d", len(cols))
	}

	// Verify existing indexes still exist.
	idxRows, err := pool.Query(ctx,
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'cost_event' ORDER BY indexname`)
	if err != nil {
		t.Fatal(err)
	}
	defer idxRows.Close()
	var idxDefs []string
	for idxRows.Next() {
		var def string
		if err := idxRows.Scan(&def); err != nil {
			t.Fatal(err)
		}
		idxDefs = append(idxDefs, def)
	}

	// Should have 6 indexes (PK + 5 user-defined from 0004 + 0005).
	if len(idxDefs) != 6 {
		t.Errorf("post-0005: expected 6 indexes, got %d", len(idxDefs))
	}

	// Verify the vendor_id index survived.
	foundVendorIdx := false
	for _, def := range idxDefs {
		// Use LIKE-style matching since index name auto-generation depends on Postgres version.
		if def == "CREATE INDEX cost_event_vendor_id_model_event_at_desc_idx ON cost_event USING btree (vendor_id, model, event_at DESC)" {
			foundVendorIdx = true
			break
		}
	}
	if !foundVendorIdx {
		// Fallback: check by column references.
		for _, def := range idxDefs {
			foundVendorIdx = foundVendorIdx || (containsAll(def, "vendor_id", "model", "event_at"))
		}
	}
	if !foundVendorIdx {
		t.Error("post-0005: missing index on (vendor_id, model, event_at DESC)")
	}
}

// containsAll reports whether s contains all substrings in subs.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestCostEventSchema0004DownRestoresPost0003(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available (need Docker): %v", err)
		return
	}
	defer func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate: %v", err)
		}
	}()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Migrate up (through the highest migration), then down enough steps to
	// undo 0004 (which renamed provider→wire). Each migration added after
	// 0004 requires one more down step here.
	if err := Migrate(ctx, pool, Migrations, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// 0006 invites + 0005 cost breakdown + 0004 vendor split = 3 steps.
	if err := MigrateDown(ctx, pool, Migrations, "migrations", 3); err != nil {
		t.Fatalf("migrate down 3 steps: %v", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'cost_event' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}

	hasProvider := false
	hasWire := false
	hasVendorID := false
	hasCurrency := false
	for _, c := range cols {
		switch c {
		case "provider":
			hasProvider = true
		case "wire":
			hasWire = true
		case "vendor_id":
			hasVendorID = true
		case "currency":
			hasCurrency = true
		}
	}
	if !hasProvider {
		t.Error("post-0004-down: provider column should be restored")
	}
	if hasWire {
		t.Error("post-0004-down: wire column should not exist")
	}
	if hasVendorID {
		t.Error("post-0004-down: vendor_id column should not exist")
	}
	if !hasCurrency {
		t.Error("post-0004-down: missing currency column")
	}

	// Verify provider index restored.
	idxRows, err := pool.Query(ctx,
		`SELECT indexname FROM pg_indexes WHERE tablename = 'cost_event' AND indexdef LIKE '%provider%' AND indexdef LIKE '%model%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer idxRows.Close()
	var indexes []string
	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, name)
	}
	if len(indexes) == 0 {
		t.Error("post-0004-down: missing index on (provider, model, event_at DESC)")
	}
}

func TestInvitesSchemaPost0006(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available (need Docker): %v", err)
		return
	}
	defer func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate: %v", err)
		}
	}()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := Migrate(ctx, pool, Migrations, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'invites' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}

	expected := []string{
		"id", "token_hash", "role", "team_id", "user_id", "created_by",
		"created_at", "expires_at", "used_at", "used_by", "machine_id",
	}
	for _, exp := range expected {
		found := false
		for _, c := range cols {
			if c == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing column %q in invites", exp)
		}
	}

	idxRows, err := pool.Query(ctx,
		`SELECT indexname FROM pg_indexes WHERE tablename = 'invites'`)
	if err != nil {
		t.Fatal(err)
	}
	defer idxRows.Close()
	var indexes []string
	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, name)
	}
	foundUnused := false
	for _, idx := range indexes {
		if idx == "invites_unused_idx" {
			foundUnused = true
		}
	}
	if !foundUnused {
		t.Errorf("missing invites_unused_idx partial index: %v", indexes)
	}

	// Role CHECK constraint must reject bad roles.
	if _, err := pool.Exec(ctx,
		`INSERT INTO invites (token_hash, role, team_id, user_id, created_by, expires_at)
		 VALUES ('badhash1', 'super_user', 'platform', 'u1', 'admin', now() + interval '1 hour')`); err == nil {
		t.Error("expected CHECK constraint to reject role='super_user', got no error")
	}

	// Token hash UNIQUE must reject duplicates.
	if _, err := pool.Exec(ctx,
		`INSERT INTO invites (token_hash, role, team_id, user_id, created_by, expires_at)
		 VALUES ('dup_hash', 'developer', 'dogfood', 'u1', 'admin', now() + interval '1 hour')`); err != nil {
		t.Fatalf("first insert failed: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO invites (token_hash, role, team_id, user_id, created_by, expires_at)
		 VALUES ('dup_hash', 'developer', 'dogfood', 'u2', 'admin', now() + interval '1 hour')`); err == nil {
		t.Error("expected UNIQUE token_hash constraint to reject duplicate, got no error")
	}
}

func TestInvitesDownDropsTable(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available (need Docker): %v", err)
		return
	}
	defer func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate: %v", err)
		}
	}()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := Migrate(ctx, pool, Migrations, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// Roll back just 0006.
	if err := MigrateDown(ctx, pool, Migrations, "migrations", 1); err != nil {
		t.Fatalf("migrate down 1: %v", err)
	}

	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_tables WHERE tablename = 'invites')`).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("invites table should not exist after 0006 down")
	}

	// Re-apply 0006 to confirm idempotent up-down-up cycle.
	if err := Migrate(ctx, pool, Migrations, "migrations"); err != nil {
		t.Fatalf("migrate re-up: %v", err)
	}
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_tables WHERE tablename = 'invites')`).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("invites table should exist after re-up")
	}
}

func TestMigrateUpDownIdempotent(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available (need Docker): %v", err)
		return
	}
	defer func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate: %v", err)
		}
	}()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Run up/down 3 times — must be idempotent.
	for cycle := 0; cycle < 3; cycle++ {
		if err := Migrate(ctx, pool, Migrations, "migrations"); err != nil {
			t.Fatalf("cycle %d up: %v", cycle, err)
		}
		if err := MigrateDown(ctx, pool, Migrations, "migrations", 0); err != nil {
			t.Fatalf("cycle %d down: %v", cycle, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Final up to verify clean state.
	if err := Migrate(ctx, pool, Migrations, "migrations"); err != nil {
		t.Fatalf("final up: %v", err)
	}
}
