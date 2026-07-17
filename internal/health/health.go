// Package health serves the ops endpoints the kubelet probes.
//
// Invariant: /healthz answers 200 unconditionally once the server is up.
// Liveness must NOT depend on the tunnel or the platform, so a
// Cloudflare or Polylane outage can never crash-loop customer pods —
// degraded state surfaces through /readyz instead. /startupz is a
// one-way latch the agent flips once registration state is in hand.
package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"sync/atomic"
)

// Sources supplies the live inputs for /readyz. A nil func reads as
// false, so a partially wired server fails closed on readiness.
type Sources struct {
	CredentialsPresent func() bool
	TunnelReady        func() bool
	KubeReachable      func() bool
	Version            string
}

// Server exposes the probe endpoints. It is an http.Handler factory,
// not a listener — the agent owns the http.Server and its timeouts.
type Server struct {
	sources Sources
	logger  *slog.Logger
	started atomic.Bool
	mux     *http.ServeMux
}

// NewServer builds the endpoint set. A nil logger means slog.Default().
func NewServer(sources Sources, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{sources: sources, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /startupz", s.handleStartupz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /version", s.handleVersion)
	s.mux = mux
	return s
}

// Handler returns the routing handler for the ops listener.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// SetStartupReady flips the one-way /startupz latch. Safe to call more
// than once; it never un-latches.
func (s *Server) SetStartupReady() {
	s.started.Store(true)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStartupz(w http.ResponseWriter, _ *http.Request) {
	if !s.started.Load() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readiness is the /readyz body, sent on both 200 and 503 so operators
// always see which dependency is the problem.
type readiness struct {
	Ready       bool   `json:"ready"`
	Credentials bool   `json:"credentials"`
	Tunnel      bool   `json:"tunnel"`
	Kube        bool   `json:"kube"`
	Version     string `json:"version"`
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	body := readiness{
		Credentials: call(s.sources.CredentialsPresent),
		Tunnel:      call(s.sources.TunnelReady),
		Kube:        call(s.sources.KubeReachable),
		Version:     s.sources.Version,
	}
	body.Ready = body.Credentials && body.Tunnel && body.Kube
	code := http.StatusOK
	if !body.Ready {
		code = http.StatusServiceUnavailable
	}
	s.writeJSON(w, code, body)
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{
		"version":   s.sources.Version,
		"goVersion": runtime.Version(),
		"platform":  runtime.GOOS + "/" + runtime.GOARCH,
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Error("health: encoding response", "error", err)
	}
}

func call(f func() bool) bool {
	return f != nil && f()
}
