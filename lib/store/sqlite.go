package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver
)

// SQLite is the production Store: a single SQLite database in WAL mode.
type SQLite struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS challenges (
	payment_hash BLOB PRIMARY KEY,
	invoice      TEXT NOT NULL,
	macaroon_b64 TEXT NOT NULL,
	root_key_id  BLOB NOT NULL,
	requirements BLOB NOT NULL,
	resource     TEXT NOT NULL,
	amount_atoms INTEGER NOT NULL,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS onchain_challenges (
	address             TEXT PRIMARY KEY,
	requirements        BLOB NOT NULL,
	resource            TEXT NOT NULL,
	amount_atoms        INTEGER NOT NULL,
	confirmations       INTEGER NOT NULL,
	macaroon_b64        TEXT NOT NULL,
	root_key_id         BLOB NOT NULL,
	credential_preimage TEXT NOT NULL,
	created_at          INTEGER NOT NULL,
	expires_at          INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settlements (
	payment_hash BLOB PRIMARY KEY,
	settled_at   INTEGER NOT NULL,
	response     BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS root_keys (
	key_id     BLOB PRIMARY KEY,
	root_key   BLOB NOT NULL,
	created_at INTEGER NOT NULL
);
`

// OpenSQLite opens (creating if needed) the store at path. Use ":memory:"
// for an ephemeral database.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	// A single writer connection sidesteps SQLITE_BUSY under concurrency;
	// SQLite serializes writes anyway.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA foreign_keys=ON;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) PutChallenge(ctx context.Context, c Challenge) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO challenges (payment_hash, invoice, macaroon_b64,
			root_key_id, requirements, resource, amount_atoms,
			created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(payment_hash) DO NOTHING`,
		c.PaymentHash[:], c.Invoice, c.MacaroonB64, c.RootKeyID[:],
		c.Requirements, c.Resource, c.AmountAtoms,
		c.CreatedAt.Unix(), c.ExpiresAt.Unix())
	return err
}

func (s *SQLite) GetChallenge(ctx context.Context, hash [32]byte) (Challenge, error) {
	var (
		c                    Challenge
		rootKeyID            []byte
		createdAt, expiresAt int64
	)
	c.PaymentHash = hash
	err := s.db.QueryRowContext(ctx, `
		SELECT invoice, macaroon_b64, root_key_id, requirements,
			resource, amount_atoms, created_at, expires_at
		FROM challenges WHERE payment_hash = ?`, hash[:]).
		Scan(&c.Invoice, &c.MacaroonB64, &rootKeyID, &c.Requirements,
			&c.Resource, &c.AmountAtoms, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Challenge{}, ErrNotFound
	}
	if err != nil {
		return Challenge{}, err
	}
	copy(c.RootKeyID[:], rootKeyID)
	c.CreatedAt = time.Unix(createdAt, 0)
	c.ExpiresAt = time.Unix(expiresAt, 0)
	return c, nil
}

func (s *SQLite) PutOnchainChallenge(ctx context.Context, c OnchainChallenge) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO onchain_challenges (address, requirements, resource,
			amount_atoms, confirmations, macaroon_b64, root_key_id,
			credential_preimage, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(address) DO NOTHING`,
		c.Address, c.Requirements, c.Resource, c.AmountAtoms,
		c.Confirmations, c.MacaroonB64, c.RootKeyID[:],
		c.CredentialPreimage, c.CreatedAt.Unix(), c.ExpiresAt.Unix())
	return err
}

func (s *SQLite) GetOnchainChallenge(ctx context.Context, address string) (OnchainChallenge, error) {
	var (
		c                    OnchainChallenge
		rootKeyID            []byte
		createdAt, expiresAt int64
	)
	c.Address = address
	err := s.db.QueryRowContext(ctx, `
		SELECT requirements, resource, amount_atoms, confirmations,
			macaroon_b64, root_key_id, credential_preimage,
			created_at, expires_at
		FROM onchain_challenges WHERE address = ?`, address).
		Scan(&c.Requirements, &c.Resource, &c.AmountAtoms, &c.Confirmations,
			&c.MacaroonB64, &rootKeyID, &c.CredentialPreimage,
			&createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OnchainChallenge{}, ErrNotFound
	}
	if err != nil {
		return OnchainChallenge{}, err
	}
	copy(c.RootKeyID[:], rootKeyID)
	c.CreatedAt = time.Unix(createdAt, 0)
	c.ExpiresAt = time.Unix(expiresAt, 0)
	return c, nil
}

func (s *SQLite) Settle(ctx context.Context, hash [32]byte, response []byte, at time.Time) ([]byte, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO settlements (payment_hash, settled_at, response)
		VALUES (?, ?, ?)
		ON CONFLICT(payment_hash) DO NOTHING`,
		hash[:], at.Unix(), response)
	if err != nil {
		return nil, false, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	var stored []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT response FROM settlements WHERE payment_hash = ?`,
		hash[:]).Scan(&stored); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return stored, inserted == 0, nil
}

func (s *SQLite) Settled(ctx context.Context, hash [32]byte) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM settlements WHERE payment_hash = ?`, hash[:]).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *SQLite) PutRootKey(ctx context.Context, keyID [32]byte, rootKey [32]byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO root_keys (key_id, root_key, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key_id) DO NOTHING`,
		keyID[:], rootKey[:], time.Now().Unix())
	return err
}

func (s *SQLite) GetRootKey(ctx context.Context, keyID [32]byte) ([32]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT root_key FROM root_keys WHERE key_id = ?`, keyID[:]).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return [32]byte{}, ErrNotFound
	}
	if err != nil {
		return [32]byte{}, err
	}
	var out [32]byte
	copy(out[:], raw)
	return out, nil
}

func (s *SQLite) DeleteRootKey(ctx context.Context, keyID [32]byte) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM root_keys WHERE key_id = ?`, keyID[:])
	return err
}

func (s *SQLite) Close() error { return s.db.Close() }

var _ Store = (*SQLite)(nil)
