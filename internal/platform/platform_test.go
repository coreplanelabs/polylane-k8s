package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coreplanelabs/polylane-k8s/internal/buildinfo"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func minimalRequest() RegisterRequest {
	return RegisterRequest{
		ClusterUID:        "uid-1",
		KubernetesVersion: "v1.31.2",
		AgentVersion:      "0.1.0",
		Namespace:         "polylane",
	}
}

func validResponseBody() map[string]any {
	return map[string]any{
		"accountId":      "acct_123",
		"agentId":        "agent_456",
		"tunnelId":       "tunnel_789",
		"tunnelToken":    "fixture-tunnel-token",
		"tunnelHostname": "k8s-acct_123.polylane.com",
		"shimSecret":     "fixture-shim-key",
		"_object":        "kubernetes_agent_registration",
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

type capturedRequest struct {
	method string
	path   string
	header http.Header
	body   map[string]any
}

func TestRegisterSendsFrozenWireFormat(t *testing.T) {
	t.Parallel()
	captured := make(chan capturedRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		captured <- capturedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}
		writeJSON(t, w, http.StatusOK, validResponseBody())
	}))
	defer srv.Close()

	// The trailing slash on BaseURL must not double up in the path.
	c := New(Options{BaseURL: srv.URL + "/", APIKey: "sk_test_123", Logger: discardLogger()})
	resp, err := c.Register(context.Background(), RegisterRequest{
		ClusterUID:        "uid-1",
		ClusterName:       "prod-us-east",
		KubernetesVersion: "v1.31.2",
		AgentVersion:      "0.1.0",
		Distribution:      "eks",
		Namespace:         "polylane",
		ChartVersion:      "1.2.3",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	got := <-captured
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/v1/cloud_accounts/kubernetes/agent/register" {
		t.Errorf("path = %s, want /v1/cloud_accounts/kubernetes/agent/register", got.path)
	}
	for header, want := range map[string]string{
		"x-api-key":                "sk_test_123",
		"User-Agent":               buildinfo.UserAgent(),
		"x-nominal-client":         "k8s-agent",
		"x-nominal-client-version": buildinfo.Version,
		"Content-Type":             "application/json",
	} {
		if g := got.header.Get(header); g != want {
			t.Errorf("header %s = %q, want %q", header, g, want)
		}
	}
	wantBody := map[string]any{
		"clusterUid":        "uid-1",
		"clusterName":       "prod-us-east",
		"kubernetesVersion": "v1.31.2",
		"agentVersion":      "0.1.0",
		"distribution":      "eks",
		"namespace":         "polylane",
		"chartVersion":      "1.2.3",
	}
	if !reflect.DeepEqual(got.body, wantBody) {
		t.Errorf("request body = %v, want %v", got.body, wantBody)
	}
	want := &RegisterResponse{
		AccountID:      "acct_123",
		AgentID:        "agent_456",
		TunnelID:       "tunnel_789",
		TunnelToken:    "fixture-tunnel-token",
		TunnelHostname: "k8s-acct_123.polylane.com",
		ShimSecret:     "fixture-shim-key",
		Object:         "kubernetes_agent_registration",
	}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("response = %+v, want %+v", resp, want)
	}
}

func TestRegisterOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()
	captured := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		captured <- body
		writeJSON(t, w, http.StatusOK, validResponseBody())
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	if _, err := c.Register(context.Background(), minimalRequest()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	body := <-captured
	wantKeys := []string{"clusterUid", "kubernetesVersion", "agentVersion", "namespace"}
	if len(body) != len(wantKeys) {
		t.Errorf("request body = %v, want exactly the keys %v", body, wantKeys)
	}
	for _, key := range wantKeys {
		if _, ok := body[key]; !ok {
			t.Errorf("request body missing key %q", key)
		}
	}
}

func TestRegisterStatusTaxonomy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status           int
		wantTerminal     bool
		wantUnauthorized bool
	}{
		{http.StatusBadRequest, true, false},
		{http.StatusUnauthorized, true, true},
		{http.StatusForbidden, true, true},
		{http.StatusUpgradeRequired, true, false},
		// Outside the documented taxonomy: terminal, because retrying
		// cannot fix a request the platform rejected.
		{http.StatusNotFound, true, false},
		{http.StatusTooManyRequests, false, false},
		{http.StatusInternalServerError, false, false},
		{http.StatusBadGateway, false, false},
		{http.StatusServiceUnavailable, false, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("status_%d", tt.status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "3")
				}
				writeJSON(t, w, tt.status, map[string]string{"error": "nope"})
			}))
			defer srv.Close()

			c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
			resp, err := c.Register(context.Background(), minimalRequest())
			if resp != nil {
				t.Errorf("response = %+v, want nil", resp)
			}
			if err == nil {
				t.Fatal("Register returned nil error")
			}
			if got := IsTerminal(err); got != tt.wantTerminal {
				t.Errorf("IsTerminal = %t, want %t (err: %v)", got, tt.wantTerminal, err)
			}
			if got := errors.Is(err, ErrUnauthorized); got != tt.wantUnauthorized {
				t.Errorf("errors.Is(err, ErrUnauthorized) = %t, want %t (err: %v)", got, tt.wantUnauthorized, err)
			}
			if tt.wantTerminal {
				var terminal *TerminalError
				if !errors.As(err, &terminal) {
					t.Fatalf("err = %v (%T), want *TerminalError", err, err)
				}
				if terminal.StatusCode != tt.status {
					t.Errorf("StatusCode = %d, want %d", terminal.StatusCode, tt.status)
				}
				if terminal.Message != "nope" {
					t.Errorf("Message = %q, want %q", terminal.Message, "nope")
				}
				return
			}
			var transient *TransientError
			if !errors.As(err, &transient) {
				t.Fatalf("err = %v (%T), want *TransientError", err, err)
			}
			if transient.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", transient.StatusCode, tt.status)
			}
			if tt.status == http.StatusTooManyRequests && transient.RetryAfter != 3*time.Second {
				t.Errorf("RetryAfter = %v, want 3s", transient.RetryAfter)
			}
		})
	}
}

func TestRegisterNetworkErrorIsTransient(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // refuse all connections

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	_, err := c.Register(context.Background(), minimalRequest())
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("err = %v (%T), want *TransientError", err, err)
	}
	if transient.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 for a network error", transient.StatusCode)
	}
	if transient.Err == nil {
		t.Error("Err = nil, want the underlying transport error")
	}
	if IsTerminal(err) {
		t.Error("IsTerminal = true for a network error")
	}
}

func TestRegisterRejectsOversizeResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strings.Repeat("a", maxResponseBytes+1)))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	_, err := c.Register(context.Background(), minimalRequest())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want a size-cap error", err)
	}
	// Neither transient nor terminal-typed: the retry loop must surface
	// it immediately rather than retrying.
	var transient *TransientError
	if errors.As(err, &transient) {
		t.Errorf("oversize response classified transient: %v", err)
	}
}

func TestRegisterRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "not json")
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	_, err := c.Register(context.Background(), minimalRequest())
	if err == nil || !strings.Contains(err.Error(), "parsing register response") {
		t.Fatalf("err = %v, want a parse error", err)
	}
}

func TestRegisterMissingRequiredFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		remove  string
		wantErr string // substring; empty means success expected
	}{
		{"accountId", "accountId"},
		{"tunnelId", "tunnelId"}, // required: kube.State.Complete() needs it
		{"tunnelToken", "tunnelToken"},
		{"tunnelHostname", "tunnelHostname"},
		{"shimSecret", "shimSecret"},
		// agentId and _object are informative, not load-bearing.
		{"agentId", ""},
		{"_object", ""},
	}
	for _, tt := range tests {
		t.Run("missing_"+tt.remove, func(t *testing.T) {
			t.Parallel()
			body := validResponseBody()
			delete(body, tt.remove)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusOK, body)
			}))
			defer srv.Close()

			c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
			resp, err := c.Register(context.Background(), minimalRequest())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Register: %v", err)
				}
				if resp == nil {
					t.Fatal("response = nil on success")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
			if resp != nil {
				t.Errorf("response = %+v, want nil", resp)
			}
		})
	}
}

func TestRegisterEmptyObjectListsAllMissingFields(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	_, err := c.Register(context.Background(), minimalRequest())
	if err == nil {
		t.Fatal("Register returned nil error for an empty response object")
	}
	for _, field := range []string{"accountId", "tunnelToken", "tunnelHostname", "shimSecret"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("err %q does not mention missing field %q", err, field)
		}
	}
}

func TestRegisterTerminalErrorMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{"json_error_field", `{"error":"invalid api key"}`, "invalid api key"},
		{"json_message_field", `{"message":"agent version too old"}`, "agent version too old"},
		{"plain_text", "  bad request  \n", "bad request"},
		{"empty_body", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
			_, err := c.Register(context.Background(), minimalRequest())
			var terminal *TerminalError
			if !errors.As(err, &terminal) {
				t.Fatalf("err = %v (%T), want *TerminalError", err, err)
			}
			if terminal.Message != tt.want {
				t.Errorf("Message = %q, want %q", terminal.Message, tt.want)
			}
		})
	}
}

func TestRegisterTerminalErrorBodyCappedAt8KiB(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, strings.Repeat("x", 3*maxErrorBodyBytes))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	_, err := c.Register(context.Background(), minimalRequest())
	var terminal *TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("err = %v (%T), want *TerminalError", err, err)
	}
	if len(terminal.Message) != maxErrorBodyBytes {
		t.Errorf("len(Message) = %d, want %d", len(terminal.Message), maxErrorBodyBytes)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Error("errors.Is(err, ErrUnauthorized) = false for a 401")
	}
}

func TestErrorTaxonomyHelpers(t *testing.T) {
	t.Parallel()
	if IsTerminal(nil) {
		t.Error("IsTerminal(nil) = true")
	}
	if IsTerminal(&TransientError{StatusCode: 500}) {
		t.Error("IsTerminal(transient) = true")
	}
	wrapped := fmt.Errorf("boot: %w", &TerminalError{StatusCode: 401, Message: "no"})
	if !IsTerminal(wrapped) {
		t.Error("IsTerminal(wrapped terminal) = false")
	}
	if !errors.Is(wrapped, ErrUnauthorized) {
		t.Error("errors.Is(wrapped 401, ErrUnauthorized) = false")
	}
	if errors.Is(fmt.Errorf("x: %w", &TerminalError{StatusCode: 400}), ErrUnauthorized) {
		t.Error("errors.Is(400, ErrUnauthorized) = true")
	}
	if errors.Is(errors.New("other"), ErrUnauthorized) {
		t.Error("errors.Is(unrelated, ErrUnauthorized) = true")
	}
	if got := (&TerminalError{StatusCode: 426}).Error(); !strings.Contains(got, "426") {
		t.Errorf("TerminalError.Error() = %q, want the status code in it", got)
	}
	if got := (&TransientError{StatusCode: 503}).Error(); !strings.Contains(got, "503") {
		t.Errorf("TransientError.Error() = %q, want the status code in it", got)
	}
	inner := errors.New("dial tcp: connection refused")
	netErr := &TransientError{Err: inner}
	if !strings.Contains(netErr.Error(), "connection refused") {
		t.Errorf("TransientError.Error() = %q, want the cause in it", netErr.Error())
	}
	if !errors.Is(netErr, inner) {
		t.Error("errors.Is(netErr, inner) = false; Unwrap broken")
	}
}
