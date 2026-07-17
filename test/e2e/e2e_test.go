//go:build e2e

// The hermetic kind-based end-to-end suite: it builds the real agent and
// fakeplatform images, installs the real chart (with the fakeplatform's
// tunnel-stub standing in for cloudflared so nothing leaves the cluster),
// and asserts the COR-32 behaviors that only a live cluster can prove:
// RBAC boundaries via `kubectl auth can-i`, shim policy through a
// port-forward to the pod's loopback, native-sidecar startup ordering,
// and the warm-boot-makes-zero-platform-calls invariant across a pod
// delete.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	clusterName = "polylane-e2e"
	kctx        = "kind-" + clusterName
	namespace   = "polylane-e2e"
	release     = "polylane-k8s" // == fullname, so the state Secret is polylane-k8s-state
	// A fake credential for the hermetic fakeplatform.
	apiKey     = "sk_test_e2e" // #nosec G101 -- not a real credential
	agentImage = "polylane-k8s-e2e:e2e"
	fakeImage  = "fakeplatform-e2e:e2e"
)

// canI wraps `kubectl auth can-i` for the agent's ServiceAccount.
func canI(t *testing.T, verb, resource string, extra ...string) bool {
	t.Helper()
	args := []string{"auth", "can-i", verb, resource,
		"--as", "system:serviceaccount:" + namespace + ":" + release,
		"-n", namespace}
	args = append(args, extra...)
	out, err := kubectlErr(t, kctx, args...)
	switch strings.TrimSpace(strings.Split(out, "\n")[0]) {
	case "yes":
		return true
	case "no":
		return false
	default:
		t.Fatalf("kubectl auth can-i %s %s: unexpected output %q (err %v)", verb, resource, out, err)
		return false
	}
}

// registrations fetches the fakeplatform's registry dump through a
// port-forward to its service.
func registrations(t *testing.T) map[string]struct {
	RegisterCalls int `json:"registerCalls"`
} {
	t.Helper()
	addr := portForward(t, kctx, namespace, "svc/fakeplatform", 8180)
	status, body := httpGet(t, "http://"+addr+"/admin/registrations", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /admin/registrations = %d: %s", status, body)
	}
	var out struct {
		Registrations map[string]struct {
			RegisterCalls int `json:"registerCalls"`
		} `json:"registrations"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("parsing registrations: %v\n%s", err, body)
	}
	return out.Registrations
}

func totalRegisterCalls(t *testing.T) int {
	t.Helper()
	total := 0
	for _, r := range registrations(t) {
		total += r.RegisterCalls
	}
	return total
}

func TestE2E(t *testing.T) {
	requireBinaries(t)

	// ---- cluster + images -------------------------------------------------
	recreateKindCluster(t, clusterName)

	run(t, "docker", "build", "-f", "containers/polylane-k8s.Dockerfile", "-t", agentImage, ".")
	run(t, "docker", "build", "-f", "containers/fakeplatform.Dockerfile", "-t", fakeImage, ".")
	run(t, "kind", "load", "docker-image", agentImage, "--name", clusterName)
	run(t, "kind", "load", "docker-image", fakeImage, "--name", clusterName)

	// ---- fakeplatform -----------------------------------------------------
	kubectl(t, kctx, "create", "namespace", namespace)
	fakeManifest := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fakeplatform
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels: {app: fakeplatform}
  template:
    metadata:
      labels: {app: fakeplatform}
    spec:
      containers:
        - name: fakeplatform
          image: %[2]s
          imagePullPolicy: Never
          args: ["serve", "--listen", ":8180", "--api-key", "%[3]s"]
          ports: [{containerPort: 8180}]
          readinessProbe:
            httpGet: {path: /healthz, port: 8180}
---
apiVersion: v1
kind: Service
metadata:
  name: fakeplatform
  namespace: %[1]s
spec:
  selector: {app: fakeplatform}
  ports: [{port: 8180, targetPort: 8180}]
`, namespace, fakeImage, apiKey)
	applyStdin(t, kctx, fakeManifest)
	kubectl(t, kctx, "-n", namespace, "rollout", "status", "deployment/fakeplatform", "--timeout=180s")

	// ---- the real chart ---------------------------------------------------
	kubectl(t, kctx, "-n", namespace, "create", "secret", "generic", "polylane-api-key",
		"--from-literal=api-key="+apiKey)
	run(t, "helm", "--kube-context", kctx, "install", release, "charts/polylane-k8s",
		"--namespace", namespace,
		"--set", "apiKey.existingSecret=polylane-api-key",
		"--set", "image.repository="+strings.Split(agentImage, ":")[0],
		"--set", "image.tag="+strings.Split(agentImage, ":")[1],
		"--set", "image.pullPolicy=Never",
		"--set", "tunnel.image.repository="+strings.Split(fakeImage, ":")[0],
		"--set", "tunnel.image.tag="+strings.Split(fakeImage, ":")[1],
		"--set", "tunnel.image.digest=",
		"--set", "tunnel.command={/fakeplatform}",
		"--set", "tunnel.args={tunnel-stub,--listen,:2000}",
		"--set", "config.platform_url=http://fakeplatform:8180",
		"--set", "config.cluster_name=kind-e2e",
	)
	waitRollout(t, kctx, namespace, release)

	// ---- assertions -------------------------------------------------------
	t.Run("RegistrationHappened", func(t *testing.T) {
		regs := registrations(t)
		if len(regs) != 1 {
			t.Fatalf("registrations = %v, want exactly one cluster", regs)
		}
		for uid, r := range regs {
			if uid == "" {
				t.Error("empty clusterUid key")
			}
			if r.RegisterCalls != 1 {
				t.Errorf("registerCalls = %d, want 1", r.RegisterCalls)
			}
		}
	})

	t.Run("StateSecretPersisted", func(t *testing.T) {
		out := kubectl(t, kctx, "-n", namespace, "get", "secret", release+"-state", "-o", "json")
		var secret struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &secret); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"tunnelToken", "shimSecret", "accountId", "tunnelId", "tunnelHostname", "registeredAt"} {
			if secret.Data[key] == "" {
				t.Errorf("state secret missing %s", key)
			}
		}
	})

	t.Run("StartupOrdering", func(t *testing.T) {
		inits, mains := podStatuses(t, kctx, namespace, podOf(t, kctx, namespace, release))
		agentStart := startedAt(t, inits, "agent")
		tunnelStart := startedAt(t, mains, "cloudflared")
		if tunnelStart.Before(agentStart) {
			t.Errorf("tunnel container started at %s, before the agent sidecar at %s", tunnelStart, agentStart)
		}
	})

	t.Run("RBACBoundaries", func(t *testing.T) {
		checks := []struct {
			verb, resource string
			extra          []string
			want           bool
		}{
			{"get", "pods", nil, true},
			{"list", "deployments.apps", nil, true},
			{"get", "pods/log", nil, true},
			{"list", "nodes", nil, true},
			{"watch", "pods", nil, false},  // no watch verb anywhere
			{"get", "secrets", nil, false}, // secrets deliberately absent
			{"list", "secrets", nil, false},
			{"create", "pods/exec", nil, false},
			{"create", "pods", nil, false},
			{"delete", "pods", nil, false},
			{"update", "secrets", []string{"--resource-name", release + "-state"}, true}, // the one pinned write
			{"update", "secrets", []string{"--resource-name", "polylane-api-key"}, false},
			{"create", "secrets", nil, false},
		}
		for _, c := range checks {
			if got := canI(t, c.verb, c.resource, c.extra...); got != c.want {
				t.Errorf("can-i %s %s %v = %v, want %v", c.verb, c.resource, c.extra, got, c.want)
			}
		}
	})

	t.Run("ShimConformance", func(t *testing.T) {
		shimKey := decodeSecretKey(t, kctx, namespace, release+"-state", "shimSecret")
		// pod loopback: exactly where the shim hides
		addr := portForward(t, kctx, namespace, "pod/"+podOf(t, kctx, namespace, release), 8080)
		base := "http://" + addr
		withKey := map[string]string{"x-polylane-shim-key": shimKey}

		if status, body := httpGet(t, base+"/version", nil); status != http.StatusUnauthorized {
			t.Errorf("no key /version = %d (%s), want 401", status, body)
		}
		if status, _ := httpGet(t, base+"/version", map[string]string{"x-polylane-shim-key": "wrong"}); status != http.StatusUnauthorized {
			t.Errorf("wrong key = %d, want 401", status)
		}
		if status, body := httpGet(t, base+"/healthz", withKey); status != http.StatusOK || !strings.Contains(body, "kubeReachable") {
			t.Errorf("shim /healthz = %d (%s), want 200 with kubeReachable", status, body)
		}
		if status, body := httpGet(t, base+"/version", withKey); status != http.StatusOK || !strings.Contains(body, "gitVersion") {
			t.Errorf("GET /version = %d (%s), want 200 with gitVersion", status, body)
		}
		if status, body := httpGet(t, base+"/api/v1/namespaces/kube-system/pods?limit=5", withKey); status != http.StatusOK || !strings.Contains(body, `"kind":"PodList"`) {
			t.Errorf("list pods = %d, want 200 PodList (%s)", status, truncate(body))
		}
		if status, _ := httpDo(t, http.MethodPost, base+"/api/v1/namespaces/kube-system/pods", withKey); status != http.StatusMethodNotAllowed {
			t.Errorf("POST = %d, want 405", status)
		}
		if status, body := httpGet(t, base+"/api/v1/namespaces/kube-system/secrets", withKey); status != http.StatusForbidden || !strings.Contains(body, "forbidden_secrets") {
			t.Errorf("secrets path = %d (%s), want 403 forbidden_secrets", status, body)
		}
		if status, body := httpGet(t, base+"/api/v1/pods?watch=true", withKey); status != http.StatusBadRequest || !strings.Contains(body, "bad_query") {
			t.Errorf("watch query = %d (%s), want 400 bad_query", status, body)
		}
		if status, body := httpGet(t, base+"/api/v1/namespaces/kube-system/pods/x/exec", withKey); status != http.StatusForbidden || !strings.Contains(body, "forbidden_subresource") {
			t.Errorf("exec subresource = %d (%s), want 403 forbidden_subresource", status, body)
		}
		if status, _ := httpGet(t, base+"/openapi/v2", withKey); status != http.StatusForbidden {
			t.Errorf("/openapi/v2 = %d, want 403 (outside allowed roots)", status)
		}
	})

	t.Run("WarmBootMakesZeroPlatformCalls", func(t *testing.T) {
		before := totalRegisterCalls(t)
		kubectl(t, kctx, "-n", namespace, "delete", "pod", podOf(t, kctx, namespace, release), "--wait=true")
		waitRollout(t, kctx, namespace, release)
		// Readiness of the new pod proves the agent fully re-initialized.
		if after := totalRegisterCalls(t); after != before {
			t.Errorf("register calls went %d -> %d across a pod restart; warm boot must make ZERO platform calls", before, after)
		}
	})
}

// Helpers used only by the hermetic suite.

func kubectlErr(t *testing.T, kctx string, args ...string) (string, error) {
	t.Helper()
	return runErr(t, "kubectl", append([]string{"--context", kctx}, args...)...)
}

func applyStdin(t *testing.T, kctx, manifest string) {
	t.Helper()
	// #nosec G204 G702 -- test harness, fixed binary with test-controlled args
	cmd := exec.Command("kubectl", "--context", kctx, "apply", "-f", "-")
	cmd.Dir = repoRoot
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply: %v\n%s", err, out)
	}
}

type containerStatus struct {
	Name  string `json:"name"`
	State struct {
		Running *struct {
			StartedAt time.Time `json:"startedAt"`
		} `json:"running"`
	} `json:"state"`
}

func startedAt(t *testing.T, statuses []containerStatus, name string) time.Time {
	t.Helper()
	for _, s := range statuses {
		if s.Name == name {
			if s.State.Running == nil {
				t.Fatalf("container %s is not running", name)
			}
			return s.State.Running.StartedAt
		}
	}
	t.Fatalf("container %s not found", name)
	return time.Time{}
}

func podStatuses(t *testing.T, kctx, ns, pod string) (inits, mains []containerStatus) {
	t.Helper()
	out := kubectl(t, kctx, ns2args(ns, "get", "pod", pod, "-o", "json")...)
	var parsed struct {
		Status struct {
			InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
			ContainerStatuses     []containerStatus `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed.Status.InitContainerStatuses, parsed.Status.ContainerStatuses
}

func ns2args(ns string, args ...string) []string {
	return append([]string{"-n", ns}, args...)
}
