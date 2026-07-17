package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreplanelabs/polylane-k8s/internal/config"
	"github.com/coreplanelabs/polylane-k8s/internal/kube"
)

// The in-process lifecycle tests exercise the whole run-loop against fake
// kube API, platform, and tunnel servers. The load-bearing assertion is
// COR-32's contract invariant: a warm boot makes ZERO platform calls.

// fakeKube is a minimal TLS kube API: identity, version, and one Secret.
type fakeKube struct {
	srv *httptest.Server

	mu     sync.Mutex
	secret map[string]string // decoded data
}

func newFakeKube(t *testing.T) *fakeKube {
	t.Helper()
	fk := &fakeKube{secret: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/namespaces/kube-system", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"metadata":{"uid":"11111111-2222-3333-4444-555555555555"}}`)
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"gitVersion":"v1.33.1"}`)
	})
	mux.HandleFunc("GET /api/v1/namespaces/polylane/secrets/polylane-state", func(w http.ResponseWriter, r *http.Request) {
		fk.mu.Lock()
		defer fk.mu.Unlock()
		data := map[string]string{}
		for k, v := range fk.secret {
			data[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
			"metadata": map[string]any{"name": "polylane-state"},
			"data":     data,
		})
	})
	mux.HandleFunc("PATCH /api/v1/namespaces/polylane/secrets/polylane-state", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // test server
		var patch struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fk.mu.Lock()
		for k, v := range patch.Data {
			raw, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				fk.mu.Unlock()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fk.secret[k] = string(raw)
		}
		fk.mu.Unlock()
		fmt.Fprint(w, `{}`)
	})
	fk.srv = httptest.NewTLSServer(mux)
	t.Cleanup(fk.srv.Close)
	return fk
}

func (fk *fakeKube) get(key string) string {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return fk.secret[key]
}

func (fk *fakeKube) client(t *testing.T) *kube.Client {
	t.Helper()
	dir := t.TempDir()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fk.srv.Certificate().Raw})
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	kc, err := kube.New(kube.Options{
		APIServerURL:  fk.srv.URL,
		TokenPath:     write("token", "test-sa-token"),
		CAPath:        write("ca.crt", string(caPEM)),
		NamespacePath: write("namespace", "polylane"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return kc
}

// fakePlatform counts registrations; optionally fails them all terminally.
type fakePlatform struct {
	srv      *httptest.Server
	calls    atomic.Int64
	failWith int // 0 => succeed
}

func newFakePlatform(t *testing.T, failWith int) *fakePlatform {
	t.Helper()
	fp := &fakePlatform{failWith: failWith}
	fp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cloud_accounts/kubernetes/agent/register" {
			http.NotFound(w, r)
			return
		}
		fp.calls.Add(1)
		if fp.failWith != 0 {
			w.WriteHeader(fp.failWith)
			fmt.Fprint(w, `{"error":"nope"}`)
			return
		}
		fmt.Fprint(w, `{
			"accountId":"acct_test","agentId":"agent_test","tunnelId":"tunnel_test",
			"tunnelToken":"tok_test","tunnelHostname":"k8s-acct-test.example.com",
			"shimSecret":"shhh_test","_object":"kubernetes_agent_registration"}`)
	}))
	t.Cleanup(fp.srv.Close)
	return fp
}

func testConfig(fk *fakeKube, fp *fakePlatform, tunnelURL string) config.Config {
	cfg := config.Default()
	cfg.PlatformURL = fp.srv.URL
	cfg.APIKeyEnv = "TEST_AGENT_API_KEY"
	cfg.ClusterName = "lifecycle-test"
	cfg.Shim.Listen = "127.0.0.1:0"
	cfg.Ops.HealthListen = "127.0.0.1:0"
	cfg.Ops.MetricsListen = "" // disabled
	cfg.Tunnel.MetricsURL = tunnelURL
	cfg.StateSecret.Name = "polylane-state"
	return cfg
}

type runHarness struct {
	addrs map[string]string
	mu    sync.Mutex
	done  chan error
}

func startRun(t *testing.T, ctx context.Context, opts Options) *runHarness {
	t.Helper()
	h := &runHarness{addrs: map[string]string{}, done: make(chan error, 1)}
	opts.OnListen = func(name, addr string) {
		h.mu.Lock()
		h.addrs[name] = addr
		h.mu.Unlock()
	}
	opts.ShutdownDrain = time.Second
	go func() { h.done <- Run(ctx, opts) }()
	return h
}

func (h *runHarness) addr(t *testing.T, name string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		addr := h.addrs[name]
		h.mu.Unlock()
		if addr != "" {
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener %q never came up", name)
	return ""
}

func waitStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec // test URL
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s never reached %d (last: %s)", url, want, last)
}

func TestColdBootRegistersOnceAndPersists(t *testing.T) {
	t.Setenv("TEST_AGENT_API_KEY", "sk_test")
	fk := newFakeKube(t)
	fp := newFakePlatform(t, 0)
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			fmt.Fprint(w, "ok")
			return
		}
		http.NotFound(w, r)
	}))
	defer ready.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := startRun(t, ctx, Options{
		Config:     testConfig(fk, fp, ready.URL),
		KubeClient: fk.client(t),
	})

	healthAddr := h.addr(t, "health")
	waitStatus(t, "http://"+healthAddr+"/startupz", http.StatusOK)
	waitStatus(t, "http://"+healthAddr+"/readyz", http.StatusOK)

	if got := fp.calls.Load(); got != 1 {
		t.Errorf("cold boot made %d platform calls, want exactly 1", got)
	}
	// Registration state persisted to the Secret the sidecar mounts.
	if got := fk.get("tunnelToken"); got != "tok_test" {
		t.Errorf("persisted tunnelToken = %q", got)
	}
	if got := fk.get("shimSecret"); got != "shhh_test" {
		t.Errorf("persisted shimSecret = %q", got)
	}

	// The shim answers its local /healthz with the registered secret.
	shimAddr := h.addr(t, "shim")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+shimAddr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-polylane-shim-key", "shhh_test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("shim /healthz with registered key = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run() = %v, want nil on clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestWarmBootMakesZeroPlatformCalls(t *testing.T) {
	t.Setenv("TEST_AGENT_API_KEY", "sk_test")
	fk := newFakeKube(t)
	fk.secret = map[string]string{
		"tunnelToken":    "tok_warm",
		"shimSecret":     "shhh_warm",
		"accountId":      "acct_warm",
		"tunnelId":       "tunnel_warm",
		"tunnelHostname": "k8s-warm.example.com",
		"registeredAt":   time.Now().UTC().Format(time.RFC3339),
	}
	fp := newFakePlatform(t, 0)
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer ready.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := startRun(t, ctx, Options{
		Config:     testConfig(fk, fp, ready.URL),
		KubeClient: fk.client(t),
	})

	healthAddr := h.addr(t, "health")
	waitStatus(t, "http://"+healthAddr+"/startupz", http.StatusOK)
	waitStatus(t, "http://"+healthAddr+"/readyz", http.StatusOK)

	if got := fp.calls.Load(); got != 0 {
		t.Errorf("warm boot made %d platform calls, want ZERO (COR-32 contract invariant)", got)
	}

	// Warm boot serves with the persisted shim secret.
	shimAddr := h.addr(t, "shim")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+shimAddr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-polylane-shim-key", "shhh_warm")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("shim /healthz with persisted key = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestUnauthorizedRegistrationExitsCode3(t *testing.T) {
	t.Setenv("TEST_AGENT_API_KEY", "sk_revoked")
	fk := newFakeKube(t)
	fp := newFakePlatform(t, http.StatusUnauthorized)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := startRun(t, ctx, Options{
		Config:     testConfig(fk, fp, "http://127.0.0.1:1"),
		KubeClient: fk.client(t),
	})

	select {
	case err := <-h.done:
		var exit *ExitError
		if !asExitError(err, &exit) || exit.Code != ExitCodeUnauthorized {
			t.Fatalf("Run() = %v, want ExitError code %d", err, ExitCodeUnauthorized)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return on terminal registration error")
	}
	if got := fp.calls.Load(); got != 1 {
		t.Errorf("terminal 401 retried: %d calls, want 1", got)
	}
}

func TestSustainedTunnelFailureReRegisters(t *testing.T) {
	t.Setenv("TEST_AGENT_API_KEY", "sk_test")
	fk := newFakeKube(t)
	fp := newFakePlatform(t, 0)
	var tunnelUp atomic.Bool
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tunnelUp.Load() {
			fmt.Fprint(w, "ok")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ready.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := startRun(t, ctx, Options{
		Config:             testConfig(fk, fp, ready.URL),
		KubeClient:         fk.client(t),
		TunnelPollInterval: 10 * time.Millisecond,
		TunnelFailureGrace: 100 * time.Millisecond,
	})

	healthAddr := h.addr(t, "health")
	waitStatus(t, "http://"+healthAddr+"/startupz", http.StatusOK)
	if got := fp.calls.Load(); got != 1 {
		t.Fatalf("cold boot calls = %d, want 1", got)
	}

	// Tunnel never comes up: after the grace window the agent re-registers
	// (idempotently) to pick up possibly-rotated credentials.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && fp.calls.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := fp.calls.Load(); got < 2 {
		t.Fatalf("no re-registration after sustained tunnel failure (calls = %d)", got)
	}

	// Tunnel recovers: readiness converges without a restart.
	tunnelUp.Store(true)
	waitStatus(t, "http://"+healthAddr+"/readyz", http.StatusOK)

	cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// Regression: a terminal error on the RE-registration path (tunnel down,
// platform answers 401 — e.g. key revoked after rotation) must make Run
// return ExitError code 3 on its own. It used to wedge in wg.Wait with
// every listener already dark, relying on a kubelet liveness kill to die.
func TestSustainedFailureTerminalErrorExitsCode3(t *testing.T) {
	t.Setenv("TEST_AGENT_API_KEY", "sk_revoked_later")
	fk := newFakeKube(t)
	fk.secret = map[string]string{
		"tunnelToken":    "tok_warm",
		"shimSecret":     "shhh_warm",
		"accountId":      "acct_warm",
		"tunnelId":       "tunnel_warm",
		"tunnelHostname": "k8s-warm.example.com",
		"registeredAt":   time.Now().UTC().Format(time.RFC3339),
	}
	fp := newFakePlatform(t, http.StatusUnauthorized)
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // tunnel never comes up
	}))
	defer ready.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := startRun(t, ctx, Options{
		Config:             testConfig(fk, fp, ready.URL),
		KubeClient:         fk.client(t),
		TunnelPollInterval: 10 * time.Millisecond,
		TunnelFailureGrace: 50 * time.Millisecond,
	})

	select {
	case err := <-h.done:
		var exit *ExitError
		if !asExitError(err, &exit) || exit.Code != ExitCodeUnauthorized {
			t.Fatalf("Run() = %v, want ExitError code %d", err, ExitCodeUnauthorized)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run wedged instead of exiting on terminal re-registration error")
	}
}

func asExitError(err error, target **ExitError) bool {
	for err != nil {
		if e, ok := err.(*ExitError); ok { //nolint:errorlint // walking manually
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
