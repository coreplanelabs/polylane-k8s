// Package metrics defines the agent's Prometheus instruments on a
// dedicated registry, so the /metrics surface is exactly the instruments
// declared here — no default-registry leakage from dependencies.
//
// RecordRequest deliberately matches the shim's MetricsRecorder interface
// shape without importing internal/shim.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/coreplanelabs/polylane-k8s/internal/buildinfo"
)

// Metrics owns the agent's instruments and their registry.
type Metrics struct {
	registry *prometheus.Registry

	tunnelReady      prometheus.Gauge
	tunnelReconnects prometheus.Counter
	shimRequests     *prometheus.CounterVec
	kubeDuration     *prometheus.HistogramVec
	registrations    *prometheus.CounterVec
}

// New builds the instrument set on a fresh registry and stamps
// build_info from the compiled-in version.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		tunnelReady: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "polylane_k8s_tunnel_ready",
			Help: "Whether cloudflared reports the tunnel ready (1) or not (0).",
		}),
		tunnelReconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "polylane_k8s_tunnel_reconnects_total",
			Help: "Tunnel not-ready to ready transitions after the first connect.",
		}),
		shimRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "polylane_k8s_shim_requests_total",
			Help: "Shim requests by policy outcome.",
		}, []string{"outcome"}),
		kubeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "polylane_k8s_kube_request_duration_seconds",
			Help:    "Latency of proxied kube API round-trips by outcome.",
			Buckets: prometheus.DefBuckets,
		}, []string{"outcome"}),
		registrations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "polylane_k8s_registrations_total",
			Help: "Platform registration attempts by result.",
		}, []string{"result"}),
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polylane_k8s_build_info",
		Help: "Build metadata; constant 1 labeled with the agent version.",
	}, []string{"version"})
	buildInfo.WithLabelValues(buildinfo.Version).Set(1)

	m.registry.MustRegister(
		buildInfo,
		m.tunnelReady,
		m.tunnelReconnects,
		m.shimRequests,
		m.kubeDuration,
		m.registrations,
	)
	return m
}

// Handler serves the exposition for this registry only.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// SetTunnelReady mirrors the tunnel monitor's readiness snapshot.
func (m *Metrics) SetTunnelReady(ready bool) {
	if ready {
		m.tunnelReady.Set(1)
		return
	}
	m.tunnelReady.Set(0)
}

// IncTunnelReconnects counts a tunnel reconnect.
func (m *Metrics) IncTunnelReconnects() {
	m.tunnelReconnects.Inc()
}

// RecordRequest counts a shim request by outcome. Only outcomes that
// involve an upstream kube round-trip carry a meaningful latency, so the
// duration histogram is limited to those.
func (m *Metrics) RecordRequest(outcome string, d time.Duration) {
	m.shimRequests.WithLabelValues(outcome).Inc()
	switch outcome {
	case "proxied", "kube_unreachable", "upstream_error":
		m.kubeDuration.WithLabelValues(outcome).Observe(d.Seconds())
	}
}

// IncRegistration counts a registration attempt by result
// (success|terminal|transient_error).
func (m *Metrics) IncRegistration(result string) {
	m.registrations.WithLabelValues(result).Inc()
}
