package shim

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "shhh-0123456789abcdef"

type metricsCapture struct {
	mu       sync.Mutex
	outcomes []string
}

func (m *metricsCapture) RecordRequest(outcome string, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes = append(m.outcomes, outcome)
}

func (m *metricsCapture) last(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.outcomes) == 0 {
		t.Fatal("no outcome recorded")
	}
	return m.outcomes[len(m.outcomes)-1]
}

func testConfig(t *testing.T, us *httptest.Server) (Config, *metricsCapture) {
	t.Helper()
	u, err := url.Parse(us.URL)
	if err != nil {
		t.Fatalf("parsing upstream URL: %v", err)
	}
	mc := &metricsCapture{}
	return Config{
		Secret:        func() string { return testKey },
		UpstreamURL:   u,
		Transport:     us.Client().Transport,
		Token:         func() (string, error) { return "sa-token", nil },
		AgentVersion:  "1.2.3",
		KubeReachable: func() bool { return true },
		Metrics:       mc,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, mc
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(b)
}

// TestPolicyDenials is the deny half of the conformance suite: every rule,
// with variants, asserting status code, exact {"error":"<code>"} body,
// Content-Type, recorded outcome — and, via Cleanup, that no denied
// request ever reached the upstream.
func TestPolicyDenials(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	us := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(us.Close)
	t.Cleanup(func() {
		if n := hits.Load(); n != 0 {
			t.Errorf("denied requests reached upstream %d times, want 0", n)
		}
	})

	tests := []struct {
		name       string
		method     string // "" => GET
		target     string
		omitKey    bool
		keySet     bool
		key        string
		header     map[string]string
		wantStatus int
		wantCode   string
	}{
		// Rule 1 — auth.
		{name: "missing key", target: "/api", omitKey: true, wantStatus: 401, wantCode: "unauthorized"},
		{name: "wrong key", target: "/api", keySet: true, key: "wrong", wantStatus: 401, wantCode: "unauthorized"},
		{name: "wrong key same length", target: "/api", keySet: true, key: strings.Repeat("x", len(testKey)), wantStatus: 401, wantCode: "unauthorized"},
		{name: "empty key header", target: "/api", keySet: true, key: "", wantStatus: 401, wantCode: "unauthorized"},
		{name: "auth precedes method", method: "POST", target: "/api", keySet: true, key: "wrong", wantStatus: 401, wantCode: "unauthorized"},
		{name: "auth precedes healthz", target: "/healthz", keySet: true, key: "wrong", wantStatus: 401, wantCode: "unauthorized"},

		// Rule 3 — method.
		{name: "POST", method: "POST", target: "/api/v1/pods", wantStatus: 405, wantCode: "method_not_allowed"},
		{name: "PUT", method: "PUT", target: "/api/v1/pods", wantStatus: 405, wantCode: "method_not_allowed"},
		{name: "PATCH", method: "PATCH", target: "/api/v1/pods", wantStatus: 405, wantCode: "method_not_allowed"},
		{name: "DELETE", method: "DELETE", target: "/api/v1/pods", wantStatus: 405, wantCode: "method_not_allowed"},
		{name: "HEAD", method: "HEAD", target: "/api", wantStatus: 405, wantCode: "method_not_allowed"},
		{name: "POST healthz", method: "POST", target: "/healthz", wantStatus: 405, wantCode: "method_not_allowed"},

		// Rule 4a — path hygiene.
		{name: "dotdot traversal", target: "/apis/apps/v1/../../secrets", wantStatus: 400, wantCode: "bad_path"},
		{name: "encoded dotdot", target: "/api/v1/%2e%2e/secrets", wantStatus: 400, wantCode: "bad_path"},
		{name: "encoded dotdot mixed case", target: "/api/v1/%2E%2e/secrets", wantStatus: 400, wantCode: "bad_path"},
		{name: "double-encoded dotdot", target: "/api/v1/%252e%252e/secrets", wantStatus: 400, wantCode: "bad_path"},
		{name: "empty segment", target: "/api//v1/pods", wantStatus: 400, wantCode: "bad_path"},
		{name: "trailing slash", target: "/api/v1/pods/", wantStatus: 400, wantCode: "bad_path"},
		{name: "dot segment", target: "/api/./v1/pods", wantStatus: 400, wantCode: "bad_path"},
		{name: "backslash", target: `/api/v1\..\secrets`, wantStatus: 400, wantCode: "bad_path"},
		{name: "encoded backslash", target: "/api/v1/%5csecrets", wantStatus: 400, wantCode: "bad_path"},
		{name: "dotdot tail", target: "/api/v1/..", wantStatus: 400, wantCode: "bad_path"},

		// Rule 4b — allowed roots.
		{name: "root", target: "/", wantStatus: 403, wantCode: "forbidden_path"},
		{name: "openapi", target: "/openapi/v2", wantStatus: 403, wantCode: "forbidden_path"},
		{name: "logs", target: "/logs", wantStatus: 403, wantCode: "forbidden_path"},
		{name: "metrics", target: "/metrics", wantStatus: 403, wantCode: "forbidden_path"},
		{name: "version subpath", target: "/version/extra", wantStatus: 403, wantCode: "forbidden_path"},
		{name: "version uppercase", target: "/VERSION", wantStatus: 403, wantCode: "forbidden_path"},
		{name: "api prefix confusion", target: "/apiextensions/v1", wantStatus: 403, wantCode: "forbidden_path"},
		{name: "apis prefix confusion", target: "/apisx/v1", wantStatus: 403, wantCode: "forbidden_path"},
		{name: "healthz subpath", target: "/healthz/nested", wantStatus: 403, wantCode: "forbidden_path"},

		// Rule 5 — subresource denylist.
		{name: "exec", target: "/api/v1/namespaces/default/pods/web-0/exec", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "EXEC uppercase", target: "/api/v1/namespaces/default/pods/web-0/EXEC", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "Exec mixed case", target: "/api/v1/namespaces/default/pods/web-0/Exec", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "encoded exec", target: "/api/v1/namespaces/default/pods/web-0/%65xec", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "attach", target: "/api/v1/namespaces/default/pods/web-0/attach", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "portforward", target: "/api/v1/namespaces/default/pods/web-0/portforward", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "node proxy", target: "/api/v1/nodes/n1/proxy", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "proxy mid-path", target: "/api/v1/nodes/n1/proxy/stats/summary", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "ephemeralcontainers", target: "/api/v1/namespaces/default/pods/web-0/ephemeralcontainers", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "finalize", target: "/api/v1/namespaces/default/finalize", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "binding", target: "/api/v1/namespaces/default/pods/web-0/binding", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "eviction", target: "/api/v1/namespaces/default/pods/web-0/eviction", wantStatus: 403, wantCode: "forbidden_subresource"},
		{name: "subresource precedes secrets", target: "/api/v1/namespaces/default/secrets/tls/exec", wantStatus: 403, wantCode: "forbidden_subresource"},

		// Rule 6 — secrets.
		{name: "secrets list namespaced", target: "/api/v1/namespaces/default/secrets", wantStatus: 403, wantCode: "forbidden_secrets"},
		{name: "secrets get", target: "/api/v1/namespaces/default/secrets/db-creds", wantStatus: 403, wantCode: "forbidden_secrets"},
		{name: "secrets list cluster", target: "/api/v1/secrets", wantStatus: 403, wantCode: "forbidden_secrets"},
		{name: "SECRETS uppercase", target: "/api/v1/namespaces/default/SECRETS", wantStatus: 403, wantCode: "forbidden_secrets"},
		{name: "encoded secrets", target: "/api/v1/namespaces/default/%73ecrets", wantStatus: 403, wantCode: "forbidden_secrets"},

		// Rule 7 — query.
		{name: "watch=true", target: "/api/v1/pods?watch=true", wantStatus: 400, wantCode: "bad_query"},
		{name: "watch empty value", target: "/api/v1/pods?watch=", wantStatus: 400, wantCode: "bad_query"},
		{name: "watch=TRUE", target: "/api/v1/pods?watch=TRUE", wantStatus: 400, wantCode: "bad_query"},
		{name: "watch among others", target: "/api/v1/pods?limit=1&watch=false", wantStatus: 400, wantCode: "bad_query"},
		{name: "follow=true", target: "/api/v1/namespaces/default/pods/web-0/log?follow=true", wantStatus: 400, wantCode: "bad_query"},
		{name: "follow empty value", target: "/api/v1/pods?follow=", wantStatus: 400, wantCode: "bad_query"},
		{name: "encoded watch key", target: "/api/v1/pods?%77atch=1", wantStatus: 400, wantCode: "bad_query"},
		{name: "unparseable query", target: "/api/v1/pods?a=%zz", wantStatus: 400, wantCode: "bad_query"},
		{name: "legacy watch path core", target: "/api/v1/watch/pods", wantStatus: 400, wantCode: "bad_query"},
		{name: "legacy watch path core namespaced", target: "/api/v1/watch/namespaces/default/pods", wantStatus: 400, wantCode: "bad_query"},
		{name: "legacy watch path group", target: "/apis/apps/v1/watch/namespaces/default/deployments", wantStatus: 400, wantCode: "bad_query"},
		{name: "legacy watch path uppercase", target: "/api/v1/WATCH/pods", wantStatus: 400, wantCode: "bad_query"},
		{name: "query precedes upgrade", target: "/api/v1/pods?watch=true", header: map[string]string{"Upgrade": "websocket"}, wantStatus: 400, wantCode: "bad_query"},

		// Rule 8 — upgrade.
		{name: "upgrade websocket", target: "/api/v1/pods", header: map[string]string{"Upgrade": "websocket"}, wantStatus: 400, wantCode: "upgrade_rejected"},
		{name: "upgrade spdy", target: "/api/v1/pods", header: map[string]string{"Upgrade": "SPDY/3.1"}, wantStatus: 400, wantCode: "upgrade_rejected"},
		{name: "connection upgrade", target: "/api/v1/pods", header: map[string]string{"Connection": "upgrade"}, wantStatus: 400, wantCode: "upgrade_rejected"},
		{name: "connection keep-alive upgrade", target: "/api/v1/pods", header: map[string]string{"Connection": "keep-alive, upgrade"}, wantStatus: 400, wantCode: "upgrade_rejected"},
		{name: "connection Upgrade case", target: "/api/v1/pods", header: map[string]string{"Connection": "keep-alive,Upgrade"}, wantStatus: 400, wantCode: "upgrade_rejected"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, mc := testConfig(t, us)
			h := NewHandler(cfg)

			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, tc.target, nil)
			if !tc.omitKey {
				key := testKey
				if tc.keySet {
					key = tc.key
				}
				req.Header.Set(headerShimKey, key)
			}
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			res := rr.Result()

			if res.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			if ct := res.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			wantBody := fmt.Sprintf(`{"error":%q}`, tc.wantCode)
			if body := readBody(t, res); body != wantBody {
				t.Errorf("body = %q, want %q", body, wantBody)
			}
			if got := mc.last(t); got != tc.wantCode {
				t.Errorf("outcome = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// TestAllowlistFixtures is the allow half of the conformance suite: one
// request per platform read action, each proven to reach the fake kube API
// with the exact path, the query passed through verbatim, the bearer token
// attached — and nothing else: no shim key, no cookies, no body.
func TestAllowlistFixtures(t *testing.T) {
	t.Parallel()

	type captured struct {
		path          string
		rawQuery      string
		header        http.Header
		contentLength int64
		bodyLen       int
	}
	var mu sync.Mutex
	var got captured

	us := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = captured{
			path:          r.URL.Path,
			rawQuery:      r.URL.RawQuery,
			header:        r.Header.Clone(),
			contentLength: r.ContentLength,
			bodyLen:       len(body),
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"kind":"Fixture"}`)
	}))
	t.Cleanup(us.Close)

	cfg, mc := testConfig(t, us)
	h := NewHandler(cfg)

	tests := []struct {
		name      string
		target    string
		wantPath  string
		wantQuery string
		header    map[string]string
	}{
		{name: "version", target: "/version", wantPath: "/version"},
		{name: "discovery api", target: "/api", wantPath: "/api"},
		{name: "discovery apis", target: "/apis", wantPath: "/apis"},
		// The legacy-watch-path denial is positional: an object merely
		// NAMED "watch" is still inspectable.
		{name: "pod named watch", target: "/api/v1/namespaces/default/pods/watch", wantPath: "/api/v1/namespaces/default/pods/watch"},
		{
			name:      "list pods paged",
			target:    "/api/v1/pods?limit=500&continue=tok",
			wantPath:  "/api/v1/pods",
			wantQuery: "limit=500&continue=tok",
		},
		{
			name:     "get pod",
			target:   "/api/v1/namespaces/default/pods/web-0",
			wantPath: "/api/v1/namespaces/default/pods/web-0",
		},
		{
			name:      "pod logs",
			target:    "/api/v1/namespaces/default/pods/web-0/log?container=app&tailLines=100&previous=true&sinceSeconds=3600",
			wantPath:  "/api/v1/namespaces/default/pods/web-0/log",
			wantQuery: "container=app&tailLines=100&previous=true&sinceSeconds=3600",
		},
		{
			name:      "events by involved object",
			target:    "/api/v1/events?fieldSelector=involvedObject.name%3Dweb-0",
			wantPath:  "/api/v1/events",
			wantQuery: "fieldSelector=involvedObject.name%3Dweb-0",
		},
		{name: "top pods", target: "/apis/metrics.k8s.io/v1beta1/pods", wantPath: "/apis/metrics.k8s.io/v1beta1/pods"},
		{name: "top nodes", target: "/apis/metrics.k8s.io/v1beta1/nodes", wantPath: "/apis/metrics.k8s.io/v1beta1/nodes"},
		{
			name:      "rollout status",
			target:    "/apis/apps/v1/namespaces/default/deployments/web?labelSelector=app%3Dweb",
			wantPath:  "/apis/apps/v1/namespaces/default/deployments/web",
			wantQuery: "labelSelector=app%3Dweb",
		},
		{
			// "watch" as a key substring must NOT trip rule 7 — the match
			// is key-exact.
			name:      "allowWatchBookmarks passes",
			target:    "/api/v1/pods?allowWatchBookmarks=true&resourceVersion=0",
			wantPath:  "/api/v1/pods",
			wantQuery: "allowWatchBookmarks=true&resourceVersion=0",
		},
		{
			// A Connection header without the upgrade token must NOT trip
			// rule 8.
			name:     "connection keep-alive passes",
			target:   "/api",
			wantPath: "/api",
			header:   map[string]string{"Connection": "keep-alive"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, strings.NewReader("never-forward-me"))
			req.Header.Set(headerShimKey, testKey)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Accept-Encoding", "identity")
			req.Header.Set("Cookie", "session=junk")
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			res := rr.Result()

			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", res.StatusCode, readBody(t, res))
			}
			if body := readBody(t, res); body != `{"kind":"Fixture"}` {
				t.Errorf("body = %q, want fixture body", body)
			}
			if ct := res.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json (passthrough)", ct)
			}
			if got := mc.last(t); got != "proxied" {
				t.Errorf("outcome = %q, want proxied", got)
			}

			mu.Lock()
			defer mu.Unlock()
			if got.path != tc.wantPath {
				t.Errorf("upstream path = %q, want %q", got.path, tc.wantPath)
			}
			if got.rawQuery != tc.wantQuery {
				t.Errorf("upstream raw query = %q, want %q (verbatim)", got.rawQuery, tc.wantQuery)
			}
			if auth := got.header.Get("Authorization"); auth != "Bearer sa-token" {
				t.Errorf("upstream Authorization = %q, want Bearer sa-token", auth)
			}
			if ua := got.header.Get("User-Agent"); ua != "polylane-k8s/1.2.3" {
				t.Errorf("upstream User-Agent = %q, want polylane-k8s/1.2.3", ua)
			}
			if acc := got.header.Get("Accept"); acc != "application/json" {
				t.Errorf("upstream Accept = %q, want application/json", acc)
			}
			if ae := got.header.Get("Accept-Encoding"); ae != "identity" {
				t.Errorf("upstream Accept-Encoding = %q, want identity", ae)
			}
			for _, forbidden := range []string{headerShimKey, "Cookie", "X-Forwarded-For", "Connection"} {
				if v := got.header.Get(forbidden); v != "" {
					t.Errorf("upstream received forbidden header %s=%q", forbidden, v)
				}
			}
			if got.bodyLen != 0 || got.contentLength != 0 {
				t.Errorf("upstream received a body (len=%d, contentLength=%d), want none", got.bodyLen, got.contentLength)
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	us := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(us.Close)

	do := func(t *testing.T, cfg Config) (int, string) {
		t.Helper()
		h := NewHandler(cfg)
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set(headerShimKey, testKey)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		res := rr.Result()
		return res.StatusCode, readBody(t, res)
	}

	t.Run("kube reachable", func(t *testing.T) {
		t.Parallel()
		cfg, mc := testConfig(t, us)
		status, body := do(t, cfg)
		if status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
		want := `{"status":"ok","kubeReachable":true,"agentVersion":"1.2.3"}`
		if body != want {
			t.Errorf("body = %q, want %q", body, want)
		}
		if got := mc.last(t); got != "healthz" {
			t.Errorf("outcome = %q, want healthz", got)
		}
	})

	t.Run("kube not reachable", func(t *testing.T) {
		t.Parallel()
		cfg, _ := testConfig(t, us)
		cfg.KubeReachable = func() bool { return false }
		want := `{"status":"ok","kubeReachable":false,"agentVersion":"1.2.3"}`
		if _, body := do(t, cfg); body != want {
			t.Errorf("body = %q, want %q", body, want)
		}
	})

	t.Run("nil KubeReachable reads false", func(t *testing.T) {
		t.Parallel()
		cfg, _ := testConfig(t, us)
		cfg.KubeReachable = nil
		want := `{"status":"ok","kubeReachable":false,"agentVersion":"1.2.3"}`
		if _, body := do(t, cfg); body != want {
			t.Errorf("body = %q, want %q", body, want)
		}
	})

	t.Cleanup(func() {
		if n := hits.Load(); n != 0 {
			t.Errorf("healthz reached upstream %d times, want 0 (never proxied)", n)
		}
	})
}

// TestSecretRotation proves the secret is re-read from Config.Secret on
// every request, so an atomic swap takes effect immediately.
func TestSecretRotation(t *testing.T) {
	t.Parallel()

	us := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(us.Close)

	keyA, keyB := "key-a", "key-b"
	var current atomic.Pointer[string]
	current.Store(&keyA)

	cfg, _ := testConfig(t, us)
	cfg.Secret = func() string { return *current.Load() }
	h := NewHandler(cfg)

	do := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set(headerShimKey, key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		res := rr.Result()
		defer res.Body.Close()
		return res.StatusCode
	}

	if got := do(keyA); got != http.StatusOK {
		t.Fatalf("pre-rotation key A: status = %d, want 200", got)
	}
	current.Store(&keyB)
	if got := do(keyA); got != http.StatusUnauthorized {
		t.Errorf("post-rotation key A: status = %d, want 401", got)
	}
	if got := do(keyB); got != http.StatusOK {
		t.Errorf("post-rotation key B: status = %d, want 200", got)
	}
}

// TestEmptySecretAlwaysDenies pins the pre-registration invariant: with no
// secret configured yet, nothing authenticates — not even an empty key.
func TestEmptySecretAlwaysDenies(t *testing.T) {
	t.Parallel()

	us := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(us.Close)

	cfg, _ := testConfig(t, us)
	cfg.Secret = func() string { return "" }
	h := NewHandler(cfg)

	for _, key := range []*string{nil, ptr(""), ptr("anything")} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		if key != nil {
			req.Header.Set(headerShimKey, *key)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		res := rr.Result()
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %v: status = %d, want 401", key, res.StatusCode)
		}
	}
}

func ptr(s string) *string { return &s }

// TestKubeErrorPassthrough: non-2xx kube responses pass through verbatim —
// the platform's error taxonomy needs the real kube status.
func TestKubeErrorPassthrough(t *testing.T) {
	t.Parallel()

	for _, code := range []int{403, 404, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()

			body := fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","code":%d}`, code)
			us := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				fmt.Fprint(w, body)
			}))
			t.Cleanup(us.Close)

			cfg, mc := testConfig(t, us)
			h := NewHandler(cfg)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/nope", nil)
			req.Header.Set(headerShimKey, testKey)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			res := rr.Result()

			if res.StatusCode != code {
				t.Errorf("status = %d, want %d", res.StatusCode, code)
			}
			if got := readBody(t, res); got != body {
				t.Errorf("body = %q, want %q (verbatim)", got, body)
			}
			if ct := res.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if got := mc.last(t); got != "proxied" {
				t.Errorf("outcome = %q, want proxied", got)
			}
		})
	}
}

// TestRedirectNotFollowed: a 3xx is a non-2xx like any other — passed
// through, never chased, and its Location never leaks (header allowlist).
func TestRedirectNotFollowed(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	us := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "/api/v1")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(us.Close)

	cfg, _ := testConfig(t, us)
	h := NewHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set(headerShimKey, testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	res := rr.Result()
	res.Body.Close()

	if res.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "" {
		t.Errorf("Location = %q leaked; header allowlist must drop it", loc)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream hits = %d, want 1 (redirect must not be followed)", n)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	t.Parallel()

	// Grab a port that is guaranteed refused: listen, note the address, close.
	us := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cfg, mc := testConfig(t, us)
	us.Close()

	h := NewHandler(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set(headerShimKey, testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	res := rr.Result()

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
	if body := readBody(t, res); body != `{"error":"kube_unreachable"}` {
		t.Errorf("body = %q, want kube_unreachable", body)
	}
	if got := mc.last(t); got != "kube_unreachable" {
		t.Errorf("outcome = %q, want kube_unreachable", got)
	}
}

func TestTokenErrorIsKubeUnreachable(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	us := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(us.Close)

	cfg, mc := testConfig(t, us)
	cfg.Token = func() (string, error) { return "", fmt.Errorf("token file vanished") }
	h := NewHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set(headerShimKey, testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	res := rr.Result()

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
	if body := readBody(t, res); body != `{"error":"kube_unreachable"}` {
		t.Errorf("body = %q, want kube_unreachable", body)
	}
	if got := mc.last(t); got != "kube_unreachable" {
		t.Errorf("outcome = %q, want kube_unreachable", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream hits = %d, want 0", n)
	}
}

// TestUpstreamTimeout: the injectable UpstreamTimeout bounds the whole
// exchange; a stalled apiserver yields 502 kube_unreachable promptly.
func TestUpstreamTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	us := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		us.Close()
	})

	cfg, mc := testConfig(t, us)
	cfg.UpstreamTimeout = 50 * time.Millisecond
	h := NewHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set(headerShimKey, testKey)
	rr := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	res := rr.Result()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
	if body := readBody(t, res); body != `{"error":"kube_unreachable"}` {
		t.Errorf("body = %q, want kube_unreachable", body)
	}
	if got := mc.last(t); got != "kube_unreachable" {
		t.Errorf("outcome = %q, want kube_unreachable", got)
	}
	if elapsed > 5*time.Second {
		t.Errorf("request took %v; the 50ms override was not honored", elapsed)
	}
}

// TestResponseCap: bodies stream through a MaxResponseBytes limiter. Over
// the cap the connection is severed (no clean end, no Content-Length);
// exactly at the cap the response passes intact.
func TestResponseCap(t *testing.T) {
	t.Parallel()

	const capBytes = 1024

	newUpstream := func(t *testing.T, payload []byte) *httptest.Server {
		t.Helper()
		us := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.Write(payload)
		}))
		t.Cleanup(us.Close)
		return us
	}

	t.Run("over cap truncates and severs", func(t *testing.T) {
		t.Parallel()

		us := newUpstream(t, bytes.Repeat([]byte("a"), 8*capBytes))
		cfg, mc := testConfig(t, us)
		cfg.MaxResponseBytes = capBytes

		srv := httptest.NewServer(NewHandler(cfg))
		t.Cleanup(srv.Close)

		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/pods", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(headerShimKey, testKey)
		req.Header.Set("Accept-Encoding", "identity")

		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("request failed before headers: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", res.StatusCode)
		}
		if cl := res.Header.Get("Content-Length"); cl != "" {
			t.Errorf("Content-Length = %q, want absent when truncating", cl)
		}
		data, readErr := io.ReadAll(res.Body)
		if readErr == nil {
			t.Error("reading truncated body succeeded; want a severed connection error")
		}
		if len(data) > capBytes {
			t.Errorf("received %d bytes, want at most the %d-byte cap", len(data), capBytes)
		}
		if got := mc.last(t); got != "proxied" {
			t.Errorf("outcome = %q, want proxied", got)
		}
	})

	t.Run("exactly cap passes intact", func(t *testing.T) {
		t.Parallel()

		payload := bytes.Repeat([]byte("b"), capBytes)
		us := newUpstream(t, payload)
		cfg, _ := testConfig(t, us)
		cfg.MaxResponseBytes = capBytes

		srv := httptest.NewServer(NewHandler(cfg))
		t.Cleanup(srv.Close)

		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/pods", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(headerShimKey, testKey)
		req.Header.Set("Accept-Encoding", "identity")

		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if cl := res.Header.Get("Content-Length"); cl != strconv.Itoa(capBytes) {
			t.Errorf("Content-Length = %q, want %d", cl, capBytes)
		}
		data, readErr := io.ReadAll(res.Body)
		if readErr != nil {
			t.Fatalf("reading body: %v", readErr)
		}
		if !bytes.Equal(data, payload) {
			t.Errorf("received %d bytes, want the intact %d-byte payload", len(data), capBytes)
		}
	})
}

// TestBodyNeverForwardedNorRead pins rule 9 in both directions: the inbound
// body is not forwarded upstream, and the shim never even reads it.
func TestBodyNeverForwardedNorRead(t *testing.T) {
	t.Parallel()

	type bodyCapture struct {
		contentLength int64
		bodyLen       int
	}
	var mu sync.Mutex
	var got bodyCapture

	us := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = bodyCapture{contentLength: r.ContentLength, bodyLen: len(body)}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(us.Close)

	cfg, _ := testConfig(t, us)
	h := NewHandler(cfg)

	trip := &tripwireReader{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", trip)
	req.Header.Set(headerShimKey, testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	res := rr.Result()
	res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if trip.read.Load() {
		t.Error("shim read the inbound body; it must be ignored, not read")
	}
	mu.Lock()
	defer mu.Unlock()
	if got.bodyLen != 0 || got.contentLength != 0 {
		t.Errorf("upstream received a body (len=%d, contentLength=%d), want none", got.bodyLen, got.contentLength)
	}
}

type tripwireReader struct {
	read atomic.Bool
}

func (r *tripwireReader) Read([]byte) (int, error) {
	r.read.Store(true)
	return 0, io.EOF
}

func TestNilMetricsIsSafe(t *testing.T) {
	t.Parallel()

	us := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(us.Close)

	cfg, _ := testConfig(t, us)
	cfg.Metrics = nil
	h := NewHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(headerShimKey, testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	res := rr.Result()
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}

func TestNewHandlerDefaults(t *testing.T) {
	t.Parallel()

	us := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(us.Close)

	cfg, _ := testConfig(t, us)
	cfg.MaxResponseBytes = 0
	cfg.UpstreamTimeout = 0
	cfg.Logger = nil
	cfg.Transport = nil

	hh, ok := NewHandler(cfg).(*handler)
	if !ok {
		t.Fatal("NewHandler did not return *handler")
	}
	if hh.cfg.MaxResponseBytes != 32<<20 {
		t.Errorf("MaxResponseBytes default = %d, want %d", hh.cfg.MaxResponseBytes, int64(32<<20))
	}
	if hh.cfg.UpstreamTimeout != 100*time.Second {
		t.Errorf("UpstreamTimeout default = %v, want 100s", hh.cfg.UpstreamTimeout)
	}
	if hh.cfg.Logger == nil {
		t.Error("Logger default not applied")
	}
	if hh.cfg.Transport == nil {
		t.Error("Transport default not applied")
	}
}

func TestNewHandlerPanicsOnMissingRequirements(t *testing.T) {
	t.Parallel()

	us := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(us.Close)

	base := func(t *testing.T) Config {
		cfg, _ := testConfig(t, us)
		return cfg
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"nil Secret", func(c *Config) { c.Secret = nil }},
		{"nil UpstreamURL", func(c *Config) { c.UpstreamURL = nil }},
		{"nil Token", func(c *Config) { c.Token = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := base(t)
			tc.mutate(&cfg)
			defer func() {
				if recover() == nil {
					t.Error("expected panic")
				}
			}()
			NewHandler(cfg)
		})
	}
}

func TestListenLoopback(t *testing.T) {
	t.Parallel()

	good := []string{"127.0.0.1:0", "localhost:0"}
	// Only exercise IPv6 loopback where the environment supports it.
	if probe, err := net.Listen("tcp", "[::1]:0"); err == nil {
		_ = probe.Close()
		good = append(good, "[::1]:0")
	}
	for _, addr := range good {
		t.Run("allows "+addr, func(t *testing.T) {
			t.Parallel()
			l, err := ListenLoopback(addr)
			if err != nil {
				t.Fatalf("ListenLoopback(%q) = %v, want nil", addr, err)
			}
			ta, ok := l.Addr().(*net.TCPAddr)
			if !ok {
				t.Fatalf("Addr() = %T, want *net.TCPAddr", l.Addr())
			}
			if !ta.IP.IsLoopback() {
				t.Errorf("bound %s, want loopback", ta.IP)
			}
			_ = l.Close()
		})
	}

	bad := []string{
		"",
		"127.0.0.1", // no port
		":0",        // empty host binds all interfaces
		"0.0.0.0:0",
		"[::]:0",
		"192.0.2.1:0",
		"8.8.8.8:0",
		"example.com:0", // non-localhost hostnames get no DNS say
	}
	for _, addr := range bad {
		t.Run(fmt.Sprintf("refuses %q", addr), func(t *testing.T) {
			t.Parallel()
			l, err := ListenLoopback(addr)
			if err == nil {
				_ = l.Close()
				t.Fatalf("ListenLoopback(%q) succeeded, want error", addr)
			}
		})
	}
}
