package oauth

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// RegisterRoutes mounts OAuth discovery and protocol endpoints. Authentication
// of the /mcp resource remains the transport adapter's responsibility.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+s.prefix+"/.well-known/oauth-authorization-server", s.authorizationMetadata)
	mux.HandleFunc("GET "+s.prefix+"/.well-known/oauth-protected-resource", s.resourceMetadata)
	mux.HandleFunc("GET "+s.prefix+"/.well-known/oauth-protected-resource/mcp", s.resourceMetadata)
	mux.HandleFunc("POST "+s.prefix+"/oauth/register", s.register)
	mux.HandleFunc("GET "+s.prefix+"/oauth/authorize", s.authorize)
	mux.HandleFunc("POST "+s.prefix+"/oauth/authorize", s.consent)
	mux.HandleFunc("POST "+s.prefix+"/oauth/token", s.token)
	mux.HandleFunc("POST "+s.prefix+"/oauth/revoke", s.revoke)
}

func (s *Server) authorizationMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": s.opts.Issuer, "authorization_endpoint": s.opts.Issuer + "/oauth/authorize", "token_endpoint": s.opts.Issuer + "/oauth/token", "registration_endpoint": s.opts.Issuer + "/oauth/register", "revocation_endpoint": s.opts.Issuer + "/oauth/revoke",
		"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "revocation_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{"vela"}, "client_id_metadata_document_supported": true,
	})
}
func (s *Server) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"resource": s.ResourceURL(), "authorization_servers": []string{s.opts.Issuer}, "scopes_supported": []string{"vela"}, "bearer_methods_supported": []string{"header"}, "resource_name": "Vela MCP"})
}

type client struct {
	ID         string   `json:"client_id"`
	Name       string   `json:"client_name"`
	Redirects  []string `json:"redirect_uris"`
	AuthMethod string   `json:"token_endpoint_auth_method"`
	Grants     []string `json:"grant_types"`
	Responses  []string `json:"response_types"`
	Scope      string   `json:"scope,omitempty"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	sensitive(w)
	if !s.allow(w, r, "register", 10, time.Hour) {
		return
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		oauthError(w, 400, "invalid_client_metadata", "JSON content type required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	var c client
	d := json.NewDecoder(r.Body)
	if err := d.Decode(&c); err != nil {
		oauthError(w, 400, "invalid_client_metadata", "Invalid registration document")
		return
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		oauthError(w, 400, "invalid_client_metadata", "Expected one registration document")
		return
	}
	if err := s.validateClient(&c); err != nil {
		oauthError(w, 400, "invalid_client_metadata", err.Error())
		return
	}
	c.ID = randomToken()
	redirects, _ := json.Marshal(c.Redirects)
	grants, _ := json.Marshal(c.Grants)
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w)
		return
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM clients`).Scan(&count); err != nil {
		serverError(w)
		return
	}
	if count >= s.opts.MaxClients {
		oauthError(w, 429, "temporarily_unavailable", "Client registration capacity reached")
		return
	}
	created := time.Now().Unix()
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO clients(id,name,redirects,grant_types,created_at) VALUES(?,?,?,?,?)`, c.ID, c.Name, string(redirects), string(grants), created); err != nil {
		serverError(w)
		return
	}
	if err = tx.Commit(); err != nil {
		serverError(w)
		return
	}
	writeJSON(w, 201, map[string]any{"client_id": c.ID, "client_id_issued_at": created, "client_name": c.Name, "redirect_uris": c.Redirects, "token_endpoint_auth_method": "none", "grant_types": c.Grants, "response_types": []string{"code"}, "scope": "vela"})
}

func (s *Server) validateClient(c *client) error {
	if c.AuthMethod != "" && c.AuthMethod != "none" {
		return errors.New("Only public clients with token_endpoint_auth_method=none are supported")
	}
	c.AuthMethod = "none"
	if c.Name == "" {
		c.Name = "MCP client"
	}
	if len(c.Name) > 128 || strings.ContainsAny(c.Name, "\r\n\x00") {
		return errors.New("Invalid client name")
	}
	if len(c.Redirects) == 0 || len(c.Redirects) > 10 {
		return errors.New("One to ten exact redirect URIs are required")
	}
	seen := map[string]bool{}
	for _, redirect := range c.Redirects {
		if err := validRedirect(redirect); err != nil {
			return err
		}
		if seen[redirect] {
			return errors.New("Duplicate redirect URI")
		}
		seen[redirect] = true
		if len(s.opts.AllowedRedirectURIs) > 0 && !contains(s.opts.AllowedRedirectURIs, redirect) {
			return errors.New("Redirect URI is not in the administrator allowlist")
		}
	}
	if len(c.Grants) == 0 {
		c.Grants = []string{"authorization_code", "refresh_token"}
	}
	for _, g := range c.Grants {
		if g != "authorization_code" && g != "refresh_token" {
			return errors.New("Unsupported grant type")
		}
	}
	if !contains(c.Grants, "authorization_code") {
		return errors.New("authorization_code grant is required")
	}
	if len(c.Responses) == 0 {
		c.Responses = []string{"code"}
	}
	if len(c.Responses) != 1 || c.Responses[0] != "code" {
		return errors.New("Only the code response type is supported")
	}
	if c.Scope != "" && c.Scope != "vela" {
		return errors.New("Only the vela scope is supported")
	}
	return nil
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	sensitive(w)
	if !s.allow(w, r, "authorize", 30, time.Minute) {
		return
	}
	q := r.URL.Query()
	if hasDuplicates(q) {
		http.Error(w, "Repeated authorization parameters", 400)
		return
	}
	c, err := s.lookupClient(r.Context(), q.Get("client_id"))
	if err != nil {
		http.Error(w, "Unknown or invalid OAuth client", 400)
		return
	}
	redirect := q.Get("redirect_uri")
	if !redirectAllowed(c.Redirects, redirect) {
		http.Error(w, "Redirect URI does not match client registration", 400)
		return
	}
	state := q.Get("state")
	if len(state) > 2048 {
		http.Error(w, "Authorization state is too long", 400)
		return
	}
	if q.Get("response_type") != "code" {
		redirectError(w, r, redirect, state, "unsupported_response_type", "Only authorization code is supported")
		return
	}
	if q.Get("resource") != s.ResourceURL() {
		redirectError(w, r, redirect, state, "invalid_target", "The exact MCP resource audience is required")
		return
	}
	if q.Get("scope") != "" && q.Get("scope") != "vela" {
		redirectError(w, r, redirect, state, "invalid_scope", "Only the vela scope is supported")
		return
	}
	challenge := q.Get("code_challenge")
	bytes, err := base64.RawURLEncoding.DecodeString(challenge)
	if q.Get("code_challenge_method") != "S256" || err != nil || len(bytes) != 32 || len(challenge) != 43 {
		redirectError(w, r, redirect, state, "invalid_request", "S256 PKCE is required")
		return
	}
	browser := randomToken()
	if cookie, err := r.Cookie(s.cookieName()); err == nil && len(cookie.Value) == 43 {
		browser = cookie.Value
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: browser, Path: "/", MaxAge: 600, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	id, csrf := randomToken(), randomToken()
	if _, err = s.db.ExecContext(r.Context(), `INSERT INTO authorization_transactions(id_hash,client_id,redirect_uri,state,challenge,resource,browser_hash,csrf_hash,expires_at) VALUES(?,?,?,?,?,?,?,?,?)`, digest(id), c.ID, redirect, state, challenge, s.ResourceURL(), digest(browser), digest(csrf), time.Now().Add(10*time.Minute).Unix()); err != nil {
		serverError(w)
		return
	}
	s.renderConsent(w, consentPage{Client: c.Name, ClientID: c.ID, Redirect: redirect, Transaction: id, CSRF: csrf, Action: s.prefix + "/oauth/authorize"})
}

type authorization struct {
	clientID, redirect, state, challenge, resource, browserHash, csrfHash string
	expires                                                               int64
	consumed                                                              int
}

func (s *Server) consent(w http.ResponseWriter, r *http.Request) {
	sensitive(w)
	if !s.allow(w, r, "consent", 10, time.Minute) {
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "Invalid authorization origin", 403)
		return
	}
	if !parseForm(w, r, 16*1024) {
		return
	}
	id, csrf := r.PostForm.Get("transaction"), r.PostForm.Get("csrf")
	if len(id) != 43 || len(csrf) != 43 {
		http.Error(w, "Invalid authorization transaction", 400)
		return
	}
	var a authorization
	err := s.db.QueryRowContext(r.Context(), `SELECT client_id,redirect_uri,state,challenge,resource,browser_hash,csrf_hash,expires_at,consumed FROM authorization_transactions WHERE id_hash=?`, digest(id)).Scan(&a.clientID, &a.redirect, &a.state, &a.challenge, &a.resource, &a.browserHash, &a.csrfHash, &a.expires, &a.consumed)
	cookie, cookieErr := r.Cookie(s.cookieName())
	if err != nil || cookieErr != nil || a.resource != s.ResourceURL() || a.expires <= time.Now().Unix() || a.consumed != 0 || subtle.ConstantTimeCompare([]byte(digest(cookie.Value)), []byte(a.browserHash)) != 1 || subtle.ConstantTimeCompare([]byte(digest(csrf)), []byte(a.csrfHash)) != 1 {
		http.Error(w, "Invalid or expired authorization transaction", 400)
		return
	}
	c, err := s.lookupClient(r.Context(), a.clientID)
	if err != nil || !redirectAllowed(c.Redirects, a.redirect) {
		http.Error(w, "Client registration is no longer valid", 400)
		return
	}
	if r.PostForm.Get("action") == "deny" {
		result, err := s.db.ExecContext(r.Context(), `UPDATE authorization_transactions SET consumed=1 WHERE id_hash=? AND consumed=0`, digest(id))
		if err != nil {
			serverError(w)
			return
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			http.Error(w, "Authorization transaction already used", 400)
			return
		}
		redirectError(w, r, a.redirect, a.state, "access_denied", "Authorization was declined")
		return
	}
	if r.PostForm.Get("action") != "approve" {
		http.Error(w, "Invalid consent action", 400)
		return
	}
	key := r.PostForm.Get("vela_key")
	if key == "" || len(key) > 8192 {
		s.renderConsent(w, consentPage{Client: c.Name, ClientID: c.ID, Redirect: a.redirect, Transaction: id, CSRF: csrf, Action: s.prefix + "/oauth/authorize", Error: "Enter a valid Vela API key."})
		return
	}
	identity, err := s.opts.VerifyKey(r.Context(), key)
	if err != nil || !identity.Scope.Valid() {
		s.renderConsent(w, consentPage{Client: c.Name, ClientID: c.ID, Redirect: a.redirect, Transaction: id, CSRF: csrf, Action: s.prefix + "/oauth/authorize", Error: "The Vela API key could not be verified. Check the key and try again."})
		return
	}
	// Only the credential submitted in this transaction is persisted. The
	// verifier supplies effective authority, never a shared backend credential.
	grantID, code := randomToken(), randomToken()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `UPDATE authorization_transactions SET consumed=1 WHERE id_hash=? AND consumed=0 AND expires_at>?`, digest(id), time.Now().Unix())
	if err != nil {
		serverError(w)
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		http.Error(w, "Authorization transaction already used or expired", 400)
		return
	}
	now := time.Now()
	lifetime := s.opts.RefreshTTL
	if !contains(c.Grants, "refresh_token") {
		lifetime = s.opts.AccessTTL
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO grants(id,client_id,resource,key_encrypted,api_scope,path_prefix,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`, grantID, c.ID, a.resource, s.encrypt([]byte(key), grantID), string(identity.Scope), identity.Prefix, now.Unix(), now.Add(lifetime).Unix()); err != nil {
		serverError(w)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO authorization_codes(code_hash,grant_id,client_id,redirect_uri,challenge,resource,expires_at) VALUES(?,?,?,?,?,?,?)`, digest(code), grantID, c.ID, a.redirect, a.challenge, a.resource, now.Add(s.opts.CodeTTL).Unix()); err != nil {
		serverError(w)
		return
	}
	if err = tx.Commit(); err != nil {
		serverError(w)
		return
	}
	u, _ := url.Parse(a.redirect)
	q := u.Query()
	q.Set("code", code)
	if a.state != "" {
		q.Set("state", a.state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (s *Server) cookieName() string {
	if s.secure {
		return "__Host-vela_oauth"
	}
	return "vela_oauth"
}
func (s *Server) sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || originOf(u) != s.origin {
			return false
		}
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil || u.User != nil || originOf(u) != s.origin {
			return false
		}
	}
	return true
}

type consentPage struct{ Client, ClientID, Redirect, Transaction, CSRF, Action, Error string }

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connect to Vela</title><style>body{font:16px system-ui,sans-serif;background:#f3f5f8;color:#17212b;padding:3rem 1rem}main{max-width:36rem;margin:auto;background:white;border-radius:12px;padding:2rem;box-shadow:0 4px 24px #0001}h1{margin-top:0}label{display:block;font-weight:600;margin:1.5rem 0 .5rem}input{box-sizing:border-box;width:100%;padding:.8rem;border:1px solid #8b96a3;border-radius:6px}button{padding:.8rem 1.2rem;border:0;border-radius:6px;margin:1rem .5rem 0 0;background:#1461ba;color:white;cursor:pointer}button.secondary{background:#e8edf3;color:#17212b}.error{color:#a21c28}small,code{overflow-wrap:anywhere}small{display:block;color:#52606d;margin:.5rem 0}p{line-height:1.5}</style></head><body><main><h1>Connect to Vela</h1><p><strong>{{.Client}}</strong> requests access to your Vela files.</p><small>Client ID: {{.ClientID}}</small><small>Return address: {{.Redirect}}</small><p>Enter your own Vela API key to approve. The connector receives only the permissions and path prefix allowed by that key. This server encrypts the key in its local database and uses it for your requests. The connector does not receive the key.</p>{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}<form method="post" action="{{.Action}}"><input type="hidden" name="transaction" value="{{.Transaction}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><label for="vela-key">Vela API key</label><input type="password" id="vela-key" name="vela_key" autocomplete="off" spellcheck="false" maxlength="8192"><button type="submit" name="action" value="approve">Authorize connector</button><button class="secondary" type="submit" name="action" value="deny">Cancel</button></form><small>Access can be revoked through the OAuth revocation endpoint or by revoking your Vela key.</small></main></body></html>`))

func (s *Server) renderConsent(w http.ResponseWriter, p consentPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Chromium applies form-action to redirects after a form submission too.
	// Permit the validated client's callback origin so the successful consent
	// POST can return its OAuth 303, while keeping forms restricted elsewhere.
	// Only an origin enters this header: query strings and client labels never do.
	formAction := "'self'"
	if origin := callbackCSPOrigin(p.Redirect); origin != "" {
		formAction += " " + origin
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action "+formAction+"; base-uri 'none'; frame-ancestors 'none'")
	_ = consentTemplate.Execute(w, p)
}

func callbackCSPOrigin(redirect string) string {
	if validRedirect(redirect) != nil {
		return ""
	}
	u, err := url.Parse(redirect)
	if err != nil {
		return ""
	}
	origin := originOf(u)
	// A URL authority must be safe as one CSP source expression. Fail closed
	// for control characters, separators, quotes, wildcards, and non-ASCII DNS
	// spellings; production IDNs should use their ordinary punycode hostname.
	for _, r := range origin {
		if r <= 32 || r >= 127 || strings.ContainsRune(";,'\"\\`*()", r) {
			return ""
		}
	}
	return origin
}

func sensitive(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
func serverError(w http.ResponseWriter) {
	oauthError(w, 500, "server_error", "Authorization server temporarily unavailable")
}
func redirectError(w http.ResponseWriter, r *http.Request, redirect, state, code, description string) {
	u, _ := url.Parse(redirect)
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func hasDuplicates(v url.Values) bool {
	for _, values := range v {
		if len(values) != 1 {
			return true
		}
	}
	return false
}
func parseForm(w http.ResponseWriter, r *http.Request, max int64) bool {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/x-www-form-urlencoded") {
		oauthError(w, 400, "invalid_request", "Form content type required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	if err := r.ParseForm(); err != nil || hasDuplicates(r.PostForm) {
		oauthError(w, 400, "invalid_request", "Invalid or repeated form parameters")
		return false
	}
	return true
}

type rateEntry struct {
	start time.Time
	count int
}
type rateLimiter struct {
	mu      sync.Mutex
	entries map[string]rateEntry
}

func (s *Server) allow(w http.ResponseWriter, r *http.Request, endpoint string, limit int, window time.Duration) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	key := endpoint + ":" + host
	lim := &s.limiter
	lim.mu.Lock()
	defer lim.mu.Unlock()
	if lim.entries == nil {
		lim.entries = map[string]rateEntry{}
	}
	now := time.Now()
	entry := lim.entries[key]
	if now.Sub(entry.start) >= window {
		entry = rateEntry{start: now}
	}
	if entry.count >= limit {
		w.Header().Set("Retry-After", "60")
		oauthError(w, 429, "temporarily_unavailable", "Too many requests; try again later")
		return false
	}
	if len(lim.entries) >= 4096 {
		for k, v := range lim.entries {
			if now.Sub(v.start) > time.Hour {
				delete(lim.entries, k)
			}
		}
		if len(lim.entries) >= 4096 {
			oauthError(w, 429, "temporarily_unavailable", "Too many authorization callers")
			return false
		}
	}
	entry.count++
	lim.entries[key] = entry
	return true
}
