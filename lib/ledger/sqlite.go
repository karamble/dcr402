package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver
)

// SQLite is the production Ledger.
type SQLite struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id            TEXT PRIMARY KEY,
	balance_atoms INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	last_seen     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS grants (
	external_ref TEXT PRIMARY KEY,
	account_id   TEXT NOT NULL,
	rail         TEXT NOT NULL,
	amount_atoms INTEGER NOT NULL,
	receipt_json BLOB,
	created_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS charges (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id   TEXT NOT NULL,
	tool         TEXT NOT NULL,
	amount_atoms INTEGER NOT NULL,
	request_ref  TEXT,
	idem_key     TEXT,
	created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS charges_account ON charges(account_id, created_at);
`

// idemMigration adds the caller-idempotency column to pre-existing charges
// tables (idempotent: the duplicate-column error on a fresh DB is ignored),
// then the UNIQUE index that dedupes a repeated (account_id, idem_key). NULL
// idem_key rows (charges with no idempotency key) never collide because SQLite
// treats NULLs as distinct in a unique index, so keyless charges are unaffected.
const idemMigration = `CREATE UNIQUE INDEX IF NOT EXISTS charges_idem ON charges(account_id, idem_key);`

// OpenSQLite opens (creating if needed) the ledger at path. The same file
// may be shared with the gate store — tables are disjoint.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("ledger: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("ledger: %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: schema: %w", err)
	}
	// Additive migration for DBs created before the idem_key column existed.
	// The CREATE TABLE above already carries it for fresh DBs, so the ALTER
	// fails with a duplicate-column error there, which is expected and ignored.
	if _, err := db.Exec(`ALTER TABLE charges ADD COLUMN idem_key TEXT`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, fmt.Errorf("ledger: migrate charges.idem_key: %w", err)
	}
	if _, err := db.Exec(idemMigration); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: charges idem index: %w", err)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) Grant(ctx context.Context, accountID, rail string, amountAtoms int64, externalRef string, receipt []byte, at time.Time) (int64, bool, error) {
	if amountAtoms <= 0 {
		return 0, false, fmt.Errorf("ledger: grant amount must be positive")
	}
	if externalRef == "" {
		return 0, false, fmt.Errorf("ledger: externalRef is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO grants (external_ref, account_id, rail, amount_atoms, receipt_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(external_ref) DO NOTHING`,
		externalRef, accountID, rail, amountAtoms, receipt, at.Unix())
	if err != nil {
		return 0, false, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}

	// The idempotent path answers with the ORIGINAL grant's account.
	owner := accountID
	if inserted == 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT account_id FROM grants WHERE external_ref = ?`,
			externalRef).Scan(&owner); err != nil {
			return 0, false, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO accounts (id, balance_atoms, created_at, last_seen)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				balance_atoms = balance_atoms + excluded.balance_atoms,
				last_seen = excluded.last_seen`,
			accountID, amountAtoms, at.Unix(), at.Unix()); err != nil {
			return 0, false, err
		}
	}
	var balance int64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance_atoms FROM accounts WHERE id = ?`, owner).Scan(&balance); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return balance, inserted == 0, nil
}

func (s *SQLite) Charge(ctx context.Context, accountID, tool string, amountAtoms int64, requestRef, idemKey string, at time.Time) (int64, error) {
	if amountAtoms <= 0 {
		return 0, fmt.Errorf("ledger: charge amount must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Record the charge first, deduping on the caller idempotency key. A NULL
	// idem_key (no key supplied) never conflicts, so keyless charges always
	// insert; a repeated non-empty key hits ON CONFLICT DO NOTHING and inserts
	// nothing. The unique constraint makes the dedup atomic.
	var key any
	if idemKey != "" {
		key = idemKey
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO charges (account_id, tool, amount_atoms, request_ref, idem_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, idem_key) DO NOTHING`,
		accountID, tool, amountAtoms, requestRef, key, at.Unix())
	if err != nil {
		return 0, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if inserted == 0 {
		// Duplicate idempotency key: already charged. Return the current
		// balance without a second debit.
		var balance int64
		switch err := tx.QueryRowContext(ctx,
			`SELECT balance_atoms FROM accounts WHERE id = ?`, accountID).Scan(&balance); {
		case errors.Is(err, sql.ErrNoRows):
			balance = 0
		case err != nil:
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return balance, nil
	}

	// Fresh charge row: debit conditionally. The WHERE clause keeps the
	// balance from going negative. RowsAffected==0 means no such account or too
	// little balance; the deferred Rollback then undoes the charge row just
	// inserted.
	deb, err := tx.ExecContext(ctx, `
		UPDATE accounts SET balance_atoms = balance_atoms - ?, last_seen = ?
		WHERE id = ? AND balance_atoms >= ?`,
		amountAtoms, at.Unix(), accountID, amountAtoms)
	if err != nil {
		return 0, err
	}
	n, err := deb.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		var balance int64
		switch err := tx.QueryRowContext(ctx,
			`SELECT balance_atoms FROM accounts WHERE id = ?`, accountID).Scan(&balance); {
		case errors.Is(err, sql.ErrNoRows):
			return 0, &InsufficientBalanceError{Balance: 0, Required: amountAtoms}
		case err != nil:
			return 0, err
		}
		return balance, &InsufficientBalanceError{Balance: balance, Required: amountAtoms}
	}
	var newBalance int64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance_atoms FROM accounts WHERE id = ?`, accountID).Scan(&newBalance); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newBalance, nil
}

func (s *SQLite) Balance(ctx context.Context, accountID string) (int64, error) {
	var balance int64
	err := s.db.QueryRowContext(ctx,
		`SELECT balance_atoms FROM accounts WHERE id = ?`, accountID).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return balance, err
}

func (s *SQLite) Close() error { return s.db.Close() }

var _ Ledger = (*SQLite)(nil)
