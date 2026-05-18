package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Invite errors returned by PgInviteStore. Handlers use these sentinels to
// map to HTTP error codes (bad_token / token_consumed / token_expired).
var (
	ErrInviteNotFound = errors.New("invite not found")
	ErrInviteConsumed = errors.New("invite already consumed")
	ErrInviteExpired  = errors.New("invite expired")
)

// Invite represents one row in the invites table.
type Invite struct {
	ID        int64
	TokenHash string
	Role      string
	TeamID    string
	UserID    string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	UsedBy    *string
	MachineID *string
}

// InviteStore is the abstract interface implemented by Pg- and in-memory
// invite stores. Handlers depend on this, not on a concrete type.
//
// The exchange path uses ConsumeAndIssueKey, which atomically marks the
// invite consumed AND inserts the corresponding api_keys row. The two
// writes either both commit or both roll back; a key-store failure leaves
// the invite redeemable. Audit writes are explicitly outside this call so
// audit failures cannot roll back user-visible key issuance, matching
// exchange-setup semantics.
type InviteStore interface {
	Create(ctx context.Context, inv Invite) error
	ConsumeAndIssueKey(ctx context.Context, tokenHash, machineID, apiKeyHash string) (*Invite, error)
}

// PgInviteStore is the Postgres-backed invite store (§13.1, §13.8).
type PgInviteStore struct {
	pool *pgxpool.Pool
}

// NewPgInviteStore creates a new invite store.
func NewPgInviteStore(pool *pgxpool.Pool) *PgInviteStore {
	return &PgInviteStore{pool: pool}
}

// Create inserts a new invite row. Caller supplies the sha256 hex hash of
// the cleartext token; the cleartext is never stored.
func (s *PgInviteStore) Create(ctx context.Context, inv Invite) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO invites
		 (token_hash, role, team_id, user_id, created_by, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		inv.TokenHash, inv.Role, inv.TeamID, inv.UserID, inv.CreatedBy, inv.ExpiresAt)
	if err != nil {
		return fmt.Errorf("pg invite create: %w", err)
	}
	return nil
}

// ConsumeAndIssueKey atomically marks the invite consumed AND inserts an
// api_keys row keyed by apiKeyHash. The two writes share a single Postgres
// transaction: any failure in either step rolls back both, leaving the
// invite redeemable. The UPDATE...WHERE used_at IS NULL fence still
// enforces the one-shot guarantee under concurrent redemption — at most
// one transaction can observe RowsAffected=1.
//
// Audit writes belong outside this call. The transaction intentionally
// covers only invites state change + api_keys insert; audit failures must
// not roll back key issuance.
func (s *PgInviteStore) ConsumeAndIssueKey(ctx context.Context, tokenHash, machineID, apiKeyHash string) (*Invite, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("pg invite tx begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	var inv Invite
	err = tx.QueryRow(ctx,
		`UPDATE invites
		 SET used_at = now(), used_by = user_id, machine_id = $2
		 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		 RETURNING id, token_hash, role, team_id, user_id, created_by,
		           created_at, expires_at, used_at, used_by, machine_id`,
		tokenHash, machineID).Scan(
		&inv.ID, &inv.TokenHash, &inv.Role, &inv.TeamID, &inv.UserID, &inv.CreatedBy,
		&inv.CreatedAt, &inv.ExpiresAt, &inv.UsedAt, &inv.UsedBy, &inv.MachineID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// UPDATE matched no row: either token doesn't exist, was already
		// consumed, or expired. Disambiguate within the same tx so the
		// handler can return the right error code.
		var (
			usedAt    *time.Time
			expiresAt time.Time
		)
		selErr := tx.QueryRow(ctx,
			`SELECT used_at, expires_at FROM invites WHERE token_hash = $1`,
			tokenHash).Scan(&usedAt, &expiresAt)
		if errors.Is(selErr, pgx.ErrNoRows) {
			return nil, ErrInviteNotFound
		}
		if selErr != nil {
			return nil, fmt.Errorf("pg invite lookup: %w", selErr)
		}
		if usedAt != nil {
			return nil, ErrInviteConsumed
		}
		if !expiresAt.After(time.Now()) {
			return nil, ErrInviteExpired
		}
		// Race: row was unused and not expired when SELECT ran but UPDATE
		// missed it. Treat as a transient consumed state — safer than
		// reporting success.
		return nil, ErrInviteConsumed
	}
	if err != nil {
		return nil, fmt.Errorf("pg invite consume: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO api_keys (key_hash, user_id, team_id, role)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (key_hash) DO NOTHING`,
		apiKeyHash, inv.UserID, inv.TeamID, inv.Role); err != nil {
		return nil, fmt.Errorf("pg api key insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("pg invite tx commit: %w", err)
	}
	committed = true
	return &inv, nil
}

// Ensure PgInviteStore implements InviteStore.
var _ InviteStore = (*PgInviteStore)(nil)

// InMemInviteStore is a test/in-memory implementation of InviteStore.
// It mirrors the Pg semantics: one-shot consume with at-most-once
// acceptance, expiry check, and distinct sentinel errors for
// not-found / consumed / expired. ConsumeAndIssueKey writes through an
// attached InMemKeyStore (wired via SetKeyStore) so a key-insert failure
// leaves the invite unconsumed — mirroring the Pg transaction's
// all-or-nothing behavior.
type InMemInviteStore struct {
	mu       sync.Mutex
	byHash   map[string]*Invite
	keyStore *InMemKeyStore
	// failIssue, if non-nil, is invoked after the invite is validated as
	// consumable but before its used_at is committed. Returning a non-nil
	// error simulates an api_key insert failure and leaves the invite
	// redeemable. For tests of the transactional contract.
	failIssue func() error
}

// NewInMemInviteStore creates an empty in-memory invite store.
func NewInMemInviteStore() *InMemInviteStore {
	return &InMemInviteStore{byHash: make(map[string]*Invite)}
}

// SetKeyStore wires the in-memory api_key store that ConsumeAndIssueKey
// will write through. Test wiring; not used in production.
func (s *InMemInviteStore) SetKeyStore(ks *InMemKeyStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyStore = ks
}

// SetFailIssue installs a hook that ConsumeAndIssueKey will invoke between
// validation and commit. If the hook returns non-nil, the invite is NOT
// consumed and no api_key is inserted. For tests that exercise the
// transactional contract.
func (s *InMemInviteStore) SetFailIssue(hook func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failIssue = hook
}

// Create inserts an invite. Returns an error if the same token_hash exists.
func (s *InMemInviteStore) Create(_ context.Context, inv Invite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byHash[inv.TokenHash]; exists {
		return fmt.Errorf("invite already exists")
	}
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = time.Now()
	}
	stored := inv
	s.byHash[inv.TokenHash] = &stored
	return nil
}

// ConsumeAndIssueKey atomically validates the invite, issues the api_key,
// and marks the invite consumed under a single mutex. If the key-store
// insert (or the injected failIssue hook) returns an error, the invite is
// not consumed.
func (s *InMemInviteStore) ConsumeAndIssueKey(_ context.Context, tokenHash, machineID, apiKeyHash string) (*Invite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.byHash[tokenHash]
	if !ok {
		return nil, ErrInviteNotFound
	}
	if inv.UsedAt != nil {
		return nil, ErrInviteConsumed
	}
	if !inv.ExpiresAt.After(time.Now()) {
		return nil, ErrInviteExpired
	}
	if s.failIssue != nil {
		if err := s.failIssue(); err != nil {
			return nil, err
		}
	}
	if s.keyStore != nil {
		if err := s.keyStore.StoreAPIKey(inv.UserID, inv.TeamID, inv.Role, apiKeyHash); err != nil {
			return nil, fmt.Errorf("inmem api key insert: %w", err)
		}
	}
	now := time.Now()
	mid := machineID
	usedBy := inv.UserID
	inv.UsedAt = &now
	inv.UsedBy = &usedBy
	inv.MachineID = &mid
	out := *inv
	return &out, nil
}

// Ensure InMemInviteStore implements InviteStore.
var _ InviteStore = (*InMemInviteStore)(nil)
