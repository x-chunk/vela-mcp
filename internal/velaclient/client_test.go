package velaclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
)

// Equal bytes at a destination do not prove that a lost creation attempt
// applied the requested metadata. HTTP transfers must leave that uncertainty
// visible while safe reads can still use the configured retry policy.
func TestHTTPUploadDoesNotRetryWhileReadsDo(t *testing.T) {
	var puts, gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			puts.Add(1)
			w.WriteHeader(503)
			w.Write([]byte(`{"error":{"code":"unavailable","message":"retry later"}}`))
			return
		}
		if gets.Add(1) == 1 {
			w.WriteHeader(503)
			w.Write([]byte(`{"error":{"code":"unavailable","message":"retry later"}}`))
			return
		}
		w.Write([]byte(`{"api_version":1}`))
	}))
	defer server.Close()
	gateway, err := (Factory{BaseURL: server.URL, Timeout: 3 * time.Second, Retries: 2, DisableUploadRetries: true}).Open(context.Background(), access.NewCredential("dummy"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = gateway.Info(context.Background()); err != nil || gets.Load() != 2 {
		t.Fatal("read retry policy was lost")
	}
	digest := sha256.Sum256([]byte("abc"))
	if _, _, err = gateway.Upload(context.Background(), "file", strings.NewReader("abc"), app.UploadOptions{Size: 3, CreateOnly: true, SHA256: hex.EncodeToString(digest[:])}); err == nil || puts.Load() != 1 {
		t.Fatal("ambiguous upload was automatically retried")
	}
}
