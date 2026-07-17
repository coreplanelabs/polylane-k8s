//go:build e2e || cfe2e

// Shared harness for the kind-based suites: the hermetic e2e suite (tag
// e2e) and the manual real-Cloudflare UAT suite (tag cfe2e).
package e2e

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var repoRoot string

func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	repoRoot = filepath.Dir(filepath.Dir(wd)) // test/e2e -> repo root
	// The Dockerfiles use BuildKit features ($BUILDPLATFORM); daemons that
	// still default to the legacy builder (e.g. colima) need the nudge.
	os.Setenv("DOCKER_BUILDKIT", "1") //nolint:errcheck // best-effort
	os.Exit(m.Run())
}

// run executes a command, failing the test on error.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := runErr(t, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func runErr(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...) // #nosec G204 G702 -- test harness runs fixed dev tools
	cmd.Dir = repoRoot
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// kubectl runs kubectl against the given kind context.
func kubectl(t *testing.T, kctx string, args ...string) string {
	t.Helper()
	return run(t, "kubectl", append([]string{"--context", kctx}, args...)...)
}

// portForward starts a kubectl port-forward and returns the local address
// once it accepts connections.
func portForward(t *testing.T, kctx, ns, target string, remotePort int) string {
	t.Helper()
	// Ask the kernel for a free port, release it, and hand it to kubectl.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrTCP, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type %T", ln.Addr())
	}
	local := addrTCP.Port
	_ = ln.Close()

	// #nosec G204 G702 -- test harness, fixed binary with test-controlled args
	cmd := exec.Command("kubectl", "--context", kctx, "-n", ns,
		"port-forward", target, fmt.Sprintf("%d:%d", local, remotePort))
	cmd.Dir = repoRoot
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	addr := fmt.Sprintf("127.0.0.1:%d", local)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return addr
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("port-forward %s never accepted connections\n%s", target, buf.String())
	return ""
}

func httpGet(t *testing.T, url string, header map[string]string) (int, string) {
	t.Helper()
	return httpDo(t, http.MethodGet, url, header)
}

func httpDo(t *testing.T, method, url string, header map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body)
}

func waitRollout(t *testing.T, kctx, ns, deployment string) {
	t.Helper()
	kubectl(t, kctx, "-n", ns, "rollout", "status", "deployment/"+deployment, "--timeout=300s")
}

func podOf(t *testing.T, kctx, ns, instance string) string {
	t.Helper()
	out := kubectl(t, kctx, "-n", ns, "get", "pod",
		"-l", "app.kubernetes.io/instance="+instance,
		"-o", "jsonpath={.items[0].metadata.name}")
	if out == "" {
		t.Fatal("no pod found for instance " + instance)
	}
	return out
}

func decodeSecretKey(t *testing.T, kctx, ns, secret, key string) string {
	t.Helper()
	out := kubectl(t, kctx, "-n", ns, "get", "secret", secret,
		"-o", fmt.Sprintf("jsonpath={.data.%s}", key))
	raw, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("decoding %s/%s: %v", secret, key, err)
	}
	return string(raw)
}

func requireBinaries(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"docker", "kind", "kubectl", "helm"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is required for this suite (hermit provides kind/kubectl/helm; docker must be running)", bin)
		}
	}
}

func recreateKindCluster(t *testing.T, name string) {
	t.Helper()
	if out, err := runErr(t, "kind", "get", "clusters"); err == nil && strings.Contains(out, name) {
		run(t, "kind", "delete", "cluster", "--name", name)
	}
	run(t, "kind", "create", "cluster", "--name", name, "--wait", "300s")
	t.Cleanup(func() {
		if os.Getenv("POLYLANE_E2E_KEEP") == "" {
			_, _ = runErr(t, "kind", "delete", "cluster", "--name", name)
		}
	})
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
