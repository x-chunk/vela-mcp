// Package config loads strict TOML plus environment secrets. Loading never
// changes the process environment. OS variables take precedence over .env.
package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/pelletier/go-toml/v2"
	"go.xchunk.org/vela-mcp/internal/catalog"
)

type Config struct {
	Vela  Vela            `toml:"vela"`
	Files Files           `toml:"files"`
	Tools map[string]bool `toml:"tools"`
	// Secrets cannot be supplied through TOML and must not be logged.
	Token string `toml:"-"`
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
		Vela:  Vela{URL: "http://127.0.0.1:8080", Timeout: "120s", Retries: 2},
		Files: Files{MaxUploadBytes: 256 << 20, MaxInlineBytes: 8 << 20, MaxReadBytes: 1 << 20, MaxDownloadBytes: 256 << 20},
		Tools: catalog.Defaults(),
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
	c.Token = strings.TrimSpace(value("VELA_TOKEN"))
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
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
