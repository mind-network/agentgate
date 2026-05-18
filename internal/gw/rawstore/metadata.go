// Package rawstore provides the raw_record metadata writer for P0.
// P0: metadata_only — object_uri/dek_*/kek_id are all NULL.
package rawstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MetadataWriter inserts raw_record metadata_only rows.
type MetadataWriter struct {
	pool *pgxpool.Pool
}

// NewMetadataWriter creates a new raw record metadata writer.
func NewMetadataWriter(pool *pgxpool.Pool) *MetadataWriter {
	return &MetadataWriter{pool: pool}
}

// Write inserts a metadata_only raw_record row.
// traceID must be a valid UUID string.
func (w *MetadataWriter) Write(ctx context.Context, traceID, tenantID, userID, teamID, repoID, sensitivity string) error {
	_, err := w.pool.Exec(ctx,
		`INSERT INTO raw_record
		 (trace_id, tenant_id, user_id, team_id, repo_id, sensitivity,
		  storage_policy, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'metadata_only',$7)`,
		traceID, tenantID, userID, teamID, repoID, sensitivity,
		time.Now().Add(90*24*time.Hour), // 90-day retention
	)
	return err
}
