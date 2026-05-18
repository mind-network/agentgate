package auth

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// PgKeyStore implements KeyStore backed by Postgres api_keys table.
type PgKeyStore struct {
	pool  *pgxpool.Pool
	cache map[string]*cacheEntry
	mu    sync.RWMutex
}

type cacheEntry struct {
	key     *APIKey
	expires time.Time
}

// NewPgKeyStore creates a new Postgres-backed key store.
func NewPgKeyStore(pool *pgxpool.Pool) *PgKeyStore {
	s := &PgKeyStore{
		pool:  pool,
		cache: make(map[string]*cacheEntry),
	}
	go s.reapLoop()
	return s
}

// LookupAPIKey finds a key by prefix from the DB and verifies via bcrypt.
func (s *PgKeyStore) LookupAPIKey(keyPrefix string) (*APIKey, error) {
	return s.lookupWithBcrypt(context.Background(), keyPrefix)
}

// StoreAPIKey inserts a new API key row.
func (s *PgKeyStore) StoreAPIKey(userID, teamID, role, hash string) error {
	return s.storeAPIKey(context.Background(), userID, teamID, role, hash)
}

// HasAnyKey returns true if at least one non-deleted key exists.
func (s *PgKeyStore) HasAnyKey() bool {
	return s.hasAnyKey(context.Background())
}

// StoreAPIKeyCtx inserts a new API key row with context.
func (s *PgKeyStore) StoreAPIKeyCtx(ctx context.Context, userID, teamID, role, hash string) error {
	return s.storeAPIKey(ctx, userID, teamID, role, hash)
}

func (s *PgKeyStore) storeAPIKey(ctx context.Context, userID, teamID, role, hash string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO api_keys (key_hash, user_id, team_id, role)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (key_hash) DO NOTHING`,
		hash, userID, teamID, role)
	if err != nil {
		return fmt.Errorf("pg store api key: %w", err)
	}
	return nil
}

func (s *PgKeyStore) lookupWithBcrypt(ctx context.Context, cleartext string) (*APIKey, error) {
	prefix := ""
	if len(cleartext) >= 8 {
		prefix = cleartext[:8]
	}

	// Check cache first — keyed by cleartext prefix.
	s.mu.RLock()
	if entry, ok := s.cache[prefix]; ok && time.Now().Before(entry.expires) {
		cached := entry.key
		s.mu.RUnlock()
		if bcrypt.CompareHashAndPassword([]byte(cached.Hash), []byte(cleartext)) == nil {
			return cached, nil
		}
	} else {
		s.mu.RUnlock()
	}

	// Cache miss: query ALL non-deleted keys and bcrypt-compare each.
	// bcrypt hashes start with "$2a$10$…" so cleartext-prefix LIKE won't match.
	// The number of active API keys in P0 is bounded (at most a few hundred).
	rows, err := s.pool.Query(ctx,
		`SELECT key_hash, user_id, team_id, role FROM api_keys
		 WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("key not found")
	}
	defer rows.Close()

	for rows.Next() {
		var k APIKey
		var fullHash string
		if err := rows.Scan(&fullHash, &k.UserID, &k.TeamID, &k.Role); err != nil {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(fullHash), []byte(cleartext)) == nil {
			k.Hash = fullHash
			s.mu.Lock()
			s.cache[prefix] = &cacheEntry{key: &k, expires: time.Now().Add(5 * time.Minute)}
			s.mu.Unlock()
			_, _ = s.pool.Exec(ctx,
				`UPDATE api_keys SET last_used_at = now() WHERE key_hash = $1`, fullHash)
			return &k, nil
		}
	}

	return nil, fmt.Errorf("key not found")
}

func (s *PgKeyStore) hasAnyKey(ctx context.Context) bool {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE deleted_at IS NULL`).Scan(&count)
	return err == nil && count > 0
}

// DeleteAPIKey soft-deletes a key.
func (s *PgKeyStore) DeleteAPIKey(ctx context.Context, keyHash string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET deleted_at = now() WHERE key_hash = $1`, keyHash)
	return err
}

func (s *PgKeyStore) reapLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for k, v := range s.cache {
			if now.After(v.expires) {
				delete(s.cache, k)
			}
		}
		s.mu.Unlock()
	}
}

// Ensure PgKeyStore implements KeyStore.
var _ KeyStore = (*PgKeyStore)(nil)

// PgSetupTokenStore manages setup tokens in the setup_tokens table.
type PgSetupTokenStore struct {
	pool *pgxpool.Pool
}

// NewPgSetupTokenStore creates a new Postgres-backed setup token store.
func NewPgSetupTokenStore(pool *pgxpool.Pool) *PgSetupTokenStore {
	return &PgSetupTokenStore{pool: pool}
}

// InsertToken stores a sha256-hashed setup token.
func (s *PgSetupTokenStore) InsertToken(ctx context.Context, tokenHash string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO setup_tokens (token_hash) VALUES ($1) ON CONFLICT DO NOTHING`, tokenHash)
	return err
}

// ConsumeToken atomically marks a setup token as consumed.
// Returns true if the token was available and consumed.
func (s *PgSetupTokenStore) ConsumeToken(ctx context.Context, tokenHash string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE setup_tokens SET consumed_at = now()
		 WHERE token_hash = $1 AND consumed_at IS NULL`, tokenHash)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	slog.Info("setup_token consumed", "token_hash_prefix", tokenHash[:16])
	return true, nil
}

// HasAnyToken returns true if at least one available setup token exists.
func (s *PgSetupTokenStore) HasAnyToken(ctx context.Context) bool {
	var count int
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM setup_tokens WHERE consumed_at IS NULL`).Scan(&count)
	return count > 0
}
