package oauth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	sensitive(w)
	if !s.allow(w, r, "token", 120, time.Minute) {
		return
	}
	if !parseForm(w, r, 16*1024) {
		return
	}
	if r.Header.Get("Authorization") != "" || r.PostForm.Get("client_secret") != "" {
		oauthError(w, 401, "invalid_client", "This server accepts public PKCE clients only")
		return
	}
	c, err := s.lookupClient(r.Context(), r.PostForm.Get("client_id"))
	if err != nil {
		oauthError(w, 400, "invalid_client", "Unknown client")
		return
	}
	if r.PostForm.Get("resource") != s.ResourceURL() {
		oauthError(w, 400, "invalid_target", "The exact MCP resource audience is required")
		return
	}
	if scope := r.PostForm.Get("scope"); scope != "" && scope != "vela" {
		oauthError(w, 400, "invalid_scope", "Only the vela scope is supported")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r, c)
	case "refresh_token":
		if !contains(c.Grants, "refresh_token") {
			oauthError(w, 400, "unauthorized_client", "Client does not support refresh tokens")
			return
		}
		s.exchangeRefresh(w, r, c)
	default:
		oauthError(w, 400, "unsupported_grant_type", "Only authorization_code and refresh_token are supported")
	}
}

type codeRecord struct {
	grantID, clientID, redirect, challenge, resource string
	expires, grantExpires                            int64
	consumed, revoked                                int
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, c client) {
	code, verifier := r.PostForm.Get("code"), r.PostForm.Get("code_verifier")
	if len(code) != 43 || !validVerifier(verifier) {
		oauthError(w, 400, "invalid_grant", "Invalid authorization code or PKCE verifier")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w)
		return
	}
	defer func() { _ = tx.Rollback() }()
	var a codeRecord
	err = tx.QueryRowContext(r.Context(), `SELECT c.grant_id,c.client_id,c.redirect_uri,c.challenge,c.resource,c.expires_at,c.consumed,g.expires_at,g.revoked FROM authorization_codes c JOIN grants g ON g.id=c.grant_id WHERE c.code_hash=?`, digest(code)).Scan(&a.grantID, &a.clientID, &a.redirect, &a.challenge, &a.resource, &a.expires, &a.consumed, &a.grantExpires, &a.revoked)
	now := time.Now().Unix()
	if errors.Is(err, sql.ErrNoRows) || err == nil && (a.clientID != c.ID || a.redirect != r.PostForm.Get("redirect_uri") || a.resource != r.PostForm.Get("resource") || a.expires <= now || a.grantExpires <= now || a.revoked != 0 || subtle.ConstantTimeCompare([]byte(digest(verifier)), []byte(a.challenge)) != 1) {
		oauthError(w, 400, "invalid_grant", "Invalid authorization code or PKCE verifier")
		return
	}
	if err != nil {
		serverError(w)
		return
	}
	if a.consumed != 0 {
		// Correctly bound replay invalidates tokens derived from this code.
		if _, err = tx.ExecContext(r.Context(), `UPDATE grants SET revoked=1 WHERE id=?`, a.grantID); err != nil {
			serverError(w)
			return
		}
		if err = tx.Commit(); err != nil {
			serverError(w)
			return
		}
		oauthError(w, 400, "invalid_grant", "Authorization code has already been used")
		return
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE authorization_codes SET consumed=1 WHERE code_hash=?`, digest(code)); err != nil {
		serverError(w)
		return
	}
	response, err := s.issueTokens(r.Context(), tx, a.grantID, c.ID, a.grantExpires, contains(c.Grants, "refresh_token"))
	if err != nil {
		serverError(w)
		return
	}
	if err = tx.Commit(); err != nil {
		serverError(w)
		return
	}
	writeJSON(w, 200, response)
}

func validVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' || r == '~') {
			return false
		}
	}
	return true
}

func (s *Server) exchangeRefresh(w http.ResponseWriter, r *http.Request, c client) {
	refresh := r.PostForm.Get("refresh_token")
	if len(refresh) != 43 {
		oauthError(w, 400, "invalid_grant", "Invalid refresh token")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w)
		return
	}
	defer func() { _ = tx.Rollback() }()
	var grantID, clientID, resource string
	var expires, grantExpires int64
	var consumed, revoked int
	err = tx.QueryRowContext(r.Context(), `SELECT t.grant_id,t.client_id,t.expires_at,t.consumed,g.resource,g.expires_at,g.revoked FROM refresh_tokens t JOIN grants g ON g.id=t.grant_id WHERE t.token_hash=?`, digest(refresh)).Scan(&grantID, &clientID, &expires, &consumed, &resource, &grantExpires, &revoked)
	now := time.Now().Unix()
	if errors.Is(err, sql.ErrNoRows) || err == nil && (clientID != c.ID || resource != r.PostForm.Get("resource") || expires <= now || grantExpires <= now || revoked != 0) {
		oauthError(w, 400, "invalid_grant", "Invalid refresh token")
		return
	}
	if err != nil {
		serverError(w)
		return
	}
	if consumed != 0 {
		// Public clients have no reusable secret. Rotation retains every spent
		// token until absolute grant expiry; replay atomically revokes the whole
		// family, including the newest access token and pending file transfers.
		if _, err = tx.ExecContext(r.Context(), `UPDATE grants SET revoked=1 WHERE id=?`, grantID); err != nil {
			serverError(w)
			return
		}
		if err = tx.Commit(); err != nil {
			serverError(w)
			return
		}
		oauthError(w, 400, "invalid_grant", "Refresh token replay detected; authorize again")
		return
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE refresh_tokens SET consumed=1 WHERE token_hash=?`, digest(refresh)); err != nil {
		serverError(w)
		return
	}
	response, err := s.issueTokens(r.Context(), tx, grantID, c.ID, grantExpires, true)
	if err != nil {
		serverError(w)
		return
	}
	if err = tx.Commit(); err != nil {
		serverError(w)
		return
	}
	writeJSON(w, 200, response)
}

func (s *Server) issueTokens(ctx context.Context, tx *sql.Tx, grantID, clientID string, grantExpires int64, withRefresh bool) (map[string]any, error) {
	now := time.Now().Unix()
	expires := time.Now().Add(s.opts.AccessTTL).Unix()
	if expires > grantExpires {
		expires = grantExpires
	}
	accessToken := randomToken()
	if _, err := tx.ExecContext(ctx, `INSERT INTO access_tokens(token_hash,grant_id,expires_at) VALUES(?,?,?)`, digest(accessToken), grantID, expires); err != nil {
		return nil, err
	}
	response := map[string]any{"access_token": accessToken, "token_type": "Bearer", "expires_in": expires - now, "scope": "vela"}
	if withRefresh {
		refreshToken := randomToken()
		if _, err := tx.ExecContext(ctx, `INSERT INTO refresh_tokens(token_hash,grant_id,client_id,expires_at) VALUES(?,?,?,?)`, digest(refreshToken), grantID, clientID, grantExpires); err != nil {
			return nil, err
		}
		response["refresh_token"] = refreshToken
	}
	return response, nil
}

// revoke accepts either kind of token and always invalidates its complete
// grant. Unknown tokens deliberately return success as required by RFC 7009.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	sensitive(w)
	if !s.allow(w, r, "revoke", 120, time.Minute) {
		return
	}
	if !parseForm(w, r, 16*1024) {
		return
	}
	if r.Header.Get("Authorization") != "" || r.PostForm.Get("client_secret") != "" {
		oauthError(w, 401, "invalid_client", "This server accepts public clients only")
		return
	}
	c, err := s.lookupClient(r.Context(), r.PostForm.Get("client_id"))
	if err != nil {
		oauthError(w, 400, "invalid_client", "Unknown client")
		return
	}
	token := r.PostForm.Get("token")
	if token == "" {
		oauthError(w, 400, "invalid_request", "Token required")
		return
	}
	if len(token) == 43 {
		_, err = s.db.ExecContext(r.Context(), `UPDATE grants SET revoked=1 WHERE client_id=? AND id IN (SELECT grant_id FROM access_tokens WHERE token_hash=? UNION SELECT grant_id FROM refresh_tokens WHERE token_hash=?)`, c.ID, digest(token), digest(token))
		if err != nil {
			serverError(w)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) registeredClient(ctx context.Context, id string) (client, int64, error) {
	var c client
	var redirects, grants string
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,redirects,grant_types,metadata_expires_at FROM clients WHERE id=?`, id).Scan(&c.ID, &c.Name, &redirects, &grants, &expires)
	if err != nil {
		return client{}, 0, err
	}
	if err = json.Unmarshal([]byte(redirects), &c.Redirects); err != nil {
		return client{}, 0, err
	}
	if err = json.Unmarshal([]byte(grants), &c.Grants); err != nil {
		return client{}, 0, err
	}
	c.AuthMethod = "none"
	c.Responses = []string{"code"}
	c.Scope = "vela"
	return c, expires, nil
}
