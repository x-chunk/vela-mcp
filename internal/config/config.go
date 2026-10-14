// Package config loads strict TOML plus environment secrets. Loading never
// changes the process environment. OS variables take precedence over .env.
package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/pelletier/go-toml/v2"
	"go.xchunk.org/vela-mcp/internal/catalog"
)

type Config struct {
	Server    Server          `toml:"server"`
	HTTP      HTTP            `toml:"http"`
	OAuth     OAuth           `toml:"oauth"`
	Transfers Transfers       `toml:"transfers"`
	Vela      Vela            `toml:"vela"`
	Files     Files           `toml:"files"`
	Tools     map[string]bool `toml:"tools"`
	// Secrets cannot be supplied through TOML and must not be logged.
	Token string `toml:"-"`
}

// Server selects a transport without changing application authorization.
type Server struct {
	Transport string `toml:"transport"`
}

// HTTP contains deployment addresses, not TLS/reverse-proxy configuration.
type HTTP struct {
	Listen             string   `toml:"listen"`
	Issuer             string   `toml:"issuer"`
	AllowedOrigins     []string `toml:"allowed_origins"`
	MaxRequestBytes    int64    `toml:"max_request_bytes"`
	MaxToolResultBytes int64    `toml:"max_tool_result_bytes"`
}

// OAuth keeps persistence paths and policy explicit. The encryption key is a
// private file outside SQLite; long-lived Vela keys are never token responses.
type OAuth struct {
	DatabasePath        string   `toml:"database_path"`
	EncryptionKeyFile   string   `toml:"encryption_key_file"`
	AccessTTL           string   `toml:"access_ttl"`
	RefreshTTL          string   `toml:"refresh_ttl"`
	CodeTTL             string   `toml:"code_ttl"`
	AllowedRedirectURIs []string `toml:"allowed_redirect_uris"`
}
type Transfers struct {
	Enabled      bool   `toml:"enabled"`
	DatabasePath string `toml:"database_path"`
	TTL          string `toml:"ttl"`
}
type Vela struct {
	URL     string `toml:"url"`
	Timeout string `toml:"timeout"`
	Retries int    `toml:"retries"`
}
type Files struct {
	UploadRoots      []string `toml:"upload_roots"`
	DownloadRoots    []string `toml:"download_roots"`
	MaxUploadBytes   int64    `toml:"max_upload_bytes"`
	MaxInlineBytes   int64    `toml:"max_inline_bytes"`
	MaxReadBytes     int64    `toml:"max_read_bytes"`
	MaxDownloadBytes int64    `toml:"max_download_bytes"`
}
type Options struct {
	ConfigPath, EnvPath string
	LookupEnv           func(string) (string, bool)
}

func Defaults() Config {
	return Config{
		Server:    Server{Transport: "stdio"},
		HTTP:      HTTP{Listen: "127.0.0.1:8081", Issuer: "http://127.0.0.1:8081", MaxRequestBytes: 50 << 20, MaxToolResultBytes: 128000},
		OAuth:     OAuth{DatabasePath: "data/oauth.db", EncryptionKeyFile: "data/encryption.key", AccessTTL: "15m", RefreshTTL: "720h", CodeTTL: "5m"},
		Transfers: Transfers{Enabled: true, DatabasePath: "data/transfers.db", TTL: "15m"},
		Vela:      Vela{URL: "http://127.0.0.1:8080", Timeout: "120s", Retries: 2},
		Files:     Files{MaxUploadBytes: 256 << 20, MaxInlineBytes: 8 << 20, MaxReadBytes: 32 << 10, MaxDownloadBytes: 256 << 20},
		Tools:     catalog.Defaults(),
	}
}

// Load reads optional ./config.toml and ./.env. An explicitly selected file
// (flag or VELA_MCP_CONFIG / VELA_MCP_ENV_FILE) must exist. Relative local file
// roots resolve against the working directory, not the config file location.
func Load(o Options) (Config, error) {
	lookup := o.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	envPath := o.EnvPath
	if envPath == "" {
		envPath, _ = lookup("VELA_MCP_ENV_FILE")
	}
	explicitEnv := envPath != ""
	if envPath == "" {
		envPath = ".env"
	}
	env := map[string]string{}
	if b, err := os.ReadFile(envPath); err == nil {
		var parseErr error
		env, parseErr = godotenv.Unmarshal(string(b))
		if parseErr != nil {
			return Config{}, fmt.Errorf("cannot parse env file (check KEY=value syntax)")
		}
	} else if explicitEnv || !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("cannot read env file")
	}
	value := func(name string) string {
		if v, ok := lookup(name); ok {
			return v
		}
		return env[name]
	}
	configPath := o.ConfigPath
	if configPath == "" {
		configPath = value("VELA_MCP_CONFIG")
	}
	explicitConfig := configPath != ""
	if configPath == "" {
		configPath = "config.toml"
	}
	c := Defaults()
	if b, err := os.ReadFile(configPath); err == nil {
		// Decode into an empty override map so unknown names are still checked.
		c.Tools = map[string]bool{}
		if err := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(&c); err != nil {
			return Config{}, fmt.Errorf("invalid config.toml: %w", err)
		}
	} else if explicitConfig || !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("cannot read config.toml")
	}
	if name, ok := catalog.Validate(c.Tools); !ok {
		return Config{}, fmt.Errorf("unknown tool %q in config.toml", name)
	}
	merged := catalog.Defaults()
	for name, enabled := range c.Tools {
		merged[name] = enabled
	}
	c.Tools = merged
	if v := value("VELA_URL"); v != "" {
		c.Vela.URL = v
	}
	if v := value("VELA_MCP_TRANSPORT"); v != "" {
		c.Server.Transport = v
	}
	if v := value("VELA_MCP_LISTEN"); v != "" {
		c.HTTP.Listen = v
	}
	if v := value("VELA_MCP_ISSUER"); v != "" {
		c.HTTP.Issuer = v
	}
	if origin, err := CanonicalOrigin(c.HTTP.Issuer); err == nil {
		c.HTTP.Issuer = origin
	} else {
		return Config{}, fmt.Errorf("http.issuer: %w", err)
	}
	for index, origin := range c.HTTP.AllowedOrigins {
		canonical, err := CanonicalOrigin(origin)
		if err != nil {
			return Config{}, fmt.Errorf("invalid http.allowed_origins")
		}
		c.HTTP.AllowedOrigins[index] = canonical
	}
	if v := value("VELA_MCP_OAUTH_DATABASE"); v != "" {
		c.OAuth.DatabasePath = v
	}
	if v := value("VELA_MCP_ENCRYPTION_KEY_FILE"); v != "" {
		c.OAuth.EncryptionKeyFile = v
	}
	if v := value("VELA_MCP_TRANSFER_DATABASE"); v != "" {
		c.Transfers.DatabasePath = v
	}
	c.Token = strings.TrimSpace(value("VELA_TOKEN"))
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	if c.Server.Transport != "stdio" && c.Server.Transport != "http" {
		return fmt.Errorf("server.transport must be stdio or http")
	}
	if _, _, err := net.SplitHostPort(c.HTTP.Listen); err != nil {
		return fmt.Errorf("http.listen must be host:port")
	}
	if err := validateOrigin(c.HTTP.Issuer); err != nil {
		return fmt.Errorf("http.issuer: %w", err)
	}
	for _, origin := range c.HTTP.AllowedOrigins {
		if err := validateOrigin(origin); err != nil {
			return fmt.Errorf("http.allowed_origins entries must be exact HTTPS or loopback HTTP origins")
		}
	}
	if c.HTTP.MaxRequestBytes < 1024 || c.HTTP.MaxRequestBytes > 64<<20 {
		return fmt.Errorf("http.max_request_bytes must be between 1024 and 64 MiB")
	}
	if c.HTTP.MaxToolResultBytes < 1024 || c.HTTP.MaxToolResultBytes > 64<<20 {
		return fmt.Errorf("http.max_tool_result_bytes must be between 1024 and 64 MiB")
	}
	if c.OAuth.DatabasePath == "" || c.OAuth.EncryptionKeyFile == "" || c.Transfers.DatabasePath == "" {
		return fmt.Errorf("SQLite and encryption key paths must not be empty")
	}
	accessTTL, err := time.ParseDuration(c.OAuth.AccessTTL)
	if err != nil || accessTTL <= 0 || accessTTL > time.Hour {
		return fmt.Errorf("oauth.access_ttl must be positive and <= 1h")
	}
	refreshTTL, err := time.ParseDuration(c.OAuth.RefreshTTL)
	if err != nil || refreshTTL <= accessTTL || refreshTTL > 365*24*time.Hour {
		return fmt.Errorf("oauth.refresh_ttl must exceed access_ttl and be <= 8760h")
	}
	if d, err := time.ParseDuration(c.OAuth.CodeTTL); err != nil || d <= 0 || d > 10*time.Minute {
		return fmt.Errorf("oauth.code_ttl must be positive and <= 10m")
	}
	if d, err := time.ParseDuration(c.Transfers.TTL); err != nil || d < time.Minute || d > time.Hour {
		return fmt.Errorf("transfers.ttl must be between 1m and 1h")
	}
	u, err := url.Parse(c.Vela.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("VELA_URL must be an http(s) URL without credentials, query or fragment")
	}
	if d, err := time.ParseDuration(c.Vela.Timeout); err != nil || d <= 0 {
		return fmt.Errorf("vela.timeout must be a positive Go duration")
	}
	if c.Vela.Retries < 0 || c.Vela.Retries > 5 {
		return fmt.Errorf("vela.retries must be between 0 and 5")
	}
	if c.Files.MaxUploadBytes <= 0 || c.Files.MaxInlineBytes <= 0 || c.Files.MaxReadBytes <= 0 || c.Files.MaxDownloadBytes <= 0 {
		return fmt.Errorf("file size limits must be positive")
	}
	if c.Files.MaxUploadBytes > 1<<40 || c.Files.MaxDownloadBytes > 1<<40 {
		return fmt.Errorf("streaming file limits must not exceed 1 TiB")
	}
	if c.Files.MaxInlineBytes > 32<<20 || c.Files.MaxReadBytes > 16<<20 {
		return fmt.Errorf("max_inline_bytes must be <= 32 MiB and max_read_bytes <= 16 MiB to bound MCP messages")
	}
	if c.Files.MaxInlineBytes > c.Files.MaxUploadBytes {
		return fmt.Errorf("max_inline_bytes must not exceed max_upload_bytes")
	}
	return nil
}

func validateOrigin(raw string) error {
	_, err := CanonicalOrigin(raw)
	return err
}

// CanonicalOrigin binds discovery, OAuth audiences and browser Origin checks
// to one identity even when an operator writes uppercase hosts/default ports.
func CanonicalOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("want an origin URL without credentials, path prefix, query or fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback())) {
		return "", fmt.Errorf("HTTPS is required except for loopback HTTP development")
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = ""
	u.RawPath = ""
	return u.String(), nil
}
