package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// patchRecord captures one PATCH request for later assertions.
type patchRecord struct {
	path        string
	contentType string
	body        []byte
}

// fakeSecretAPI is a minimal kube secrets endpoint: GET serves the stored
// data map, strategic-merge PATCH merges into it.
type fakeSecretAPI struct {
	t        *testing.T
	wantPath string

	mu      sync.Mutex
	data    map[string]string
	patches []patchRecord
}

func (f *fakeSecretAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path != f.wantPath {
		f.t.Errorf("request path = %q, want %q", r.URL.Path, f.wantPath)
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"kind": "Secret", "data": f.data}); err != nil {
			f.t.Errorf("encoding secret: %v", err)
		}
	case http.MethodPatch:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("reading patch body: %v", err)
		}
		f.patches = append(f.patches, patchRecord{
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})
		var patch struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if f.data == nil {
			f.data = map[string]string{}
		}
		for k, v := range patch.Data {
			f.data[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"kind":"Secret"}`)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeSecretAPI) lastPatch(t *testing.T) patchRecord {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) == 0 {
		t.Fatal("no PATCH recorded")
	}
	return f.patches[len(f.patches)-1]
}

func TestStateComplete(t *testing.T) {
	t.Parallel()

	complete := State{
		TunnelToken:    "tok",
		ShimSecret:     "shh",
		AccountID:      "acct_1",
		TunnelID:       "tunnel_1",
		TunnelHostname: "k8s-acct.example.com",
	}

	tests := []struct {
		name   string
		mutate func(*State)
		want   bool
	}{
		{name: "all set", mutate: func(s *State) {}, want: true},
		{name: "registeredAt irrelevant", mutate: func(s *State) { s.RegisteredAt = time.Time{} }, want: true},
		{name: "missing tunnelToken", mutate: func(s *State) { s.TunnelToken = "" }, want: false},
		{name: "missing shimSecret", mutate: func(s *State) { s.ShimSecret = "" }, want: false},
		{name: "missing accountId", mutate: func(s *State) { s.AccountID = "" }, want: false},
		{name: "missing tunnelId", mutate: func(s *State) { s.TunnelID = "" }, want: false},
		{name: "missing tunnelHostname", mutate: func(s *State) { s.TunnelHostname = "" }, want: false},
		{name: "zero value", mutate: func(s *State) { *s = State{} }, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := complete
			tt.mutate(&st)
			if got := st.Complete(); got != tt.want {
				t.Errorf("Complete() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStateStoreSaveThenLoadRoundTrip(t *testing.T) {
	t.Parallel()

	fake := &fakeSecretAPI{t: t, wantPath: "/api/v1/namespaces/polylane/secrets/agent-state"}
	tc := newTestClient(t, fake)
	store := NewStateStore(tc.c, "polylane", "agent-state")

	registeredAt := time.Date(2026, 7, 17, 12, 34, 56, 0, time.UTC)
	want := State{
		TunnelToken:    "tok-round",
		ShimSecret:     "shh-round",
		AccountID:      "acct_round",
		TunnelID:       "tunnel_round",
		TunnelHostname: "k8s-acct-round.fake.test",
		RegisteredAt:   registeredAt,
	}

	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	patch := fake.lastPatch(t)
	if got, want := patch.contentType, "application/strategic-merge-patch+json"; got != want {
		t.Errorf("PATCH Content-Type = %q, want %q", got, want)
	}
	if got, want := patch.path, "/api/v1/namespaces/polylane/secrets/agent-state"; got != want {
		t.Errorf("PATCH path = %q, want %q", got, want)
	}

	var payload struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(patch.body, &payload); err != nil {
		t.Fatalf("PATCH body is not JSON: %v\nbody: %s", err, patch.body)
	}
	wantData := map[string]string{
		"tunnelToken":    b64(want.TunnelToken),
		"shimSecret":     b64(want.ShimSecret),
		"accountId":      b64(want.AccountID),
		"tunnelId":       b64(want.TunnelID),
		"tunnelHostname": b64(want.TunnelHostname),
		"registeredAt":   b64("2026-07-17T12:34:56Z"),
	}
	if len(payload.Data) != len(wantData) {
		t.Errorf("PATCH data has %d keys, want exactly %d", len(payload.Data), len(wantData))
	}
	for key, wantVal := range wantData {
		got, ok := payload.Data[key]
		if !ok {
			t.Errorf("PATCH data missing key %q", key)
			continue
		}
		if got != wantVal {
			t.Errorf("PATCH data[%q] = %q, want base64 %q", key, got, wantVal)
		}
		if _, err := base64.StdEncoding.DecodeString(got); err != nil {
			t.Errorf("PATCH data[%q] is not valid base64: %v", key, err)
		}
	}
	for key := range payload.Data {
		if _, ok := wantData[key]; !ok {
			t.Errorf("PATCH data has unexpected key %q", key)
		}
	}

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil state after Save")
	}
	if *got != want {
		t.Errorf("Load() = %+v, want %+v", *got, want)
	}
}

func TestStateStoreLoad(t *testing.T) {
	t.Parallel()

	const statePath = "/api/v1/namespaces/polylane/secrets/agent-state"
	fullBody := `{"kind":"Secret","data":{` +
		`"tunnelToken":"` + b64("tok-1") + `",` +
		`"shimSecret":"` + b64("shh-1") + `",` +
		`"accountId":"` + b64("acct_1") + `",` +
		`"tunnelId":"` + b64("tunnel_1") + `",` +
		`"tunnelHostname":"` + b64("k8s-acct_1.fake.test") + `",` +
		`"registeredAt":"` + b64("2026-07-17T01:02:03Z") + `"}}`

	tests := []struct {
		name    string
		status  int
		body    string
		want    *State
		wantNil bool
		wantErr string
	}{
		{
			name:   "populated secret",
			status: http.StatusOK,
			body:   fullBody,
			want: &State{
				TunnelToken:    "tok-1",
				ShimSecret:     "shh-1",
				AccountID:      "acct_1",
				TunnelID:       "tunnel_1",
				TunnelHostname: "k8s-acct_1.fake.test",
				RegisteredAt:   time.Date(2026, 7, 17, 1, 2, 3, 0, time.UTC),
			},
		},
		{
			name:    "empty data map",
			status:  http.StatusOK,
			body:    `{"kind":"Secret","data":{}}`,
			wantNil: true,
		},
		{
			name:    "no data field",
			status:  http.StatusOK,
			body:    `{"kind":"Secret","metadata":{"name":"agent-state"}}`,
			wantNil: true,
		},
		{
			name:    "null data",
			status:  http.StatusOK,
			body:    `{"kind":"Secret","data":null}`,
			wantNil: true,
		},
		{
			name:    "secret not found",
			status:  http.StatusNotFound,
			body:    `{"kind":"Status","reason":"NotFound","code":404}`,
			wantErr: "the chart must pre-create it",
		},
		{
			name:    "forbidden",
			status:  http.StatusForbidden,
			body:    `{"kind":"Status","reason":"Forbidden","code":403}`,
			wantErr: "403",
		},
		{
			name:    "malformed json",
			status:  http.StatusOK,
			body:    `{"kind":"Secret","data":`,
			wantErr: "decoding secret",
		},
		{
			name:    "data value not base64",
			status:  http.StatusOK,
			body:    `{"kind":"Secret","data":{"tunnelToken":"!!!not-base64!!!"}}`,
			wantErr: "decoding secret",
		},
		{
			name:    "registeredAt not RFC3339",
			status:  http.StatusOK,
			body:    `{"kind":"Secret","data":{"registeredAt":"` + b64("yesterday") + `"}}`,
			wantErr: "parsing registeredAt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != statePath {
					t.Errorf("request path = %q, want %q", r.URL.Path, statePath)
				}
				if r.Method != http.MethodGet {
					t.Errorf("request method = %q, want GET", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			store := NewStateStore(tc.c, "polylane", "agent-state")

			got, err := store.Load(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tt.wantNil {
				if got != nil {
					t.Fatalf("Load() = %+v, want nil state for empty-data secret", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("Load() = nil, want state")
			}
			if *got != *tt.want {
				t.Errorf("Load() = %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestStateStoreSaveErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr string
	}{
		{name: "not found", status: http.StatusNotFound, wantErr: "404"},
		{name: "forbidden", status: http.StatusForbidden, wantErr: "403"},
		{name: "conflict", status: http.StatusConflict, wantErr: "409"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"kind":"Status"}`, tt.status)
			}))
			store := NewStateStore(tc.c, "polylane", "agent-state")
			err := store.Save(context.Background(), State{TunnelToken: "tok"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Save error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestStateStoreEscapesPathSegments(t *testing.T) {
	t.Parallel()

	paths := make(chan string, 1)
	tc := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"kind":"Secret","data":{}}`)
	}))
	store := NewStateStore(tc.c, "poly lane", "agent/state")

	if _, err := store.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := <-paths
	want := "/api/v1/namespaces/poly%20lane/secrets/agent%2Fstate"
	if got != want {
		t.Errorf("request escaped path = %q, want %q", got, want)
	}
}
