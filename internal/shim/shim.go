// Package shim implements the agent's security boundary: a read-only HTTP
// pass-through proxy in front of the Kubernetes API, reached only through
// the Cloudflare Tunnel and bound only to loopback (ListenLoopback).
//
// Every request runs an ordered, deny-first policy — auth, local health,
// method, path hygiene, allowed roots, subresource denylist, secrets
// denylist, query, upgrade — and the FIRST failing rule decides the
// response with a fixed status code and an {"error":"<code>"} body.
// Surviving requests are re-issued upstream as fresh GETs with a nil body
// and an allowlisted header set, so writes, exec, watches, and protocol
// upgrades cannot transit the shim by construction.
package shim

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Outcome labels reported to the MetricsRecorder: one per policy rule plus
// the local health endpoint and the two relay results.
const (
	outcomeProxied           = "proxied"
	outcomeHealthz           = "healthz"
	outcomeUnauthorized      = "unauthorized"
	outcomeMethodNotAllowed  = "method_not_allowed"
	outcomeBadPath           = "bad_path"
	outcomeForbiddenPath     = "forbidden_path"
	outcomeForbiddenSubresrc = "forbidden_subresource"
	outcomeForbiddenSecrets  = "forbidden_secrets"
	outcomeBadQuery          = "bad_query"
	outcomeUpgradeRejected   = "upgrade_rejected"
	outcomeKubeUnreachable   = "kube_unreachable"
	outcomeUpstreamError     = "upstream_error"
)

const (
	// headerShimKey authenticates the platform to the shim. It is consumed
	// by rule 1 and never forwarded upstream.
	headerShimKey = "x-polylane-shim-key"

	defaultMaxResponseBytes = 32 << 20
	defaultUpstreamTimeout  = 100 * time.Second
)

// MetricsRecorder receives exactly one outcome per request. Outcomes:
// "proxied", "healthz", "unauthorized", "method_not_allowed", "bad_path",
// "forbidden_path", "forbidden_subresource", "forbidden_secrets",
// "bad_query", "upgrade_rejected", "kube_unreachable", "upstream_error".
type MetricsRecorder interface {
	RecordRequest(outcome string, duration time.Duration)
}

// Config wires the handler. Secret, UpstreamURL, and Token are required;
// everything else has safe defaults.
type Config struct {
	// Secret returns the CURRENT shim secret. It is a func, not a value, so
	// the agent can swap it atomically when the platform rotates it; every
	// request re-reads it. An empty secret never authenticates anything.
	Secret func() string
	// UpstreamURL is the kube API base URL (https://kubernetes.default.svc
	// in-cluster, an httptest server in tests).
	UpstreamURL *url.URL
	// Transport must trust the upstream's CA. It carries no credentials —
	// the shim adds the bearer token per request. Defaults to
	// http.DefaultTransport.
	Transport http.RoundTripper
	// Token returns the current ServiceAccount bearer token for upstream
	// requests.
	Token func() (string, error)
	// AgentVersion appears in the local /healthz body and in the upstream
	// User-Agent ("polylane-k8s/<version>").
	AgentVersion string
	// KubeReachable feeds the local /healthz body; nil reads as false.
	KubeReachable func() bool
	// Metrics receives one outcome per request; nil disables recording.
	Metrics MetricsRecorder
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// MaxResponseBytes caps relayed response bodies (default 32 MiB). At
	// the cap the connection is aborted so a truncated body can never be
	// mistaken for a complete one.
	MaxResponseBytes int64
	// UpstreamTimeout bounds the whole upstream exchange (default 100s,
	// sized for kube API pagination worst cases).
	UpstreamTimeout time.Duration
}

// NewHandler builds the shim's http.Handler. Secret, UpstreamURL, and Token
// are the security-load-bearing inputs; omitting one is a programming error
// and panics rather than defaulting to anything permissive.
func NewHandler(cfg Config) http.Handler {
	switch {
	case cfg.Secret == nil:
		panic("shim: Config.Secret is required")
	case cfg.UpstreamURL == nil:
		panic("shim: Config.UpstreamURL is required")
	case cfg.Token == nil:
		panic("shim: Config.Token is required")
	}
	if cfg.Transport == nil {
		cfg.Transport = http.DefaultTransport
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = defaultMaxResponseBytes
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = defaultUpstreamTimeout
	}
	return &handler{
		cfg: cfg,
		client: &http.Client{
			Transport: cfg.Transport,
			// Redirects are NOT followed: a 3xx from the apiserver is
			// passed through verbatim like any other non-2xx, and the shim
			// never chases a Location it has not policy-checked.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type handler struct {
	cfg    Config
	client *http.Client
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	outcome, abort := h.serve(w, r)
	d := time.Since(start)
	if h.cfg.Metrics != nil {
		h.cfg.Metrics.RecordRequest(outcome, d)
	}
	level := slog.LevelDebug
	if outcome != outcomeProxied && outcome != outcomeHealthz {
		level = slog.LevelWarn
	}
	h.cfg.Logger.Log(r.Context(), level, "shim request",
		"method", r.Method,
		"path", r.URL.EscapedPath(),
		"outcome", outcome,
		"duration", d,
	)
	if abort {
		// The sanctioned way to sever the connection: a truncated or broken
		// relay must never look like a cleanly terminated body downstream.
		panic(http.ErrAbortHandler)
	}
}

// serve runs the ordered policy. First failure wins; the returned outcome
// doubles as the metrics label, and abort asks ServeHTTP to kill the
// connection after recording.
func (h *handler) serve(w http.ResponseWriter, r *http.Request) (outcome string, abort bool) {
	// Rule 1 — auth. Runs before everything else so the unauthenticated
	// surface is a single constant-time 401 regardless of path or method.
	if !h.authorized(r) {
		h.deny(w, http.StatusUnauthorized, outcomeUnauthorized)
		return outcomeUnauthorized, false
	}

	// Rule 2 — the shim's own health endpoint, answered locally and never
	// proxied, so platform-side monitoring generates zero kube API load.
	if r.Method == http.MethodGet && r.URL.EscapedPath() == "/healthz" {
		h.writeHealthz(w)
		return outcomeHealthz, false
	}

	// Rule 3 — read-only means GET, literally. HEAD is rejected too: it
	// runs the same handlers upstream and complicates cap accounting for
	// zero platform value.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.deny(w, http.StatusMethodNotAllowed, outcomeMethodNotAllowed)
		return outcomeMethodNotAllowed, false
	}

	// Rule 4a — path hygiene on the RAW (still-encoded) path; exactly one
	// decode pass, anything ambiguous is refused rather than interpreted.
	decoded, ok := decodePath(r.URL.EscapedPath())
	if !ok {
		h.deny(w, http.StatusBadRequest, outcomeBadPath)
		return outcomeBadPath, false
	}

	// Rule 4b — allowed roots: /version exactly, or the /api and /apis
	// trees. Everything else on the apiserver (/openapi, /logs, /metrics,
	// /debug, ...) is out of scope for a read-only inspector.
	if !allowedRoot(decoded) {
		h.deny(w, http.StatusForbidden, outcomeForbiddenPath)
		return outcomeForbiddenPath, false
	}

	// Rule 5 — subresources that execute, mutate, or open long-lived
	// channels. Segment-exact at any depth, case-insensitive, so a pod
	// named "exec" is (acceptably) uninspectable rather than the rule
	// being bypassable.
	if hasDeniedSubresource(decoded) {
		h.deny(w, http.StatusForbidden, outcomeForbiddenSubresrc)
		return outcomeForbiddenSubresrc, false
	}

	// Rule 6 — Secret objects never transit the shim, in any namespace, at
	// any depth. RBAC already withholds them from the ServiceAccount; this
	// layer holds even if cluster RBAC is later loosened by mistake.
	if hasSecretsSegment(decoded) {
		h.deny(w, http.StatusForbidden, outcomeForbiddenSecrets)
		return outcomeForbiddenSecrets, false
	}

	// Rule 7 — no long-lived streams: a watch or follow key rejects the
	// request whatever its value, and a query that does not even parse is
	// refused rather than guessed at. Everything else is forwarded
	// verbatim below — the shim never rewrites a query it accepted.
	// The deprecated path form of the same thing (/api/v1/watch/...,
	// /apis/<g>/<v>/watch/...) is refused too: RBAC's missing watch verb
	// already denies it at the apiserver, but streaming is a non-goal and
	// this layer should not lean on the next one. Positional, so a
	// resource merely NAMED "watch" is unaffected.
	if q, err := url.ParseQuery(r.URL.RawQuery); err != nil || q.Has("watch") || q.Has("follow") || legacyWatchPath(decoded) {
		h.deny(w, http.StatusBadRequest, outcomeBadQuery)
		return outcomeBadQuery, false
	}

	// Rule 8 — protocol-upgrade kill switch (WebSocket, SPDY).
	if requestsUpgrade(r.Header) {
		h.deny(w, http.StatusBadRequest, outcomeUpgradeRejected)
		return outcomeUpgradeRejected, false
	}

	// Rule 9 + forward — see proxy: fresh outbound GET, nil body,
	// allowlisted headers only.
	return h.proxy(w, r, decoded)
}

// authorized implements rule 1. Both sides are hashed before the
// constant-time compare so even the secret's length is not leaked through
// timing.
func (h *handler) authorized(r *http.Request) bool {
	secret := h.cfg.Secret()
	if secret == "" {
		// No secret yet (registration not complete) — nothing may pass.
		return false
	}
	presented := sha256.Sum256([]byte(r.Header.Get(headerShimKey)))
	expected := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}

func (h *handler) writeHealthz(w http.ResponseWriter) {
	reachable := h.cfg.KubeReachable != nil && h.cfg.KubeReachable()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","kubeReachable":%t,"agentVersion":%q}`, reachable, h.cfg.AgentVersion)
}

// deny writes a policy verdict: the fixed status code plus the
// {"error":"<code>"} body the conformance suite pins byte-for-byte.
func (h *handler) deny(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, code)
}

// proxy relays an approved request upstream. The outbound request is built
// from scratch — nil body, only Accept and Accept-Encoding copied from the
// inbound request, plus Authorization and User-Agent — so nothing the
// policy did not inspect (including x-polylane-shim-key) ever reaches the
// kube API.
func (h *handler) proxy(w http.ResponseWriter, r *http.Request, decoded string) (string, bool) {
	token, err := h.cfg.Token()
	if err != nil {
		h.cfg.Logger.Error("reading kube token", "error", err)
		h.deny(w, http.StatusBadGateway, outcomeKubeUnreachable)
		return outcomeKubeUnreachable, false
	}

	out := *h.cfg.UpstreamURL
	out.Path = decoded
	out.RawPath = ""
	if raw := r.URL.EscapedPath(); raw != decoded {
		// Preserve the original escaping: upstream decodes exactly once,
		// like we did, so it sees precisely the path the policy checked.
		out.RawPath = raw
	}
	out.RawQuery = r.URL.RawQuery

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.UpstreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, out.String(), nil)
	if err != nil {
		h.cfg.Logger.Error("building upstream request", "error", err)
		h.deny(w, http.StatusBadGateway, outcomeKubeUnreachable)
		return outcomeKubeUnreachable, false
	}
	for _, v := range r.Header.Values("Accept") {
		req.Header.Add("Accept", v)
	}
	for _, v := range r.Header.Values("Accept-Encoding") {
		req.Header.Add("Accept-Encoding", v)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "polylane-k8s/"+h.cfg.AgentVersion)

	resp, err := h.client.Do(req)
	if err != nil {
		h.cfg.Logger.Warn("kube api unreachable", "error", err)
		h.deny(w, http.StatusBadGateway, outcomeKubeUnreachable)
		return outcomeKubeUnreachable, false
	}
	defer resp.Body.Close()

	// Status passes through verbatim — including non-2xx: the platform's
	// error taxonomy needs the real kube status. Headers are allowlisted;
	// Content-Length only when we know we will not truncate.
	hdr := w.Header()
	for _, k := range []string{"Content-Type", "Content-Encoding"} {
		for _, v := range resp.Header.Values(k) {
			hdr.Add(k, v)
		}
	}
	if cl := resp.ContentLength; cl >= 0 && cl <= h.cfg.MaxResponseBytes {
		hdr.Set("Content-Length", strconv.FormatInt(cl, 10))
	}
	w.WriteHeader(resp.StatusCode)

	n, copyErr := io.Copy(w, io.LimitReader(resp.Body, h.cfg.MaxResponseBytes))
	if copyErr != nil {
		h.cfg.Logger.Warn("relaying kube response failed", "error", copyErr, "bytes", n)
		return outcomeUpstreamError, true
	}
	if n == h.cfg.MaxResponseBytes {
		// At the cap: probe one byte to distinguish an exactly-cap-sized
		// body from a truncated one.
		var probe [1]byte
		if m, _ := resp.Body.Read(probe[:]); m > 0 {
			h.cfg.Logger.Warn("kube response truncated at cap",
				"cap_bytes", h.cfg.MaxResponseBytes, "path", decoded)
			// Flush what was relayed, then abort so the client sees a
			// severed stream, never a clean end.
			_ = http.NewResponseController(w).Flush()
			return outcomeProxied, true
		}
	}
	return outcomeProxied, false
}

// decodePath applies rule 4's hygiene checks to the raw (still-encoded)
// request path and returns the single-pass-decoded form that all later
// rules and the upstream request use.
func decodePath(raw string) (string, bool) {
	if raw == "" || raw[0] != '/' {
		return "", false
	}
	lower := strings.ToLower(raw)
	// Encoded dots are refused wholesale: %2e never appears in a
	// legitimate kube API path and is the building block of every
	// traversal encoding.
	if strings.Contains(lower, "%2e") {
		return "", false
	}
	// Backslashes, raw or encoded: some path parsers treat them as
	// separators; the kube API never needs them.
	if strings.Contains(raw, `\`) || strings.Contains(lower, "%5c") {
		return "", false
	}
	// Empty segments.
	if strings.Contains(raw, "//") {
		return "", false
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", false
	}
	// A %2e or %5c surviving the decode means the input was double-encoded
	// (%252e). One decode pass is all it gets; refuse.
	decodedLower := strings.ToLower(decoded)
	if strings.Contains(decodedLower, "%2e") ||
		strings.Contains(decodedLower, "%5c") ||
		strings.Contains(decoded, `\`) {
		return "", false
	}
	// ".." segments, named explicitly even though path.Clean below also
	// catches them: traversal is the attack this rule exists for.
	for _, seg := range strings.Split(decoded[1:], "/") {
		if seg == ".." {
			return "", false
		}
	}
	// Idempotence under path.Clean catches everything else non-canonical:
	// "." segments, trailing slashes, post-decode empty segments.
	if path.Clean(decoded) != decoded {
		return "", false
	}
	return decoded, true
}

// allowedRoot is rule 4's allowlist. Matching is segment-exact: /apisx or
// /apiextensions do not ride along on the /api prefix.
func allowedRoot(p string) bool {
	return p == "/version" ||
		p == "/api" || strings.HasPrefix(p, "/api/") ||
		p == "/apis" || strings.HasPrefix(p, "/apis/")
}

// deniedSubresources are kube subresources that execute code, mutate
// state, or open long-lived channels — everything a read-only agent must
// not touch (rule 5).
var deniedSubresources = map[string]bool{
	"exec":                true,
	"attach":              true,
	"portforward":         true,
	"proxy":               true,
	"ephemeralcontainers": true,
	"finalize":            true,
	"binding":             true,
	"eviction":            true,
}

func hasDeniedSubresource(p string) bool {
	for _, seg := range strings.Split(p[1:], "/") {
		if deniedSubresources[strings.ToLower(seg)] {
			return true
		}
	}
	return false
}

// hasSecretsSegment implements rule 6: any path segment equal to "secrets",
// case-insensitively, at any depth.
func hasSecretsSegment(p string) bool {
	for _, seg := range strings.Split(p[1:], "/") {
		if strings.EqualFold(seg, "secrets") {
			return true
		}
	}
	return false
}

// legacyWatchPath detects the deprecated watch path form the apiserver
// still honors: /api/v1/watch/... and /apis/<group>/<version>/watch/....
// The check is positional (segment 3 of /api, segment 4 of /apis) so a
// resource or object legitimately named "watch" elsewhere in the path is
// not caught.
func legacyWatchPath(p string) bool {
	segs := strings.Split(p[1:], "/")
	switch {
	case len(segs) >= 3 && segs[0] == "api" && strings.EqualFold(segs[2], "watch"):
		return true
	case len(segs) >= 4 && segs[0] == "apis" && strings.EqualFold(segs[3], "watch"):
		return true
	}
	return false
}

// requestsUpgrade implements rule 8: any Upgrade header, or a Connection
// header listing the upgrade token (case-insensitive).
func requestsUpgrade(hd http.Header) bool {
	if hd.Get("Upgrade") != "" {
		return true
	}
	for _, v := range hd.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// ListenLoopback enforces the shim's loopback-only invariant at bind time:
// the kube proxy path must exist solely behind the tunnel, never on a pod
// or cluster IP. Hostnames other than "localhost" are refused outright —
// no DNS lookup gets a say in where the shim listens.
func ListenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("shim: listen address %q is not host:port: %w", addr, err)
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil {
			return nil, fmt.Errorf("shim: listen host %q must be localhost or a literal loopback IP", host)
		}
		if !ip.IsLoopback() {
			return nil, fmt.Errorf("shim: refusing non-loopback listen address %q; the shim must only be reachable through the tunnel", addr)
		}
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("shim: listening on %q: %w", addr, err)
	}
	// Re-verify the bound address: "localhost" must actually have resolved
	// to a loopback IP.
	if ta, ok := l.Addr().(*net.TCPAddr); ok && !ta.IP.IsLoopback() {
		_ = l.Close()
		return nil, fmt.Errorf("shim: %q bound non-loopback %s; refusing", addr, ta.IP)
	}
	return l, nil
}
