package kube

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClient bundles a Client wired against a fake TLS kube API with the
// on-disk credential files the tests mutate.
type testClient struct {
	c         *Client
	ts        *httptest.Server
	tokenPath string
	nsPath    string
	caPath    string
}

func newTestClient(t *testing.T, handler http.Handler) *testClient {
	t.Helper()
	if handler == nil {
		handler = http.NotFoundHandler()
	}
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	caPath := writeFile(t, dir, "ca.crt", string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: ts.Certificate().Raw,
	})))
	tokenPath := writeFile(t, dir, "token", "test-token\n")
	nsPath := writeFile(t, dir, "namespace", "polylane\n")

	c, err := New(Options{
		APIServerURL:  ts.URL,
		TokenPath:     tokenPath,
		CAPath:        caPath,
		NamespacePath: nsPath,
		Logger:        discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testClient{c: c, ts: ts, tokenPath: tokenPath, nsPath: nsPath, caPath: caPath}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func jsonHandler(t *testing.T, wantPath, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			t.Errorf("request path = %q, want %q", r.URL.Path, wantPath)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

func TestNewErrors(t *testing.T) {
	t.Parallel()

	valid := func(t *testing.T) Options {
		t.Helper()
		dir := t.TempDir()
		// Any syntactically valid PEM certificate works: New only builds
		// the pool, it does not dial.
		ts := httptest.NewTLSServer(http.NotFoundHandler())
		t.Cleanup(ts.Close)
		ca := writeFile(t, dir, "ca.crt", string(pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: ts.Certificate().Raw,
		})))
		return Options{
			APIServerURL:  "https://10.0.0.1:443",
			TokenPath:     writeFile(t, dir, "token", "tok"),
			CAPath:        ca,
			NamespacePath: writeFile(t, dir, "namespace", "ns"),
			Logger:        discardLogger(),
		}
	}

	tests := []struct {
		name    string
		mutate  func(t *testing.T, o *Options)
		wantErr string
	}{
		{
			name:    "valid options",
			mutate:  func(t *testing.T, o *Options) { t.Helper() },
			wantErr: "",
		},
		{
			name: "missing CA file",
			mutate: func(t *testing.T, o *Options) {
				t.Helper()
				o.CAPath = filepath.Join(t.TempDir(), "missing.crt")
			},
			wantErr: "reading CA bundle",
		},
		{
			name: "CA bundle without certificates",
			mutate: func(t *testing.T, o *Options) {
				t.Helper()
				o.CAPath = writeFile(t, t.TempDir(), "ca.crt", "not a pem")
			},
			wantErr: "contains no certificates",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := valid(t)
			tt.mutate(t, &opts)
			_, err := New(opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewInClusterDefaults(t *testing.T) {
	// t.Setenv — no t.Parallel.
	ca := writeFile(t, t.TempDir(), "ca.crt", testCertPEM(t))

	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	c, err := New(Options{CAPath: ca, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := c.BaseURL(), "https://10.96.0.1:443"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
	if got, want := c.tokenPath, defaultTokenPath; got != want {
		t.Errorf("tokenPath = %q, want %q", got, want)
	}
	if got, want := c.namespacePath, defaultNamespacePath; got != want {
		t.Errorf("namespacePath = %q, want %q", got, want)
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := New(Options{CAPath: ca, Logger: discardLogger()}); err == nil ||
		!strings.Contains(err.Error(), "API server address is empty") {
		t.Errorf("New without address: error = %v, want API-server-address error", err)
	}
}

func testCertPEM(t *testing.T) string {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}))
}

func TestTokenReread(t *testing.T) {
	t.Parallel()
	tc := newTestClient(t, nil)

	got, err := tc.c.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "test-token" {
		t.Fatalf("Token() = %q, want %q (trimmed)", got, "test-token")
	}

	fi, err := os.Stat(tc.tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	origMTime := fi.ModTime()

	// Rewrite the file but pin the mtime back: within the TTL the cached
	// token must be served without re-reading.
	if err := os.WriteFile(tc.tokenPath, []byte("second-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tc.tokenPath, origMTime, origMTime); err != nil {
		t.Fatal(err)
	}
	if got, err := tc.c.Token(); err != nil || got != "test-token" {
		t.Fatalf("Token() after same-mtime rewrite = %q, %v; want cached %q", got, err, "test-token")
	}

	// Bump the mtime: the new content must be picked up.
	bumped := origMTime.Add(2 * time.Second)
	if err := os.Chtimes(tc.tokenPath, bumped, bumped); err != nil {
		t.Fatal(err)
	}
	if got, err := tc.c.Token(); err != nil || got != "second-token" {
		t.Fatalf("Token() after mtime bump = %q, %v; want %q", got, err, "second-token")
	}

	// Rewrite again with the mtime pinned, then expire the TTL: the file
	// must be re-read even though the mtime never moved.
	if err := os.WriteFile(tc.tokenPath, []byte("third-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tc.tokenPath, bumped, bumped); err != nil {
		t.Fatal(err)
	}
	if got, err := tc.c.Token(); err != nil || got != "second-token" {
		t.Fatalf("Token() before TTL expiry = %q, %v; want cached %q", got, err, "second-token")
	}
	tc.c.tokenTTL = 0
	if got, err := tc.c.Token(); err != nil || got != "third-token" {
		t.Fatalf("Token() after TTL expiry = %q, %v; want %q", got, err, "third-token")
	}
}

func TestTokenErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(t *testing.T, tc *testClient)
		wantErr string
	}{
		{
			name: "missing file",
			mutate: func(t *testing.T, tc *testClient) {
				t.Helper()
				if err := os.Remove(tc.tokenPath); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "reading token",
		},
		{
			name: "empty file",
			mutate: func(t *testing.T, tc *testClient) {
				t.Helper()
				if err := os.WriteFile(tc.tokenPath, []byte("  \n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newTestClient(t, nil)
			tt.mutate(t, tc)
			if _, err := tc.c.Token(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Token error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestTokenConcurrent(t *testing.T) {
	t.Parallel()
	tc := newTestClient(t, nil)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := tc.c.Token(); err != nil || got != "test-token" {
				t.Errorf("Token() = %q, %v; want %q", got, err, "test-token")
			}
		}()
	}
	wg.Wait()
}

func TestNamespace(t *testing.T) {
	t.Parallel()

	t.Run("read trimmed and cached", func(t *testing.T) {
		t.Parallel()
		tc := newTestClient(t, nil)
		got, err := tc.c.Namespace()
		if err != nil || got != "polylane" {
			t.Fatalf("Namespace() = %q, %v; want %q", got, err, "polylane")
		}
		// Cached: removing the file must not matter anymore.
		if err := os.Remove(tc.nsPath); err != nil {
			t.Fatal(err)
		}
		if got, err := tc.c.Namespace(); err != nil || got != "polylane" {
			t.Fatalf("Namespace() after file removal = %q, %v; want cached %q", got, err, "polylane")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		tc := newTestClient(t, nil)
		if err := os.Remove(tc.nsPath); err != nil {
			t.Fatal(err)
		}
		if _, err := tc.c.Namespace(); err == nil || !strings.Contains(err.Error(), "reading namespace") {
			t.Fatalf("Namespace error = %v, want reading-namespace error", err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		t.Parallel()
		tc := newTestClient(t, nil)
		if err := os.WriteFile(tc.nsPath, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := tc.c.Namespace(); err == nil || !strings.Contains(err.Error(), "is empty") {
			t.Fatalf("Namespace error = %v, want is-empty error", err)
		}
	})
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler func(t *testing.T) http.Handler
		want    string
		wantErr string
	}{
		{
			name: "ok",
			handler: func(t *testing.T) http.Handler {
				t.Helper()
				return jsonHandler(t, "/api/v1/namespaces/kube-system",
					`{"metadata":{"name":"kube-system","uid":"11e029c8-9c4e-4a34-a558-9c02b3b8d1a4"}}`)
			},
			want: "11e029c8-9c4e-4a34-a558-9c02b3b8d1a4",
		},
		{
			name: "missing uid",
			handler: func(t *testing.T) http.Handler {
				t.Helper()
				return jsonHandler(t, "/api/v1/namespaces/kube-system", `{"metadata":{"name":"kube-system"}}`)
			},
			wantErr: "no metadata.uid",
		},
		{
			name: "malformed json",
			handler: func(t *testing.T) http.Handler {
				t.Helper()
				return jsonHandler(t, "/api/v1/namespaces/kube-system", `{"metadata":`)
			},
			wantErr: "decoding response",
		},
		{
			name: "forbidden",
			handler: func(t *testing.T) http.Handler {
				t.Helper()
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, `{"kind":"Status","code":403}`, http.StatusForbidden)
				})
			},
			wantErr: "403",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newTestClient(t, tt.handler(t))
			got, err := tc.c.Identity(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Identity error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Identity: %v", err)
			}
			if got != tt.want {
				t.Errorf("Identity() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{name: "ok", body: `{"major":"1","minor":"31","gitVersion":"v1.31.2"}`, want: "v1.31.2"},
		{name: "missing gitVersion", body: `{"major":"1"}`, wantErr: "no gitVersion"},
		{name: "malformed json", body: `{"gitVersion"`, wantErr: "decoding response"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newTestClient(t, jsonHandler(t, "/version", tt.body))
			got, err := tc.c.Version(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Version error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Version: %v", err)
			}
			if got != tt.want {
				t.Errorf("Version() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr string
	}{
		{name: "ok", status: http.StatusOK},
		{name: "server error", status: http.StatusInternalServerError, wantErr: "500"},
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: "401"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, `{"gitVersion":"v1.31.2"}`)
			}))
			err := tc.c.Ping(context.Background())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Ping: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Ping error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestRequestHeaders(t *testing.T) {
	t.Parallel()

	headers := make(chan http.Header, 1)
	tc := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		fmt.Fprint(w, `{"gitVersion":"v1.31.2"}`)
	}))

	if err := tc.c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	h := <-headers
	if got, want := h.Get("Authorization"), "Bearer test-token"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := h.Get("User-Agent"); !strings.HasPrefix(got, "polylane-k8s/") {
		t.Errorf("User-Agent = %q, want polylane-k8s/<version>", got)
	}
	if got, want := h.Get("Accept"), "application/json"; got != want {
		t.Errorf("Accept = %q, want %q", got, want)
	}
}

func TestClientDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	redirected := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	tc := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	if err := tc.c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("Ping error = %v, want redirect status", err)
	}
	select {
	case <-redirected:
		t.Error("kube client followed redirect away from the configured API server")
	default:
	}
}

func TestTransportTrustsClusterCAWithoutAuth(t *testing.T) {
	t.Parallel()

	headers := make(chan http.Header, 1)
	tc := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))

	hc := &http.Client{Transport: tc.c.Transport()}
	resp, err := hc.Get(tc.c.BaseURL() + "/probe")
	if err != nil {
		t.Fatalf("GET via Transport(): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := (<-headers).Get("Authorization"); got != "" {
		t.Errorf("Transport() added Authorization = %q, want none", got)
	}
}
