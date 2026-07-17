// Package kube is a deliberately small in-cluster Kubernetes client built
// on raw net/http — no client-go. It answers four questions (who is this
// cluster, what version is it, is the API reachable, what namespace am I
// in) and persists agent state into one pre-created Secret.
//
// The projected ServiceAccount token rotates on disk; Token re-reads it
// when the file's mtime changes or after a fixed TTL, so a long-lived pod
// never presents an expired token.
package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreplanelabs/polylane-k8s/internal/buildinfo"
)

const (
	defaultTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- path to the token file, not a credential

	defaultCAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	// defaultTokenTTL bounds how stale a cached token may get even when
	// the file's mtime never changes (kubelet refresh can rewrite the
	// symlinked projected volume without the visible mtime moving).
	defaultTokenTTL = 5 * time.Minute

	// maxAPIResponseBytes caps how much of a kube API response this
	// client buffers; every object it reads is tiny.
	maxAPIResponseBytes = 4 << 20

	dialTimeout = 5 * time.Second
)

// Options configures the in-cluster client. Zero values take in-cluster
// defaults: the API server address from KUBERNETES_SERVICE_HOST/PORT and
// the projected ServiceAccount paths under
// /var/run/secrets/kubernetes.io/serviceaccount.
type Options struct {
	APIServerURL  string
	TokenPath     string
	CAPath        string
	NamespacePath string
	Logger        *slog.Logger
}

// Client talks to the kube REST API with the pod's ServiceAccount
// credentials. Per-call deadlines come from the caller's context; the
// client itself only bounds dialing.
type Client struct {
	baseURL       string
	tokenPath     string
	namespacePath string
	logger        *slog.Logger
	transport     http.RoundTripper
	hc            *http.Client

	// tokenTTL is a field (not the const) so tests can force expiry
	// without sleeping.
	tokenTTL time.Duration

	tokenMu     sync.Mutex
	token       string
	tokenMTime  time.Time
	tokenReadAt time.Time

	nsMu      sync.Mutex
	namespace string
}

// New builds a Client, loading the cluster CA eagerly: a pod that cannot
// read its own CA bundle can never talk to the API, so failing at
// construction beats failing on first use.
func New(opts Options) (*Client, error) {
	if opts.APIServerURL == "" {
		host := os.Getenv("KUBERNETES_SERVICE_HOST")
		port := os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, errors.New("kube: API server address is empty (set Options.APIServerURL or run in-cluster with KUBERNETES_SERVICE_HOST/PORT)")
		}
		opts.APIServerURL = "https://" + net.JoinHostPort(host, port)
	}
	if opts.TokenPath == "" {
		opts.TokenPath = defaultTokenPath
	}
	if opts.CAPath == "" {
		opts.CAPath = defaultCAPath
	}
	if opts.NamespacePath == "" {
		opts.NamespacePath = defaultNamespacePath
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	caPEM, err := os.ReadFile(opts.CAPath)
	if err != nil {
		return nil, fmt.Errorf("kube: reading CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("kube: CA bundle %s contains no certificates", opts.CAPath)
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
		DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	return &Client{
		baseURL:       strings.TrimRight(opts.APIServerURL, "/"),
		tokenPath:     opts.TokenPath,
		namespacePath: opts.NamespacePath,
		logger:        opts.Logger,
		transport:     transport,
		hc:            &http.Client{Transport: transport},
		tokenTTL:      defaultTokenTTL,
	}, nil
}

// Token returns the current ServiceAccount token, re-reading the file when
// its mtime changes or the TTL lapses. Safe for concurrent use.
func (c *Client) Token() (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	fi, err := os.Stat(c.tokenPath)
	if err != nil {
		return "", fmt.Errorf("kube: reading token: %w", err)
	}
	if c.token != "" && time.Since(c.tokenReadAt) < c.tokenTTL && fi.ModTime().Equal(c.tokenMTime) {
		return c.token, nil
	}

	data, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return "", fmt.Errorf("kube: reading token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("kube: token file %s is empty", c.tokenPath)
	}
	if c.token != "" && token != c.token {
		c.logger.Debug("serviceaccount token rotated", "path", c.tokenPath)
	}
	c.token, c.tokenMTime, c.tokenReadAt = token, fi.ModTime(), time.Now()
	return c.token, nil
}

// Namespace returns the pod's own namespace, read once from the downward
// API file and cached on success.
func (c *Client) Namespace() (string, error) {
	c.nsMu.Lock()
	defer c.nsMu.Unlock()

	if c.namespace != "" {
		return c.namespace, nil
	}
	data, err := os.ReadFile(c.namespacePath)
	if err != nil {
		return "", fmt.Errorf("kube: reading namespace: %w", err)
	}
	ns := strings.TrimSpace(string(data))
	if ns == "" {
		return "", fmt.Errorf("kube: namespace file %s is empty", c.namespacePath)
	}
	c.namespace = ns
	return ns, nil
}

// BaseURL returns the API server base URL without a trailing slash.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// Transport returns a RoundTripper trusting the cluster CA. It adds no
// Authorization header — callers that need one attach it per request.
func (c *Client) Transport() http.RoundTripper {
	return c.transport
}

// Identity returns the UID of the kube-system namespace: the closest thing
// Kubernetes has to a stable cluster identifier, surviving agent
// reinstalls and node churn.
func (c *Client) Identity(ctx context.Context) (string, error) {
	var out struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if err := c.getJSON(ctx, "/api/v1/namespaces/kube-system", &out); err != nil {
		return "", err
	}
	if out.Metadata.UID == "" {
		return "", errors.New("kube: kube-system namespace response has no metadata.uid")
	}
	return out.Metadata.UID, nil
}

// Version returns the API server's gitVersion (e.g. "v1.31.2").
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := c.getJSON(ctx, "/version", &out); err != nil {
		return "", err
	}
	if out.GitVersion == "" {
		return "", errors.New("kube: /version response has no gitVersion")
	}
	return out.GitVersion, nil
}

// Ping reports whether the API server answers at all; nil on any 2xx.
func (c *Client) Ping(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("kube: GET /version: %w", err)
	}
	defer resp.Body.Close()
	if !is2xx(resp.StatusCode) {
		return statusError(http.MethodGet, "/version", resp)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxAPIResponseBytes))
	return nil
}

// newRequest builds an authenticated API request; the Authorization header
// carries the freshest token so rotation propagates on the next call.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	token, err := c.Token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("kube: building %s %s request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	return req, nil
}

// getJSON GETs path and decodes a 2xx response body into out.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("kube: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if !is2xx(resp.StatusCode) {
		return statusError(http.MethodGet, path, resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("kube: GET %s: decoding response: %w", path, err)
	}
	return nil
}

func is2xx(code int) bool {
	return code >= 200 && code < 300
}

// statusError renders a non-2xx response as an error, folding in a short
// body excerpt — kube Status messages are worth surfacing, in full they
// are not.
func statusError(method, path string, resp *http.Response) error {
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(excerpt))
	if msg == "" {
		return fmt.Errorf("kube: %s %s: unexpected status %s", method, path, resp.Status)
	}
	return fmt.Errorf("kube: %s %s: unexpected status %s: %s", method, path, resp.Status, msg)
}
