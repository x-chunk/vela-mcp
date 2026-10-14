// Package oauth implements a local OAuth authorization server for the HTTP MCP
// resource. Each authorization grant owns its user's encrypted Vela credential;
// OAuth clients never inherit a process-wide upstream key.
package oauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela/api"
)

// ErrInvalidToken deliberately does not distinguish unknown, expired, revoked,
// or replayed credentials. HTTP adapters should respond with invalid_token.
var ErrInvalidToken = errors.New("invalid or expired OAuth token")

// Options describes the public issuer and private local persistence. Issuer is
// the externally reachable root origin (without a deployment path prefix). The key
// file contains exactly 32 random bytes and is generated with mode 0600 when
// absent. Preserve it alongside database backups; losing it loses credentials.
type Options struct {
	Issuer            string
	DatabasePath      string
	EncryptionKeyFile string
	AccessTTL         time.Duration
	RefreshTTL        time.Duration
	CodeTTL           time.Duration
	VerifyKey         func(context.Context, string) (access.Identity, error)
	// AllowedRedirectURIs, when nonempty, restricts dynamic registrations to
	// this exact allowlist in addition to ordinary URI security checks.
	AllowedRedirectURIs []string
	MaxClients          int
}

// Server owns OAuth persistence and HTTP handlers. It is safe for concurrent
// requests; Close stops housekeeping and releases the SQLite connection.
type Server struct {
	opts      Options
	db        *sql.DB
	aead      cipher.AEAD
	prefix    string
	origin    string
	secure    bool
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	limiter   rateLimiter
}

// Open initializes durable storage and checks that its encryption key matches
// the database. Defaults are 15-minute access tokens, 30-day absolute grants,
// and five-minute authorization codes. Refreshing never extends grant lifetime.
func Open(ctx context.Context, opts Options) (*Server, error) {
	opts.Issuer = strings.TrimRight(opts.Issuer, "/")
	u, err := url.Parse(opts.Issuer)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (strings.ToLower(u.Scheme) != "https" && !(strings.ToLower(u.Scheme) == "http" && loopback(u.Hostname()))) {
		return nil, errors.New("OAuth issuer must be HTTPS (or loopback HTTP for local development)")
	}
	if u.Path != "" {
		return nil, errors.New("OAuth issuer must be a root origin without a path prefix")
	}
	opts.Issuer = originOf(u)
	if opts.VerifyKey == nil {
		return nil, errors.New("OAuth Vela key verifier is required")
	}
	if opts.DatabasePath == "" || opts.EncryptionKeyFile == "" {
		return nil, errors.New("OAuth database and encryption key paths are required")
	}
	if opts.AccessTTL == 0 {
		opts.AccessTTL = 15 * time.Minute
	}
	if opts.RefreshTTL == 0 {
		opts.RefreshTTL = 30 * 24 * time.Hour
	}
	if opts.CodeTTL == 0 {
		opts.CodeTTL = 5 * time.Minute
	}
	if opts.AccessTTL < time.Second || opts.RefreshTTL < opts.AccessTTL || opts.CodeTTL < time.Second || opts.CodeTTL > 10*time.Minute {
		return nil, errors.New("invalid OAuth token lifetimes")
	}
	if opts.MaxClients == 0 {
		opts.MaxClients = 10000
	}
	if opts.MaxClients < 1 {
		return nil, errors.New("OAuth maximum clients must be positive")
	}
	for _, redirect := range opts.AllowedRedirectURIs {
		if err := validRedirect(redirect); err != nil {
			return nil, fmt.Errorf("allowed redirect URI: %w", err)
		}
	}
	// Never generate a replacement key for existing encrypted storage: even an
	// empty authorization database contains an encryption-check sentinel.
	if _, err := os.Stat(opts.EncryptionKeyFile); errors.Is(err, os.ErrNotExist) {
		if info, dbErr := os.Stat(opts.DatabasePath); dbErr == nil && info.Size() > 0 {
			return nil, errors.New("OAuth encryption key is missing for an existing database; restore the original key")
		}
	}
	key, err := encryptionKey(opts.EncryptionKeyFile)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := openStore(ctx, opts.DatabasePath)
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, db: db, aead: aead, prefix: u.Path, origin: opts.Issuer, secure: strings.ToLower(u.Scheme) == "https", stop: make(chan struct{}), done: make(chan struct{})}
	if err := s.checkEncryptionKey(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.cleanup(ctx); err != nil {
		db.Close()
		return nil, err
	}
	go s.housekeep()
	return s, nil
}

// Close stops housekeeping and closes persistence. Call it after HTTP requests
// have drained; identities already handed to callers contain private keys.
func (s *Server) Close() error {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.done
	return s.db.Close()
}

// ResourceURL is the exact RFC 8707 audience accepted by authorization and
// token endpoints. A grant cannot be exchanged for a different MCP server.
func (s *Server) ResourceURL() string { return s.opts.Issuer + "/mcp" }

// MetadataURL is the protected-resource discovery location to advertise in
// the HTTP adapter's WWW-Authenticate challenge.
func (s *Server) MetadataURL() string {
	return s.opts.Issuer + "/.well-known/oauth-protected-resource/mcp"
}

// Authenticate resolves an opaque access token into an active OAuth grant.
// Tokens are hashed at rest; Vela credentials are decrypted only after all
// grant and token validity checks succeed.
func (s *Server) Authenticate(ctx context.Context, token string) (access.Identity, error) {
	if len(token) != 43 {
		return access.Identity{}, ErrInvalidToken
	}
	var subject string
	err := s.db.QueryRowContext(ctx, `SELECT grant_id FROM access_tokens WHERE token_hash=? AND expires_at>?`, digest(token), time.Now().Unix()).Scan(&subject)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return access.Identity{}, ErrInvalidToken
		}
		return access.Identity{}, err
	}
	return s.LookupIdentity(ctx, subject)
}

// LookupIdentity is also the transfer broker's authorization boundary. Every
// byte transfer rechecks the grant, so a previously issued transfer URL stops
// working immediately after revocation, refresh replay, or grant expiry.
func (s *Server) LookupIdentity(ctx context.Context, subject string) (access.Identity, error) {
	var ciphertext []byte
	var scope, prefix string
	err := s.db.QueryRowContext(ctx, `SELECT key_encrypted,api_scope,path_prefix FROM grants WHERE id=? AND resource=? AND revoked=0 AND expires_at>?`, subject, s.ResourceURL(), time.Now().Unix()).Scan(&ciphertext, &scope, &prefix)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return access.Identity{}, ErrInvalidToken
		}
		return access.Identity{}, err
	}
	key, err := s.decrypt(ciphertext, subject)
	if err != nil {
		return access.Identity{}, errors.New("OAuth credential decryption failed")
	}
	return access.Identity{Subject: subject, Scope: api.Scope(scope), Prefix: prefix, Credential: access.NewCredential(string(key))}, nil
}

// IsActive checks grant status without decrypting its credential. It includes
// the current resource audience, so an issuer change invalidates old grants.
func (s *Server) IsActive(ctx context.Context, subject string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM grants WHERE id=? AND resource=? AND revoked=0 AND expires_at>?`, subject, s.ResourceURL(), time.Now().Unix()).Scan(&count)
	return count == 1, err
}

// ValidateIdentity rejects an identity whose grant has expired or was revoked.
// Call LookupIdentity when the current upstream credential is needed instead.
func (s *Server) ValidateIdentity(ctx context.Context, identity access.Identity) error {
	active, err := s.IsActive(ctx, identity.Subject)
	if err != nil {
		return err
	}
	if !active {
		return ErrInvalidToken
	}
	return nil
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cryptographic random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func (s *Server) encrypt(data []byte, aad string) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("cryptographic random source unavailable")
	}
	return s.aead.Seal(nonce, nonce, data, []byte(aad))
}
func (s *Server) decrypt(data []byte, aad string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(data) < n {
		return nil, errors.New("invalid encrypted credential")
	}
	return s.aead.Open(nil, data[:n], data[n:], []byte(aad))
}

func encryptionKey(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create encryption key directory: %w", err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			return encryptionKey(path)
		}
		if err != nil {
			return nil, fmt.Errorf("create encryption key: %w", err)
		}
		if _, err = f.Write(key); err != nil {
			f.Close()
			return nil, err
		}
		if err = f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read encryption key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("encryption key must be a regular file accessible only to its owner (mode 0600)")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("encryption key file must contain exactly 32 raw bytes")
	}
	return key, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// originOf follows browser origin serialization for case and default ports.
// Prefixes, query strings, fragments, and credentials do not belong to an origin.
func originOf(u *url.URL) string {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}
func validRedirect(raw string) error {
	if len(raw) > 2048 {
		return errors.New("redirect URI is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(raw, "\r\n") {
		return errors.New("invalid redirect URI")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname())) {
		return errors.New("redirect URI must use HTTPS (or loopback HTTP)")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid redirect URI port")
		}
	}
	return nil
}

// redirectAllowed follows RFC 8252's loopback exception: a native application
// may choose an ephemeral local callback port. Every other URI component stays
// exact, and localhost is never interchangeable with a loopback IP literal.
// The accepted concrete URI is stored in the authorization code and must still
// match exactly when the code is redeemed.
func redirectAllowed(registered []string, requested string) bool {
	if validRedirect(requested) != nil {
		return false
	}
	if contains(registered, requested) {
		return true
	}
	u, err := url.Parse(requested)
	if err != nil || u.Scheme != "http" || !loopback(u.Hostname()) {
		return false
	}
	for _, raw := range registered {
		v, err := url.Parse(raw)
		if err == nil && v.Scheme == "http" && v.Hostname() == u.Hostname() && v.EscapedPath() == u.EscapedPath() && v.RawQuery == u.RawQuery && v.ForceQuery == u.ForceQuery {
			return true
		}
	}
	return false
}
