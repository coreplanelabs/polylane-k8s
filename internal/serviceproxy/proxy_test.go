package serviceproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coreplanelabs/polylane-k8s/internal/config"
	"github.com/coreplanelabs/polylane-k8s/internal/kube"
	"github.com/coreplanelabs/polylane-k8s/internal/shim"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func grafanaBinding() config.Service {
	return config.Service{ID: "grafana", Namespace: "monitoring", Name: "grafana", Port: 80, Scheme: "http",
		Routes: []config.ServiceRoute{{Method: "GET", PathPrefix: "/api/"}, {Method: "POST", Path: "/api/ds/query"}}}
}

func grafanaService() kube.Service {
	var service kube.Service
	service.Spec.Type = "ClusterIP"
	service.Spec.ClusterIP = "10.96.0.20"
	service.Spec.Selector = map[string]string{"app": "grafana"}
	service.Spec.Ports = []kube.ServicePort{{Port: 80, Protocol: "TCP"}}
	return service
}

func newProxy(t *testing.T, binding config.Service, resolve func(context.Context, string, string) (kube.Service, error), transport http.RoundTripper) *Handler {
	t.Helper()
	h, err := NewHandler(Config{Services: []config.Service{binding}, Resolve: resolve, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if transport != nil {
		h.targets[binding.ID].client.Transport = transport
	}
	t.Cleanup(h.CloseIdleConnections)
	return h
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func request(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("x-polylane-shim-key", "shim-secret")
	return r
}

func TestRelay(t *testing.T) {
	t.Parallel()
	binding := grafanaBinding()
	binding.BasePath = "/grafana"
	var resolutions, requests int
	h := newProxy(t, binding, func(ctx context.Context, namespace, name string) (kube.Service, error) {
		resolutions++
		if namespace != "monitoring" || name != "grafana" {
			t.Errorf("lookup = %s/%s", namespace, name)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("lookup has no deadline")
		}
		s := grafanaService()
		if resolutions > 1 {
			s.Spec.ClusterIP = "10.96.0.21"
		}
		return s, nil
	}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		wantHost := "10.96.0.20:80"
		if requests > 1 {
			wantHost = "10.96.0.21:80"
		}
		if r.URL.Host != wantHost || r.Host != "grafana.monitoring.svc:80" || r.URL.Path != "/grafana/api/ds/query" || r.URL.RawQuery != "orgId=1&expr=a%2Bb" {
			t.Errorf("upstream URL = %s, Host = %s", r.URL, r.Host)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"queries":[]}` || r.Method != "POST" {
			t.Errorf("upstream request = %s %q, %v", r.Method, body, err)
		}
		if r.Header.Get("Authorization") != "Bearer grafana-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("provider headers = %v", r.Header)
		}
		for _, name := range []string{"X-Polylane-Shim-Key", "CF-Access-Client-Id", "CF-Access-Client-Secret", "Cookie", "X-Webauth-User", "X-Forwarded-Host", "Connection"} {
			if r.Header.Get(name) != "" {
				t.Errorf("credential or untrusted header forwarded: %s", name)
			}
		}
		res := response(429, `{"message":"rate limited"}`)
		res.Header.Set("Content-Type", "application/json")
		res.Header.Set("Retry-After", "5")
		res.Header.Set("Set-Cookie", "session=secret")
		return res, nil
	}))
	for range 2 {
		r := request("POST", "/services/grafana/api/ds/query?orgId=1&expr=a%2Bb", `{"queries":[]}`)
		r.Header.Set("Authorization", "Bearer grafana-token")
		r.Header.Set("Content-Type", "application/json")
		for _, name := range []string{"CF-Access-Client-Id", "CF-Access-Client-Secret", "Cookie", "X-Webauth-User", "X-Forwarded-Host"} {
			r.Header.Set(name, "must-not-forward")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 429 || w.Body.String() != `{"message":"rate limited"}` || w.Header().Get("Retry-After") != "5" || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response = %d %s %v", w.Code, w.Body, w.Header())
		}
	}
	if resolutions != 2 || requests != 2 {
		t.Fatalf("resolutions = %d, requests = %d", resolutions, requests)
	}
}

func TestPolicyDenials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, method, path, body string
		headers                  map[string]string
		status                   int
	}{
		{name: "unknown service", method: "GET", path: "/services/other/api/search", status: 404},
		{name: "missing path", method: "GET", path: "/services/grafana", status: 404},
		{name: "write route", method: "POST", path: "/services/grafana/api/dashboards/db", status: 403},
		{name: "query child", method: "POST", path: "/services/grafana/api/ds/query/child", status: 403},
		{name: "delete", method: "DELETE", path: "/services/grafana/api/ds/query", status: 403},
		{name: "prefix sibling", method: "GET", path: "/services/grafana/apix/search", status: 403},
		{name: "traversal", method: "GET", path: "/services/grafana/api/../admin", status: 400},
		{name: "encoded traversal", method: "GET", path: "/services/grafana/api/%2e%2e/admin", status: 400},
		{name: "double encoding", method: "GET", path: "/services/grafana/api/%252e%252e/admin", status: 400},
		{name: "encoded slash", method: "POST", path: "/services/grafana/api%2fds/query", status: 400},
		{name: "backslash", method: "GET", path: "/services/grafana/api/%5cadmin", status: 400},
		{name: "empty segment", method: "GET", path: "/services/grafana/api//search", status: 400},
		{name: "bad query", method: "GET", path: "/services/grafana/api/search?x=%zz", status: 400},
		{name: "upgrade", method: "GET", path: "/services/grafana/api/live/ws", headers: map[string]string{"Upgrade": "websocket"}, status: 400},
		{name: "connection upgrade", method: "GET", path: "/services/grafana/api/search", headers: map[string]string{"Connection": "keep-alive, Upgrade"}, status: 400},
		{name: "get body", method: "GET", path: "/services/grafana/api/search", body: "unsafe", status: 400},
		{name: "body cap", method: "POST", path: "/services/grafana/api/ds/query", body: strings.Repeat("x", maxRequestBytes+1), status: 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newProxy(t, grafanaBinding(), func(context.Context, string, string) (kube.Service, error) {
				t.Error("denied request reached the resolver")
				return grafanaService(), nil
			}, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("denied request reached the service")
				return response(200, ""), nil
			}))
			r := request(tt.method, tt.path, tt.body)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("response = %d %s, want %d", w.Code, w.Body, tt.status)
			}
		})
	}
}

func TestDestinationPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*kube.Service)
	}{
		{"external name", func(s *kube.Service) { s.Spec.Type = "ExternalName" }},
		{"load balancer", func(s *kube.Service) { s.Spec.Type = "LoadBalancer" }},
		{"no selector", func(s *kube.Service) { s.Spec.Selector = nil }},
		{"headless", func(s *kube.Service) { s.Spec.ClusterIP = "None" }},
		{"loopback", func(s *kube.Service) { s.Spec.ClusterIP = "127.0.0.1" }},
		{"ipv6 loopback", func(s *kube.Service) { s.Spec.ClusterIP = "::1" }},
		{"mapped loopback", func(s *kube.Service) { s.Spec.ClusterIP = "::ffff:127.0.0.1" }},
		{"metadata", func(s *kube.Service) { s.Spec.ClusterIP = "169.254.169.254" }},
		{"unspecified", func(s *kube.Service) { s.Spec.ClusterIP = "0.0.0.0" }},
		{"multicast", func(s *kube.Service) { s.Spec.ClusterIP = "224.0.0.1" }},
		{"hostname", func(s *kube.Service) { s.Spec.ClusterIP = "metadata.google.internal" }},
		{"wrong port", func(s *kube.Service) { s.Spec.Ports[0].Port = 3000 }},
		{"udp", func(s *kube.Service) { s.Spec.Ports[0].Protocol = "UDP" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := grafanaService()
			tt.change(&s)
			h := newProxy(t, grafanaBinding(), func(context.Context, string, string) (kube.Service, error) { return s, nil }, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("forbidden destination received a request")
				return response(200, ""), nil
			}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, request("GET", "/services/grafana/api/search", ""))
			if w.Code != 403 {
				t.Fatalf("response = %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestRedirectAndUpstreamFailure(t *testing.T) {
	t.Parallel()
	for _, status := range []int{301, 302, 307, 308, 101} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var calls int
			h := newProxy(t, grafanaBinding(), func(context.Context, string, string) (kube.Service, error) { return grafanaService(), nil }, roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				r := response(status, "")
				r.Header.Set("Location", "http://169.254.169.254/latest/meta-data")
				return r, nil
			}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, request("GET", "/services/grafana/api/search", ""))
			if w.Code != 502 || calls != 1 || w.Header().Get("Location") != "" {
				t.Fatalf("response = %d %s, upstream calls = %d", w.Code, w.Body, calls)
			}
		})
	}
	t.Run("lookup unavailable", func(t *testing.T) {
		t.Parallel()
		h := newProxy(t, grafanaBinding(), func(context.Context, string, string) (kube.Service, error) {
			return kube.Service{}, errors.New("unavailable")
		}, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request("GET", "/services/grafana/api/search", ""))
		if w.Code != 502 || !strings.Contains(w.Body.String(), "service_unavailable") {
			t.Fatalf("response = %d %s", w.Code, w.Body)
		}
	})
}

func TestShimAuthenticationAndIsolation(t *testing.T) {
	t.Parallel()
	var key atomic.Value
	key.Store("shim-secret")
	var serviceCalls, kubeCalls atomic.Int64
	services := newProxy(t, grafanaBinding(), func(context.Context, string, string) (kube.Service, error) { return grafanaService(), nil }, roundTripFunc(func(*http.Request) (*http.Response, error) {
		serviceCalls.Add(1)
		return response(200, `{"results":{}}`), nil
	}))
	h := shim.NewHandler(shim.Config{
		ServiceProxy: services,
		Secret:       func() string { value, _ := key.Load().(string); return value },
		UpstreamURL:  &url.URL{Scheme: "https", Host: "kubernetes.default.svc"},
		Token:        func() (string, error) { return "kube-only-token", nil },
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			kubeCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer kube-only-token" {
				t.Error("Kubernetes token missing on kube route")
			}
			return response(200, `{}`), nil
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tests := []struct {
		name, method, path, key string
		status                  int
	}{
		{"catalog needs auth", "GET", "/services", "", 401},
		{"query needs auth", "POST", "/services/grafana/api/ds/query", "", 401},
		{"catalog", "GET", "/services", "shim-secret", 200},
		{"service query", "POST", "/services/grafana/api/ds/query", "shim-secret", 200},
		{"kube get", "GET", "/api/v1/namespaces", "shim-secret", 200},
		{"kube post", "POST", "/api/v1/namespaces", "shim-secret", 405},
		{"kube proxy", "GET", "/api/v1/namespaces/monitoring/services/grafana/proxy", "shim-secret", 403},
		{"kube secrets", "GET", "/api/v1/secrets", "shim-secret", 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := request(tt.method, tt.path, "")
			r.Header.Set("x-polylane-shim-key", tt.key)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("response = %d %s, want %d", w.Code, w.Body, tt.status)
			}
			if tt.name == "catalog" && !strings.Contains(w.Body.String(), `"namespace":"monitoring"`) {
				t.Fatalf("catalog = %s", w.Body)
			}
		})
	}
	key.Store("rotated-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request("POST", "/services/grafana/api/ds/query", ""))
	if w.Code != 401 {
		t.Fatal("old shim key still works after rotation")
	}
	r := request("POST", "/services/grafana/api/ds/query", "")
	r.Header.Set("x-polylane-shim-key", "rotated-secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || serviceCalls.Load() != 2 || kubeCalls.Load() != 1 {
		t.Fatalf("rotation response = %d, service calls = %d, kube calls = %d", w.Code, serviceCalls.Load(), kubeCalls.Load())
	}
}
