// Command fakeplatform is development tooling for polylane-k8s tests: an
// in-memory stand-in for the Polylane registration API ("serve") and for
// cloudflared's metrics endpoint ("tunnel-stub"), reused by the kind e2e
// suite. It is never shipped to customers.
//
// It deliberately does NOT import internal/platform: the wire protocol is
// implemented independently from the frozen COR-32 contract so that drift
// between agent and platform shows up in e2e instead of being defined away.
//
// The registry enforces the register-never-rotates invariant: re-POSTing a
// clusterUid returns the exact credentials minted on first contact.
// Credentials change only through the explicit /admin/rotate operation.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
)

const registerPath = "/v1/cloud_accounts/kubernetes/agent/register"

type cli struct {
	Serve      serveCmd      `cmd:"" help:"Run the fake registration API."`
	TunnelStub tunnelStubCmd `cmd:"" help:"Serve cloudflared's /ready endpoint so the kind e2e pod topology works without Cloudflare egress."`
}

type serveCmd struct {
	Listen string `help:"Listen address." default:":8180"`
	APIKey string `help:"API key registrations must present in x-api-key." default:"sk_test_123"`
}

func (c *serveCmd) Run(logger *slog.Logger) error {
	srv := newPlatformServer(c.APIKey, logger)
	logger.Info("fakeplatform registration api listening", "listen", c.Listen)
	return serveHTTP(c.Listen, srv.handler(), logger)
}

type tunnelStubCmd struct {
	Listen string `help:"Listen address." default:":2000"`
}

func (c *tunnelStubCmd) Run(logger *slog.Logger) error {
	logger.Info("fakeplatform tunnel stub listening", "listen", c.Listen)
	return serveHTTP(c.Listen, tunnelStubHandler(logger), logger)
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var c cli
	kctx := kong.Parse(&c,
		kong.Name("fakeplatform"),
		kong.Description("In-memory Polylane platform stand-in for tests. Development tooling only — never shipped."),
		kong.UsageOnError(),
	)
	kctx.FatalIfErrorf(kctx.Run(logger))
}

// registerRequest mirrors the frozen registration wire request. Kept
// independent of internal/platform on purpose (see package doc).
type registerRequest struct {
	ClusterUID        string `json:"clusterUid"`
	ClusterName       string `json:"clusterName"`
	KubernetesVersion string `json:"kubernetesVersion"`
	AgentVersion      string `json:"agentVersion"`
	Distribution      string `json:"distribution"`
	Namespace         string `json:"namespace"`
	ChartVersion      string `json:"chartVersion"`
}

// missingFields returns the required wire fields absent from the request.
// clusterName, distribution, and chartVersion are optional per the contract.
func (r registerRequest) missingFields() []string {
	var missing []string
	for _, f := range []struct{ name, val string }{
		{"clusterUid", r.ClusterUID},
		{"kubernetesVersion", r.KubernetesVersion},
		{"agentVersion", r.AgentVersion},
		{"namespace", r.Namespace},
	} {
		if f.val == "" {
			missing = append(missing, f.name)
		}
	}
	return missing
}

// registerResponse mirrors the frozen registration wire response.
type registerResponse struct {
	AccountID      string `json:"accountId"`
	AgentID        string `json:"agentId"`
	TunnelID       string `json:"tunnelId"`
	TunnelToken    string `json:"tunnelToken"`
	TunnelHostname string `json:"tunnelHostname"`
	ShimSecret     string `json:"shimSecret"`
	Object         string `json:"_object"`
}

type errorBody struct {
	Error string `json:"error"`
}

// registration is one cluster's registry entry. Credential fields are set
// once on the first successful register and only ever change via
// /admin/rotate; the metadata fields track the latest register call. The
// JSON shape is what GET /admin/registrations dumps for e2e assertions.
type registration struct {
	AccountID      string `json:"accountId"`
	AgentID        string `json:"agentId"`
	TunnelID       string `json:"tunnelId"`
	TunnelToken    string `json:"tunnelToken"`
	TunnelHostname string `json:"tunnelHostname"`
	ShimSecret     string `json:"shimSecret"`

	ClusterName       string `json:"clusterName"`
	KubernetesVersion string `json:"kubernetesVersion"`
	AgentVersion      string `json:"agentVersion"`
	Distribution      string `json:"distribution"`
	Namespace         string `json:"namespace"`
	ChartVersion      string `json:"chartVersion"`

	// RegisterCalls counts every authenticated, well-formed register POST
	// for this clusterUid — including injected failures — so e2e can prove
	// a warm boot made zero registration calls.
	RegisterCalls int `json:"registerCalls"`
}

func (reg *registration) response() registerResponse {
	return registerResponse{
		AccountID:      reg.AccountID,
		AgentID:        reg.AgentID,
		TunnelID:       reg.TunnelID,
		TunnelToken:    reg.TunnelToken,
		TunnelHostname: reg.TunnelHostname,
		ShimSecret:     reg.ShimSecret,
		Object:         "kubernetes_agent_registration",
	}
}

// platformServer is the in-memory registration API.
type platformServer struct {
	apiKey string
	logger *slog.Logger

	mu            sync.Mutex
	registrations map[string]*registration
	failMode      int // HTTP status injected while failCount > 0
	failCount     int // remaining register calls to fail
}

func newPlatformServer(apiKey string, logger *slog.Logger) *platformServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &platformServer{
		apiKey:        apiKey,
		logger:        logger,
		registrations: make(map[string]*registration),
	}
}

func (s *platformServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+registerPath, s.handleRegister)
	mux.HandleFunc("POST /admin/rotate", s.handleRotate)
	mux.HandleFunc("POST /admin/fail", s.handleFail)
	mux.HandleFunc("GET /admin/registrations", s.handleRegistrations)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return logRequests(mux, s.logger)
}

func (s *platformServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-api-key") != s.apiKey {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "invalid api key"})
		return
	}

	var req registerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "malformed json body: " + err.Error()})
		return
	}
	setClusterUID(r, req.ClusterUID)

	if missing := req.missingFields(); len(missing) > 0 {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing required fields: " + strings.Join(missing, ", ")})
		return
	}

	s.mu.Lock()
	reg, ok := s.registrations[req.ClusterUID]
	if !ok {
		reg = &registration{}
		s.registrations[req.ClusterUID] = reg
	}
	reg.RegisterCalls++

	if s.failCount > 0 {
		s.failCount--
		mode := s.failMode
		s.mu.Unlock()
		if mode == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		writeJSON(w, mode, errorBody{Error: fmt.Sprintf("injected %d failure", mode)})
		return
	}

	if reg.AccountID == "" {
		mintCredentials(reg, req.ClusterUID)
	}
	reg.ClusterName = req.ClusterName
	reg.KubernetesVersion = req.KubernetesVersion
	reg.AgentVersion = req.AgentVersion
	reg.Distribution = req.Distribution
	reg.Namespace = req.Namespace
	reg.ChartVersion = req.ChartVersion
	resp := reg.response()
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

// handleRotate is the explicit platform-side credential rotation: it
// replaces BOTH the tunnel token and the shim secret. Register never does.
func (s *platformServer) handleRotate(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("clusterUid")
	setClusterUID(r, uid)
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "clusterUid query parameter is required"})
		return
	}

	s.mu.Lock()
	reg, ok := s.registrations[uid]
	if !ok || reg.AccountID == "" {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, errorBody{Error: "unknown clusterUid"})
		return
	}
	reg.TunnelToken = mintTunnelToken(reg.AccountID, reg.TunnelID)
	reg.ShimSecret = mintShimSecret()
	resp := reg.response()
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

func (s *platformServer) handleFail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode, err := strconv.Atoi(q.Get("mode"))
	if err != nil || (mode != http.StatusInternalServerError &&
		mode != http.StatusTooManyRequests && mode != http.StatusUnauthorized) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "mode must be 500, 429, or 401"})
		return
	}
	count := 1
	if c := q.Get("count"); c != "" {
		count, err = strconv.Atoi(c)
		if err != nil || count < 0 {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "count must be a non-negative integer"})
			return
		}
	}

	s.mu.Lock()
	s.failMode = mode
	s.failCount = count
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]int{"mode": mode, "count": count})
}

// handleRegistrations dumps the registry keyed by clusterUid, including
// per-cluster register call counts (see registration.RegisterCalls).
func (s *platformServer) handleRegistrations(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	dump := make(map[string]registration, len(s.registrations))
	for uid, reg := range s.registrations {
		dump[uid] = *reg
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]map[string]registration{"registrations": dump})
}

func (s *platformServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// tunnelStubHandler mimics the one cloudflared endpoint the agent and the
// pod probes depend on. Everything else is 404 so accidental coupling to
// other cloudflared endpoints fails loudly in e2e.
func tunnelStubHandler(logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/ready" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		http.NotFound(w, r)
	}), logger)
}

// mintCredentials fills reg with identifiers derived deterministically from
// the clusterUid (so repeated e2e runs are reproducible) plus random
// per-registration credentials.
func mintCredentials(reg *registration, clusterUID string) {
	sum := sha256.Sum256([]byte(clusterUID))
	digest := hex.EncodeToString(sum[:])
	reg.AccountID = "acct_" + digest[:12]
	reg.AgentID = "agent_" + digest[12:24]
	reg.TunnelID = "tunnel_" + digest[24:36]
	reg.TunnelHostname = "k8s-" + reg.AccountID + ".fake.test"
	reg.TunnelToken = mintTunnelToken(reg.AccountID, reg.TunnelID)
	reg.ShimSecret = mintShimSecret()
}

// mintTunnelToken fakes cloudflared's token shape: base64 over a small JSON
// blob. The random secret component makes every mint distinct.
func mintTunnelToken(accountID, tunnelID string) string {
	blob, _ := json.Marshal(map[string]string{
		"a": accountID,
		"t": tunnelID,
		"s": base64.StdEncoding.EncodeToString(randomBytes(16)),
	})
	return base64.StdEncoding.EncodeToString(blob)
}

func mintShimSecret() string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(32))
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	// crypto/rand.Read never returns an error (it aborts the process if
	// the platform's randomness source is broken).
	_, _ = rand.Read(b)
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clusterUIDKey carries a pointer that handlers fill in so the access-log
// middleware can attach clusterUid to its one line per request.
type clusterUIDKey struct{}

func setClusterUID(r *http.Request, uid string) {
	if p, ok := r.Context().Value(clusterUIDKey{}).(*string); ok {
		*p = uid
	}
}

func logRequests(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := new(string)
		r = r.WithContext(context.WithValue(r.Context(), clusterUIDKey{}, uid))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"clusterUid", *uid,
			"status", rec.status,
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// serveHTTP runs handler on addr until the listener fails or the process
// receives SIGINT/SIGTERM, then drains connections briefly.
func serveHTTP(addr string, handler http.Handler, logger *slog.Logger) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return fmt.Errorf("fakeplatform: serving on %s: %w", addr, err)
	case <-ctx.Done():
		logger.Info("shutting down", "listen", addr)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
