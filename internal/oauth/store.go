package oauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Storage uses one SQLite connection and explicit transactions for redemption.
// Keeping refresh history until grant expiry makes token-family replay visible
// across concurrent requests and process restarts.
func openStore(ctx context.Context, path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("OAuth database must be a regular file")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("open OAuth database: %w", err)
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS oauth_metadata (name TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS clients (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, redirects TEXT NOT NULL, grant_types TEXT NOT NULL,
 created_at INTEGER NOT NULL, metadata_expires_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS authorization_transactions (
 id_hash TEXT PRIMARY KEY, client_id TEXT NOT NULL, redirect_uri TEXT NOT NULL,
 state TEXT NOT NULL, challenge TEXT NOT NULL, resource TEXT NOT NULL,
 browser_hash TEXT NOT NULL, csrf_hash TEXT NOT NULL,
 expires_at INTEGER NOT NULL, consumed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS grants (
 id TEXT PRIMARY KEY, client_id TEXT NOT NULL, resource TEXT NOT NULL,
 key_encrypted BLOB NOT NULL, api_scope TEXT NOT NULL, path_prefix TEXT NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS authorization_codes (
 code_hash TEXT PRIMARY KEY, grant_id TEXT NOT NULL REFERENCES grants(id) ON DELETE CASCADE,
 client_id TEXT NOT NULL, redirect_uri TEXT NOT NULL, challenge TEXT NOT NULL,
 resource TEXT NOT NULL, expires_at INTEGER NOT NULL, consumed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS access_tokens (
 token_hash TEXT PRIMARY KEY, grant_id TEXT NOT NULL REFERENCES grants(id) ON DELETE CASCADE,
 expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS refresh_tokens (
 token_hash TEXT PRIMARY KEY, grant_id TEXT NOT NULL REFERENCES grants(id) ON DELETE CASCADE,
 client_id TEXT NOT NULL, expires_at INTEGER NOT NULL, consumed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS access_grant ON access_tokens(grant_id);
CREATE INDEX IF NOT EXISTS refresh_grant ON refresh_tokens(grant_id);
CREATE INDEX IF NOT EXISTS auth_transaction_expiry ON authorization_transactions(expires_at);
CREATE INDEX IF NOT EXISTS grant_expiry ON grants(expires_at);`); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize OAuth schema: %w", err)
	}
	// SQLite creates sidecars with the database's permissions. Tighten existing
	// sidecars from a previous deployment as well.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

func (s *Server) checkEncryptionKey(ctx context.Context) error {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM oauth_metadata WHERE name='encryption_check'`).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.ExecContext(ctx, `INSERT INTO oauth_metadata(name,value) VALUES('encryption_check',?)`, s.encrypt([]byte("vela-mcp-oauth-key-v1"), "encryption_check"))
		return err
	}
	if err != nil {
		return err
	}
	plain, err := s.decrypt(sealed, "encryption_check")
	if err != nil || string(plain) != "vela-mcp-oauth-key-v1" {
		return errors.New("OAuth encryption key does not match database; restore the original key")
	}
	return nil
}

func (s *Server) cleanup(ctx context.Context) error {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{`DELETE FROM authorization_transactions WHERE expires_at<=?`, `DELETE FROM authorization_codes WHERE expires_at<=?`, `DELETE FROM access_tokens WHERE expires_at<=?`, `DELETE FROM grants WHERE expires_at<=?`} {
		if _, err := tx.ExecContext(ctx, query, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) housekeep() {
	defer close(s.done)
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = s.cleanup(ctx)
			cancel()
		}
	}
}
