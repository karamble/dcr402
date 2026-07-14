package bazaar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver
)

// facSchema is the dcrbazaar SQLite schema. The resources table (discovery) is
// created here too so enabling discovery needs no migration.
const facSchema = `
CREATE TABLE IF NOT EXISTS settlements (
	id         TEXT PRIMARY KEY,
	response   BLOB NOT NULL,
	receipt    BLOB,
	settled_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS resources (
	url          TEXT PRIMARY KEY,
	data         BLOB NOT NULL,
	last_updated INTEGER NOT NULL
);
`

// SQLite is the persistent Store: a single SQLite database in WAL mode.
type SQLite struct{ db *sql.DB }

// OpenSQLite opens (creating if needed) the dcrbazaar database at path.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("dcrbazaar: open sqlite: %w", err)
	}
	// A single writer connection sidesteps SQLITE_BUSY under concurrency.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("dcrbazaar: %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(facSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("dcrbazaar: schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

// Settle implements Store with the same insert-or-return-stored idempotency
// the lib store uses for gate settlements.
func (s *SQLite) Settle(ctx context.Context, key string, response, receipt []byte, at time.Time) ([]byte, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO settlements (id, response, receipt, settled_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		key, response, receipt, at.Unix())
	if err != nil {
		return nil, false, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if inserted == 1 {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return response, false, nil
	}

	var stored []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT response FROM settlements WHERE id = ?`, key).Scan(&stored); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return stored, true, nil
}

// GetSettlement implements Store.
func (s *SQLite) GetSettlement(ctx context.Context, key string) ([]byte, bool, error) {
	var resp []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT response FROM settlements WHERE id = ?`, key).Scan(&resp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return resp, true, nil
}

// PutResource implements Store, upserting by URL. The full Resource is stored
// as one JSON blob; the filtering happens in Go over the loaded set.
func (s *SQLite) PutResource(ctx context.Context, r Resource) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO resources (url, data, last_updated)
		VALUES (?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET data = excluded.data, last_updated = excluded.last_updated`,
		r.URL, data, r.LastUpdated.Unix())
	return err
}

// Resources implements Store.
func (s *SQLite) Resources(ctx context.Context) ([]Resource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM resources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var r Resource
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Close implements Store.
func (s *SQLite) Close() error { return s.db.Close() }
