// Package agent is the run-loop wiring the whole pod together: ops
// listeners first, then cluster discovery, then persisted-state load with
// registration only when state is absent or credentials were rejected,
// then the loopback shim and the tunnel monitor.
//
// Two invariants govern the shape of this loop. Warm restarts make ZERO
// platform calls: if the state Secret already holds credentials, the agent
// serves entirely from it. And liveness never depends on the tunnel or the
// platform: a Cloudflare or Polylane outage must not crash-loop customer
// pods, so /healthz is up before anything can fail.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreplanelabs/polylane-k8s/internal/buildinfo"
	"github.com/coreplanelabs/polylane-k8s/internal/config"
	"github.com/coreplanelabs/polylane-k8s/internal/health"
	"github.com/coreplanelabs/polylane-k8s/internal/kube"
	"github.com/coreplanelabs/polylane-k8s/internal/metrics"
	"github.com/coreplanelabs/polylane-k8s/internal/platform"
	"github.com/coreplanelabs/polylane-k8s/internal/shim"
	"github.com/coreplanelabs/polylane-k8s/internal/tunnel"
)

// Exit codes: 3 signals a rejected/underscoped API key and 4 any other
// terminal registration answer (bad payload, plan limit). Both make the
// pod CrashLoopBackOff — the deliberate operator signal for "this install
// needs a human", per COR-32.
const (
	ExitCodeUnauthorized = 3
	ExitCodeTerminal     = 4
)

// ExitError asks main to exit with a specific status code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// Options configures Run.
type Options struct {
	Config config.Config
	// ChartVersion is injected by the Helm chart via the CHART_VERSION env
	// var; empty outside the chart.
	ChartVersion string
	Logger       *slog.Logger

	// Overridable for tests.
	KubeClient         *kube.Client
	ShutdownDrain      time.Duration           // default 25s (inside the chart's 70s grace)
	OnListen           func(name, addr string) // called as each listener binds
	TunnelPollInterval time.Duration           // default: tunnel package default (10s)
	TunnelFailureGrace time.Duration           // default: tunnel package default (3m)
}

// Run executes the agent until ctx is canceled (SIGTERM) or a fatal error
// occurs. A clean shutdown returns nil.
func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

func Run(ctx context.Context, opts Options) error {
	cfg := opts.Config
	log := opts.logger()
	drain := opts.ShutdownDrain
	if drain == 0 {
		drain = 25 * time.Second
	}

	met := metrics.New()

	// Shared mutable state, wired into health/readiness and the shim.
	var (
		credsPresent atomic.Bool
		kubeOK       atomic.Bool
		shimSecret   atomic.Value // string
		monitorPtr   atomic.Pointer[tunnel.Monitor]
	)
	shimSecret.Store("")

	hs := health.NewServer(health.Sources{
		CredentialsPresent: credsPresent.Load,
		TunnelReady: func() bool {
			m := monitorPtr.Load()
			return m != nil && m.Status().Ready
		},
		KubeReachable: kubeOK.Load,
		Version:       buildinfo.Version,
	}, log)

	// runCtx is canceled on ANY exit path — including a fatal error — so
	// every goroutine parked on the context (monitor, mirror, pending
	// fatal sends) unwinds and wg.Wait below cannot deadlock.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// Ops listeners first: liveness must answer before any step that can
	// block (discovery, registration) begins.
	fatal := make(chan error, 8)
	// sendFatal never blocks past shutdown: once runCtx is canceled a
	// full buffer just drops the (now moot) error instead of wedging the
	// sending goroutine.
	sendFatal := func(err error) {
		select {
		case fatal <- err:
		case <-runCtx.Done():
		}
	}
	var servers []*http.Server
	var wg sync.WaitGroup
	serve := func(name, addr string, handler http.Handler) error {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("agent: listening on %s (%s): %w", addr, name, err)
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				sendFatal(fmt.Errorf("agent: %s server: %w", name, err))
			}
		}()
		log.Info("listener up", "name", name, "addr", ln.Addr().String())
		if opts.OnListen != nil {
			opts.OnListen(name, ln.Addr().String())
		}
		return nil
	}
	// teardown is the single exit gate: unwind goroutines, drain the
	// servers, and wait for everything before Run returns.
	teardown := func() {
		cancelRun()
		// The tunnel container stops first (native-sidecar ordering); we
		// drain the shim for up to the drain budget, then close ops.
		drainCtx, cancel := context.WithTimeout(context.Background(), drain)
		defer cancel()
		for _, srv := range servers {
			srv.Shutdown(drainCtx) //nolint:errcheck // best-effort drain
		}
		wg.Wait()
	}

	if err := serve("health", cfg.Ops.HealthListen, hs.Handler()); err != nil {
		return err
	}
	if cfg.Ops.MetricsListen != "" {
		if err := serve("metrics", cfg.Ops.MetricsListen, met.Handler()); err != nil {
			teardown()
			return err
		}
	}

	// Cluster discovery: identity is the kube-system namespace UID —
	// stable across reinstalls, so re-installs converge on the same
	// Polylane account.
	kc := opts.KubeClient
	if kc == nil {
		var err error
		kc, err = kube.New(kube.Options{
			TokenPath:     cfg.Kube.TokenPath,
			CAPath:        cfg.Kube.CAPath,
			NamespacePath: cfg.Kube.NamespacePath,
			Logger:        log,
		})
		if err != nil {
			teardown()
			return fmt.Errorf("agent: building kube client: %w", err)
		}
	}

	clusterUID, kubeVersion, err := discover(runCtx, kc, log)
	if err != nil {
		teardown()
		return err
	}
	kubeOK.Store(true)
	namespace, err := kc.Namespace()
	if err != nil {
		teardown()
		return fmt.Errorf("agent: reading namespace: %w", err)
	}

	store := kube.NewStateStore(kc, namespace, cfg.StateSecret.Name)
	pc := platform.New(platform.Options{
		BaseURL: cfg.PlatformURL,
		APIKey:  cfg.APIKey(),
		Logger:  log,
	})
	regReq := platform.RegisterRequest{
		ClusterUID:        clusterUID,
		ClusterName:       cfg.ClusterName,
		KubernetesVersion: kubeVersion,
		AgentVersion:      buildinfo.Version,
		Distribution:      cfg.Distribution,
		Namespace:         namespace,
		ChartVersion:      opts.ChartVersion,
	}

	register := func(ctx context.Context) error {
		resp, err := pc.RegisterWithRetry(ctx, regReq)
		if err != nil {
			met.IncRegistration("terminal")
			return err
		}
		st := kube.State{
			TunnelToken:    resp.TunnelToken,
			ShimSecret:     resp.ShimSecret,
			AccountID:      resp.AccountID,
			TunnelID:       resp.TunnelID,
			TunnelHostname: resp.TunnelHostname,
			RegisteredAt:   time.Now().UTC(),
		}
		if err := saveWithRetry(ctx, store, st, log); err != nil {
			return fmt.Errorf("agent: persisting registration state: %w", err)
		}
		shimSecret.Store(resp.ShimSecret)
		credsPresent.Store(true)
		met.IncRegistration("success")
		log.Info("registered with platform",
			"account_id", resp.AccountID, "tunnel_id", resp.TunnelID,
			"tunnel_hostname", resp.TunnelHostname)
		return nil
	}

	// Load persisted state first: when it is complete, this boot makes no
	// platform call at all.
	st, err := store.Load(runCtx)
	if err != nil {
		teardown()
		return fmt.Errorf("agent: loading state secret %s/%s: %w", namespace, cfg.StateSecret.Name, err)
	}
	switch {
	case st != nil && st.Complete():
		shimSecret.Store(st.ShimSecret)
		credsPresent.Store(true)
		log.Info("loaded persisted state; skipping registration (warm boot)",
			"account_id", st.AccountID, "tunnel_id", st.TunnelID)
	default:
		log.Info("no usable persisted state; registering (cold boot)")
		if err := register(runCtx); err != nil {
			teardown()
			return classifyRegistrationErr(err, log)
		}
	}

	// The shim: loopback-only by construction.
	upstream, err := url.Parse(kc.BaseURL())
	if err != nil {
		teardown()
		return fmt.Errorf("agent: parsing kube API URL %q: %w", kc.BaseURL(), err)
	}
	shimHandler := shim.NewHandler(shim.Config{
		Secret:        func() string { s, _ := shimSecret.Load().(string); return s },
		UpstreamURL:   upstream,
		Transport:     kc.Transport(),
		Token:         kc.Token,
		AgentVersion:  buildinfo.Version,
		KubeReachable: kubeOK.Load,
		Metrics:       met,
		Logger:        log,
	})
	shimLn, err := shim.ListenLoopback(cfg.Shim.Listen)
	if err != nil {
		teardown()
		return fmt.Errorf("agent: shim listener: %w", err)
	}
	shimSrv := &http.Server{Handler: shimHandler, ReadHeaderTimeout: 10 * time.Second}
	servers = append(servers, shimSrv)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := shimSrv.Serve(shimLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			sendFatal(fmt.Errorf("agent: shim server: %w", err))
		}
	}()
	log.Info("shim up", "addr", shimLn.Addr().String())
	if opts.OnListen != nil {
		opts.OnListen("shim", shimLn.Addr().String())
	}

	// State is loaded-or-registered and the shim is listening: let the
	// tunnel container start.
	hs.SetStartupReady()

	// Tunnel monitor: sustained failure means our credentials were likely
	// rotated platform-side; re-register (idempotent — register never
	// rotates) and persist whatever the platform says is current.
	mon := tunnel.New(tunnel.Options{
		MetricsURL:   cfg.Tunnel.MetricsURL,
		Interval:     opts.TunnelPollInterval,
		FailureGrace: opts.TunnelFailureGrace,
		Logger:       log,
		OnSustainedFailure: func(ctx context.Context) {
			log.Warn("tunnel not ready beyond grace period; re-registering")
			if err := register(ctx); err != nil {
				if platform.IsTerminal(err) {
					sendFatal(classifyRegistrationErr(err, log))
					return
				}
				log.Error("re-registration failed", "error", err)
			}
		},
	})
	monitorPtr.Store(mon)
	wg.Add(1)
	go func() {
		defer wg.Done()
		mon.Run(runCtx) //nolint:errcheck // returns ctx.Err() only
	}()

	// Mirror tunnel state and kube reachability into metrics/readiness.
	wg.Add(1)
	go func() {
		defer wg.Done()
		mirror(runCtx, mon, kc, met, &kubeOK)
	}()

	log.Info("agent running",
		"version", buildinfo.String(),
		"cluster_uid", clusterUID,
		"kubernetes_version", kubeVersion,
		"namespace", namespace)

	var runErr error
	select {
	case <-runCtx.Done():
	case runErr = <-fatal:
	}
	teardown()
	return runErr
}

// discover retries cluster identity + version until success: the kube API
// is expected to be reachable from inside the cluster, so failures here
// are transient (pod networking still coming up).
func discover(ctx context.Context, kc *kube.Client, log *slog.Logger) (uid, version string, err error) {
	backoff := time.Second
	for {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		uid, err = kc.Identity(callCtx)
		if err == nil {
			version, err = kc.Version(callCtx)
		}
		cancel()
		if err == nil {
			return uid, version, nil
		}
		log.Warn("cluster discovery failed; retrying", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("agent: cluster discovery: %w", err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func saveWithRetry(ctx context.Context, store *kube.StateStore, st kube.State, log *slog.Logger) error {
	var err error
	backoff := time.Second
	for attempt := range 5 {
		if err = store.Save(ctx, st); err == nil {
			return nil
		}
		log.Warn("saving state secret failed; retrying", "attempt", attempt+1, "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return err
}

// classifyRegistrationErr turns terminal registration answers into the
// documented exit codes; anything else passes through unchanged.
func classifyRegistrationErr(err error, log *slog.Logger) error {
	if errors.Is(err, platform.ErrUnauthorized) {
		log.Error("platform rejected the API key: it is revoked, invalid, or missing the cloud_accounts:write scope. " +
			"Update the Secret referenced by apiKey.existingSecret (or recreate the key in the Polylane console) and restart.")
		return &ExitError{Code: ExitCodeUnauthorized, Err: err}
	}
	if platform.IsTerminal(err) {
		log.Error("platform refused the registration terminally; see the error for the reason (plan limit or malformed payload)", "error", err)
		return &ExitError{Code: ExitCodeTerminal, Err: err}
	}
	return err
}

// mirror keeps metrics and readiness in sync with observed reality:
// tunnel gauge + reconnect counter from the monitor, kube reachability
// from a periodic ping.
func mirror(ctx context.Context, mon *tunnel.Monitor, kc *kube.Client, met *metrics.Metrics, kubeOK *atomic.Bool) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	pingTick := time.NewTicker(30 * time.Second)
	defer pingTick.Stop()
	var lastReconnects uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			st := mon.Status()
			met.SetTunnelReady(st.Ready)
			for lastReconnects < st.Reconnects {
				met.IncTunnelReconnects()
				lastReconnects++
			}
		case <-pingTick.C:
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			kubeOK.Store(kc.Ping(pingCtx) == nil)
			cancel()
		}
	}
}
