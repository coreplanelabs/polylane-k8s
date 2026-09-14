// Package serviceproxy serves explicitly permitted HTTP API routes behind the
// shim's shared-secret authentication. It never receives Kubernetes credentials.
package serviceproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
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

	"github.com/coreplanelabs/polylane-k8s/internal/config"
	"github.com/coreplanelabs/polylane-k8s/internal/kube"
)

const (
	maxRequestBytes  = 1 << 20
	maxResponseBytes = 32 << 20
	requestTimeout   = 100 * time.Second
)

type Config struct {
	Services []config.Service
	Resolve  func(context.Context, string, string) (kube.Service, error)
	Logger   *slog.Logger
}

type target struct {
	service config.Service
	client  *http.Client
}

type Handler struct {
	cfg     Config
	targets map[string]target
}

func NewHandler(cfg Config) (*Handler, error) {
	if err := config.ValidateServices(cfg.Services); err != nil {
		return nil, err
	}
	if cfg.Resolve == nil {
		return nil, fmt.Errorf("serviceproxy: service resolver is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Services == nil {
		cfg.Services = []config.Service{}
	}
	h := &Handler{cfg: cfg, targets: make(map[string]target, len(cfg.Services))}
	for _, service := range cfg.Services {
		transport := &http.Transport{
			DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				ServerName: service.Name + "." + service.Namespace + ".svc",
			},
			TLSHandshakeTimeout:    10 * time.Second,
			ResponseHeaderTimeout:  30 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
			MaxIdleConns:           10,
			MaxConnsPerHost:        10,
			IdleConnTimeout:        90 * time.Second,
			DisableCompression:     true,
		}
		h.targets[service.ID] = target{service: service, client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}}
	}
	return h, nil
}

func (h *Handler) CloseIdleConnections() {
	for _, target := range h.targets {
		target.client.CloseIdleConnections()
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if hasUpgrade(r.Header) {
		deny(w, http.StatusBadRequest, "service_upgrade_rejected")
		return
	}
	decoded, ok := decodePath(r.URL.EscapedPath())
	if !ok {
		deny(w, http.StatusBadRequest, "service_bad_path")
		return
	}
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		deny(w, http.StatusBadRequest, "service_bad_query")
		return
	}
	if decoded == "/services" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			deny(w, http.StatusMethodNotAllowed, "service_method_not_allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(struct {
			Services []config.Service `json:"services"`
		}{Services: h.cfg.Services}); err != nil {
			h.cfg.Logger.Warn("writing service catalog", "error", err)
		}
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(decoded, "/services/"), "/", 2)
	if len(parts) != 2 || !strings.HasPrefix(decoded, "/services/") {
		deny(w, http.StatusNotFound, "service_not_found")
		return
	}
	t, ok := h.targets[parts[0]]
	if !ok {
		deny(w, http.StatusNotFound, "service_not_found")
		return
	}
	relativePath := "/" + parts[1]
	if !allowed(t.service.Routes, r.Method, relativePath) {
		deny(w, http.StatusForbidden, "service_route_forbidden")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(requestTimeout)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	defer func() {
		_ = controller.SetReadDeadline(time.Time{})
		_ = controller.SetWriteDeadline(time.Time{})
	}()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		deny(w, http.StatusBadRequest, "service_bad_body")
		return
	}
	if len(body) > maxRequestBytes {
		deny(w, http.StatusRequestEntityTooLarge, "service_body_too_large")
		return
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && len(body) != 0 {
		deny(w, http.StatusBadRequest, "service_bad_body")
		return
	}

	service, err := h.cfg.Resolve(ctx, t.service.Namespace, t.service.Name)
	if err != nil {
		h.cfg.Logger.Warn("resolving service", "service", t.service.ID, "error", err)
		deny(w, http.StatusBadGateway, "service_unavailable")
		return
	}
	ip, ok := destination(service, t.service.Port)
	if !ok {
		deny(w, http.StatusForbidden, "service_destination_forbidden")
		return
	}
	out := url.URL{
		Scheme:   t.service.Scheme,
		Host:     net.JoinHostPort(ip.String(), strconv.Itoa(t.service.Port)),
		Path:     t.service.BasePath + relativePath,
		RawQuery: r.URL.RawQuery,
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, out.String(), bytes.NewReader(body))
	if err != nil {
		deny(w, http.StatusBadRequest, "service_bad_request")
		return
	}
	req.Host = net.JoinHostPort(t.service.Name+"."+t.service.Namespace+".svc", strconv.Itoa(t.service.Port))
	copyHeaders(req.Header, r.Header, "Authorization", "Accept", "Content-Type", "Content-Encoding")
	req.Header.Set("User-Agent", "polylane-k8s/service-proxy")
	response, err := t.client.Do(req) // #nosec G704 -- Host is the validated ClusterIP and configured Service port; redirects and environment proxies are disabled.
	if err != nil {
		h.cfg.Logger.Warn("requesting service", "service", t.service.ID, "error", err)
		deny(w, http.StatusBadGateway, "service_unavailable")
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		deny(w, http.StatusBadGateway, "service_redirect_rejected")
		return
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		deny(w, http.StatusBadGateway, "service_upgrade_rejected")
		return
	}
	if response.ContentLength > maxResponseBytes {
		deny(w, http.StatusBadGateway, "service_response_too_large")
		return
	}
	copyHeaders(w.Header(), response.Header, "Content-Type", "Content-Encoding", "Retry-After")
	w.Header().Set("Cache-Control", "no-store")
	if response.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
	}
	w.WriteHeader(response.StatusCode)
	n, copyErr := io.Copy(w, io.LimitReader(response.Body, maxResponseBytes))
	if copyErr != nil {
		h.cfg.Logger.Warn("relaying service response", "service", t.service.ID, "error", copyErr)
		panic(http.ErrAbortHandler)
	}
	if n == maxResponseBytes {
		var extra [1]byte
		if count, err := response.Body.Read(extra[:]); count != 0 || (err != nil && err != io.EOF) {
			panic(http.ErrAbortHandler)
		}
	}
}

func destination(service kube.Service, port int) (net.IP, bool) {
	if service.Spec.Type != "ClusterIP" || len(service.Spec.Selector) == 0 {
		return nil, false
	}
	ip := net.ParseIP(service.Spec.ClusterIP)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return nil, false
	}
	for _, p := range service.Spec.Ports {
		if p.Port == port && p.Protocol == "TCP" {
			return ip, true
		}
	}
	return nil, false
}

func allowed(routes []config.ServiceRoute, method, p string) bool {
	for _, route := range routes {
		if route.Method == method && (route.Path == p || (route.PathPrefix != "" && strings.HasPrefix(p, route.PathPrefix))) {
			return true
		}
	}
	return false
}

func decodePath(raw string) (string, bool) {
	p, err := url.PathUnescape(raw)
	if err != nil || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "%\\\x00\r\n\t") || strings.Contains(strings.ToLower(raw), "%2f") {
		return "", false
	}
	if p != "/" && path.Clean(p) != strings.TrimSuffix(p, "/") {
		return "", false
	}
	return p, true
}

func hasUpgrade(header http.Header) bool {
	if header.Get("Upgrade") != "" {
		return true
	}
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func copyHeaders(to, from http.Header, names ...string) {
	hop := make(map[string]bool)
	for _, value := range from.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			hop[http.CanonicalHeaderKey(strings.TrimSpace(token))] = true
		}
	}
	for _, name := range names {
		if !hop[name] {
			for _, value := range from.Values(name) {
				to.Add(name, value)
			}
		}
	}
}

func deny(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, code)
}
