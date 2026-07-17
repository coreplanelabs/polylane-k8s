// Package platform implements the Polylane registration client (COR-32
// §2, frozen wire contract). The agent registers once on first boot and
// again only after a credentials rejection — never in steady state — so
// this client is mostly a retry-taxonomy machine: transient failures
// (429, 5xx, network) are retried forever with capped full-jitter
// backoff, terminal rejections (400, 401, 403, 426) surface immediately
// so a bad API key exits crisply instead of spinning.
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coreplanelabs/polylane-k8s/internal/buildinfo"
)

// registerPath is the registration endpoint, appended to Options.BaseURL.
const registerPath = "/v1/cloud_accounts/kubernetes/agent/register"

const (
	// maxResponseBytes caps a success response body. A registration
	// payload is a handful of identifiers plus a tunnel token; anything
	// bigger is not one.
	maxResponseBytes = 1 << 20
	// maxErrorBodyBytes caps how much of a failure body is read for the
	// error message.
	maxErrorBodyBytes = 8 << 10
)

// RegisterRequest is the registration payload (COR-32 §2). The JSON field
// names are the frozen wire contract — never rename them.
type RegisterRequest struct {
	ClusterUID        string `json:"clusterUid"`
	ClusterName       string `json:"clusterName,omitempty"`
	KubernetesVersion string `json:"kubernetesVersion"`
	AgentVersion      string `json:"agentVersion"`
	Distribution      string `json:"distribution,omitempty"`
	Namespace         string `json:"namespace"`
	ChartVersion      string `json:"chartVersion,omitempty"`
}

// RegisterResponse is the platform's registration result. The credential
// fields are what the agent persists; Object is informational
// ("kubernetes_agent_registration").
type RegisterResponse struct {
	AccountID      string `json:"accountId"`
	AgentID        string `json:"agentId"`
	TunnelID       string `json:"tunnelId"`
	TunnelToken    string `json:"tunnelToken"`
	TunnelHostname string `json:"tunnelHostname"`
	ShimSecret     string `json:"shimSecret"`
	Object         string `json:"_object"`
}

// Options configures the registration client.
type Options struct {
	// BaseURL is the platform API root, e.g. https://api.polylane.com.
	BaseURL string
	// APIKey is sent as x-api-key on every request.
	APIKey string
	// HTTPClient overrides the default client. The default uses a fresh
	// proxy-aware transport (HTTPS_PROXY/NO_PROXY via
	// http.ProxyFromEnvironment) with the system trust store — which
	// honors SSL_CERT_FILE natively — and a 30s overall timeout.
	HTTPClient *http.Client
	// Logger receives retry/backoff logs. Defaults to slog.Default().
	Logger *slog.Logger
}

// Client registers the agent with the Polylane platform. Construct with
// New; the zero value is not usable.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	logger     *slog.Logger

	// sleep waits between retries; injectable so tests record durations
	// instead of spending real time.
	sleep func(context.Context, time.Duration) error
}

// New builds a Client from opts.
func New(opts Options) *Client {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:   true,
				TLSHandshakeTimeout: 10 * time.Second,
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		apiKey:     opts.APIKey,
		httpClient: httpClient,
		logger:     logger,
		sleep:      sleepCtx,
	}
}

// Register performs one registration attempt. Failures are classified per
// the COR-32 taxonomy: *TransientError (429, 5xx, network) may be
// retried, *TerminalError (any other non-2xx) must not be. Malformed
// success responses return plain errors and are likewise not retryable.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("platform: encoding register request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+registerPath, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("platform: building register request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("User-Agent", buildinfo.UserAgent())
	httpReq.Header.Set("x-nominal-client", "k8s-agent")
	httpReq.Header.Set("x-nominal-client-version", buildinfo.Version)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, &TransientError{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, classify(resp)
	}
	return parseRegisterResponse(resp.Body)
}

// classify maps a non-2xx response onto the retry taxonomy. Statuses
// outside the documented set (e.g. a 404 from a misrouted gateway) are
// terminal: retrying cannot repair a request the platform rejected.
func classify(resp *http.Response) error {
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return &TransientError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	return &TerminalError{StatusCode: resp.StatusCode, Message: errorMessage(resp.Body)}
}

// errorMessage extracts a human-readable message from a failure body,
// reading at most maxErrorBodyBytes. JSON {"error":...} / {"message":...}
// shapes are unwrapped; anything else is returned as trimmed text.
func errorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		if payload.Error != "" {
			return payload.Error
		}
		if payload.Message != "" {
			return payload.Message
		}
	}
	return strings.TrimSpace(string(raw))
}

// parseRegisterResponse decodes a success body, enforcing the size cap
// and the presence of every field the agent cannot operate without.
func parseRegisterResponse(body io.Reader) (*RegisterResponse, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		// The status line said success but the body transfer broke:
		// network-shaped, so retryable.
		return nil, &TransientError{Err: fmt.Errorf("reading register response: %w", err)}
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("platform: register response exceeds %d bytes", maxResponseBytes)
	}
	var out RegisterResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("platform: parsing register response: %w", err)
	}
	// Required set matches kube.State.Complete(): anything less would be
	// persisted as incomplete state and silently re-register every boot.
	var missing []string
	for _, field := range []struct{ name, value string }{
		{"accountId", out.AccountID},
		{"tunnelId", out.TunnelID},
		{"tunnelToken", out.TunnelToken},
		{"tunnelHostname", out.TunnelHostname},
		{"shimSecret", out.ShimSecret},
	} {
		if field.value == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("platform: register response missing required fields: %s", strings.Join(missing, ", "))
	}
	return &out, nil
}
