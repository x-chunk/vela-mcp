package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/oauth"
)

type testOAuth struct{ calls atomic.Int64 }

func (a *testOAuth) MetadataURL() string {
	return "https://mcp.example.com/.well-known/oauth-protected-resource/mcp"
}
func (a *testOAuth) Authenticate(_ context.Context, token string) (access.Identity, error) {
	a.calls.Add(1)
	if token != "alice" && token != "bob" {
		return access.Identity{}, errors.New("invalid token")
	}
	return access.Identity{Subject: token}, nil
}
func (a *testOAuth) RegisterRoutes(mux *http.ServeMux) {
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server", "/oauth/token", "/oauth/register", "/oauth/revoke", "/oauth/authorize"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	}
}

type testIdentity struct {
	Subject string `json:"subject"`
}

func testOptions() Options {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "identity", Description: "Read the authenticated identity"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, testIdentity, error) {
		i, err := (access.ContextResolver{}).Resolve(ctx)
		return nil, testIdentity{Subject: i.Subject}, err
	})
	return Options{Issuer: "https://mcp.example.com", Listen: "127.0.0.1:8080", MCP: s}
}

func request(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://mcp.example.com"+path, strings.NewReader(body))
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEveryMCPMethodAuthenticatesBeforeTransport(t *testing.T) {
	auth := &testOAuth{}
	h, err := handler(testOptions(), auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodHead} {
		w := request(t, h, method, "/mcp", "", nil)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), auth.MetadataURL()) {
			t.Fatalf("unauthenticated %s: %d, %s", method, w.Code, w.Body.String())
		}
		w = request(t, h, method, "/mcp", "", map[string]string{"Authorization": "Bearer invalid"})
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
			t.Fatalf("invalid bearer %s: %d, %s", method, w.Code, w.Body.String())
		}
	}
	for _, name := range []string{"access_token", "oauth_token", "token"} {
		w := request(t, h, http.MethodGet, "/mcp?"+name+"=alice", "", map[string]string{"Authorization": "Bearer alice"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("query credential %s accepted: %d", name, w.Code)
		}
	}
	for _, header := range []string{"Bearer", "Basic alice", "Bearer alice extra", "Bearer " + strings.Repeat("x", 4097)} {
		if w := request(t, h, http.MethodGet, "/mcp", "", map[string]string{"Authorization": header}); w.Code != http.StatusUnauthorized {
			t.Fatalf("invalid Authorization accepted: %d", w.Code)
		}
	}
	if w := request(t, h, http.MethodGet, "/mcp", "", map[string]string{"Authorization": "Bearer alice"}); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("authenticated stateless GET: %d", w.Code)
	}
}

func TestStatelessSessionsCarryOnlyCurrentRequestIdentity(t *testing.T) {
	auth := &testOAuth{}
	h, err := handler(testOptions(), auth)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		for n := 0; n < 16; n++ {
			group.Add(1)
			go func(version string, n int) {
				defer group.Done()
				token := "alice"
				if n%2 == 1 {
					token = "bob"
				}
				params := map[string]any{"name": "identity", "arguments": map[string]any{}}
				if version == "2026-07-28" {
					params["_meta"] = map[string]any{
						mcp.MetaKeyProtocolVersion:                   version,
						"io.modelcontextprotocol/clientCapabilities": map[string]any{},
						"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test", "version": "1"},
					}
				}
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": n + 1, "method": "tools/call", "params": params})
				w := request(t, h, http.MethodPost, "/mcp", string(body), map[string]string{
					"Authorization": "Bearer " + token, "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
					"MCP-Protocol-Version": version, "MCP-Session-Id": "same-attacker-chosen-session",
					"Mcp-Method": "tools/call", "Mcp-Name": "identity",
				})
				var response struct {
					Result struct {
						Identity testIdentity `json:"structuredContent"`
					} `json:"result"`
				}
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Result.Identity.Subject != token {
					t.Errorf("%s current identity %s: %d, %s", version, token, w.Code, w.Body.String())
				}
				if w.Header().Get("MCP-Session-Id") != "" {
					t.Error("stateless response exposed a persistent session")
				}
			}(version, n)
		}
	}
	group.Wait()
	if auth.calls.Load() != 32 {
		t.Fatalf("authenticated %d of 32 requests", auth.calls.Load())
	}
}

func TestHostAllowlistDoesNotTrustForwardedHeaders(t *testing.T) {
	h, err := handler(testOptions(), &testOAuth{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host string
		want int
	}{
		{"mcp.example.com", 200}, {"MCP.EXAMPLE.COM:443", 200}, {"127.0.0.1:8080", 200},
		{"mcp.example.com:80", 403}, {"127.0.0.1:8081", 403}, {"localhost:8080", 403},
		{"attacker.example", 403}, {"mcp.example.com.attacker.example", 403}, {"mcp.example.com,attacker.example", 403},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://localhost/healthz", nil)
		r.Host = tc.host
		r.Header.Set("X-Forwarded-Host", "mcp.example.com")
		r.Header.Set("Forwarded", "host=mcp.example.com;proto=https")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("Host %s: %d, want %d", tc.host, w.Code, tc.want)
		}
	}
	opts := testOptions()
	opts.Listen = "0.0.0.0:8080"
	h, err = handler(opts, &testOAuth{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("wildcard bind implicitly trusted a local Host")
	}
}

func TestLoopbackProxyMayPreservePublicHost(t *testing.T) {
	h, err := handler(testOptions(), &testOAuth{})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"identity","arguments":{}}}`
	r := httptest.NewRequest(http.MethodPost, "http://mcp.example.com/mcp", strings.NewReader(body))
	r.Host = "mcp.example.com"
	r.Header.Set("Authorization", "Bearer alice")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-11-25")
	// net/http supplies this value on a real socket. The SDK's default local
	// protection would reject a public Host on this loopback connection.
	ctx := context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(ctx))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"subject":"alice"`) {
		t.Fatalf("public Host through loopback proxy: %d, %s", w.Code, w.Body.String())
	}
}

func TestOriginsAndPublicOAuthCORS(t *testing.T) {
	opts := testOptions()
	opts.AllowedOrigins = []string{"https://client.example"}
	h, err := handler(opts, &testOAuth{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, origin string
		want         int
		cors         string
	}{
		{"/healthz", "", 200, ""}, {"/healthz", "https://mcp.example.com", 200, "https://mcp.example.com"},
		{"/healthz", "https://client.example", 200, "https://client.example"}, {"/healthz", "https://attacker.example", 403, ""},
		{"/mcp", "https://attacker.example", 403, ""}, {"/oauth/authorize", "https://attacker.example", 403, ""},
		{"/transfers/download/ticket", "https://attacker.example", 403, ""},
		{"/oauth/token", "https://client-other.example", 200, "*"}, {"/oauth/register", "http://localhost:3000", 200, "*"},
		{"/.well-known/oauth-protected-resource/mcp", "https://client-other.example", 200, "*"},
		{"/.well-known/oauth-authorization-server", "null", 403, ""}, {"/oauth/token", "http://attacker.example", 403, ""},
		{"/healthz", "https://mcp.example.com/path", 403, ""}, {"/healthz", "https://user@mcp.example.com", 403, ""},
	} {
		headers := map[string]string{}
		if tc.origin != "" {
			headers["Origin"] = tc.origin
		}
		w := request(t, h, http.MethodGet, tc.path, "", headers)
		if w.Code != tc.want || w.Header().Get("Access-Control-Allow-Origin") != tc.cors {
			t.Errorf("%s Origin %q: %d, CORS %q", tc.path, tc.origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"))
		}
		if w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("cross-origin cookies enabled")
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://mcp.example.com/healthz", nil)
	r.Header.Add("Origin", "https://mcp.example.com")
	r.Header.Add("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("multiple Origins accepted")
	}
}

func TestCORSPreflightDoesNotRequireBearer(t *testing.T) {
	auth := &testOAuth{}
	h, err := handler(testOptions(), auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, origin, method string
		want                 int
	}{
		{"/mcp", "https://mcp.example.com", "POST", 204},
		{"/oauth/register", "https://client.example", "POST", 204},
		{"/transfers/downloads/ticket", "https://mcp.example.com", "GET", 204},
		{"/transfers/uploads/ticket", "https://mcp.example.com", "PUT", 204},
		{"/transfers/downloads/ticket", "https://mcp.example.com", "PUT", 405},
		{"/mcp", "https://mcp.example.com", "PUT", 405},
		{"/mcp", "https://attacker.example", "POST", 403},
		{"/oauth/authorize", "https://attacker.example", "POST", 403},
		{"/mcp", "", "POST", 400},
		{"/missing", "https://mcp.example.com", "POST", 404},
	} {
		headers := map[string]string{"Access-Control-Request-Method": tc.method}
		if tc.origin != "" {
			headers["Origin"] = tc.origin
		}
		w := request(t, h, http.MethodOptions, tc.path, "", headers)
		if w.Code != tc.want {
			t.Errorf("%s %s preflight: %d, want %d", tc.origin, tc.path, w.Code, tc.want)
		}
	}
	if auth.calls.Load() != 0 {
		t.Fatal("preflight attempted OAuth token validation")
	}
}

func TestMCPBodyLimitAndIndependentStreamingTransfers(t *testing.T) {
	opts := testOptions()
	opts.MCPMaxBodyBytes = 64
	opts.Transfers = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, r.Body) })
	h, err := handler(opts, &testOAuth{})
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, h, http.MethodPost, "/mcp", strings.Repeat("x", 65), map[string]string{"Authorization": "Bearer alice", "Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("MCP body limit: %d, %s", w.Code, w.Body.String())
	}
	body := strings.Repeat("bytes", 10000)
	w = request(t, h, http.MethodPut, "/transfers/upload/ticket", body, nil)
	if w.Code != http.StatusOK || w.Body.String() != body {
		t.Fatal("MCP size limit or buffering changed a transfer stream")
	}
	for _, header := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options", "Referrer-Policy", "Cache-Control"} {
		if w.Header().Get(header) == "" {
			t.Errorf("missing security header %s", header)
		}
	}
}

func TestConfigurationValidation(t *testing.T) {
	for _, issuer := range []string{"", "http://external.example", "https://mcp.example.com/prefix", "https://user@mcp.example.com", "https://mcp.example.com?query=1", "https://mcp.example.com#fragment"} {
		opts := testOptions()
		opts.Issuer = issuer
		if _, err := handler(opts, &testOAuth{}); err == nil {
			t.Errorf("invalid issuer %q accepted", issuer)
		}
	}
	for _, mutate := range []func(*Options){
		func(o *Options) { o.Listen = "127.0.0.1" },
		func(o *Options) { o.MCP = nil },
		func(o *Options) { o.MCPMaxBodyBytes = -1 },
		func(o *Options) { o.RequestTimeout = time.Millisecond },
		func(o *Options) { o.AllowedOrigins = []string{"null"} },
	} {
		opts := testOptions()
		mutate(&opts)
		if _, err := handler(opts, &testOAuth{}); err == nil {
			t.Error("invalid HTTP configuration accepted")
		}
	}
	for _, issuer := range []string{"https://mcp.example.com/", "http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost:8080"} {
		opts := testOptions()
		opts.Issuer = issuer
		if _, err := handler(opts, &testOAuth{}); err != nil {
			t.Errorf("valid issuer %q: %v", issuer, err)
		}
	}
	if _, err := Handler(testOptions()); err == nil {
		t.Fatal("HTTP transport accepted missing OAuth server")
	}
}

func TestHTTPAndOAuthPublicAudiencesMustAgree(t *testing.T) {
	directory := t.TempDir()
	auth, err := oauth.Open(context.Background(), oauth.Options{
		Issuer: "https://mcp.example.com", DatabasePath: filepath.Join(directory, "oauth.db"), EncryptionKeyFile: filepath.Join(directory, "oauth.key"),
		VerifyKey: func(context.Context, string) (access.Identity, error) {
			return access.Identity{}, errors.New("unused verifier")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	opts := testOptions()
	opts.OAuth = auth
	if _, err := Handler(opts); err != nil {
		t.Fatalf("matching public audiences: %v", err)
	}
	opts.Issuer = "https://another-resource.example"
	if _, err := Handler(opts); err == nil {
		t.Fatal("HTTP adapter accepted tokens intended for a different public resource")
	}
}

func ExampleOptions() {
	opts := Options{Issuer: "https://mcp.example.com", Listen: "127.0.0.1:8080", MCPMaxBodyBytes: 50 << 20, RequestTimeout: 2 * time.Minute}
	fmt.Println(opts.Issuer + "/mcp")
	// Output: https://mcp.example.com/mcp
}
