package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreplanelabs/polylane-k8s/internal/buildinfo"
)

// Mirror of shim.MetricsRecorder (the shim package must not be imported
// here); proves *Metrics satisfies its shape.
type metricsRecorder interface {
	RecordRequest(outcome string, duration time.Duration)
}

var _ metricsRecorder = (*Metrics)(nil)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("scrape = %d, want 200", rr.Code)
	}
	return rr.Body.String()
}

func TestExpositionContainsAllInstruments(t *testing.T) {
	t.Parallel()
	m := New()
	m.SetTunnelReady(true)
	m.IncTunnelReconnects()
	m.RecordRequest("proxied", 42*time.Millisecond)
	m.IncRegistration("success")

	body := scrape(t, m)
	want := []string{
		`polylane_k8s_build_info{version="` + buildinfo.Version + `"} 1`,
		"polylane_k8s_tunnel_ready 1",
		"polylane_k8s_tunnel_reconnects_total 1",
		`polylane_k8s_shim_requests_total{outcome="proxied"} 1`,
		`polylane_k8s_kube_request_duration_seconds_count{outcome="proxied"} 1`,
		`polylane_k8s_registrations_total{result="success"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("exposition missing %q\n%s", w, body)
		}
	}
}

func TestTunnelReadyGaugeToggles(t *testing.T) {
	t.Parallel()
	m := New()
	m.SetTunnelReady(true)
	m.SetTunnelReady(false)
	if body := scrape(t, m); !strings.Contains(body, "polylane_k8s_tunnel_ready 0") {
		t.Error("gauge did not return to 0")
	}
}

func TestRecordRequestObservesDurationOnlyForKubeOutcomes(t *testing.T) {
	t.Parallel()
	kubeOutcomes := []string{"proxied", "kube_unreachable", "upstream_error"}
	localOutcomes := []string{"unauthorized", "healthz", "forbidden_secrets", "bad_query"}

	m := New()
	for _, o := range kubeOutcomes {
		m.RecordRequest(o, 10*time.Millisecond)
	}
	for _, o := range localOutcomes {
		m.RecordRequest(o, 10*time.Millisecond)
	}

	body := scrape(t, m)
	for _, o := range kubeOutcomes {
		if !strings.Contains(body, `polylane_k8s_kube_request_duration_seconds_count{outcome="`+o+`"} 1`) {
			t.Errorf("histogram missing observation for %q", o)
		}
	}
	for _, o := range localOutcomes {
		if !strings.Contains(body, `polylane_k8s_shim_requests_total{outcome="`+o+`"} 1`) {
			t.Errorf("counter missing outcome %q", o)
		}
		if strings.Contains(body, `polylane_k8s_kube_request_duration_seconds_count{outcome="`+o+`"}`) {
			t.Errorf("histogram observed local outcome %q; it must track kube round-trips only", o)
		}
	}
}

func TestRegistrationResults(t *testing.T) {
	t.Parallel()
	m := New()
	m.IncRegistration("success")
	m.IncRegistration("terminal")
	m.IncRegistration("transient_error")
	m.IncRegistration("transient_error")

	body := scrape(t, m)
	want := []string{
		`polylane_k8s_registrations_total{result="success"} 1`,
		`polylane_k8s_registrations_total{result="terminal"} 1`,
		`polylane_k8s_registrations_total{result="transient_error"} 2`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("exposition missing %q", w)
		}
	}
}

func TestOwnRegistryIsolation(t *testing.T) {
	t.Parallel()
	// Two instances must not collide — proof New does not touch the
	// default registry (double-registration there would panic).
	m1, m2 := New(), New()
	m1.IncTunnelReconnects()
	if body := scrape(t, m2); strings.Contains(body, "polylane_k8s_tunnel_reconnects_total 1") {
		t.Error("instances share a registry")
	}
	if body := scrape(t, m1); !strings.Contains(body, "polylane_k8s_tunnel_reconnects_total 1") {
		t.Error("increment lost")
	}
}
