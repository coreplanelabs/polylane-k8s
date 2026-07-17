package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testAPIKey = "sk_test_123"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(newPlatformServer(testAPIKey, testLogger()).handler())
	t.Cleanup(ts.Close)
	return ts
}

func validBody(uid string) map[string]any {
	return map[string]any{
		"clusterUid":        uid,
		"clusterName":       "test-cluster",
		"kubernetesVersion": "v1.31.0",
		"agentVersion":      "0.1.0",
		"distribution":      "kind",
		"namespace":         "polylane",
		"chartVersion":      "0.1.0",
	}
}

// do issues a request and returns status, headers, and the full body.
func do(t *testing.T, method, url string, headers map[string]string, body any) (int, http.Header, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshaling body: %v", err)
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, resp.Header, data
}

// rawRegister is the goroutine-safe variant of register: it returns errors
// instead of calling t.Fatal.
func rawRegister(baseURL string, body map[string]any) (registerResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return registerResponse{}, err
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+registerPath, bytes.NewReader(data))
	if err != nil {
		return registerResponse{}, err
	}
	req.Header.Set("x-api-key", testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return registerResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return registerResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return registerResponse{}, fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	var out registerResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return registerResponse{}, err
	}
	return out, nil
}

func register(t *testing.T, ts *httptest.Server, body map[string]any) registerResponse {
	t.Helper()
	resp, err := rawRegister(ts.URL, body)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return resp
}

func getRegistrations(t *testing.T, ts *httptest.Server) map[string]registration {
	t.Helper()
	status, _, data := do(t, http.MethodGet, ts.URL+"/admin/registrations", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /admin/registrations: got status %d, body %s", status, data)
	}
	var dump struct {
		Registrations map[string]registration `json:"registrations"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		t.Fatalf("decoding registrations dump: %v", err)
	}
	return dump.Registrations
}

func TestRegisterIdempotent(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)

	first := register(t, ts, validBody("uid-idem"))
	second := register(t, ts, validBody("uid-idem"))

	if first != second {
		t.Errorf("re-register changed the response:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	if first.Object != "kubernetes_agent_registration" {
		t.Errorf("_object: got %q, want %q", first.Object, "kubernetes_agent_registration")
	}
	for _, f := range []struct{ name, got, wantPrefix string }{
		{"accountId", first.AccountID, "acct_"},
		{"agentId", first.AgentID, "agent_"},
		{"tunnelId", first.TunnelID, "tunnel_"},
	} {
		if !strings.HasPrefix(f.got, f.wantPrefix) {
			t.Errorf("%s: got %q, want prefix %q", f.name, f.got, f.wantPrefix)
		}
	}
	if want := "k8s-" + first.AccountID + ".fake.test"; first.TunnelHostname != want {
		t.Errorf("tunnelHostname: got %q, want %q", first.TunnelHostname, want)
	}

	blob, err := base64.StdEncoding.DecodeString(first.TunnelToken)
	if err != nil {
		t.Fatalf("tunnelToken is not base64: %v", err)
	}
	var tok map[string]string
	if err := json.Unmarshal(blob, &tok); err != nil {
		t.Errorf("tunnelToken is not a base64 JSON blob: %v", err)
	}

	secret, err := base64.RawURLEncoding.DecodeString(first.ShimSecret)
	if err != nil {
		t.Fatalf("shimSecret is not base64url: %v", err)
	}
	if len(secret) != 32 {
		t.Errorf("shimSecret: got %d bytes, want 32", len(secret))
	}
}

func TestRegisterMintsDistinctCredentialsPerCluster(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)

	a := register(t, ts, validBody("uid-a"))
	b := register(t, ts, validBody("uid-b"))

	if a.AccountID == b.AccountID {
		t.Errorf("distinct clusters share accountId %q", a.AccountID)
	}
	if a.TunnelToken == b.TunnelToken {
		t.Errorf("distinct clusters share tunnelToken")
	}
	if a.ShimSecret == b.ShimSecret {
		t.Errorf("distinct clusters share shimSecret")
	}
}

func TestRegisterAuth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		apiKey string
		want   int
	}{
		{"missing key", "", http.StatusUnauthorized},
		{"wrong key", "sk_wrong", http.StatusUnauthorized},
		{"correct key", testAPIKey, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			headers := map[string]string{}
			if tt.apiKey != "" {
				headers["x-api-key"] = tt.apiKey
			}
			status, _, data := do(t, http.MethodPost, ts.URL+registerPath, headers, validBody("uid-auth"))
			if status != tt.want {
				t.Errorf("got status %d, want %d (body %s)", status, tt.want, data)
			}
		})
	}
}

func TestRegisterValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mutate     func(m map[string]any)
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "missing clusterUid",
			mutate:     func(m map[string]any) { delete(m, "clusterUid") },
			wantStatus: http.StatusBadRequest,
			wantSubstr: "clusterUid",
		},
		{
			name:       "missing kubernetesVersion",
			mutate:     func(m map[string]any) { delete(m, "kubernetesVersion") },
			wantStatus: http.StatusBadRequest,
			wantSubstr: "kubernetesVersion",
		},
		{
			name:       "missing agentVersion",
			mutate:     func(m map[string]any) { delete(m, "agentVersion") },
			wantStatus: http.StatusBadRequest,
			wantSubstr: "agentVersion",
		},
		{
			name:       "missing namespace",
			mutate:     func(m map[string]any) { delete(m, "namespace") },
			wantStatus: http.StatusBadRequest,
			wantSubstr: "namespace",
		},
		{
			name: "optional fields absent",
			mutate: func(m map[string]any) {
				delete(m, "clusterName")
				delete(m, "distribution")
				delete(m, "chartVersion")
			},
			wantStatus: http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			body := validBody("uid-validate")
			tt.mutate(body)
			status, _, data := do(t, http.MethodPost, ts.URL+registerPath,
				map[string]string{"x-api-key": testAPIKey}, body)
			if status != tt.wantStatus {
				t.Errorf("got status %d, want %d (body %s)", status, tt.wantStatus, data)
			}
			if tt.wantSubstr != "" && !strings.Contains(string(data), tt.wantSubstr) {
				t.Errorf("body %s does not mention %q", data, tt.wantSubstr)
			}
		})
	}

	t.Run("malformed json", func(t *testing.T) {
		t.Parallel()
		ts := newTestServer(t)
		req, err := http.NewRequest(http.MethodPost, ts.URL+registerPath, strings.NewReader("{not json"))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("x-api-key", testAPIKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("got status %d, want 400", resp.StatusCode)
		}
	})
}

func TestRegisterUpdatesMutableMetadata(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)

	first := register(t, ts, validBody("uid-meta"))

	body := validBody("uid-meta")
	body["clusterName"] = "renamed"
	body["agentVersion"] = "0.2.0"
	second := register(t, ts, body)

	if first != second {
		t.Errorf("metadata update changed credentials:\nfirst:  %+v\nsecond: %+v", first, second)
	}

	reg, ok := getRegistrations(t, ts)["uid-meta"]
	if !ok {
		t.Fatal("uid-meta missing from registrations dump")
	}
	if reg.ClusterName != "renamed" {
		t.Errorf("clusterName: got %q, want %q", reg.ClusterName, "renamed")
	}
	if reg.AgentVersion != "0.2.0" {
		t.Errorf("agentVersion: got %q, want %q", reg.AgentVersion, "0.2.0")
	}
	if reg.AccountID != first.AccountID {
		t.Errorf("stored accountId %q does not match minted %q", reg.AccountID, first.AccountID)
	}
}

func TestRotateRotatesBothCredentials(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)

	before := register(t, ts, validBody("uid-rotate"))

	status, _, data := do(t, http.MethodPost, ts.URL+"/admin/rotate?clusterUid=uid-rotate", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("rotate: got status %d, body %s", status, data)
	}
	var after registerResponse
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("decoding rotate response: %v", err)
	}

	if after.TunnelToken == before.TunnelToken {
		t.Errorf("rotate did not change tunnelToken")
	}
	if after.ShimSecret == before.ShimSecret {
		t.Errorf("rotate did not change shimSecret")
	}
	if after.AccountID != before.AccountID || after.AgentID != before.AgentID ||
		after.TunnelID != before.TunnelID || after.TunnelHostname != before.TunnelHostname {
		t.Errorf("rotate changed stable identifiers:\nbefore: %+v\nafter:  %+v", before, after)
	}

	// Register after rotate returns the rotated credentials — the registry
	// holds one truth and register never mints twice.
	again := register(t, ts, validBody("uid-rotate"))
	if again != after {
		t.Errorf("register after rotate:\ngot:  %+v\nwant: %+v", again, after)
	}
}

func TestRotateErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"unknown clusterUid", "?clusterUid=uid-nope", http.StatusNotFound},
		{"missing clusterUid", "", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			status, _, data := do(t, http.MethodPost, ts.URL+"/admin/rotate"+tt.query, nil, nil)
			if status != tt.want {
				t.Errorf("got status %d, want %d (body %s)", status, tt.want, data)
			}
		})
	}
}

func TestFailModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		mode           int
		wantRetryAfter string
	}{
		{"500", http.StatusInternalServerError, ""},
		{"429 sets Retry-After", http.StatusTooManyRequests, "1"},
		{"401", http.StatusUnauthorized, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)

			before := register(t, ts, validBody("uid-fail"))

			status, _, data := do(t, http.MethodPost,
				fmt.Sprintf("%s/admin/fail?mode=%d&count=2", ts.URL, tt.mode), nil, nil)
			if status != http.StatusOK {
				t.Fatalf("setting fail mode: got status %d, body %s", status, data)
			}

			for i := range 2 {
				st, hdr, body := do(t, http.MethodPost, ts.URL+registerPath,
					map[string]string{"x-api-key": testAPIKey}, validBody("uid-fail"))
				if st != tt.mode {
					t.Fatalf("register attempt %d: got status %d, want %d (body %s)", i+1, st, tt.mode, body)
				}
				if got := hdr.Get("Retry-After"); got != tt.wantRetryAfter {
					t.Errorf("register attempt %d: Retry-After: got %q, want %q", i+1, got, tt.wantRetryAfter)
				}
			}

			// Failures exhausted: registration recovers with the SAME credentials.
			after := register(t, ts, validBody("uid-fail"))
			if after != before {
				t.Errorf("credentials changed across injected failures:\nbefore: %+v\nafter:  %+v", before, after)
			}
		})
	}

	t.Run("invalid mode rejected", func(t *testing.T) {
		t.Parallel()
		ts := newTestServer(t)
		status, _, data := do(t, http.MethodPost, ts.URL+"/admin/fail?mode=418&count=1", nil, nil)
		if status != http.StatusBadRequest {
			t.Errorf("got status %d, want 400 (body %s)", status, data)
		}
	})

	t.Run("count zero clears", func(t *testing.T) {
		t.Parallel()
		ts := newTestServer(t)
		status, _, _ := do(t, http.MethodPost, ts.URL+"/admin/fail?mode=500&count=5", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("setting fail mode: got status %d", status)
		}
		status, _, _ = do(t, http.MethodPost, ts.URL+"/admin/fail?mode=500&count=0", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("clearing fail mode: got status %d", status)
		}
		register(t, ts, validBody("uid-clear")) // fatals on non-200
	})
}

func TestRegistrationCallCounts(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)

	register(t, ts, validBody("uid-once"))
	for range 3 {
		register(t, ts, validBody("uid-thrice"))
	}

	// A failed attempt still counts — the e2e invariant is "warm boots make
	// ZERO register calls", so every attempt must be visible.
	if status, _, _ := do(t, http.MethodPost, ts.URL+"/admin/fail?mode=500&count=1", nil, nil); status != http.StatusOK {
		t.Fatalf("setting fail mode: got status %d", status)
	}
	if status, _, _ := do(t, http.MethodPost, ts.URL+registerPath,
		map[string]string{"x-api-key": testAPIKey}, validBody("uid-thrice")); status != http.StatusInternalServerError {
		t.Fatalf("injected failure: got status %d, want 500", status)
	}

	regs := getRegistrations(t, ts)
	if got := regs["uid-once"].RegisterCalls; got != 1 {
		t.Errorf("uid-once registerCalls: got %d, want 1", got)
	}
	if got := regs["uid-thrice"].RegisterCalls; got != 4 {
		t.Errorf("uid-thrice registerCalls: got %d, want 4", got)
	}
	if len(regs) != 2 {
		t.Errorf("got %d registrations, want 2: %+v", len(regs), regs)
	}
	if _, ok := regs["uid-never"]; ok {
		t.Errorf("uid-never present in dump despite zero calls")
	}
}

func TestRegisterConcurrentSameCluster(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)

	const n = 8
	results := make(chan registerResponse, n)
	errs := make(chan error, n)
	for range n {
		go func() {
			resp, err := rawRegister(ts.URL, validBody("uid-conc"))
			if err != nil {
				errs <- err
				return
			}
			results <- resp
		}()
	}

	var first registerResponse
	for i := range n {
		select {
		case err := <-errs:
			t.Fatalf("concurrent register: %v", err)
		case resp := <-results:
			if i == 0 {
				first = resp
			} else if resp != first {
				t.Errorf("concurrent register returned different credentials:\nfirst: %+v\ngot:   %+v", first, resp)
			}
		}
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	status, _, data := do(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if status != http.StatusOK {
		t.Errorf("got status %d, want 200 (body %s)", status, data)
	}
}

func TestTunnelStub(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(tunnelStubHandler(testLogger()))
	t.Cleanup(ts.Close)

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"ready", http.MethodGet, "/ready", http.StatusOK},
		{"post ready", http.MethodPost, "/ready", http.StatusNotFound},
		{"metrics", http.MethodGet, "/metrics", http.StatusNotFound},
		{"root", http.MethodGet, "/", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, _, data := do(t, tt.method, ts.URL+tt.path, nil, nil)
			if status != tt.want {
				t.Errorf("%s %s: got status %d, want %d", tt.method, tt.path, status, tt.want)
			}
			if tt.want == http.StatusOK {
				var body map[string]string
				if err := json.Unmarshal(data, &body); err != nil {
					t.Fatalf("decoding /ready body: %v", err)
				}
				if body["status"] != "ok" {
					t.Errorf("/ready body: got %s, want status ok", data)
				}
			}
		})
	}
}
