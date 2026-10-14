// Package transfers brokers short-lived, scoped binary transfers between cloud
// clients and Vela. MCP is the control plane; HTTP transports original bytes.
// Persistent records contain a grant subject and hashed capability, never an
// OAuth access/refresh token or upstream Vela credential.
package transfers

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type record struct {
	ID, Subject, Kind, State, Hash string
	Binding, Receipt               []byte
	ExpiresAt, UpdatedAt           int64
	Uses                           int
}

type repository struct{ db *sql.DB }

var errPendingQuota = errors.New("too many pending transfers")

func openRepository(ctx context.Context, filename string) (*repository, error) {
	abs, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(abs); err == nil && !st.Mode().IsRegular() {
		return nil, errors.New("transfer database must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Set the file's mode before SQLite opens it, so its WAL/SHM files inherit
	// private permissions and there is no initial world-readable creation window.
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=secure_delete(ON)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(abs, 0600); err != nil {
		db.Close()
		return nil, err
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS file_transfers (
		id TEXT PRIMARY KEY,
		subject TEXT NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('upload','download')),
		state TEXT NOT NULL,
		capability_hash TEXT NOT NULL,
		binding BLOB NOT NULL,
		receipt BLOB,
		expires_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		uses INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS file_transfers_owner ON file_transfers(subject, expires_at);
	UPDATE file_transfers SET state='uncertain' WHERE kind='upload' AND state='uploading';`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &repository{db}, nil
}

func (r *repository) insert(ctx context.Context, v record, now int64) error {
	// Bound outstanding grants per identity in the same statement as insertion.
	// Expired terminal records are retained for seven days for lost-response
	// recovery; they do not consume the active capability quota.
	if _, err := r.db.ExecContext(ctx, `DELETE FROM file_transfers WHERE updated_at < ? AND expires_at < ?`, now-int64((7*24*time.Hour)/time.Second), now); err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO file_transfers(id,subject,kind,state,capability_hash,binding,expires_at,updated_at)
		SELECT ?,?,?,?,?,?,?,? WHERE (SELECT COUNT(*) FROM file_transfers WHERE subject=? AND expires_at>? AND state IN ('prepared','uploading')) < 64`,
		v.ID, v.Subject, v.Kind, v.State, v.Hash, v.Binding, v.ExpiresAt, v.UpdatedAt, v.Subject, now)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return errPendingQuota
	}
	return err
}

func (r *repository) get(ctx context.Context, id string) (record, error) {
	var v record
	err := r.db.QueryRowContext(ctx, `SELECT id,subject,kind,state,capability_hash,binding,COALESCE(receipt,'null'),expires_at,updated_at,uses FROM file_transfers WHERE id=?`, id).
		Scan(&v.ID, &v.Subject, &v.Kind, &v.State, &v.Hash, &v.Binding, &v.Receipt, &v.ExpiresAt, &v.UpdatedAt, &v.Uses)
	return v, err
}

// claim serializes upload redemption across processes and reserves one bounded
// download use. Capability possession alone cannot alter its bound destination.
func (r *repository) claim(ctx context.Context, v record, now int64) (bool, error) {
	var result sql.Result
	var err error
	if v.Kind == "upload" {
		result, err = r.db.ExecContext(ctx, `UPDATE file_transfers SET state='uploading',updated_at=?,uses=uses+1 WHERE id=? AND capability_hash=? AND kind='upload' AND state='prepared' AND expires_at>?`, now, v.ID, v.Hash, now)
	} else {
		result, err = r.db.ExecContext(ctx, `UPDATE file_transfers SET updated_at=?,uses=uses+1 WHERE id=? AND capability_hash=? AND kind='download' AND state IN ('prepared','succeeded') AND expires_at>? AND uses<32`, now, v.ID, v.Hash, now)
	}
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (r *repository) finish(ctx context.Context, id, state string, receipt []byte, now int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE file_transfers SET state=?,receipt=?,updated_at=? WHERE id=?`, state, receipt, now, id)
	return err
}
