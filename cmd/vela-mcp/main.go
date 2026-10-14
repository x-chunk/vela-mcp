// vela-mcp is a stdio MCP server. This is the composition root: configuration,
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
	"go.xchunk.org/vela-mcp/internal/transport/stdio"
	"go.xchunk.org/vela-mcp/internal/velaclient"
)

func main() {
	if err := run(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}
func run() error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var configPath, envPath string
	var check, list, version bool
	flag.StringVar(&configPath, "config", "", "TOML config (default: optional ./config.toml)")
	flag.StringVar(&envPath, "env-file", "", "env file (default: optional ./.env)")
	flag.BoolVar(&check, "check", false, "validate config and Vela connectivity, then exit")
	flag.BoolVar(&list, "list-tools", false, "print configured tool names, availability and purpose, then exit")
	flag.BoolVar(&version, "version", false, "print version and exit")
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if version {
		fmt.Println(mcpserver.Version)
		return nil
	}
	c, err := config.Load(config.Options{ConfigPath: configPath, EnvPath: envPath})
	if err != nil {
		return err
	}
	if list {
		for _, t := range catalog.Tools() {
			fmt.Printf("%-30s enabled=%-5t %s\n", t.Name, c.Tools[t.Name], t.Purpose)
		}
		return nil
	}
	if c.Token == "" {
		return fmt.Errorf("set VELA_TOKEN in the environment or env file")
	}
	timeout, _ := time.ParseDuration(c.Vela.Timeout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	factory := velaclient.Factory{BaseURL: c.Vela.URL, Timeout: timeout, Retries: c.Vela.Retries}
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
	slog.Info("starting Vela MCP", "transport", "stdio", "version", mcpserver.Version)
	return stdio.Run(ctx, mcpserver.New(service, c.Tools, timeout))
}
