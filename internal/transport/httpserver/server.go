// Package httpserver adapts an authenticated MCP server to plain HTTP behind a
// reverse proxy. The configured issuer is authoritative: forwarding headers do
// not control discovery URLs, allowed hosts, or the authorization origin.
//
// MCP requests use short-lived, stateless SDK sessions. Every request resolves
// its own OAuth credential before entering the transport, so a session header
// can never reuse another user's identity. Browser requests to protected routes
// require an explicitly allowed Origin. Public discovery and OAuth token/client
// registration routes allow cross-origin requests without browser credentials;
// PKCE and client validation remain the authorization server's responsibility.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/oauth"
)

// Options separates the public identity from the private HTTP listen address.
// TLS belongs to the operator's reverse proxy, rather than this adapter.
type Options struct {
	Issuer          string
	Listen          string
	AllowedOrigins  []string
	OAuth           *oauth.Server
	MCP             *mcp.Server
	Transfers       http.Handler
	MCPMaxBodyBytes int64
	RequestTimeout  time.Duration
}

// oauthProvider is deliberately private: production uses the concrete OAuth
// server, while transport tests can exercise identity isolation without SQLite.
type oauthProvider interface {
	RegisterRoutes(*http.ServeMux)
	Authenticate(context.Context, string) (access.Identity, error)
	MetadataURL() string
}

// Handler assembles OAuth discovery, browser authorization, MCP and optional
// file-transfer routes behind the same host, Origin and response-header policy.
func Handler(opts Options) (http.Handler, error) {
	if opts.OAuth == nil {
		return nil, errors.New("HTTP transport requires an OAuth server")
	}
	normalized, _, err := normalize(opts)
	if err != nil {
		return nil, err
	}
	// Keep host validation, discovery and token audience tied to one public
	// resource even if a future composition root wires these independently.
	if opts.OAuth.ResourceURL() != normalized.Issuer+"/mcp" {
		return nil, errors.New("HTTP issuer must match the OAuth resource audience")
	}
	return handler(normalized, opts.OAuth)
}

func handler(opts Options, auth oauthProvider) (http.Handler, error) {
	opts, issuer, err := normalize(opts)
	if err != nil {
		return nil, err
	}
	policy, err := newPolicy(opts, issuer)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	auth.RegisterRoutes(mux)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return opts.MCP }, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		MaxRequestBodyBytes:          opts.MCPMaxBodyBytes,
		PropagateRequestCancellation: true,
		// This adapter replaces the SDK's loopback-only Host check with an
		// explicit issuer/upstream allowlist. Nginx commonly connects over
		// loopback while forwarding the public issuer Host.
		DisableLocalhostProtection: true,
	})
	mux.Handle("/mcp", bearer(auth, transport))
	if opts.Transfers != nil {
		// Transfer capabilities have their own short-lived authorization and
		// byte limits. OAuth bearer middleware does not consume their body.
		mux.Handle("/transfers/", opts.Transfers)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = landing.Execute(w, opts.Issuer+"/mcp")
	})
	return policy.wrap(mux, opts.RequestTimeout), nil
}

// Run serves plain HTTP until ctx is cancelled, then drains requests for up to
// fifteen seconds. It returns listen/serve errors and closes remaining sockets
// if a request prevents graceful shutdown. HTTP bytes never share stdio output.
func Run(ctx context.Context, opts Options) error {
	opts, _, err := normalize(opts)
	if err != nil {
		return err
	}
	h, err := Handler(opts)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("listen on HTTP address: %w", err)
	}
	server := &http.Server{
		Addr:              opts.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       opts.RequestTimeout,
		WriteTimeout:      opts.RequestTimeout + 15*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			<-done
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		<-done
		return nil
	}
}

func normalize(opts Options) (Options, *url.URL, error) {
	issuer, err := url.Parse(strings.TrimSuffix(opts.Issuer, "/"))
	if err != nil || issuer == nil || !validOriginURL(issuer) {
		return opts, nil, errors.New("HTTP issuer must be an HTTPS origin or a loopback HTTP origin")
	}
	if issuer.Path != "" {
		return opts, nil, errors.New("HTTP issuer must not contain a path prefix")
	}
	opts.Issuer = canonicalOrigin(issuer)
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(opts.Listen); err != nil {
		return opts, nil, errors.New("HTTP listen address must contain a host and port")
	}
	if opts.MCP == nil {
		return opts, nil, errors.New("HTTP transport requires an MCP server")
	}
	if opts.MCPMaxBodyBytes == 0 {
		opts.MCPMaxBodyBytes = 50 << 20
	}
	if opts.MCPMaxBodyBytes < 1 {
		return opts, nil, errors.New("HTTP MCP request body limit must be positive")
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 2 * time.Minute
	}
	if opts.RequestTimeout < time.Second {
		return opts, nil, errors.New("HTTP request timeout must be at least one second")
	}
	return opts, issuer, nil
}

func bearer(auth oauthProvider, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OAuth access tokens must never travel in URLs, where proxies and
		// browser history can retain them. Transfer capability URLs are a
		// separate route and deliberately do not use this middleware.
		query := r.URL.Query()
		for _, name := range []string{"access_token", "oauth_token", "token"} {
			if query.Has(name) {
				w.Header().Set("WWW-Authenticate", challenge(auth.MetadataURL(), "invalid_request"))
				http.Error(w, "Bearer tokens must use the Authorization header", http.StatusBadRequest)
				return
			}
		}
		values := r.Header.Values("Authorization")
		if len(values) == 0 {
			unauthorized(w, auth.MetadataURL(), "")
			return
		}
		fields := strings.Fields(values[0])
		if len(values) != 1 || len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(fields[1]) > 4096 {
			unauthorized(w, auth.MetadataURL(), "invalid_token")
			return
		}
		identity, err := auth.Authenticate(r.Context(), fields[1])
		if err != nil {
			unauthorized(w, auth.MetadataURL(), "invalid_token")
			return
		}
		next.ServeHTTP(w, r.WithContext(access.WithIdentity(r.Context(), identity)))
	})
}

func challenge(metadata, code string) string {
	value := fmt.Sprintf("Bearer resource_metadata=%q, scope=%q", metadata, "vela")
	if code != "" {
		value += fmt.Sprintf(", error=%q", code)
	}
	return value
}

func unauthorized(w http.ResponseWriter, metadata, code string) {
	w.Header().Set("WWW-Authenticate", challenge(metadata, code))
	http.Error(w, "OAuth authorization is required", http.StatusUnauthorized)
}

var landing = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Vela MCP</title><style>body{font:17px system-ui;max-width:44rem;margin:12vh auto;padding:1.5rem;line-height:1.6}code{word-break:break-all}a{color:#245ac5}</style>
<h1>Vela MCP</h1><p>This server connects an MCP client to Vela.</p>
<p>Connector URL: <code>{{.}}</code></p>
<p>Add this URL in your MCP client's connector settings. The client opens the authorization page, where you enter a Vela API key and approve access.</p>
<p><a href="/.well-known/oauth-protected-resource/mcp">OAuth resource metadata</a></p></html>`))
