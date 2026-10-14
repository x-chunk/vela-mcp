// vela-mcp serves MCP over stdio or HTTP. This is the composition root: configuration,
// upstream credential verification, adapters, use cases and transport meet here.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela-mcp/internal/catalog"
	"go.xchunk.org/vela-mcp/internal/config"
	"go.xchunk.org/vela-mcp/internal/localfiles"
	"go.xchunk.org/vela-mcp/internal/mcpserver"
	"go.xchunk.org/vela-mcp/internal/oauth"
	"go.xchunk.org/vela-mcp/internal/transfers"
	"go.xchunk.org/vela-mcp/internal/transport/httpserver"
	"go.xchunk.org/vela-mcp/internal/transport/stdio"
	"go.xchunk.org/vela-mcp/internal/velaclient"
	buildversion "go.xchunk.org/vela-mcp/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}
func run() error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var configPath, envPath, transport, listen, issuer string
	var check, list, version bool
	flag.StringVar(&configPath, "config", "", "TOML config (default: optional ./config.toml)")
	flag.StringVar(&envPath, "env-file", "", "env file (default: optional ./.env)")
	flag.StringVar(&transport, "transport", "", "stdio or http (overrides config)")
	flag.StringVar(&listen, "listen", "", "HTTP bind host:port (overrides config)")
	flag.StringVar(&issuer, "issuer", "", "external HTTP OAuth issuer origin (overrides config)")
	flag.BoolVar(&check, "check", false, "validate config and Vela connectivity, then exit")
	flag.BoolVar(&list, "list-tools", false, "print configured tool names, availability and purpose, then exit")
	flag.BoolVar(&version, "version", false, "print version and exit")
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if version {
		fmt.Println(buildversion.Get().String())
		return nil
	}
	c, err := config.Load(config.Options{ConfigPath: configPath, EnvPath: envPath})
	if err != nil {
		return err
	}
	if transport != "" {
		c.Server.Transport = transport
	}
	if listen != "" {
		c.HTTP.Listen = listen
	}
	if issuer != "" {
		c.HTTP.Issuer, err = config.CanonicalOrigin(issuer)
		if err != nil {
			return fmt.Errorf("invalid issuer: %w", err)
		}
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if list {
		for _, t := range catalog.Tools() {
			available := !catalog.HTTPOnly(t.Name) || (c.Server.Transport == "http" && c.Transfers.Enabled)
			if c.Server.Transport == "http" && t.Name == "download_file" {
				available = false
			}
			fmt.Printf("%-30s enabled=%-5t available=%-5t %s\n", t.Name, c.Tools[t.Name], available, t.Purpose)
		}
		return nil
	}
	if c.Token == "" && c.Server.Transport == "stdio" {
		return fmt.Errorf("set VELA_TOKEN in the environment or env file")
	}
	timeout, _ := time.ParseDuration(c.Vela.Timeout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	factory := velaclient.Factory{BaseURL: c.Vela.URL, Timeout: timeout, Retries: c.Vela.Retries}
	if c.Server.Transport == "http" {
		return runHTTP(ctx, c, factory, timeout, check)
	}
	credential := access.NewCredential(c.Token)
	gw, err := factory.Open(ctx, credential)
	if err != nil {
		return fmt.Errorf("cannot initialize Vela client")
	}
	startup, cancel := context.WithTimeout(ctx, timeout)
	info, err := gw.Info(startup)
	cancel()
	if err != nil {
		return fmt.Errorf("cannot verify Vela API/key: check VELA_URL, VELA_TOKEN and connectivity")
	}
	files, err := localfiles.New(c.Files.UploadRoots, c.Files.DownloadRoots)
	if err != nil {
		return fmt.Errorf("configured upload_roots/download_roots must be accessible existing directories")
	}
	defer files.Close()
	if check {
		b, _ := json.Marshal(map[string]any{"ok": true, "api_version": info.APIVersion, "server_version": info.ServerVersion, "scope": info.Key.Scope, "prefix": info.Key.Prefix})
		fmt.Println(string(b))
		return nil
	}
	service := &app.Service{Resolver: access.Static{Identity: access.Identity{Subject: "vela-key:" + info.Key.ID, Scope: info.Key.Scope, Prefix: info.Key.Prefix, Credential: credential}}, Gateways: factory, Files: files, Limits: app.Limits{MaxUploadBytes: c.Files.MaxUploadBytes, MaxInlineBytes: c.Files.MaxInlineBytes, MaxReadBytes: c.Files.MaxReadBytes, MaxDownloadBytes: c.Files.MaxDownloadBytes}}
	slog.Info("starting Vela MCP", "transport", "stdio", "version", buildversion.Get().Version)
	return stdio.Run(ctx, mcpserver.New(service, c.Tools, timeout))
}

// runHTTP deliberately has no fallback to VELA_TOKEN: every connector grant
// must come from its owner's verified browser authorization flow.
func runHTTP(ctx context.Context, c config.Config, factory velaclient.Factory, timeout time.Duration, check bool) error {
	// General reads retain bounded retries, while an interrupted HTTP upload
	// remains uncertain instead of inferring metadata success from equal bytes.
	factory.DisableUploadRetries = true
	accessTTL, _ := time.ParseDuration(c.OAuth.AccessTTL)
	refreshTTL, _ := time.ParseDuration(c.OAuth.RefreshTTL)
	codeTTL, _ := time.ParseDuration(c.OAuth.CodeTTL)
	auth, err := oauth.Open(ctx, oauth.Options{Issuer: c.HTTP.Issuer, DatabasePath: c.OAuth.DatabasePath, EncryptionKeyFile: c.OAuth.EncryptionKeyFile, AccessTTL: accessTTL, RefreshTTL: refreshTTL, CodeTTL: codeTTL, AllowedRedirectURIs: c.OAuth.AllowedRedirectURIs, VerifyKey: func(ctx context.Context, key string) (access.Identity, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		credential := access.NewCredential(key)
		gateway, err := factory.Open(ctx, credential)
		if err != nil {
			return access.Identity{}, err
		}
		info, err := gateway.Info(ctx)
		if err != nil {
			return access.Identity{}, err
		}
		return access.Identity{Subject: "vela-key:" + info.Key.ID, Scope: info.Key.Scope, Prefix: info.Key.Prefix, Credential: credential}, nil
	}})
	if err != nil {
		return fmt.Errorf("initialize OAuth: %w", err)
	}
	defer auth.Close()
	// HTTP users cannot use local paths, even when stdio roots were configured.
	service := &app.Service{Resolver: access.ContextResolver{}, Gateways: factory, Limits: app.Limits{MaxUploadBytes: c.Files.MaxUploadBytes, MaxInlineBytes: c.Files.MaxInlineBytes, MaxReadBytes: c.Files.MaxReadBytes, MaxDownloadBytes: c.Files.MaxDownloadBytes}}
	var broker *transfers.Service
	if c.Transfers.Enabled {
		ttl, _ := time.ParseDuration(c.Transfers.TTL)
		broker, err = transfers.Open(ctx, transfers.Options{BaseURL: c.HTTP.Issuer, DatabasePath: c.Transfers.DatabasePath, Service: service, LookupIdentity: auth.LookupIdentity, TTL: ttl, MaxUploadBytes: c.Files.MaxUploadBytes, MaxDownloadBytes: c.Files.MaxDownloadBytes})
		if err != nil {
			return fmt.Errorf("initialize transfers: %w", err)
		}
		defer broker.Close()
	}
	if check {
		b, _ := json.Marshal(map[string]any{"ok": true, "transport": "http", "issuer": c.HTTP.Issuer, "mcp_url": auth.ResourceURL(), "transfers": broker != nil})
		fmt.Println(string(b))
		return nil
	}
	tools := map[string]bool{}
	for name, enabled := range c.Tools {
		tools[name] = enabled
	}
	tools["download_file"] = false
	server := mcpserver.NewWithTransfers(service, tools, timeout, broker, c.HTTP.MaxToolResultBytes)
	opts := httpserver.Options{Issuer: c.HTTP.Issuer, Listen: c.HTTP.Listen, AllowedOrigins: c.HTTP.AllowedOrigins, OAuth: auth, MCP: server, MCPMaxBodyBytes: c.HTTP.MaxRequestBytes, RequestTimeout: timeout}
	if broker != nil {
		opts.Transfers = broker.Handler()
	}
	slog.Info("starting Vela MCP", "transport", "http", "listen", c.HTTP.Listen, "issuer", c.HTTP.Issuer, "version", buildversion.Get().Version)
	return httpserver.Run(ctx, opts)
}
