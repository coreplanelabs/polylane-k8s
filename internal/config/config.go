// Package config defines the agent's configuration: one YAML file whose
// shape is the single mental model shared by the CLI flags, the Helm
// values, and the docs. Precedence is flags > env > config file > defaults.
//
// Secrets are never inlined: api_key_env names an environment variable to
// read at use time, so config files stay committable.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration.
type Config struct {
	// PlatformURL is the base URL of the Polylane API the agent registers
	// against. The agent talks to it only on first boot and after a
	// credentials rejection — never in steady state.
	PlatformURL string `yaml:"platform_url"`
	// AllowInsecurePlatform permits a cleartext platform_url. It exists only
	// for hermetic development environments; production registration carries
	// an API key and must use HTTPS.
	AllowInsecurePlatform bool `yaml:"allow_insecure_platform,omitempty"`
	// APIKeyEnv names the environment variable holding the Polylane API
	// key (scoped cloud_accounts:write). Never the key itself.
	APIKeyEnv string `yaml:"api_key_env"`
	// ClusterName is the human-readable name shown in the console. Empty
	// means the platform names the cluster cluster-<uid[:8]>.
	ClusterName string `yaml:"cluster_name,omitempty"`
	// Distribution optionally identifies the cluster flavor (eks|gke|aks|...).
	Distribution string `yaml:"distribution,omitempty"`

	Shim        Shim        `yaml:"shim"`
	Ops         Ops         `yaml:"ops"`
	Tunnel      Tunnel      `yaml:"tunnel"`
	StateSecret StateSecret `yaml:"state_secret"`
	Kube        Kube        `yaml:"kube"`
	Log         Log         `yaml:"log"`
}

// Shim configures the read-only kube-API pass-through proxy. The shim is
// only ever reached through the Cloudflare Tunnel; it must stay on
// loopback by construction.
type Shim struct {
	// Listen is the shim's listen address. Validation refuses anything
	// that does not resolve to a loopback IP.
	Listen string `yaml:"listen"`
}

// Ops configures the agent's cluster-facing listeners: health probes and
// Prometheus metrics.
type Ops struct {
	HealthListen  string `yaml:"health_listen"`
	MetricsListen string `yaml:"metrics_listen"`
}

// Tunnel configures how the agent observes the cloudflared sidecar.
type Tunnel struct {
	// MetricsURL is cloudflared's metrics endpoint; the agent polls
	// {metrics_url}/ready to monitor tunnel health cross-container.
	MetricsURL string `yaml:"metrics_url"`
}

// StateSecret locates the pre-created Secret that persists registration
// state ({tunnelToken, shimSecret, accountId, tunnelId, tunnelHostname})
// so warm restarts make zero platform calls.
type StateSecret struct {
	// Name is the Secret's name in the agent's own namespace. The chart
	// sets it; required.
	Name string `yaml:"name"`
}

// Kube locates the projected ServiceAccount credentials.
type Kube struct {
	TokenPath     string `yaml:"token_path"`
	CAPath        string `yaml:"ca_path"`
	NamespacePath string `yaml:"namespace_path"`
}

// Log configures structured logging.
type Log struct {
	Level  string `yaml:"level"`  // "debug" | "info" | "warn" | "error"
	Format string `yaml:"format"` // "json" | "text"
}

// APIKey reads the Polylane API key from the configured environment variable.
func (c Config) APIKey() string {
	if c.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.APIKeyEnv)
}

// Default returns the baseline configuration that YAML, env, and flags
// override.
func Default() Config {
	return Config{
		PlatformURL: "https://api.polylane.com",
		// The NAME of an env var, not a credential.
		APIKeyEnv: "POLYLANE_API_KEY", // #nosec G101
		Shim: Shim{
			Listen: "127.0.0.1:8080",
		},
		Ops: Ops{
			HealthListen:  ":8081",
			MetricsListen: ":9090",
		},
		Tunnel: Tunnel{
			MetricsURL: "http://127.0.0.1:2000",
		},
		// Standard projected-ServiceAccount paths, not credentials.
		Kube: Kube{ // #nosec G101
			TokenPath:     "/var/run/secrets/kubernetes.io/serviceaccount/token",
			CAPath:        "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
			NamespacePath: "/var/run/secrets/kubernetes.io/serviceaccount/namespace",
		},
		Log: Log{
			Level:  "info",
			Format: "json",
		},
	}
}

// Load reads path (YAML) over the default configuration.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("config: reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	// A stray "---" mid-file starts a second YAML document that would
	// otherwise be ignored silently — everything after it dropped. Refuse.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return cfg, fmt.Errorf("config: %s contains multiple YAML documents; remove the stray --- separator", path)
	}
	return cfg, nil
}

// Validate checks the configuration for consistency.
func (c *Config) Validate() error {
	var errs []error

	switch u, err := url.Parse(c.PlatformURL); {
	case c.PlatformURL == "":
		errs = append(errs, errors.New("platform_url is required"))
	case err != nil:
		errs = append(errs, fmt.Errorf("platform_url %q is invalid: %v", c.PlatformURL, err))
	case u.Scheme != "https" && u.Scheme != "http":
		errs = append(errs, fmt.Errorf("platform_url %q is invalid (want an https URL)", c.PlatformURL))
	case u.Hostname() == "":
		errs = append(errs, fmt.Errorf("platform_url %q is invalid (host is required)", c.PlatformURL))
	case u.User != nil:
		errs = append(errs, fmt.Errorf("platform_url %q is invalid (userinfo is not allowed)", c.PlatformURL))
	case u.RawQuery != "" || u.ForceQuery:
		errs = append(errs, fmt.Errorf("platform_url %q is invalid (query parameters are not allowed)", c.PlatformURL))
	case u.Fragment != "":
		errs = append(errs, fmt.Errorf("platform_url %q is invalid (fragments are not allowed)", c.PlatformURL))
	case u.Scheme == "http" && !c.AllowInsecurePlatform:
		errs = append(errs, errors.New("platform_url must use https (allow_insecure_platform is for development only)"))
	}

	if c.APIKeyEnv == "" {
		errs = append(errs, errors.New("api_key_env is required (the NAME of the environment variable holding the API key)"))
	}

	if err := validateLoopback(c.Shim.Listen); err != nil {
		errs = append(errs, fmt.Errorf("shim.listen: %w", err))
	}

	if c.Ops.HealthListen == "" {
		errs = append(errs, errors.New("ops.health_listen is required (the startup probe gates the tunnel container on it)"))
	}

	if err := validateTunnelMetricsURL(c.Tunnel.MetricsURL); err != nil {
		errs = append(errs, fmt.Errorf("tunnel.metrics_url: %w", err))
	}

	if c.StateSecret.Name == "" {
		errs = append(errs, errors.New("state_secret.name is required (the chart pre-creates it)"))
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q is invalid (want debug|info|warn|error)", c.Log.Level))
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format %q is invalid (want json|text)", c.Log.Format))
	}

	return errors.Join(errs...)
}

// validateLoopback enforces the shim's loopback-only invariant: the kube
// proxy path must exist solely behind the tunnel, never on a pod IP.
func validateLoopback(addr string) error {
	if addr == "" {
		return errors.New("listen address is required")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %v", addr, err)
	}
	return validateLoopbackHost(host)
}

func validateLoopbackHost(host string) error {
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("host %q must use a literal loopback IP or localhost", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("host %q is not a loopback address", host)
	}
	return nil
}

// validateTunnelMetricsURL keeps the cloudflared probe on the pod's loopback
// interface. It is an internal sidecar endpoint, never a general HTTP target.
func validateTunnelMetricsURL(raw string) error {
	if raw == "" {
		return errors.New("is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is invalid: %v", raw, err)
	}
	switch {
	case u.Scheme != "http":
		return fmt.Errorf("%q is invalid (want an http URL)", raw)
	case u.Hostname() == "":
		return fmt.Errorf("%q is invalid (host is required)", raw)
	case u.User != nil:
		return fmt.Errorf("%q is invalid (userinfo is not allowed)", raw)
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Errorf("%q is invalid (query parameters are not allowed)", raw)
	case u.Fragment != "":
		return fmt.Errorf("%q is invalid (fragments are not allowed)", raw)
	case u.Path != "" && u.Path != "/":
		return fmt.Errorf("%q is invalid (path must be empty)", raw)
	}
	if err := validateLoopbackHost(u.Hostname()); err != nil {
		return err
	}
	return nil
}
