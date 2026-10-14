package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela/api"
)

const testKey = "private-user-vela-key-never-print"

type testOAuth struct {
	s    *Server
	mux  *http.ServeMux
	opts Options
}

func newTestOAuth(t *testing.T) *testOAuth {
	t.Helper()
	dir := t.TempDir()
	opts := Options{Issuer: "http://127.0.0.1:8081", DatabasePath: filepath.Join(dir, "oauth.db"), EncryptionKeyFile: filepath.Join(dir, "oauth.key"), VerifyKey: func(_ context.Context, key string) (access.Identity, error) {
		if key != testKey {
			return access.Identity{}, errors.New("secret backend failure " + key)
		}
		return access.Identity{Subject: "upstream-user", Scope: api.ScopeRW, Prefix: "users/alice/", Credential: access.NewCredential(key)}, nil
	}}
	s, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	o := &testOAuth{s: s, opts: opts}
	o.mount()
	t.Cleanup(func() { _ = o.s.Close() })
	return o
}
func (o *testOAuth) mount() { o.mux = http.NewServeMux(); o.s.RegisterRoutes(o.mux) }
func (o *testOAuth) request(method, path, body, contentType string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, o.opts.Issuer+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.20:4321"
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	o.mux.ServeHTTP(w, r)
	return w
}
func (o *testOAuth) register(t *testing.T) string {
	t.Helper()
	w := o.request("POST", "/oauth/register", `{"client_name":"Test connector","redirect_uris":["https://client.example/callback"],"token_endpoint_auth_method":"none"}`, "application/json")
	if w.Code != 201 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result["client_id"].(string)
}

type consent struct {
	transaction, csrf, verifier, clientID string
	cookie                                *http.Cookie
}

func (o *testOAuth) begin(t *testing.T, id string) consent {
	t.Helper()
	verifier := randomToken()
	q := url.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {"https://client.example/callback"}, "state": {"client-state"}, "code_challenge": {digest(verifier)}, "code_challenge_method": {"S256"}, "resource": {o.s.ResourceURL()}, "scope": {"vela"}}
	w := o.request("GET", "/oauth/authorize?"+q.Encode(), "", "")
	if w.Code != 200 {
		t.Fatalf("begin: %d %s", w.Code, w.Body.String())
	}
	field := func(name string) string {
		match := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(match) != 2 {
			t.Fatalf("missing form field %s: %s", name, w.Body.String())
		}
		return match[1]
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing browser cookie")
	}
	return consent{transaction: field("transaction"), csrf: field("csrf"), verifier: verifier, clientID: id, cookie: cookies[0]}
}
func (o *testOAuth) approve(t *testing.T, c consent) string {
	t.Helper()
	form := url.Values{"transaction": {c.transaction}, "csrf": {c.csrf}, "action": {"approve"}, "vela_key": {testKey}}
	w := o.request("POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", c.cookie)
	if w.Code != 303 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("state") != "client-state" {
		t.Fatal("OAuth state not preserved")
	}
	code := u.Query().Get("code")
	if code == "" {
		t.Fatal("no authorization code")
	}
	return code
}
func (o *testOAuth) exchange(t *testing.T, c consent, code string) map[string]any {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {c.clientID}, "code": {code}, "code_verifier": {c.verifier}, "redirect_uri": {"https://client.example/callback"}, "resource": {o.s.ResourceURL()}}
	w := o.request("POST", "/oauth/token", form.Encode(), "application/x-www-form-urlencoded")
	if w.Code != 200 {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("token response is cacheable")
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func (o *testOAuth) flow(t *testing.T) (consent, map[string]any) {
	c := o.begin(t, o.register(t))
	return c, o.exchange(t, c, o.approve(t, c))
}
func (o *testOAuth) refresh(c consent, token string) *httptest.ResponseRecorder {
	return o.request("POST", "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {c.clientID}, "refresh_token": {token}, "resource": {o.s.ResourceURL()}}.Encode(), "application/x-www-form-urlencoded")
}

func TestAuthorizationPersistenceAndEncryptedStorage(t *testing.T) {
	o := newTestOAuth(t)
	_, tokens := o.flow(t)
	token := tokens["access_token"].(string)
	identity, err := o.s.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Subject == "upstream-user" || identity.Subject == "" || identity.Scope != api.ScopeRW || identity.Prefix != "users/alice/" || identity.Credential.Token() != testKey {
		t.Fatalf("wrong identity: %+v", identity)
	}
	var stored, storedAccess string
	if err = o.s.db.QueryRow(`SELECT key_encrypted FROM grants WHERE id=?`, identity.Subject).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, testKey) {
		t.Fatal("plaintext Vela key persisted")
	}
	if err = o.s.db.QueryRow(`SELECT token_hash FROM access_tokens`).Scan(&storedAccess); err != nil {
		t.Fatal(err)
	}
	if storedAccess == token {
		t.Fatal("plaintext access token persisted")
	}
	if err = o.s.Close(); err != nil {
		t.Fatal(err)
	}
	o.s, err = Open(context.Background(), o.opts)
	if err != nil {
		t.Fatal(err)
	}
	o.mount()
	after, err := o.s.Authenticate(context.Background(), token)
	if err != nil || after.Subject != identity.Subject || after.Credential.Token() != testKey {
		t.Fatalf("restart lost authorization: %+v %v", after, err)
	}
	for _, path := range []string{o.opts.DatabasePath, o.opts.EncryptionKeyFile} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe mode on %s: %o", path, info.Mode().Perm())
		}
	}
}

func TestRefreshRotationReplayRevokesFamilyAndTransfers(t *testing.T) {
	o := newTestOAuth(t)
	c, tokens := o.flow(t)
	oldAccess := tokens["access_token"].(string)
	oldRefresh := tokens["refresh_token"].(string)
	identity, err := o.s.Authenticate(context.Background(), oldAccess)
	if err != nil {
		t.Fatal(err)
	}
	w := o.refresh(c, oldRefresh)
	if w.Code != 200 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	var next map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &next)
	if next["refresh_token"] == oldRefresh {
		t.Fatal("refresh token did not rotate")
	}
	if _, err = o.s.Authenticate(context.Background(), next["access_token"].(string)); err != nil {
		t.Fatal(err)
	}
	w = o.refresh(c, oldRefresh)
	if w.Code != 400 {
		t.Fatalf("replay accepted: %d", w.Code)
	}
	for _, token := range []string{oldAccess, next["access_token"].(string)} {
		if _, err = o.s.Authenticate(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("replay left access active: %v", err)
		}
	}
	if _, err = o.s.LookupIdentity(context.Background(), identity.Subject); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replay left transfer authorization active: %v", err)
	}
	if active, err := o.s.IsActive(context.Background(), identity.Subject); err != nil || active {
		t.Fatalf("replayed grant active: %v %v", active, err)
	}
	if w = o.refresh(c, next["refresh_token"].(string)); w.Code != 400 {
		t.Fatal("newest refresh token survived replay")
	}
}

func TestConcurrentRefreshReplayIsTransactional(t *testing.T) {
	o := newTestOAuth(t)
	c, tokens := o.flow(t)
	refresh := tokens["refresh_token"].(string)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- o.refresh(c, refresh).Code }()
	}
	wg.Wait()
	close(codes)
	success, failure := 0, 0
	for code := range codes {
		if code == 200 {
			success++
		}
		if code == 400 {
			failure++
		}
	}
	if success != 1 || failure != 1 {
		t.Fatalf("expected one exchange and one replay rejection: %d %d", success, failure)
	}
	if _, err := o.s.Authenticate(context.Background(), tokens["access_token"].(string)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("concurrent replay did not revoke grant")
	}
}

func TestPKCEAudienceClientAndRedirectBindings(t *testing.T) {
	o := newTestOAuth(t)
	c := o.begin(t, o.register(t))
	code := o.approve(t, c)
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {c.clientID}, "code": {code}, "code_verifier": {c.verifier}, "redirect_uri": {"https://client.example/callback"}, "resource": {o.s.ResourceURL()}}
	for name, bad := range map[string]string{"code_verifier": randomToken(), "client_id": o.register(t), "redirect_uri": "https://attacker.example/callback", "resource": "https://other-resource.example/mcp"} {
		original := form.Get(name)
		form.Set(name, bad)
		w := o.request("POST", "/oauth/token", form.Encode(), "application/x-www-form-urlencoded")
		if w.Code != 400 {
			t.Fatalf("%s binding missing: %d %s", name, w.Code, w.Body.String())
		}
		form.Set(name, original)
	}
	tokens := o.exchange(t, c, code)
	identity, err := o.s.Authenticate(context.Background(), tokens["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	w := o.request("POST", "/oauth/token", form.Encode(), "application/x-www-form-urlencoded")
	if w.Code != 400 {
		t.Fatal("authorization code replay accepted")
	}
	if err = o.s.ValidateIdentity(context.Background(), identity); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("code replay did not revoke derived grant")
	}
}

func TestConsentCSRFAndNoCredentialDisclosure(t *testing.T) {
	o := newTestOAuth(t)
	c := o.begin(t, o.register(t))
	form := url.Values{"transaction": {c.transaction}, "csrf": {c.csrf}, "action": {"approve"}, "vela_key": {testKey}}
	w := o.request("POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded")
	if w.Code != 400 {
		t.Fatal("consent without browser binding accepted")
	}
	form.Set("csrf", randomToken())
	w = o.request("POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", c.cookie)
	if w.Code != 400 {
		t.Fatal("bad CSRF accepted")
	}
	form.Set("csrf", c.csrf)
	r := httptest.NewRequest("POST", o.opts.Issuer+"/oauth/authorize", strings.NewReader(form.Encode()))
	r.AddCookie(c.cookie)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://attacker.example")
	w = httptest.NewRecorder()
	o.mux.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("foreign consent origin accepted")
	}
	form.Set("vela_key", "incorrect-secret-value")
	w = o.request("POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", c.cookie)
	if w.Code != 200 || strings.Contains(w.Body.String(), "incorrect-secret-value") || strings.Contains(w.Body.String(), "secret backend failure") {
		t.Fatal("key verification error leaked credential or backend details")
	}
	_ = o.approve(t, c)
	w = o.request("POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", c.cookie)
	if w.Code != 400 {
		t.Fatal("consent transaction reused")
	}
}

func TestRevocationIsClientBoundAndInvalidatesGrant(t *testing.T) {
	o := newTestOAuth(t)
	c, tokens := o.flow(t)
	token := tokens["access_token"].(string)
	identity, err := o.s.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"client_id": {o.register(t)}, "token": {token}}
	w := o.request("POST", "/oauth/revoke", form.Encode(), "application/x-www-form-urlencoded")
	if w.Code != 200 {
		t.Fatal("foreign client revocation should hide token existence")
	}
	if _, err = o.s.Authenticate(context.Background(), token); err != nil {
		t.Fatal("foreign client revoked another client's grant")
	}
	form.Set("client_id", c.clientID)
	w = o.request("POST", "/oauth/revoke", form.Encode(), "application/x-www-form-urlencoded")
	if w.Code != 200 {
		t.Fatal("revocation failed")
	}
	if _, err = o.s.LookupIdentity(context.Background(), identity.Subject); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("revocation left grant active")
	}
	if w = o.refresh(c, tokens["refresh_token"].(string)); w.Code != 400 {
		t.Fatal("revocation left refresh token usable")
	}
	form.Set("token", randomToken())
	if w = o.request("POST", "/oauth/revoke", form.Encode(), "application/x-www-form-urlencoded"); w.Code != 200 {
		t.Fatal("unknown token revocation is not idempotent")
	}
}

func TestExpiryBoundaries(t *testing.T) {
	o := newTestOAuth(t)
	c, tokens := o.flow(t)
	token := tokens["access_token"].(string)
	identity, err := o.s.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.s.db.Exec(`UPDATE access_tokens SET expires_at=? WHERE token_hash=?`, time.Now().Unix()-1, digest(token))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = o.s.Authenticate(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("expired access token accepted")
	}
	if _, err = o.s.LookupIdentity(context.Background(), identity.Subject); err != nil {
		t.Fatal("expiring one access token unexpectedly revoked grant")
	}
	_, err = o.s.db.Exec(`UPDATE grants SET expires_at=? WHERE id=?`, time.Now().Unix()-1, identity.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = o.s.LookupIdentity(context.Background(), identity.Subject); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("expired grant accepted by transfer broker")
	}
	if w := o.refresh(c, tokens["refresh_token"].(string)); w.Code != 400 {
		t.Fatal("refresh extended an expired grant")
	}
}

func TestMissingAndWrongEncryptionKeysFailClosed(t *testing.T) {
	o := newTestOAuth(t)
	_, _ = o.flow(t)
	if err := o.s.Close(); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(o.opts.EncryptionKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(o.opts.EncryptionKeyFile); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(context.Background(), o.opts); err == nil {
		s.Close()
		t.Fatal("generated replacement key for existing DB")
	}
	if _, err = os.Stat(o.opts.EncryptionKeyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing key recreated despite existing DB")
	}
	if err = os.WriteFile(o.opts.EncryptionKeyFile, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(context.Background(), o.opts); err == nil {
		s.Close()
		t.Fatal("wrong encryption key accepted")
	}
	if err = os.WriteFile(o.opts.EncryptionKeyFile, key, 0600); err != nil {
		t.Fatal(err)
	}
	o.s, err = Open(context.Background(), o.opts)
	if err != nil {
		t.Fatal(err)
	}
	o.mount()
}

func TestRedirectValidationAndDiscovery(t *testing.T) {
	for _, bad := range []string{"http://client.example/callback", "https://user:pass@client.example/cb", "https://client.example/cb#fragment", "javascript:alert(1)", "https://client.example/cb\r\nInjected: x"} {
		if err := validRedirect(bad); err == nil {
			t.Fatalf("unsafe redirect accepted: %q", bad)
		}
	}
	for _, good := range []string{"https://client.example/callback", "http://127.0.0.1:9876/callback", "http://[::1]:9876/callback", "http://localhost:9876/callback"} {
		if err := validRedirect(good); err != nil {
			t.Fatalf("valid redirect rejected: %s %v", good, err)
		}
	}
	o := newTestOAuth(t)
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		w := o.request("GET", path, "", "")
		if w.Code != 200 {
			t.Fatalf("missing metadata %s", path)
		}
		var data map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
	}
	w := o.request("POST", "/oauth/register", `{"redirect_uris":["https://client.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, "application/json")
	if w.Code != 400 {
		t.Fatal("secret-auth client accepted")
	}
	c := o.register(t)
	q := url.Values{"client_id": {c}, "redirect_uri": {"https://attacker.example/cb"}, "response_type": {"code"}, "resource": {o.s.ResourceURL()}}
	w = o.request("GET", "/oauth/authorize?"+q.Encode(), "", "")
	if w.Code != 400 || w.Header().Get("Location") != "" {
		t.Fatal("invalid redirect reflected in OAuth redirect")
	}
	q.Set("redirect_uri", "https://client.example/callback")
	q.Set("code_challenge", digest(randomToken()))
	q.Set("code_challenge_method", "plain")
	w = o.request("GET", "/oauth/authorize?"+q.Encode(), "", "")
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "invalid_request") {
		t.Fatal("plain PKCE allowed")
	}
}

func TestClientMetadataSSRFProtection(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "::1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "169.254.169.254", "100.100.100.200", "0.0.0.0", "192.0.2.10", "198.18.0.1", "2001:db8::1", "fc00::1", "::ffff:127.0.0.1", "64:ff9b::a00:1"} {
		if publicAddress(netip.MustParseAddr(address)) {
			t.Fatalf("non-public address accepted: %s", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicAddress(netip.MustParseAddr(address)) {
			t.Fatalf("public address rejected: %s", address)
		}
	}
	if _, err := fetchClientMetadata(context.Background(), "https://127.0.0.1:9/client.json"); err == nil || !strings.Contains(err.Error(), "public IP") {
		t.Fatal("private metadata destination was not rejected before network access")
	}
}

func TestChangingIssuerCannotReuseAudienceBoundTokensOrTransfers(t *testing.T) {
	o := newTestOAuth(t)
	c, tokens := o.flow(t)
	identity, err := o.s.Authenticate(context.Background(), tokens["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if err = o.s.Close(); err != nil {
		t.Fatal(err)
	}
	o.opts.Issuer = "http://127.0.0.1:8082"
	o.s, err = Open(context.Background(), o.opts)
	if err != nil {
		t.Fatal(err)
	}
	o.mount()
	if _, err = o.s.Authenticate(context.Background(), tokens["access_token"].(string)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("token from previous audience accepted")
	}
	if _, err = o.s.LookupIdentity(context.Background(), identity.Subject); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("previous audience grant accepted by transfer service")
	}
	if active, err := o.s.IsActive(context.Background(), identity.Subject); err != nil || active {
		t.Fatal("previous audience grant still active")
	}
	if w := o.refresh(c, tokens["refresh_token"].(string)); w.Code != 400 {
		t.Fatal("refresh token changed audience")
	}
}

func TestIssuerAndBrowserOriginsAreCanonical(t *testing.T) {
	o := newTestOAuth(t)
	if err := o.s.Close(); err != nil {
		t.Fatal(err)
	}
	o.opts.Issuer = "https://EXAMPLE.COM:443/"
	var err error
	o.s, err = Open(context.Background(), o.opts)
	if err != nil {
		t.Fatal(err)
	}
	if o.s.ResourceURL() != "https://example.com/mcp" {
		t.Fatal("issuer was not canonicalized")
	}
	r := httptest.NewRequest("POST", "https://example.com/oauth/authorize", nil)
	r.Header.Set("Origin", "https://example.com")
	r.Header.Set("Referer", "https://EXAMPLE.COM:443/oauth/authorize")
	if !o.s.sameOrigin(r) {
		t.Fatal("equivalent browser origin rejected")
	}
	r.Header.Set("Origin", "https://example.com:8443")
	if o.s.sameOrigin(r) {
		t.Fatal("different origin port accepted")
	}
	for _, issuer := range []string{"https://example.com/prefix", "http://remote.example", "https://user:password@example.com", "https://example.com?query=1"} {
		opts := o.opts
		opts.Issuer = issuer
		if s, err := Open(context.Background(), opts); err == nil {
			s.Close()
			t.Fatalf("unsafe or unsupported issuer accepted: %s", issuer)
		}
	}
}

func TestNativeLoopbackRedirectPortException(t *testing.T) {
	registered := []string{"http://localhost/callback?client=claude", "http://127.0.0.1/callback", "http://[::1]:4321/callback", "https://client.example/callback"}
	for _, uri := range []string{"http://localhost:3118/callback?client=claude", "http://127.0.0.1:4567/callback", "http://[::1]:5678/callback", "https://client.example/callback"} {
		if !redirectAllowed(registered, uri) {
			t.Fatalf("valid ephemeral callback port rejected: %s", uri)
		}
	}
	for _, uri := range []string{"http://localhost:3118/other?client=claude", "http://localhost:3118/callback?client=attacker", "http://localhost:3118/callback", "http://127.0.0.2:3118/callback", "http://evil.example:3118/callback", "https://localhost:3118/callback?client=claude", "https://client.example:8443/callback", "http://localhost:99999/callback?client=claude"} {
		if redirectAllowed(registered, uri) {
			t.Fatalf("redirect drift accepted: %s", uri)
		}
	}
	if redirectAllowed([]string{"http://localhost/callback"}, "http://127.0.0.1:3118/callback") {
		t.Fatal("localhost was aliased to IP literal")
	}
	if redirectAllowed([]string{"http://127.0.0.1/callback"}, "http://localhost:3118/callback") {
		t.Fatal("IP literal was aliased to localhost")
	}

	o := newTestOAuth(t)
	w := o.request("POST", "/oauth/register", `{"redirect_uris":["http://127.0.0.1/callback"]}`, "application/json")
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var registration map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &registration)
	id := registration["client_id"].(string)
	verifier := randomToken()
	redirect := "http://127.0.0.1:3118/callback"
	q := url.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {redirect}, "code_challenge": {digest(verifier)}, "code_challenge_method": {"S256"}, "resource": {o.s.ResourceURL()}}
	w = o.request("GET", "/oauth/authorize?"+q.Encode(), "", "")
	if w.Code != 200 {
		t.Fatalf("native authorization rejected: %d %s", w.Code, w.Body.String())
	}
	field := func(name string) string {
		return regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindStringSubmatch(w.Body.String())[1]
	}
	form := url.Values{"transaction": {field("transaction")}, "csrf": {field("csrf")}, "action": {"approve"}, "vela_key": {testKey}}
	w = o.request("POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", w.Result().Cookies()[0])
	if w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	u, _ := url.Parse(w.Header().Get("Location"))
	code := u.Query().Get("code")
	form = url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {"http://127.0.0.1:3119/callback"}, "resource": {o.s.ResourceURL()}}
	w = o.request("POST", "/oauth/token", form.Encode(), "application/x-www-form-urlencoded")
	if w.Code != 400 {
		t.Fatal("token exchange accepted different concrete callback port")
	}
	form.Set("redirect_uri", redirect)
	w = o.request("POST", "/oauth/token", form.Encode(), "application/x-www-form-urlencoded")
	if w.Code != 200 {
		t.Fatalf("exact native code exchange failed: %d %s", w.Code, w.Body.String())
	}
}

func TestConsentCSPAllowsOnlyValidatedCallbackOrigin(t *testing.T) {
	o := newTestOAuth(t)
	w := httptest.NewRecorder()
	o.s.renderConsent(w, consentPage{Client: "Attacker; script-src *", ClientID: "id", Redirect: "https://client.example:8443/callback?value=;script-src%20*", Action: "/oauth/authorize"})
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' https://client.example:8443;") {
		t.Fatalf("callback redirect will be blocked: %s", csp)
	}
	if strings.Contains(csp, "script-src") || strings.Contains(csp, "callback") || strings.Contains(csp, "value=") {
		t.Fatalf("non-origin input reached CSP: %s", csp)
	}
	if callbackCSPOrigin("http://127.0.0.1:3118/callback") != "http://127.0.0.1:3118" {
		t.Fatal("native callback port missing in CSP")
	}
	for _, redirect := range []string{"https://client.example;default-src/callback", "https://client.example,evil.example/callback", "https://*.example/callback", "http://evil.example/callback", "https://client.example/callback\r\nInjected: value", "https://user:pass@client.example/callback", "https://client.example/callback#fragment"} {
		if origin := callbackCSPOrigin(redirect); origin != "" {
			t.Fatalf("unsafe CSP callback source accepted: %q", origin)
		}
	}
}
