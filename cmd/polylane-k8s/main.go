// Command polylane-k8s is the in-cluster Polylane agent: it registers the
// cluster once over plain HTTPS, persists the returned Cloudflare Tunnel
// credentials to a state Secret for the cloudflared sidecar, and serves a
// strict read-only kube-API shim on loopback that is reachable only
// through the tunnel.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/coreplanelabs/polylane-k8s/internal/agent"
	"github.com/coreplanelabs/polylane-k8s/internal/buildinfo"
	"github.com/coreplanelabs/polylane-k8s/internal/config"
)

type cli struct {
	Run     runCmd           `cmd:"" help:"Run the agent."`
	Config  configCmd        `cmd:"" help:"Configuration utilities."`
	Version kong.VersionFlag `help:"Print version and exit."`
	Ver     versionCmd       `cmd:"" name:"version" help:"Print version."`
}

type runCmd struct {
	Config string `short:"c" env:"POLYLANE_CONFIG" placeholder:"config.yaml" help:"Path to the YAML configuration file."`

	// Flag overrides: flags > env > config file > defaults.
	PlatformURL  string `env:"POLYLANE_PLATFORM_URL" help:"Override platform_url."`
	ClusterName  string `env:"POLYLANE_CLUSTER_NAME" help:"Override cluster_name."`
	ChartVersion string `env:"CHART_VERSION" hidden:"" help:"Chart version, injected by the Helm chart."`
	LogLevel     string `env:"POLYLANE_LOG_LEVEL" help:"Override log.level (debug|info|warn|error)."`
	LogFormat    string `env:"POLYLANE_LOG_FORMAT" help:"Override log.format (json|text)."`
}

func (c *runCmd) load() (config.Config, error) {
	cfg := config.Default()
	if c.Config != "" {
		var err error
		cfg, err = config.Load(c.Config)
		if err != nil {
			return cfg, err
		}
	}
	if c.PlatformURL != "" {
		cfg.PlatformURL = c.PlatformURL
	}
	if c.ClusterName != "" {
		cfg.ClusterName = c.ClusterName
	}
	if c.LogLevel != "" {
		cfg.Log.Level = c.LogLevel
	}
	if c.LogFormat != "" {
		cfg.Log.Format = c.LogFormat
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("configuration is invalid:\n%w", err)
	}
	return cfg, nil
}

func (c *runCmd) Run() error {
	cfg, err := c.load()
	if err != nil {
		return err
	}
	if cfg.APIKey() == "" {
		return fmt.Errorf("environment variable %s is empty; is the API key Secret mounted?", cfg.APIKeyEnv)
	}

	log := newLogger(cfg.Log)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting polylane-k8s", "version", buildinfo.String())
	return agent.Run(ctx, agent.Options{
		Config:       cfg,
		ChartVersion: c.ChartVersion,
		Logger:       log,
	})
}

type configCmd struct {
	Validate configValidateCmd `cmd:"" help:"Validate a configuration file."`
}

type configValidateCmd struct {
	Config string `arg:"" placeholder:"config.yaml" help:"Path to the YAML configuration file."`
}

func (c *configValidateCmd) Run() error {
	cfg, err := config.Load(c.Config)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%s is invalid:\n%w", c.Config, err)
	}
	fmt.Printf("%s is valid\n", c.Config)
	return nil
}

type versionCmd struct{}

func (versionCmd) Run() error {
	fmt.Println("polylane-k8s " + buildinfo.String())
	return nil
}

func newLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func main() {
	k := kong.Parse(&cli{},
		kong.Name("polylane-k8s"),
		kong.Description("Minimal read-only Kubernetes agent connecting a cluster to Polylane via Cloudflare Tunnel."),
		kong.UsageOnError(),
		kong.Vars{"version": "polylane-k8s " + buildinfo.String()},
	)
	if err := k.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "polylane-k8s: "+err.Error())
		var exit *agent.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}
