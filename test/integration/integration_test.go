//go:build integration

// Integration tests are explicitly opt-in: they mutate a disposable Vela store.
// All file operations and cleanup are confined to a unique test prefix. The bulk
// purge test uses a 520-week cutoff; the root-policy test restores its exact rule.
package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela-mcp/internal/catalog"
	"go.xchunk.org/vela-mcp/internal/config"
	"go.xchunk.org/vela-mcp/internal/localfiles"
	"go.xchunk.org/vela-mcp/internal/mcpserver"
	"go.xchunk.org/vela-mcp/internal/velaclient"
	"go.xchunk.org/vela/api"
	"go.xchunk.org/vela/client"
)

func settings(t *testing.T) (string, map[string]string) {
	t.Helper()
	path := os.Getenv("VELA_MCP_TEST_KEYS")
	if path == "" {
		t.Skip("set VELA_MCP_TEST_KEYS to a disposable store's admin/ro/rw key file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read test key file")
	}
	keys := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		label, key, ok := strings.Cut(line, ":")
		if ok {
			keys[strings.TrimSpace(label)] = strings.TrimSpace(key)
		}
	}
	for _, label := range []string{"admin", "ro", "rw"} {
		if keys[label] == "" {
			t.Fatalf("missing %s test key", label)
		}
	}
	url := os.Getenv("VELA_MCP_TEST_URL")
	if url == "" {
		url = "http://127.0.0.1:8080"
	}
	return url, keys
}
func backend(t *testing.T, url, key string) *client.Client {
	t.Helper()
	c, err := client.New(url, key, client.WithHTTPClient(&http.Client{Timeout: 20 * time.Second}), client.WithRetries(0))
	if err != nil {
		t.Fatal("invalid test URL")
	}
	return c
}
func session(t *testing.T, url, key string, all bool, files *localfiles.Files) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	factory := velaclient.Factory{BaseURL: url, Timeout: 20 * time.Second, Retries: 0}
	credential := access.NewCredential(key)
	gateway, err := factory.Open(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	info, err := gateway.Info(ctx)
	if err != nil {
		t.Fatal("cannot authenticate test key")
	}
	cfg := config.Defaults()
	if all {
		for name := range cfg.Tools {
			cfg.Tools[name] = true
		}
	}
	s := &app.Service{Resolver: access.Static{Identity: access.Identity{Subject: "test:" + info.Key.ID, Scope: info.Key.Scope, Prefix: info.Key.Prefix, Credential: credential}}, Gateways: factory, Files: files, Limits: app.Limits{MaxUploadBytes: cfg.Files.MaxUploadBytes, MaxInlineBytes: cfg.Files.MaxInlineBytes, MaxReadBytes: cfg.Files.MaxReadBytes, MaxDownloadBytes: cfg.Files.MaxDownloadBytes}}
	a, b := mcp.NewInMemoryTransports()
	server, err := mcpserver.New(s, cfg.Tools, 20*time.Second).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "integration"}, nil)
	result, err := c.Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { result.Close(); server.Close() })
	return result
}
func result(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s protocol failure: %v", name, err)
	}
	if res.IsError {
		b, _ := json.Marshal(res.Content)
		t.Fatalf("%s failed: %s", name, b)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s missing structured result", name)
	}
	return out
}
func expectError(t *testing.T, s *mcp.ClientSession, name string, args map[string]any, code string) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("%s unexpectedly succeeded", name)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Error struct {
			Code string `json:"code"`
		}
	}
	json.Unmarshal(b, &out)
	if out.Error.Code != code {
		t.Fatalf("%s error code %q, want %q: %s", name, out.Error.Code, code, b)
	}
}
func TestAllBusinessTools(t *testing.T) {
	url, keys := settings(t)
	ctx := context.Background()
	admin := backend(t, url, keys["admin"])
	info, err := admin.Info(ctx)
	if err != nil {
		t.Fatal("admin authentication failed")
	}
	prefix := info.Key.Prefix + fmt.Sprintf("mcp-it-%d/", time.Now().UnixNano())
	dir := t.TempDir()
	files, err := localfiles.New([]string{dir}, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	s := session(t, url, keys["admin"], true, files)
	seen := map[string]bool{}
	call := func(name string, args map[string]any) map[string]any {
		seen[name] = true
		return result(t, s, name, args)
	}
	t.Cleanup(func() {
		admin.Delete(ctx, prefix, client.DeleteOptions{Recursive: true, Purge: true})
		if policies, err := admin.ListAllDirPolicies(ctx, client.DirListOptions{Prefix: prefix, Recursive: true}); err == nil {
			for _, p := range policies.Policies {
				admin.DeleteDir(ctx, p.Prefix, client.DirWriteOptions{})
			}
		}
		if trash, err := admin.ListAllTrash(ctx, client.TrashOptions{Prefix: prefix}); err == nil {
			for _, e := range trash {
				admin.PurgeTrashEntry(ctx, e.ID)
			}
		}
	})
	call("server_info", map[string]any{})
	path := prefix + "docs/привет #1.txt"
	text := "Привет MCP 😀\n"
	created := call("create_text_file", map[string]any{"path": path, "text": text, "metadata": map[string]any{"title": "Заголовок", "description": "Описание", "tags": []string{"Test", "mcp"}, "content_type": "text/plain; charset=utf-8", "disposition": "attachment", "expires_at": "7d"}})["object"].(map[string]any)
	if created["visibility"] != "private" || created["title"] != "Заголовок" || created["expires_at"] == nil {
		t.Fatal("creation metadata lost")
	}
	expectError(t, s, "create_text_file", map[string]any{"path": path, "text": "overwrite"}, "already_exists")
	stat := call("get_file_metadata", map[string]any{"path": path})
	if stat["id"] != created["id"] {
		t.Fatal("wrong object identity")
	}
	read := call("read_file", map[string]any{"path": path})
	if read["content"] != text || read["truncated"] != false {
		t.Fatal("text content lost")
	}
	call("read_file", map[string]any{"path": path, "max_bytes": 4})
	call("download_file", map[string]any{"path": path, "local_path": filepath.Join(dir, "download.txt")})
	data, _ := os.ReadFile(filepath.Join(dir, "download.txt"))
	if string(data) != text {
		t.Fatal("local download differs")
	}
	patched := call("update_file_metadata", map[string]any{"path": path, "expected_revision": stat["revision"], "patch": map[string]any{"title": "Updated", "description": nil, "tags": []string{}, "expires_at": nil}})
	if patched["expires_at"] != nil || patched["description"] != "" {
		t.Fatal("null merge patch lost")
	}
	expectError(t, s, "set_file_visibility", map[string]any{"path": path, "visibility": "public", "expected_revision": stat["revision"]}, "precondition_failed")
	visible := call("set_file_visibility", map[string]any{"path": path, "visibility": "unlisted", "expected_revision": patched["revision"]})
	call("replace_file", map[string]any{"path": path, "text": "replacement", "expected_revision": visible["revision"], "metadata": map[string]any{"title": "Replaced"}})
	expectError(t, s, "replace_file", map[string]any{"path": path, "text": "stale", "expected_revision": visible["revision"]}, "precondition_failed")
	source := filepath.Join(dir, "source.bin")
	binary := []byte{0, 255, 1, 2, 3}
	os.WriteFile(source, binary, 0600)
	call("upload_file", map[string]any{"path": prefix + "docs/attachment.bin", "local_path": source, "metadata": map[string]any{"visibility": "private", "title": "Приложение", "description": "Бинарный файл", "tags": []string{"binary"}, "content_type": "application/octet-stream", "disposition": "attachment", "expires_at": "2d"}})
	call("upload_file", map[string]any{"path": prefix + "docs/base64.bin", "content_base64": base64.StdEncoding.EncodeToString(binary)})
	expectError(t, s, "read_file", map[string]any{"path": prefix + "docs/base64.bin"}, "invalid_request")
	binaryResult := call("read_file", map[string]any{"path": prefix + "docs/base64.bin", "encoding": "base64"})
	if binaryResult["encoding"] != "base64" || binaryResult["content"] != base64.StdEncoding.EncodeToString(binary) {
		t.Fatal("binary read differs")
	}
	listing := call("list_directory", map[string]any{"prefix": prefix, "recursive": true, "limit": 1})
	search := call("search_files", map[string]any{"query": "привет", "prefix": prefix})
	if len(search["files"].([]any)) != 1 {
		t.Fatal("metadata search did not find the Unicode path")
	}
	if listing["next_cursor"] == "" {
		t.Fatal("missing cursor")
	}
	call("list_directory", map[string]any{"prefix": prefix, "recursive": true, "limit": 1, "cursor": listing["next_cursor"]})
	link := call("create_share_link", map[string]any{"path": path, "ttl": "2h", "note": "integration"})
	call("list_share_links", map[string]any{"path": path})
	newpath := prefix + "docs/moved.txt"
	call("move_file", map[string]any{"from": path, "to": newpath})
	// Signed URLs survive path changes and keep returning the same file.
	if shareURL, ok := link["url"].(string); ok {
		u := shareURL
		publicBase := strings.TrimRight(info.PublicURL, "/")
		if strings.HasPrefix(u, publicBase+"/") {
			u = strings.TrimRight(url, "/") + strings.TrimPrefix(u, publicBase)
		}
		hc := http.Client{Timeout: 10 * time.Second}
		response, err := hc.Get(u)
		if err != nil {
			t.Fatal("cannot fetch signed URL")
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || string(body) != "replacement" {
			t.Fatal("signed link did not survive move")
		}
	} else {
		t.Fatal("share returned no URL")
	}
	call("revoke_share_link", map[string]any{"link_id": link["id"], "path": newpath})
	call("update_directory_policy", map[string]any{"prefix": prefix + "docs/", "patch": map[string]any{"index": "on", "hidden": true, "noindex": "on", "title": "Документы", "description": "test"}})
	call("inspect_directory", map[string]any{"prefix": prefix + "docs/"})
	call("list_directory_policies", map[string]any{"prefix": prefix, "recursive": true})
	call("move_directory", map[string]any{"from": prefix + "docs/", "to": prefix + "archive/"})
	newpath = prefix + "archive/moved.txt"
	policy := call("inspect_directory", map[string]any{"prefix": prefix + "archive/"})
	call("reset_directory_policy", map[string]any{"prefix": prefix + "archive/", "expected_revision": policy["rule"].(map[string]any)["revision"]})
	deleted := call("trash_file", map[string]any{"path": newpath})
	call("list_trash", map[string]any{"prefix": prefix, "path": newpath})
	trashID := deleted["trash_ids"].([]any)[0]
	call("restore_file", map[string]any{"trash_id": trashID, "original_path": newpath, "to_path": prefix + "restored.txt"})
	call("purge_file", map[string]any{"path": prefix + "restored.txt"})
	bulk := call("trash_directory", map[string]any{"path": prefix + "archive/"})
	if bulk["deleted"].(float64) != 2 {
		t.Fatal("bulk trash missed files")
	}
	entries := call("list_trash", map[string]any{"prefix": prefix})["entries"].([]any)
	for _, raw := range entries {
		e := raw.(map[string]any)
		call("purge_trash_entry", map[string]any{"trash_id": e["id"], "original_path": e["original_path"]})
	}
	if info.Key.Prefix == "" {
		call("empty_trash", map[string]any{"older_than": "520w"})
		before, err := admin.ShowDir(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if before.Rule == nil {
				admin.DeleteDir(ctx, "", client.DirWriteOptions{})
			} else {
				admin.PutDir(ctx, "", api.DirPolicyRequest{Index: before.Rule.Index, Hidden: before.Rule.Hidden, NoIndex: before.Rule.NoIndex, Title: before.Rule.Title, Description: before.Rule.Description}, client.DirWriteOptions{})
			}
		})
		call("update_root_directory_policy", map[string]any{"patch": map[string]any{"title": "MCP integration temporary title"}})
	}
	for _, tool := range catalog.Tools() {
		if catalog.HTTPOnly(tool.Name) {
			continue
		}
		if !seen[tool.Name] && info.Key.Prefix == "" {
			t.Errorf("tool %s not exercised", tool.Name)
		}
	}
}

func TestReadonlyAndRWAuthorization(t *testing.T) {
	url, keys := settings(t)
	ro := session(t, url, keys["ro"], false, nil)
	result(t, ro, "server_info", map[string]any{})
	result(t, ro, "list_directory", map[string]any{})
	expectError(t, ro, "create_text_file", map[string]any{"path": "mcp-denied.txt", "text": "denied"}, "forbidden")
	expectError(t, ro, "list_share_links", map[string]any{}, "forbidden")
	rw := session(t, url, keys["rw"], true, nil)
	result(t, rw, "server_info", map[string]any{})
	expectError(t, rw, "purge_file", map[string]any{"path": "mcp-denied.txt"}, "forbidden")
	expectError(t, rw, "empty_trash", map[string]any{"older_than": "520w"}, "forbidden")
}

// TestStdio uses the real executable (or Docker image) and MCP CommandTransport,
// so any accidental stdout logging breaks initialization and fails this test.
func TestStdio(t *testing.T) {
	url, keys := settings(t)
	binary := os.Getenv("VELA_MCP_TEST_BINARY")
	image := os.Getenv("VELA_MCP_TEST_IMAGE")
	if binary == "" && image == "" {
		t.Skip("set VELA_MCP_TEST_BINARY or VELA_MCP_TEST_IMAGE for subprocess test")
	}
	var cmd *exec.Cmd
	dir := t.TempDir()
	input := filepath.Join(dir, "in")
	output := filepath.Join(dir, "out")
	if err := os.Mkdir(input, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(output, 0777); err != nil {
		t.Fatal(err)
	}
	os.Chmod(output, 0777)
	if err := os.WriteFile(filepath.Join(input, "attachment.bin"), []byte{0, 255, 4, 5}, 0644); err != nil {
		t.Fatal(err)
	}
	inputPath, outputPath := input, output
	if image != "" {
		inputPath = "/files/in"
		outputPath = "/files/out"
	}
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("[files]\nupload_roots=[%q,%q]\ndownload_roots=[%q]\n", inputPath, outputPath, outputPath)), 0644); err != nil {
		t.Fatal(err)
	}
	if image != "" {
		cmd = exec.Command("docker", "run", "--rm", "-i", "--network", "host", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--env", "VELA_URL", "--env", "VELA_TOKEN", "-v", configPath+":/app/config.toml:ro", "-v", input+":/files/in:ro", "-v", output+":/files/out", image)
	} else {
		cmd = exec.Command(binary, "-config", configPath)
	}
	cmd.Env = append(os.Environ(), "VELA_URL="+url, "VELA_TOKEN="+keys["rw"])
	cmd.Dir = t.TempDir()
	cmd.Stderr = os.Stderr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host := mcp.NewClient(&mcp.Implementation{Name: "stdio-integration"}, nil)
	s, err := host.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal("stdio initialization failed")
	}
	defer s.Close()
	listed, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 22 {
		t.Fatalf("default stdio tool count %d, want 22", len(listed.Tools))
	}
	info := result(t, s, "server_info", map[string]any{})
	key := info["key"].(map[string]any)
	prefix := key["prefix"].(string) + fmt.Sprintf("mcp-stdio-%d/", time.Now().UnixNano())
	p := prefix + "hello.txt"
	admin := backend(t, url, keys["admin"])
	t.Cleanup(func() {
		admin.Delete(context.Background(), prefix, client.DeleteOptions{Recursive: true, Purge: true})
		if entries, err := admin.ListAllTrash(context.Background(), client.TrashOptions{Prefix: prefix}); err == nil {
			for _, e := range entries {
				admin.PurgeTrashEntry(context.Background(), e.ID)
			}
		}
	})
	result(t, s, "create_text_file", map[string]any{"path": p, "text": "stdio smoke"})
	if res := result(t, s, "read_file", map[string]any{"path": p}); res["content"] != "stdio smoke" {
		t.Fatal("stdio content differs")
	}
	attached := prefix + "attachment.bin"
	uploaded := result(t, s, "upload_file", map[string]any{"path": attached, "local_path": filepath.Join(inputPath, "attachment.bin"), "metadata": map[string]any{"title": "Mounted attachment", "visibility": "private"}})
	result(t, s, "download_file", map[string]any{"path": attached, "local_path": filepath.Join(outputPath, "attachment.bin")})
	// Docker creates 0600 output owned by UID 65532. Verify it through the same
	// process identity rather than attempting an unauthorized host-side read.
	roundtrip := result(t, s, "upload_file", map[string]any{"path": prefix + "roundtrip.bin", "local_path": filepath.Join(outputPath, "attachment.bin")})
	if uploaded["object"].(map[string]any)["sha256"] != roundtrip["object"].(map[string]any)["sha256"] {
		t.Fatal("mounted attachment transfer failed")
	}
	trash := result(t, s, "trash_file", map[string]any{"path": p})
	for _, id := range trash["trash_ids"].([]any) {
		admin.PurgeTrashEntry(context.Background(), id.(string))
	}
}
