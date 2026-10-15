//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela-mcp/internal/catalog"
	"go.xchunk.org/vela-mcp/internal/mcpserver"
	"go.xchunk.org/vela-mcp/internal/oauth"
	"go.xchunk.org/vela-mcp/internal/transfers"
	"go.xchunk.org/vela-mcp/internal/transport/httpserver"
	"go.xchunk.org/vela-mcp/internal/velaclient"
	"go.xchunk.org/vela/client"
)

// oauthBrowser executes the real consent/PKCE flow without relying on a manual
// browser. Its cookie jar binds CSRF to the browser transaction; keys stay in POST.
func oauthBrowser(t *testing.T, issuer, key string) map[string]any {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redirect := "http://127.0.0.1:48197/callback"
	registration, _ := json.Marshal(map[string]any{"client_name": "Vela integration client", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}})
	res, err := browser.Post(issuer+"/oauth/register", "application/json", bytes.NewReader(registration))
	if err != nil {
		t.Fatal(err)
	}
	var registered map[string]any
	json.NewDecoder(res.Body).Decode(&registered)
	res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatalf("client registration status %d", res.StatusCode)
	}
	id := registered["client_id"].(string)
	secret := make([]byte, 32)
	rand.Read(secret)
	verifier := base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {redirect}, "resource": {issuer + "/mcp"}, "scope": {"vela"}, "state": {"integration-state"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}}
	res, err = browser.Get(issuer + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("authorization page status %d", res.StatusCode)
	}
	field := func(name string) string {
		pattern := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`)
		m := pattern.FindSubmatch(page)
		if len(m) != 2 {
			t.Fatalf("authorization missing %s", name)
		}
		return string(m[1])
	}
	form := url.Values{"transaction": {field("transaction")}, "csrf": {field("csrf")}, "vela_key": {key}, "action": {"approve"}}
	req, _ := http.NewRequest(http.MethodPost, issuer+"/oauth/authorize", bytes.NewBufferString(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", issuer)
	res, err = browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 303 {
		t.Fatalf("consent status %d", res.StatusCode)
	}
	location, err := url.Parse(res.Header.Get("Location"))
	if err != nil || location.Query().Get("state") != "integration-state" || location.Query().Get("code") == "" {
		t.Fatal("bad OAuth callback")
	}
	res, err = browser.PostForm(issuer+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {location.Query().Get("code")}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {issuer + "/mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	var tokens map[string]any
	json.NewDecoder(res.Body).Decode(&tokens)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("token exchange status %d", res.StatusCode)
	}
	tokens["client_id"] = id
	return tokens
}

// TestHTTPExecutable verifies the composition root, including the scratch image
// running without a process-wide VELA_TOKEN. Docker's writable state volume is
// owned by the runtime UID; no auxiliary proxy or setup image is needed.
func TestHTTPExecutable(t *testing.T) {
	velaURL, keys := settings(t)
	binary, image := os.Getenv("VELA_MCP_TEST_BINARY"), os.Getenv("VELA_MCP_TEST_IMAGE")
	if binary == "" && image == "" {
		t.Skip("set VELA_MCP_TEST_BINARY or VELA_MCP_TEST_IMAGE")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	issuer := "http://" + address
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	name := fmt.Sprintf("vela-mcp-http-test-%d", time.Now().UnixNano())
	var cmd *exec.Cmd
	if image != "" {
		cmd = exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name, "--network", "host", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--env", "VELA_URL", image, "-transport", "http", "-listen", address, "-issuer", issuer)
	} else {
		config := fmt.Sprintf("[oauth]\ndatabase_path=%q\nencryption_key_file=%q\n[transfers]\ndatabase_path=%q\n", filepath.Join(dir, "oauth.db"), filepath.Join(dir, "encryption.key"), filepath.Join(dir, "transfers.db"))
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		cmd = exec.CommandContext(ctx, binary, "-transport", "http", "-listen", address, "-issuer", issuer, "-config", filepath.Join(dir, "config.toml"))
	}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "VELA_URL="+velaURL, "VELA_TOKEN=")
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if image != "" {
			exec.Command("docker", "stop", "-t", "2", name).Run()
		} else {
			cmd.Process.Signal(os.Interrupt)
		}
		cmd.Wait()
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	ready := false
	poll := http.Client{Timeout: time.Second}
	for attempt := 0; attempt < 100; attempt++ {
		res, err := poll.Get(issuer + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("HTTP executable did not become healthy")
	}
	tokens := oauthBrowser(t, issuer, keys["ro"])
	host := mcp.NewClient(&mcp.Implementation{Name: "executable-http-test", Version: "1"}, nil)
	session, err := host.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: issuer + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{tokens["access_token"].(string)}, Timeout: 10 * time.Second}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	info := result(t, session, "server_info", map[string]any{})
	if info["key"].(map[string]any)["scope"] != "ro" {
		t.Fatal("web login did not use the submitted read-only key")
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 24 {
		t.Fatal("HTTP executable tool inventory failed")
	}
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header = req.Header.Clone()
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

func TestHTTPConnectorOAuthAndBinaryTransfers(t *testing.T) {
	velaURL, keys := settings(t)
	ctx := context.Background()
	admin := backend(t, velaURL, keys["admin"])
	server := httptest.NewUnstartedServer(nil)
	issuer := "http://" + server.Listener.Addr().String()
	dir := t.TempDir()
	factory := velaclient.Factory{BaseURL: velaURL, Timeout: 20 * time.Second, Retries: 0}
	auth, err := oauth.Open(ctx, oauth.Options{Issuer: issuer, DatabasePath: filepath.Join(dir, "oauth.db"), EncryptionKeyFile: filepath.Join(dir, "encryption.key"), VerifyKey: func(ctx context.Context, key string) (access.Identity, error) {
		credential := access.NewCredential(key)
		g, err := factory.Open(ctx, credential)
		if err != nil {
			return access.Identity{}, err
		}
		info, err := g.Info(ctx)
		if err != nil {
			return access.Identity{}, err
		}
		return access.Identity{Subject: info.Key.ID, Scope: info.Key.Scope, Prefix: info.Key.Prefix, Credential: credential}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	svc := &app.Service{Resolver: access.ContextResolver{}, Gateways: factory, Limits: app.Limits{MaxUploadBytes: 8 << 20, MaxInlineBytes: 1 << 20, MaxReadBytes: 1 << 20, MaxDownloadBytes: 8 << 20}}
	broker, err := transfers.Open(ctx, transfers.Options{BaseURL: issuer, DatabasePath: filepath.Join(dir, "transfers.db"), Service: svc, LookupIdentity: auth.LookupIdentity})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	tools := catalog.Defaults()
	tools["download_file"] = false
	handler, err := httpserver.Handler(httpserver.Options{Issuer: issuer, Listen: server.Listener.Addr().String(), OAuth: auth, MCP: mcpserver.NewWithTransfers(svc, tools, 20*time.Second, broker), Transfers: broker.Handler()})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	unauthorized, err := http.Get(issuer + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != 401 || unauthorized.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("missing connector OAuth challenge")
	}
	tokens := oauthBrowser(t, issuer, keys["rw"])
	accessToken := tokens["access_token"].(string)
	host := mcp.NewClient(&mcp.Implementation{Name: "http-connector-test", Version: "1"}, nil)
	session, err := host.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: issuer + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{accessToken}, Timeout: 20 * time.Second}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 24 {
		t.Fatalf("HTTP standard inventory %d, expected 24", len(listed.Tools))
	}
	info := result(t, session, "server_info", map[string]any{})
	prefix := info["key"].(map[string]any)["prefix"].(string) + fmt.Sprintf("mcp-http-%d/", time.Now().UnixNano())
	t.Cleanup(func() { admin.Delete(context.Background(), prefix, client.DeleteOptions{Recursive: true, Purge: true}) })
	body := []byte{0, 255, 1, 2, 3, 128, 0, 254}
	sum := sha256.Sum256(body)
	path := prefix + "attachment.bin"
	upload := result(t, session, "prepare_file_upload", map[string]any{"path": path, "size": len(body), "sha256": hex.EncodeToString(sum[:]), "metadata": map[string]any{"title": "HTTP attachment", "description": "binary", "tags": []string{"http"}, "visibility": "private", "disposition": "attachment"}})
	req, _ := http.NewRequest(http.MethodPut, upload["upload_url"].(string), bytes.NewReader(body))
	for name, value := range upload["headers"].(map[string]any) {
		req.Header.Set(name, value.(string))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	receiptBody, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 201 && res.StatusCode != 200 {
		t.Fatalf("HTTP upload status %d: %s", res.StatusCode, receiptBody)
	}
	status := result(t, session, "get_file_transfer_status", map[string]any{"transfer_id": upload["transfer_id"]})
	if status["state"] != "succeeded" {
		t.Fatalf("upload was not recorded as succeeded")
	}
	download := result(t, session, "prepare_file_download", map[string]any{"path": path})
	res, err = http.Get(download["download_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !bytes.Equal(downloaded, body) {
		t.Fatal("binary roundtrip differs")
	}
	req, _ = http.NewRequest(http.MethodGet, download["download_url"].(string), nil)
	req.Header.Set("Range", "bytes=1-3")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 206 || !bytes.Equal(part, body[1:4]) {
		t.Fatal("binary Range failed")
	}
	readonly := oauthBrowser(t, issuer, keys["ro"])
	roSession, err := host.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: issuer + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{readonly["access_token"].(string)}, Timeout: 20 * time.Second}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roSession.Close()
	result(t, roSession, "prepare_file_download", map[string]any{"path": path})
	expectError(t, roSession, "prepare_file_upload", map[string]any{"path": prefix + "denied.bin", "size": len(body), "sha256": hex.EncodeToString(sum[:])}, "forbidden")
	// Revocation must terminate both ordinary OAuth access and delegated links.
	res, err = http.PostForm(issuer+"/oauth/revoke", url.Values{"token": {tokens["refresh_token"].(string)}, "client_id": {tokens["client_id"].(string)}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("revocation failed")
	}
	res, err = http.Get(download["download_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("download survived OAuth revocation: %d", res.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodGet, issuer+"/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	res, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("access survived OAuth revocation")
	}
}
