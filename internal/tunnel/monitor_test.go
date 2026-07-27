package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testInterval = 2 * time.Millisecond
	testGrace    = 40 * time.Millisecond
	testDeadline = 5 * time.Second
)

// minGrace is the shortest jittered window (-10%), minus a rounding
// epsilon, for elapsed-time assertions.
const minGrace = testGrace*9/10 - time.Millisecond

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor deadline-polls cond; no fixed sleeps for synchronization.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeTunnel is a toggling cloudflared metrics endpoint.
type fakeTunnel struct {
	srv   *httptest.Server
	ready atomic.Bool
}

func newFakeTunnel(t *testing.T) *fakeTunnel {
	t.Helper()
	f := &fakeTunnel{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			http.NotFound(w, r)
			return
		}
		if f.ready.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// startMonitor runs m until test cleanup and fails the test if Run does
// not stop after cancellation.
func startMonitor(t *testing.T, opts Options) *Monitor {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	m := New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testDeadline):
			t.Error("monitor did not stop after cancel")
		}
	})
	return m
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	m := New(Options{MetricsURL: "http://127.0.0.1:2000/"})
	if m.interval != 10*time.Second {
		t.Errorf("interval = %v, want 10s", m.interval)
	}
	if m.grace != 3*time.Minute {
		t.Errorf("grace = %v, want 3m", m.grace)
	}
	if m.client.Timeout != 3*time.Second {
		t.Errorf("client timeout = %v, want 3s", m.client.Timeout)
	}
	transport, ok := m.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", m.client.Transport)
	}
	if transport.Proxy != nil {
		t.Error("client transport consults a proxy for the loopback probe")
	}
	if m.readyURL != "http://127.0.0.1:2000/ready" {
		t.Errorf("readyURL = %q, want trailing slash collapsed", m.readyURL)
	}
}

func TestMonitorBecomesReady(t *testing.T) {
	t.Parallel()
	f := newFakeTunnel(t)
	f.ready.Store(true)
	var fires atomic.Int64
	m := startMonitor(t, Options{
		MetricsURL:         f.srv.URL,
		Interval:           testInterval,
		FailureGrace:       time.Hour,
		OnSustainedFailure: func(context.Context) { fires.Add(1) },
	})

	waitFor(t, "ready", func() bool { return m.Status().Ready })
	st := m.Status()
	if st.LastReady.IsZero() {
		t.Error("LastReady is zero after a successful probe")
	}
	if st.LastChecked.IsZero() {
		t.Error("LastChecked is zero after a probe")
	}
	if st.Reconnects != 0 {
		t.Errorf("Reconnects = %d on first success, want 0", st.Reconnects)
	}
	if n := fires.Load(); n != 0 {
		t.Errorf("OnSustainedFailure fired %d times while ready", n)
	}
}

func TestMonitorCountsReconnects(t *testing.T) {
	t.Parallel()
	f := newFakeTunnel(t)
	f.ready.Store(true)
	m := startMonitor(t, Options{
		MetricsURL:   f.srv.URL,
		Interval:     testInterval,
		FailureGrace: time.Hour,
	})

	waitFor(t, "initial ready", func() bool { return m.Status().Ready })
	f.ready.Store(false)
	waitFor(t, "not ready", func() bool { return !m.Status().Ready })
	f.ready.Store(true)
	waitFor(t, "first reconnect", func() bool {
		st := m.Status()
		return st.Ready && st.Reconnects == 1
	})
	f.ready.Store(false)
	waitFor(t, "not ready again", func() bool { return !m.Status().Ready })
	f.ready.Store(true)
	waitFor(t, "second reconnect", func() bool { return m.Status().Reconnects == 2 })
}

func TestMonitorNeverReadyFiresAfterGrace(t *testing.T) {
	t.Parallel()
	f := newFakeTunnel(t) // never ready
	fires := make(chan time.Time, 32)
	start := time.Now()
	m := startMonitor(t, Options{
		MetricsURL:   f.srv.URL,
		Interval:     testInterval,
		FailureGrace: testGrace,
		OnSustainedFailure: func(context.Context) {
			select {
			case fires <- time.Now():
			default:
			}
		},
	})

	var fired time.Time
	select {
	case fired = <-fires:
	case <-time.After(testDeadline):
		t.Fatal("OnSustainedFailure never fired")
	}
	if elapsed := fired.Sub(start); elapsed < minGrace {
		t.Errorf("fired after %v, want >= %v (grace window from Run start)", elapsed, minGrace)
	}
	st := m.Status()
	if st.Ready {
		t.Error("Ready = true, want false")
	}
	if !st.LastReady.IsZero() {
		t.Error("LastReady set without any successful probe")
	}
}

func TestMonitorRefiresOncePerGraceWindow(t *testing.T) {
	t.Parallel()
	f := newFakeTunnel(t) // never ready
	fires := make(chan time.Time, 32)
	startMonitor(t, Options{
		MetricsURL:   f.srv.URL,
		Interval:     testInterval,
		FailureGrace: testGrace,
		OnSustainedFailure: func(context.Context) {
			select {
			case fires <- time.Now():
			default:
			}
		},
	})

	var first, second time.Time
	select {
	case first = <-fires:
	case <-time.After(testDeadline):
		t.Fatal("first fire never happened")
	}
	select {
	case second = <-fires:
	case <-time.After(testDeadline):
		t.Fatal("second fire never happened while failure persisted")
	}
	if gap := second.Sub(first); gap < minGrace {
		t.Errorf("re-fired after %v, want >= %v (once per grace window)", gap, minGrace)
	}
}

func TestMonitorGraceStartsAtReadyToNotReadyTransition(t *testing.T) {
	t.Parallel()
	f := newFakeTunnel(t)
	f.ready.Store(true)
	fires := make(chan time.Time, 32)
	start := time.Now()
	m := startMonitor(t, Options{
		MetricsURL:   f.srv.URL,
		Interval:     testInterval,
		FailureGrace: testGrace,
		OnSustainedFailure: func(context.Context) {
			select {
			case fires <- time.Now():
			default:
			}
		},
	})

	waitFor(t, "ready", func() bool { return m.Status().Ready })
	// Outlive the initial (never-ready) window while healthy, so a stale
	// deadline would fire immediately after the flip below.
	waitFor(t, "initial window to lapse", func() bool {
		return time.Since(start) > 2*testGrace
	})
	if len(fires) != 0 {
		t.Fatal("OnSustainedFailure fired while tunnel was ready")
	}

	flipped := time.Now()
	f.ready.Store(false)
	var fired time.Time
	select {
	case fired = <-fires:
	case <-time.After(testDeadline):
		t.Fatal("OnSustainedFailure never fired after sustained failure")
	}
	if elapsed := fired.Sub(flipped); elapsed < minGrace {
		t.Errorf("fired %v after transition, want >= %v (window restarts at true->false)", elapsed, minGrace)
	}
}

func TestMonitorStatusAvailableDuringBlockingCallback(t *testing.T) {
	t.Parallel()
	f := newFakeTunnel(t) // never ready
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	m := startMonitor(t, Options{
		MetricsURL:   f.srv.URL,
		Interval:     testInterval,
		FailureGrace: testGrace,
		OnSustainedFailure: func(context.Context) {
			once.Do(func() {
				entered <- struct{}{}
				<-release
			})
		},
	})

	select {
	case <-entered:
	case <-time.After(testDeadline):
		t.Fatal("callback never entered")
	}
	got := make(chan Status, 1)
	go func() { got <- m.Status() }()
	select {
	case st := <-got:
		if st.Ready {
			t.Error("Ready = true during sustained failure")
		}
	case <-time.After(testDeadline):
		t.Error("Status() blocked while callback was running")
	}
	close(release)
}

func TestMonitorRunReturnsOnCancel(t *testing.T) {
	t.Parallel()
	f := newFakeTunnel(t)
	m := New(Options{
		MetricsURL:   f.srv.URL,
		Interval:     testInterval,
		FailureGrace: time.Hour,
		Logger:       discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	waitFor(t, "first probe", func() bool { return !m.Status().LastChecked.IsZero() })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(testDeadline):
		t.Fatal("Run did not return after cancel")
	}
}

func TestMonitorUnreachableEndpointIsNotReady(t *testing.T) {
	t.Parallel()
	f := newFakeTunnel(t)
	f.ready.Store(true)
	f.srv.Close() // connection refused from here on
	m := startMonitor(t, Options{
		MetricsURL:   f.srv.URL,
		Interval:     testInterval,
		FailureGrace: time.Hour,
	})

	waitFor(t, "probe against dead endpoint", func() bool { return !m.Status().LastChecked.IsZero() })
	if m.Status().Ready {
		t.Error("Ready = true against an unreachable endpoint")
	}
}

func TestMonitorDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	redirected := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	m := New(Options{MetricsURL: source.URL, HTTPClient: source.Client(), Logger: discardLogger()})
	if m.checkReady(context.Background()) {
		t.Error("redirect response counted as ready")
	}
	select {
	case <-redirected:
		t.Error("tunnel monitor followed redirect away from its configured endpoint")
	default:
	}
}
