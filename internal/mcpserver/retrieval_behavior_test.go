package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela-mcp/internal/catalog"
	"go.xchunk.org/vela-mcp/internal/transfers"
	"go.xchunk.org/vela/api"
)

func TestRetrievalFormatsPreserveReusableRevision(t *testing.T) {
	for _, tool := range []string{"list_directory", "read_file"} {
		for _, format := range []string{"", "detailed"} {
			t.Run(tool+"/"+format, func(t *testing.T) {
				g := &retrievalBehaviorGateway{retrievalFixture: newRetrievalFixture(1, "Editable text.\n")}
				session := retrievalBehaviorSession(t, g, nil, 128000, nil)
				args := map[string]any{}
				if tool == "list_directory" {
					args["prefix"] = "reports/2026/"
				} else {
					args["path"] = g.objects[0].Path
				}
				if format != "" {
					args["response_format"] = format
				}
				data := retrievalData(t, retrievalCall(t, session, tool, args))
				file := data
				if tool == "list_directory" {
					file = data["objects"].([]any)[0].(map[string]any)
				} else {
					if data["content"] != g.content {
						t.Fatal("read lost the requested content")
					}
					_, hasObject := data["object"]
					if hasObject != (format == "detailed") {
						t.Fatal("read metadata did not follow response_format")
					}
				}
				if file["path"] != g.objects[0].Path || file["revision"] != float64(7) {
					t.Fatal("retrieval lost the file path or revision needed by a subsequent edit")
				}
				metadata := file
				if tool == "read_file" && format == "detailed" {
					metadata = data["object"].(map[string]any)
				}
				for _, field := range []string{"sha256", "description", "created_at"} {
					value, present := metadata[field]
					if present != (format == "detailed") || (present && value == "") {
						t.Fatalf("%s metadata presence does not follow response_format", field)
					}
				}
				// Reuse the discovered revision directly instead of a second
				// metadata call. The backend must receive that same guard.
				retrievalCall(t, session, "update_file_metadata", map[string]any{
					"path": file["path"], "expected_revision": file["revision"], "patch": map[string]any{"title": "Reviewed"},
				})
				wantStat := 0
				if tool == "read_file" {
					wantStat = 1
				}
				if len(g.patchRevisions) != 1 || g.patchRevisions[0] != 7 || g.calls.stat != wantStat {
					t.Fatalf("revision reuse caused an extra preflight or lost its guard: patches=%v stat_calls=%d", g.patchRevisions, g.calls.stat)
				}
			})
		}
	}
}

func TestBinaryPreviewRecoveryAndBudgetBoundaries(t *testing.T) {
	t.Run("auto_binary_and_explicit_base64", func(t *testing.T) {
		for _, downloadEnabled := range []bool{true, false} {
			t.Run(fmt.Sprint(downloadEnabled), func(t *testing.T) {
				raw := []byte{0xff, 0, 1, 0xfe}
				g := &retrievalBehaviorGateway{retrievalFixture: newRetrievalFixture(1, string(raw))}
				g.objects[0].ContentType = "application/octet-stream"
				enabled := catalog.Defaults()
				enabled["prepare_file_download"] = downloadEnabled
				session := retrievalBehaviorSession(t, g, &transfers.Service{}, 128000, enabled)
				result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{"path": g.objects[0].Path}})
				if err != nil || result == nil || !result.IsError {
					t.Fatalf("auto silently returned binary bytes: err=%v result=%v", err, result)
				}
				detail := retrievalData(t, result)["error"].(map[string]any)
				message := detail["message"].(string)
				if detail["code"] != "invalid_request" || !strings.Contains(message, "base64") || strings.Contains(message, "prepare_file_download") != downloadEnabled {
					t.Fatalf("binary recovery recommends an unavailable operation: %v", detail)
				}
				decoded := retrievalData(t, retrievalCall(t, session, "read_file", map[string]any{"path": g.objects[0].Path, "encoding": "base64"}))
				content, err := base64.StdEncoding.Strict().DecodeString(decoded["content"].(string))
				if err != nil || !bytes.Equal(content, raw) || decoded["bytes_returned"] != float64(len(raw)) || decoded["truncated"] != false {
					t.Fatalf("explicit byte inspection changed the file: %v, %v", decoded, err)
				}
			})
		}
	})

	for _, encoding := range []string{"utf-8", "base64"} {
		t.Run("small_budget/"+encoding, func(t *testing.T) {
			raw := []byte(strings.Repeat("α😀<", 512))
			if encoding == "base64" {
				raw = bytes.Repeat([]byte{0, 0xff, 1, 0xfe, 2, 3, 4}, 512)
			}
			g := &retrievalBehaviorGateway{retrievalFixture: newRetrievalFixture(1, string(raw))}
			session := retrievalBehaviorSession(t, g, &transfers.Service{}, 1024, nil)
			result := retrievalCall(t, session, "read_file", map[string]any{"path": g.objects[0].Path, "encoding": encoding})
			data := retrievalData(t, result)
			returned := []byte(data["content"].(string))
			if encoding == "base64" {
				var err error
				returned, err = base64.StdEncoding.Strict().DecodeString(string(returned))
				if err != nil {
					t.Fatalf("truncated base64 is not independently decodable: %v", err)
				}
			} else if !utf8.Valid(returned) {
				t.Fatal("budget truncation split a UTF-8 rune")
			}
			if len(returned) == 0 || len(returned) >= len(raw) || !bytes.HasPrefix(raw, returned) || data["truncated"] != true || data["bytes_returned"] != float64(len(returned)) {
				t.Fatal("budget truncation did not preserve an accurately described raw prefix")
			}
			if data["revision"] != float64(7) || data["size"] != float64(len(raw)) || retrievalBytes(t, result) > 1024 || g.calls.stat != 1 || g.calls.download != 1 {
				t.Fatal("fitting a preview lost its identity, exceeded the wire budget, or downloaded it again")
			}
		})
	}
}

func TestUploadDiscoveryRejectsUnavailableLocalPathsBeforeOpeningWorkspace(t *testing.T) {
	for _, mode := range []string{"no_broker", "broker_upload_disabled", "broker"} {
		t.Run(mode, func(t *testing.T) {
			g := &retrievalBehaviorGateway{retrievalFixture: newRetrievalFixture(1, "existing")}
			enabled := catalog.Defaults()
			enabled["replace_file"] = true
			var broker *transfers.Service
			if mode != "no_broker" {
				broker = &transfers.Service{}
			}
			if mode == "broker_upload_disabled" {
				enabled["prepare_file_upload"] = false
			}
			session := retrievalBehaviorSession(t, g, broker, 128000, enabled)
			listed, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]bool{}
			for _, tool := range listed.Tools {
				if tool.Name != "upload_file" && tool.Name != "replace_file" {
					continue
				}
				found[tool.Name] = true
				schemaJSON, err := json.Marshal(tool.InputSchema)
				if err != nil || strings.Contains(string(schemaJSON), "local_path") {
					t.Fatalf("%s advertises inaccessible local filesystem input: %s", tool.Name, schemaJSON)
				}
				if tool.Name == "upload_file" && strings.Contains(tool.Description, "prepare_file_upload") != (mode == "broker") {
					t.Fatal("upload description recommends a transfer tool that is not available")
				}
				args := map[string]any{"path": g.objects[0].Path, "local_path": "/tmp/client-only-attachment"}
				if tool.Name == "replace_file" {
					args["expected_revision"] = 7
				}
				result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.Name, Arguments: args})
				if err == nil && (result == nil || !result.IsError) {
					t.Fatalf("%s accepted a local path without filesystem capability", tool.Name)
				}
			}
			if !found["upload_file"] || !found["replace_file"] || g.opens != 0 {
				t.Fatalf("unavailable inputs reached application setup: tools=%v workspace_opens=%d", found, g.opens)
			}
		})
	}
}

func TestOversizedMutationReceiptDoesNotBecomeWriteFailure(t *testing.T) {
	g := &retrievalBehaviorGateway{retrievalFixture: newRetrievalFixture(0, "")}
	server := mcp.NewServer(&mcp.Implementation{Name: "vela-mcp", Version: "test"}, nil)
	r := &registrar{server: server, service: retrievalBehaviorService(g), enabled: map[string]bool{"create_text_file": true}, timeout: time.Second, maxOutputBytes: 1024}
	writes := 0
	register(r, "create_text_file", "test successful write", object(schema{}), func(context.Context, *app.Workspace, noArgs) (any, error) {
		writes++
		return map[string]any{"large_receipt": strings.Repeat("x", 4096)}, nil
	})
	result := retrievalCall(t, connect(t, server), "create_text_file", map[string]any{})
	data := retrievalData(t, result)
	if data["completed"] != true || data["receipt_truncated"] != true || data["tool"] != "create_text_file" || writes != 1 || g.opens != 1 {
		t.Fatalf("successful write lost its completion status or was repeated: %v writes=%d opens=%d", data, writes, g.opens)
	}
	if recovery, _ := data["recovery"].(string); recovery == "" || !strings.Contains(recovery, "Inspect") || retrievalBytes(t, result) > 1024 {
		t.Fatal("oversized success receipt did not fit or explain recovery")
	}
}

type retrievalBehaviorGateway struct {
	*retrievalFixture
	opens          int
	patchRevisions []int64
}

func (g *retrievalBehaviorGateway) Open(context.Context, access.Credential) (app.Gateway, error) {
	g.opens++
	return g, nil
}

func (g *retrievalBehaviorGateway) Patch(_ context.Context, path string, patch api.ObjectPatch, revision int64) (*api.Object, error) {
	g.patchRevisions = append(g.patchRevisions, revision)
	for i := range g.objects {
		if g.objects[i].Path == path && g.objects[i].Revision == revision {
			g.objects[i].Revision++
			if patch.Title.HasValue() {
				g.objects[i].Title = patch.Title.Value
			}
			return &g.objects[i], nil
		}
	}
	return nil, fmt.Errorf("file or revision changed in fixture")
}

func retrievalBehaviorService(g *retrievalBehaviorGateway) *app.Service {
	return &app.Service{
		Resolver: access.Static{Identity: access.Identity{Subject: "retrieval-behavior", Scope: api.ScopeRW, Credential: access.NewCredential("fixture-only")}},
		Gateways: g,
		Limits:   app.Limits{MaxReadBytes: 32 << 10, MaxInlineBytes: 1 << 20, MaxUploadBytes: 8 << 20, MaxDownloadBytes: 8 << 20},
	}
}

func retrievalBehaviorSession(t *testing.T, g *retrievalBehaviorGateway, broker *transfers.Service, budget int64, enabled map[string]bool) *mcp.ClientSession {
	t.Helper()
	if enabled == nil {
		enabled = catalog.Defaults()
	}
	return connect(t, NewWithTransfers(retrievalBehaviorService(g), enabled, time.Second, broker, budget))
}
