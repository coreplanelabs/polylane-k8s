// Package tunnel watches the cloudflared sidecar by polling the /ready
// route of its metrics endpoint.
//
// The monitor only observes: it never restarts cloudflared and it never
// feeds pod liveness. Its one action hook, OnSustainedFailure, runs
// serially and blocking inside the polling loop once readiness has been
// down for a full grace window, so a handler (typically re-registration)
// can never overlap a previous invocation.
package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Status is a point-in-time snapshot of tunnel readiness.
type Status struct {
	Ready       bool
	LastReady   time.Time // zero until the first successful probe
	LastChecked time.Time
	Reconnects  uint64 // not-ready -> ready transitions after the first
}

// Options configures a Monitor. MetricsURL is the only required field.
type Options struct {
	// MetricsURL is cloudflared's metrics endpoint; the monitor probes
	// GET {MetricsURL}/ready and treats any 2xx as ready.
	MetricsURL string
	// Interval is the probe period. Default 10s.
	Interval time.Duration
	// FailureGrace is how long readiness must stay down before
	// OnSustainedFailure fires. Each window is jittered by ±10% so a
	// fleet of agents does not act in lockstep. Default 3m.
	FailureGrace time.Duration
	// OnSustainedFailure is invoked serially and blocking from the
	// polling loop; the grace window restarts only after it returns, so
	// it re-fires at most once per window while the failure persists.
	OnSustainedFailure func(ctx context.Context)
	Logger             *slog.Logger
	// HTTPClient defaults to a client with a 3s timeout so a wedged
	// cloudflared cannot stall the polling loop.
	HTTPClient *http.Client
}

const (
	defaultInterval     = 10 * time.Second
	defaultFailureGrace = 3 * time.Minute
	defaultProbeTimeout = 3 * time.Second
)

// Monitor polls cloudflared's readiness endpoint and tracks transitions.
type Monitor struct {
	readyURL           string
	interval           time.Duration
	grace              time.Duration
	onSustainedFailure func(ctx context.Context)
	logger             *slog.Logger
	client             *http.Client

	mu     sync.Mutex
	status Status

	// Owned by the Run goroutine; never touched elsewhere.
	everReady     bool
	graceDeadline time.Time
}

// New builds a Monitor; call Run to start polling.
func New(opts Options) *Monitor {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	grace := opts.FailureGrace
	if grace <= 0 {
		grace = defaultFailureGrace
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultProbeTimeout}
	}
	return &Monitor{
		readyURL:           strings.TrimRight(opts.MetricsURL, "/") + "/ready",
		interval:           interval,
		grace:              grace,
		onSustainedFailure: opts.OnSustainedFailure,
		logger:             logger,
		client:             client,
	}
}

// Run polls until ctx is done and returns ctx.Err(). It probes once
// immediately so Status is populated without waiting a full interval.
func (m *Monitor) Run(ctx context.Context) error {
	// If the tunnel has never been ready, the grace window is measured
	// from the start of the loop.
	m.graceDeadline = time.Now().Add(jittered(m.grace))
	m.probe(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.probe(ctx)
		}
	}
}

// Status returns a concurrency-safe snapshot.
func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Monitor) probe(ctx context.Context) {
	ready := m.checkReady(ctx)
	now := time.Now()

	m.mu.Lock()
	wasReady := m.status.Ready
	m.status.Ready = ready
	m.status.LastChecked = now
	if ready {
		m.status.LastReady = now
		if !wasReady && m.everReady {
			m.status.Reconnects++
		}
	}
	reconnects := m.status.Reconnects
	m.mu.Unlock()

	if ready {
		if !wasReady {
			if m.everReady {
				m.logger.Info("tunnel ready again", "reconnects", reconnects)
			} else {
				m.logger.Info("tunnel ready")
			}
		}
		m.everReady = true
		return
	}

	if wasReady {
		// A ready -> not-ready transition starts a fresh grace window;
		// only failure sustained past it triggers the handler.
		m.graceDeadline = now.Add(jittered(m.grace))
		m.logger.Warn("tunnel not ready", "grace", m.grace)
		return
	}

	if now.Before(m.graceDeadline) {
		return
	}

	m.logger.Warn("tunnel failure sustained past grace window", "grace", m.grace)
	if m.onSustainedFailure != nil {
		m.onSustainedFailure(ctx)
	}
	// Restart the window only after the handler returns: at most one
	// invocation per window, never overlapping itself.
	m.graceDeadline = time.Now().Add(jittered(m.grace))
}

// checkReady reports whether cloudflared answered 2xx on /ready. Any
// error — connection refused, timeout, bad URL — counts as not ready.
func (m *Monitor) checkReady(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.readyURL, nil)
	if err != nil {
		m.logger.Error("tunnel: building probe request", "url", m.readyURL, "error", err)
		return false
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// jittered spreads d over [0.9d, 1.1d]. crypto/rand keeps gosec quiet and
// the call is far off any hot path.
func jittered(d time.Duration) time.Duration {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return d
	}
	f := float64(binary.LittleEndian.Uint64(b[:])) / float64(math.MaxUint64)
	return time.Duration(float64(d) * (0.9 + 0.2*f))
}
