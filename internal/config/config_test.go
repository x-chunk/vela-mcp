package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func absent(string) (string, bool) { return "", false }
func TestPrecedenceAndStrictConfig(t *testing.T) {
	d := t.TempDir()
	env := write(t, d, ".env", "VELA_URL=http://env:8080\nVELA_TOKEN=dummy-env\n")
	config := write(t, d, "config.toml", "[vela]\nurl='http://toml:8080'\n[tools]\nread_file=false\npurge_file=true\n")
	c, err := Load(Options{ConfigPath: config, EnvPath: env, LookupEnv: func(k string) (string, bool) {
		if k == "VELA_URL" {
			return "http://os:8080", true
		}
		return "", false
	}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Vela.URL != "http://os:8080" || c.Token != "dummy-env" || c.Tools["read_file"] || !c.Tools["purge_file"] || !c.Tools["upload_file"] {
		t.Fatalf("precedence or defaults failed")
	}
	for _, body := range []string{"[tools]\nread_flie=true", "[vela]\ntimeuot='2s'", "[files]\nmax_read_bytes=0", "[tools]\nread_file='false'"} {
		p := write(t, d, "bad.toml", body)
		if _, err := Load(Options{ConfigPath: p, EnvPath: env, LookupEnv: absent}); err == nil {
			t.Errorf("accepted invalid configuration %q", body)
		}
	}
}
func TestEnvironmentBlankSecretOverridesFile(t *testing.T) {
	d := t.TempDir()
	env := write(t, d, ".env", "VELA_TOKEN=dummy\n")
	c, err := Load(Options{EnvPath: env, ConfigPath: write(t, d, "empty.toml", ""), LookupEnv: func(k string) (string, bool) { return "", k == "VELA_TOKEN" }})
	if err != nil || c.Token != "" {
		t.Fatal("blank OS variable must override file")
	}
}
func TestMissingExplicitFilesAndURL(t *testing.T) {
	d := t.TempDir()
	if _, err := Load(Options{EnvPath: filepath.Join(d, "missing"), LookupEnv: absent}); err == nil {
		t.Fatal("missing explicit env accepted")
	}
	c := Defaults()
	for _, url := range []string{"file:///tmp/x", "http://secret:token@localhost", "http://localhost?token=x", "http://localhost#x"} {
		c.Vela.URL = url
		if c.Validate() == nil {
			t.Fatalf("accepted unsafe URL")
		}
	}
}
