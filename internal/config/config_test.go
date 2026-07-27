package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validBase() Config {
	cfg := Default()
	cfg.StateSecret.Name = "polylane-state"
	return cfg
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string // "" means valid
	}{
		{"valid base", func(c *Config) {}, ""},
		{"platform path", func(c *Config) { c.PlatformURL = "https://example.com/api" }, ""},
		{"insecure platform explicitly allowed", func(c *Config) {
			c.PlatformURL = "http://fakeplatform:8180"
			c.AllowInsecurePlatform = true
		}, ""},
		{"localhost shim listen", func(c *Config) { c.Shim.Listen = "localhost:8080" }, ""},
		{"ipv6 loopback shim listen", func(c *Config) { c.Shim.Listen = "[::1]:8080" }, ""},
		{"localhost tunnel metrics", func(c *Config) { c.Tunnel.MetricsURL = "http://localhost:2000" }, ""},
		{"ipv6 tunnel metrics", func(c *Config) { c.Tunnel.MetricsURL = "http://[::1]:2000" }, ""},
		{"missing platform url", func(c *Config) { c.PlatformURL = "" }, "platform_url is required"},
		{"bad platform url scheme", func(c *Config) { c.PlatformURL = "ftp://x" }, "want an https URL"},
		{"insecure platform", func(c *Config) { c.PlatformURL = "http://example.com" }, "must use https"},
		{"platform url missing host", func(c *Config) { c.PlatformURL = "https:///api" }, "host is required"},
		{"platform url userinfo", func(c *Config) { c.PlatformURL = "https://user@example.com" }, "userinfo"},
		{"platform url query", func(c *Config) { c.PlatformURL = "https://example.com?tenant=x" }, "query parameters"},
		{"platform url fragment", func(c *Config) { c.PlatformURL = "https://example.com/#x" }, "fragments"},
		{"missing api key env", func(c *Config) { c.APIKeyEnv = "" }, "api_key_env is required"},
		{"non-loopback shim listen", func(c *Config) { c.Shim.Listen = "0.0.0.0:8080" }, "not a loopback address"},
		{"hostname shim listen", func(c *Config) { c.Shim.Listen = "example.com:8080" }, "literal loopback IP"},
		{"shim listen without port", func(c *Config) { c.Shim.Listen = "127.0.0.1" }, "not host:port"},
		{"empty shim listen", func(c *Config) { c.Shim.Listen = "" }, "shim.listen"},
		{"missing health listen", func(c *Config) { c.Ops.HealthListen = "" }, "ops.health_listen is required"},
		{"missing tunnel metrics url", func(c *Config) { c.Tunnel.MetricsURL = "" }, "tunnel.metrics_url: is required"},
		{"remote tunnel metrics", func(c *Config) { c.Tunnel.MetricsURL = "http://example.com:2000" }, "loopback"},
		{"https tunnel metrics", func(c *Config) { c.Tunnel.MetricsURL = "https://127.0.0.1:2000" }, "want an http URL"},
		{"tunnel metrics path", func(c *Config) { c.Tunnel.MetricsURL = "http://127.0.0.1:2000/metrics" }, "path must be empty"},
		{"tunnel metrics query", func(c *Config) { c.Tunnel.MetricsURL = "http://127.0.0.1:2000?x=1" }, "query parameters"},
		{"missing state secret", func(c *Config) { c.StateSecret.Name = "" }, "state_secret.name is required"},
		{"bad log level", func(c *Config) { c.Log.Level = "verbose" }, "log.level"},
		{"bad log format", func(c *Config) { c.Log.Format = "logfmt" }, "log.format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validBase()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantSub == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("Validate() = %v, want substring %q", err, tt.wantSub)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	// Deployments rely on these exact values; the chart and docs mirror them.
	cfg := Default()
	if cfg.PlatformURL != "https://api.polylane.com" {
		t.Errorf("PlatformURL = %q", cfg.PlatformURL)
	}
	if cfg.AllowInsecurePlatform {
		t.Error("AllowInsecurePlatform = true, want secure default")
	}
	if cfg.APIKeyEnv != "POLYLANE_API_KEY" {
		t.Errorf("APIKeyEnv = %q", cfg.APIKeyEnv)
	}
	if cfg.Shim.Listen != "127.0.0.1:8080" {
		t.Errorf("Shim.Listen = %q", cfg.Shim.Listen)
	}
	if cfg.Ops.HealthListen != ":8081" || cfg.Ops.MetricsListen != ":9090" {
		t.Errorf("Ops = %+v", cfg.Ops)
	}
	if cfg.Tunnel.MetricsURL != "http://127.0.0.1:2000" {
		t.Errorf("Tunnel.MetricsURL = %q", cfg.Tunnel.MetricsURL)
	}
	if cfg.Kube.TokenPath != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Errorf("Kube.TokenPath = %q", cfg.Kube.TokenPath)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Errorf("Log = %+v", cfg.Log)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, `
platform_url: https://api.baseberry.cc
cluster_name: prod-us-east
state_secret:
  name: polylane-state
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlatformURL != "https://api.baseberry.cc" {
		t.Errorf("PlatformURL = %q", cfg.PlatformURL)
	}
	if cfg.ClusterName != "prod-us-east" {
		t.Errorf("ClusterName = %q", cfg.ClusterName)
	}
	// Unset fields keep defaults.
	if cfg.Shim.Listen != "127.0.0.1:8080" {
		t.Errorf("Shim.Listen = %q, want default", cfg.Shim.Listen)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "platform_urk: https://typo.example\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "platform_urk") {
		t.Fatalf("Load() = %v, want unknown-key error naming platform_urk", err)
	}
}

func TestMultiDocumentRejected(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "cluster_name: a\n---\ncluster_name: b\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("Load() = %v, want multiple-documents error", err)
	}
}

func TestAPIKeyIndirection(t *testing.T) {
	cfg := validBase()
	cfg.APIKeyEnv = "TEST_POLYLANE_KEY"
	t.Setenv("TEST_POLYLANE_KEY", "sk_test_123")
	if got := cfg.APIKey(); got != "sk_test_123" {
		t.Errorf("APIKey() = %q", got)
	}
	cfg.APIKeyEnv = ""
	if got := cfg.APIKey(); got != "" {
		t.Errorf("APIKey() with empty env name = %q, want empty", got)
	}
}
