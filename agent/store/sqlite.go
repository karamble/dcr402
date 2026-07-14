package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver
)

// SQLite is the production Store.
type SQLite struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS attempts (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	tool         TEXT NOT NULL,
	rail         TEXT NOT NULL,
	destination  TEXT NOT NULL,
	amount_atoms INTEGER NOT NULL,
	memo         TEXT NOT NULL,
	decision     TEXT NOT NULL,
	rule_trace   BLOB NOT NULL,
	created_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS payments (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	attempt_id   INTEGER NOT NULL,
	rail         TEXT NOT NULL,
	amount_atoms INTEGER NOT NULL,
	fee_atoms    INTEGER NOT NULL DEFAULT 0,
	invoice      TEXT NOT NULL DEFAULT '',
	preimage     TEXT NOT NULL DEFAULT '',
	txid         TEXT NOT NULL DEFAULT '',
	l402_service TEXT NOT NULL DEFAULT '',
	receipt      BLOB,
	created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS payments_created ON payments(created_at);
CREATE TABLE IF NOT EXISTS l402_tokens (
	service_host  TEXT PRIMARY KEY,
	authorization TEXT NOT NULL,
	valid_until   INTEGER NOT NULL DEFAULT 0,
	paid_atoms    INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	last_used     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS approvals (
	id          TEXT PRIMARY KEY,
	attempt_id  INTEGER NOT NULL,
	channel     TEXT NOT NULL,
	status      TEXT NOT NULL,
	responder   TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	resolved_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS state (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);
`

// OpenSQLite opens (creating if needed) the agent store at path.
func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
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

func (s *SQLite) RecordAttempt(ctx context.Context, a Attempt) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO attempts (tool, rail, destination, amount_atoms, memo,
			decision, rule_trace, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Tool, a.Rail, a.Destination, a.AmountAtoms, a.Memo,
		string(a.Decision), a.RuleTrace, a.CreatedAt.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanAttempt(row interface{ Scan(...any) error }) (Attempt, error) {
	var (
		a         Attempt
		decision  string
		createdAt int64
	)
	err := row.Scan(&a.ID, &a.Tool, &a.Rail, &a.Destination, &a.AmountAtoms,
		&a.Memo, &decision, &a.RuleTrace, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, err
	}
	a.Decision = Decision(decision)
	a.CreatedAt = time.Unix(createdAt, 0).UTC()
	return a, nil
}

const attemptCols = `id, tool, rail, destination, amount_atoms, memo, decision, rule_trace, created_at`

func (s *SQLite) GetAttempt(ctx context.Context, id int64) (Attempt, error) {
	return scanAttempt(s.db.QueryRowContext(ctx,
		`SELECT `+attemptCols+` FROM attempts WHERE id = ?`, id))
}

func (s *SQLite) ListAttempts(ctx context.Context, limit int, beforeID int64) ([]Attempt, error) {
	q := `SELECT ` + attemptCols + ` FROM attempts`
	args := []any{}
	if beforeID > 0 {
		q += ` WHERE id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLite) RecordPayment(ctx context.Context, p Payment) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO payments (attempt_id, rail, amount_atoms, fee_atoms,
			invoice, preimage, txid, l402_service, receipt, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.AttemptID, p.Rail, p.AmountAtoms, p.FeeAtoms,
		p.Invoice, p.Preimage, p.TxID, p.L402Service, p.Receipt,
		p.CreatedAt.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanPayment(row interface{ Scan(...any) error }) (Payment, error) {
	var (
		p         Payment
		createdAt int64
	)
	err := row.Scan(&p.ID, &p.AttemptID, &p.Rail, &p.AmountAtoms, &p.FeeAtoms,
		&p.Invoice, &p.Preimage, &p.TxID, &p.L402Service, &p.Receipt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, err
	}
	p.CreatedAt = time.Unix(createdAt, 0).UTC()
	return p, nil
}

const paymentCols = `id, attempt_id, rail, amount_atoms, fee_atoms, invoice, preimage, txid, l402_service, receipt, created_at`

func (s *SQLite) GetPayment(ctx context.Context, id int64) (Payment, error) {
	return scanPayment(s.db.QueryRowContext(ctx,
		`SELECT `+paymentCols+` FROM payments WHERE id = ?`, id))
}

func (s *SQLite) ListPayments(ctx context.Context, limit int, beforeID int64) ([]Payment, error) {
	q := `SELECT ` + paymentCols + ` FROM payments`
	args := []any{}
	if beforeID > 0 {
		q += ` WHERE id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLite) Usage(ctx context.Context, now time.Time) (Usage, error) {
	var u Usage
	day, week := DayStart(now).Unix(), WeekStart(now).Unix()
	hourAgo := now.Add(-time.Hour).Unix()
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN created_at >= ? THEN amount_atoms END), 0),
			COALESCE(SUM(CASE WHEN created_at >= ? THEN amount_atoms END), 0),
			COALESCE(SUM(CASE WHEN created_at > ? THEN 1 END), 0)
		FROM payments`, day, week, hourAgo).
		Scan(&u.DayAtoms, &u.WeekAtoms, &u.HourCount)
	return u, err
}

func (s *SQLite) PutToken(ctx context.Context, t Token) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO l402_tokens (service_host, authorization, valid_until,
			paid_atoms, created_at, last_used)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(service_host) DO UPDATE SET
			authorization = excluded.authorization,
			valid_until = excluded.valid_until,
			paid_atoms = excluded.paid_atoms,
			created_at = excluded.created_at,
			last_used = excluded.last_used`,
		t.ServiceHost, t.Authorization, t.ValidUntil, t.PaidAtoms,
		t.CreatedAt.Unix(), t.LastUsed.Unix())
	return err
}

func (s *SQLite) GetToken(ctx context.Context, host string) (Token, error) {
	var (
		t                   Token
		createdAt, lastUsed int64
	)
	t.ServiceHost = host
	err := s.db.QueryRowContext(ctx, `
		SELECT authorization, valid_until, paid_atoms, created_at, last_used
		FROM l402_tokens WHERE service_host = ?`, host).
		Scan(&t.Authorization, &t.ValidUntil, &t.PaidAtoms, &createdAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, err
	}
	t.CreatedAt = time.Unix(createdAt, 0).UTC()
	t.LastUsed = time.Unix(lastUsed, 0).UTC()
	return t, nil
}

func (s *SQLite) TouchToken(ctx context.Context, host string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE l402_tokens SET last_used = ? WHERE service_host = ?`,
		at.Unix(), host)
	return err
}

func (s *SQLite) DeleteToken(ctx context.Context, host string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM l402_tokens WHERE service_host = ?`, host)
	return err
}

func (s *SQLite) CreateApproval(ctx context.Context, a Approval) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (id, attempt_id, channel, status, responder,
			created_at, expires_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		a.ID, a.AttemptID, a.Channel, a.Status, a.Responder,
		a.CreatedAt.Unix(), a.ExpiresAt.Unix())
	return err
}

func scanApproval(row interface{ Scan(...any) error }) (Approval, error) {
	var (
		a                                Approval
		createdAt, expiresAt, resolvedAt int64
	)
	err := row.Scan(&a.ID, &a.AttemptID, &a.Channel, &a.Status, &a.Responder,
		&createdAt, &expiresAt, &resolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	if err != nil {
		return Approval{}, err
	}
	a.CreatedAt = time.Unix(createdAt, 0).UTC()
	a.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	if resolvedAt > 0 {
		a.ResolvedAt = time.Unix(resolvedAt, 0).UTC()
	}
	return a, nil
}

const approvalCols = `id, attempt_id, channel, status, responder, created_at, expires_at, resolved_at`

func (s *SQLite) GetApproval(ctx context.Context, id string) (Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx,
		`SELECT `+approvalCols+` FROM approvals WHERE id = ?`, id))
}

func (s *SQLite) ResolveApproval(ctx context.Context, id, status, responder string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE approvals SET status = ?, responder = ?, resolved_at = ?
		WHERE id = ? AND status = ? AND expires_at >= ?`,
		status, responder, at.Unix(), id, ApprovalPending, at.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *SQLite) ListPendingApprovals(ctx context.Context, now time.Time) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+approvalCols+` FROM approvals
		WHERE status = ? AND expires_at >= ?
		ORDER BY created_at ASC`, ApprovalPending, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLite) ExpireApprovals(ctx context.Context, now time.Time) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+approvalCols+` FROM approvals
		WHERE status = ? AND expires_at < ?`, ApprovalPending, now.Unix())
	if err != nil {
		return nil, err
	}
	var expired []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range expired {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE approvals SET status = ?, resolved_at = ?
			WHERE id = ? AND status = ?`,
			ApprovalExpired, now.Unix(), expired[i].ID, ApprovalPending); err != nil {
			return nil, err
		}
		expired[i].Status = ApprovalExpired
		expired[i].ResolvedAt = now
	}
	return expired, nil
}

func (s *SQLite) GetState(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM state WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *SQLite) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO state (k, v) VALUES (?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`, key, value)
	return err
}

func (s *SQLite) Close() error { return s.db.Close() }

var _ Store = (*SQLite)(nil)
