//go:build cfe2e

// The manual real-Cloudflare UAT suite (task e2e:cf, COR-32 M4): a kind
// cluster running the REAL chart with the REAL cloudflared sidecar,
// registered against a REAL platform (UAT). It validates what the
// hermetic suite cannot: the official cloudflared image consumes the
// provisioned tunnel token, connects to Cloudflare's edge, and the shim
// is reachable end-to-end THROUGH the tunnel hostname — including the
// COR-32 §6 open question of whether Access enforcement holds.
//
// Deliberately manual: it creates real tunnel/DNS state in the Coreplane
// Cloudflare account and a real cloud account in the target workspace.
// Clean up with a console disconnect afterwards. Rotation-loop validation
// requires a platform operator to trigger the rotate op — follow
// "Rotation-loop validation" in DEVELOPMENT.md while this suite's
// cluster is still running.
package e2e

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	cfClusterName = "polylane-cf-e2e"
	cfKctx        = "kind-" + cfClusterName
	cfNamespace   = "polylane-cf-e2e"
	cfRelease     = "polylane-k8s"
	cfAgentImage  = "polylane-k8s-e2e:cfe2e"
)

func TestRealCloudflareTunnel(t *testing.T) {
	requireBinaries(t)
	platformURL := os.Getenv("POLYLANE_E2E_PLATFORM_URL")
	apiKey := os.Getenv("POLYLANE_E2E_API_KEY")
	if platformURL == "" || apiKey == "" {
		t.Fatal("set POLYLANE_E2E_PLATFORM_URL (UAT gateway) and POLYLANE_E2E_API_KEY (cloud_accounts:write key) — see DEVELOPMENT.md")
	}

	recreateKindCluster(t, cfClusterName)
	run(t, "docker", "build", "-f", "containers/polylane-k8s.Dockerfile", "-t", cfAgentImage, ".")
	run(t, "kind", "load", "docker-image", cfAgentImage, "--name", cfClusterName)

	kubectl(t, cfKctx, "create", "namespace", cfNamespace)
	kubectl(t, cfKctx, "-n", cfNamespace, "create", "secret", "generic", "polylane-api-key",
		"--from-literal=api-key="+apiKey)
	run(t, "helm", "--kube-context", cfKctx, "install", cfRelease, "charts/polylane-k8s",
		"--namespace", cfNamespace,
		"--set", "apiKey.existingSecret=polylane-api-key",
		"--set", "image.repository="+strings.Split(cfAgentImage, ":")[0],
		"--set", "image.tag="+strings.Split(cfAgentImage, ":")[1],
		"--set", "image.pullPolicy=Never",
		"--set", "config.platform_url="+platformURL,
		"--set", "config.cluster_name=cf-e2e-uat",
	)
	// Registration + real edge connection can take a while on first boot.
	waitRollout(t, cfKctx, cfNamespace, cfRelease)

	// The pod being Ready already implies cloudflared's /ready saw an edge
	// connection; make it explicit through the agent's own readyz.
	t.Run("AgentReady", func(t *testing.T) {
		addr := portForward(t, cfKctx, cfNamespace, "pod/"+podOf(t, cfKctx, cfNamespace, cfRelease), 8081)
		deadline := time.Now().Add(2 * time.Minute)
		for {
			status, body := httpGet(t, "http://"+addr+"/readyz", nil)
			if status == http.StatusOK && strings.Contains(body, `"tunnel":true`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("agent never became ready with a live tunnel: %d %s", status, body)
			}
			time.Sleep(3 * time.Second)
		}
	})

	hostname := decodeSecretKey(t, cfKctx, cfNamespace, cfRelease+"-state", "tunnelHostname")
	shimKey := decodeSecretKey(t, cfKctx, cfNamespace, cfRelease+"-state", "shimSecret")
	base := "https://" + hostname
	withKey := map[string]string{"x-polylane-shim-key": shimKey}

	// End-to-end through Cloudflare's edge: DNS -> tunnel -> shim.
	// NOTE on Access (COR-32 §6.1): from outside Cloudflare, an Access
	// Service Auth policy on this hostname would answer 403 without
	// CF-Access-Client-Id/Secret. Record which of the two branches you
	// observe — that is the empirical answer UAT wants.
	t.Run("ThroughTheTunnel", func(t *testing.T) {
		deadline := time.Now().Add(3 * time.Minute) // DNS propagation
		var status int
		var body string
		for {
			status, body = httpGet(t, base+"/healthz", withKey)
			if status != 0 && (status < 500 || time.Now().After(deadline)) {
				break
			}
			time.Sleep(5 * time.Second)
		}
		switch status {
		case http.StatusOK:
			if !strings.Contains(body, "kubeReachable") {
				t.Errorf("tunnel /healthz body = %s", truncate(body))
			}
			t.Log("Access verdict: same-zone requests reach the shim WITHOUT service tokens — shim secret is the effective gate (expected per design; record in COR-32 §6.1)")
		case http.StatusForbidden:
			t.Log("Access verdict: Cloudflare Access blocks external requests without service tokens (belt-and-suspenders holds; record in COR-32 §6.1)")
		default:
			t.Fatalf("GET %s/healthz = %d (%s)", base, status, truncate(body))
		}

		// Whichever way Access answered, the shim's own gates must hold.
		if status == http.StatusOK {
			if s, _ := httpGet(t, base+"/version", nil); s != http.StatusUnauthorized {
				t.Errorf("no shim key through tunnel = %d, want 401", s)
			}
			if s, _ := httpDo(t, http.MethodPost, base+"/api/v1/namespaces/default/pods", withKey); s != http.StatusMethodNotAllowed {
				t.Errorf("POST through tunnel = %d, want 405", s)
			}
			if s, _ := httpGet(t, base+"/api/v1/namespaces/default/secrets", withKey); s != http.StatusForbidden {
				t.Errorf("secrets through tunnel = %d, want 403", s)
			}
		}
	})

	t.Log("UAT checklist continues in the console: cluster shows Live, sync populates the infra graph, kubectl read tools work, writes denied.")
	t.Log("Remember: helm uninstall leaves the cloud account; disconnect it in the console to tear down the tunnel + DNS record.")
}
