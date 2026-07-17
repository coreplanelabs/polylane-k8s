package health

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func decode(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding body %q: %v", rr.Body.String(), err)
	}
}

func boolSource(v bool) func() bool {
	return func() bool { return v }
}

func TestHealthzAlways200(t *testing.T) {
	t.Parallel()
	// Everything down, startup latch never flipped: liveness still 200.
	s := NewServer(Sources{
		CredentialsPresent: boolSource(false),
		TunnelReady:        boolSource(false),
		KubeReachable:      boolSource(false),
	}, discardLogger())

	rr := get(t, s, "/healthz")
	if rr.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 regardless of dependency state", rr.Code)
	}
	var body map[string]string
	decode(t, rr, &body)
	if body["status"] != "ok" {
		t.Errorf(`status = %q, want "ok"`, body["status"])
	}
}

func TestHealthzWithNilSources(t *testing.T) {
	t.Parallel()
	s := NewServer(Sources{}, nil)
	if rr := get(t, s, "/healthz"); rr.Code != http.StatusOK {
		t.Fatalf("/healthz = %d with nil sources, want 200", rr.Code)
	}
}

func TestStartupzLatch(t *testing.T) {
	t.Parallel()
	s := NewServer(Sources{}, discardLogger())

	if rr := get(t, s, "/startupz"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/startupz before latch = %d, want 503", rr.Code)
	}
	s.SetStartupReady()
	if rr := get(t, s, "/startupz"); rr.Code != http.StatusOK {
		t.Fatalf("/startupz after latch = %d, want 200", rr.Code)
	}
	// One-way: a second flip is a no-op, never an un-latch.
	s.SetStartupReady()
	if rr := get(t, s, "/startupz"); rr.Code != http.StatusOK {
		t.Fatalf("/startupz after second flip = %d, want 200", rr.Code)
	}
}

func TestReadyzTruthTable(t *testing.T) {
	t.Parallel()
	for _, creds := range []bool{false, true} {
		for _, tun := range []bool{false, true} {
			for _, kube := range []bool{false, true} {
				name := fmt.Sprintf("creds=%t_tunnel=%t_kube=%t", creds, tun, kube)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					s := NewServer(Sources{
						CredentialsPresent: boolSource(creds),
						TunnelReady:        boolSource(tun),
						KubeReachable:      boolSource(kube),
						Version:            "1.2.3",
					}, discardLogger())

					rr := get(t, s, "/readyz")
					wantReady := creds && tun && kube
					wantCode := http.StatusServiceUnavailable
					if wantReady {
						wantCode = http.StatusOK
					}
					if rr.Code != wantCode {
						t.Errorf("/readyz = %d, want %d", rr.Code, wantCode)
					}
					var body struct {
						Ready       bool   `json:"ready"`
						Credentials bool   `json:"credentials"`
						Tunnel      bool   `json:"tunnel"`
						Kube        bool   `json:"kube"`
						Version     string `json:"version"`
					}
					decode(t, rr, &body)
					if body.Ready != wantReady || body.Credentials != creds || body.Tunnel != tun || body.Kube != kube {
						t.Errorf("body = %+v, want ready=%t credentials=%t tunnel=%t kube=%t",
							body, wantReady, creds, tun, kube)
					}
					if body.Version != "1.2.3" {
						t.Errorf("version = %q, want 1.2.3", body.Version)
					}
				})
			}
		}
	}
}

func TestReadyzNilSourcesFailClosed(t *testing.T) {
	t.Parallel()
	s := NewServer(Sources{Version: "0.0.1"}, discardLogger())
	rr := get(t, s, "/readyz")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with nil sources = %d, want 503", rr.Code)
	}
	var body struct {
		Ready bool `json:"ready"`
	}
	decode(t, rr, &body)
	if body.Ready {
		t.Error("ready = true with nil sources, want fail-closed false")
	}
}

func TestVersionEndpoint(t *testing.T) {
	t.Parallel()
	s := NewServer(Sources{Version: "9.8.7"}, discardLogger())
	rr := get(t, s, "/version")
	if rr.Code != http.StatusOK {
		t.Fatalf("/version = %d, want 200", rr.Code)
	}
	var body map[string]string
	decode(t, rr, &body)
	if body["version"] != "9.8.7" {
		t.Errorf("version = %q, want 9.8.7", body["version"])
	}
	if body["goVersion"] != runtime.Version() {
		t.Errorf("goVersion = %q, want %q", body["goVersion"], runtime.Version())
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; body["platform"] != want {
		t.Errorf("platform = %q, want %q", body["platform"], want)
	}
}

func TestMethodAndPathRouting(t *testing.T) {
	t.Parallel()
	s := NewServer(Sources{}, discardLogger())
	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodPut, "/readyz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/nope", http.StatusNotFound},
	}
	for _, tt := range tests {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(tt.method, tt.path, nil))
		if rr.Code != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rr.Code, tt.want)
		}
	}
}
