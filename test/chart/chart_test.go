// Package chart_test renders the polylane-k8s Helm chart across value
// permutations and asserts its security invariants: the ClusterRole can
// never name a credential-bearing resource, the state Secret is the only
// write grant (pinned via resourceNames), the shim stays on loopback, and
// both containers keep the hardened securityContext.
//
// The tests shell out to helm and kubeconform (hermit-pinned in bin/);
// they skip only when the binary is truly absent from PATH.
package chart_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// releaseName yields the fullname below via the chart's fullname helper.
// chartDir is relative: go test always runs this package with its own
// directory as the working directory.
const (
	releaseName = "test"
	fullname    = "test-polylane-k8s"
	chartDir    = "../../charts/polylane-k8s"
)

// requireBinary skips the test when name is truly absent from PATH; the
// callers invoke it by its literal name so the lookup here is only the
// skip check.
func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH: %v", name, err)
	}
}

func chartVersion(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	match := regexp.MustCompile(`(?m)^version:\s*['"]?([^'"\s]+)['"]?\s*$`).FindSubmatch(contents)
	if match == nil {
		t.Fatal("Chart.yaml has no version")
	}
	return string(match[1])
}

// runHelmTemplate renders the chart with the given --set pairs, returning
// combined output and the raw error (schema-rejection tests want both).
func runHelmTemplate(t *testing.T, sets ...string) (string, error) {
	t.Helper()
	requireBinary(t, "helm")
	args := []string{"template", releaseName, chartDir}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// render is runHelmTemplate for permutations that must succeed.
func render(t *testing.T, sets ...string) string {
	t.Helper()
	out, err := runHelmTemplate(t, sets...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return out
}

// doc extracts the single rendered document of the given kind.
func doc(t *testing.T, rendered, kind string) string {
	t.Helper()
	kindLine := regexp.MustCompile(`(?m)^kind: ` + kind + `$`)
	var found []string
	for _, d := range strings.Split("\n"+rendered, "\n---\n") {
		if kindLine.MatchString(d) {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s document, got %d", kind, len(found))
	}
	return found[0]
}

// containerSections splits a Deployment document into its initContainers
// and containers blocks so assertions can target the right container.
func containerSections(t *testing.T, deployment string) (initSection, mainSection string) {
	t.Helper()
	initIdx := strings.Index(deployment, "initContainers:")
	if initIdx < 0 {
		t.Fatalf("deployment has no initContainers:\n%s", deployment)
	}
	rest := deployment[initIdx:]
	mainIdx := strings.Index(rest, "\n      containers:")
	if mainIdx < 0 {
		t.Fatalf("deployment has no containers block after initContainers:\n%s", deployment)
	}
	volIdx := strings.Index(rest, "\n      volumes:")
	if volIdx < 0 {
		volIdx = len(rest)
	}
	return rest[:mainIdx], rest[mainIdx:volIdx]
}

func TestHelmLint(t *testing.T) {
	t.Parallel()
	requireBinary(t, "helm")
	cmd := exec.Command("helm", "lint", chartDir, "--quiet",
		"--set", "apiKey.existingSecret=polylane-api-key")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helm lint: %v\n%s", err, out)
	}
}

func TestClusterRoleReadOnlyAndNeverSecrets(t *testing.T) {
	t.Parallel()
	rendered := render(t, "apiKey.existingSecret=polylane-api-key")
	cr := doc(t, rendered, "ClusterRole")

	// The load-bearing invariant: no rule (nor any other line of the
	// document) may mention secrets in any casing.
	if strings.Contains(strings.ToLower(cr), "secret") {
		t.Errorf("ClusterRole must never mention secrets:\n%s", cr)
	}

	verbs := regexp.MustCompile(`verbs: \[([^\]]*)\]`).FindAllStringSubmatch(cr, -1)
	if len(verbs) == 0 {
		t.Fatalf("no verbs found in ClusterRole:\n%s", cr)
	}
	for _, m := range verbs {
		if m[1] != "get, list" {
			t.Errorf("ClusterRole verbs must be exactly [get, list], got [%s]", m[1])
		}
	}
}

func TestRolePinnedToStateSecret(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sets []string
		want string
	}{
		{"default", nil, fullname + "-state"},
		{"override", []string{"stateSecret.name=custom-state"}, "custom-state"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sets := append([]string{"apiKey.existingSecret=polylane-api-key"}, tc.sets...)
			rendered := render(t, sets...)

			role := doc(t, rendered, "Role")
			if want := "resourceNames: [" + tc.want + "]"; !strings.Contains(role, want) {
				t.Errorf("Role not pinned via %q:\n%s", want, role)
			}
			if want := "verbs: [get, update, patch]"; !strings.Contains(role, want) {
				t.Errorf("Role verbs must be exactly %q:\n%s", want, role)
			}

			// The same name must thread through the Secret itself, the
			// tunnel's volume, and the rendered config.
			secret := doc(t, rendered, "Secret")
			if !strings.Contains(secret, "name: "+tc.want) {
				t.Errorf("state Secret not named %q:\n%s", tc.want, secret)
			}
			deployment := doc(t, rendered, "Deployment")
			if !strings.Contains(deployment, "secretName: "+tc.want) {
				t.Errorf("state volume does not reference %q:\n%s", tc.want, deployment)
			}
			cm := doc(t, rendered, "ConfigMap")
			if !strings.Contains(cm, "name: "+tc.want) {
				t.Errorf("rendered config state_secret.name is not %q:\n%s", tc.want, cm)
			}
		})
	}
}

func TestRenderedConfigShimStaysLoopback(t *testing.T) {
	t.Parallel()
	rendered := render(t, "apiKey.existingSecret=polylane-api-key")
	cm := doc(t, rendered, "ConfigMap")
	if !regexp.MustCompile(`listen: "?127\.0\.0\.1:8080"?`).MatchString(cm) {
		t.Errorf("rendered config must keep the shim on loopback:\n%s", cm)
	}
}

func TestDeploymentHardening(t *testing.T) {
	t.Parallel()
	rendered := render(t, "apiKey.existingSecret=polylane-api-key")
	deployment := doc(t, rendered, "Deployment")

	// Fields counted twice must appear in BOTH the agent's and
	// cloudflared's securityContext.
	checks := []struct {
		want  string
		count int
	}{
		{"automountServiceAccountToken: false", 1},
		{"terminationGracePeriodSeconds: 70", 1},
		{"strategy:\n    type: Recreate", 1},
		{"runAsNonRoot: true", 2},
		{"runAsUser: 65532", 2},
		{"readOnlyRootFilesystem: true", 2},
		{"allowPrivilegeEscalation: false", 2},
		{`drop: ["ALL"]`, 2},
		{"type: RuntimeDefault", 2},
		{"expirationSeconds: 3600", 1},
	}
	for _, c := range checks {
		if got := strings.Count(deployment, c.want); got != c.count {
			t.Errorf("deployment contains %q %d times, want %d", c.want, got, c.count)
		}
	}
}

func TestAgentIsNativeSidecar(t *testing.T) {
	t.Parallel()
	rendered := render(t, "apiKey.existingSecret=polylane-api-key")
	initSection, mainSection := containerSections(t, doc(t, rendered, "Deployment"))

	if !strings.Contains(initSection, "restartPolicy: Always") {
		t.Errorf("agent initContainer must set restartPolicy Always:\n%s", initSection)
	}
	if !strings.Contains(initSection, "- name: agent") {
		t.Errorf("initContainers must hold the agent:\n%s", initSection)
	}
	if !strings.Contains(mainSection, "- name: cloudflared") {
		t.Errorf("containers must hold cloudflared:\n%s", mainSection)
	}
}

func TestAgentImage(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name string
		sets []string
		want string
	}{
		{"defaults to chart version", nil, "ghcr.io/coreplanelabs/polylane-k8s:" + chartVersion(t)},
		{"tag override", []string{"image.tag=custom"}, "ghcr.io/coreplanelabs/polylane-k8s:custom"},
		{"digest overrides tag", []string{"image.tag=custom", "image.digest=" + digest}, "ghcr.io/coreplanelabs/polylane-k8s@" + digest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sets := append([]string{"apiKey.existingSecret=polylane-api-key"}, tc.sets...)
			initSection, _ := containerSections(t, doc(t, render(t, sets...), "Deployment"))
			if want := `image: "` + tc.want + `"`; !strings.Contains(initSection, want) {
				t.Errorf("agent image does not contain %q:\n%s", want, initSection)
			}
		})
	}
}

func TestCloudflaredDefaults(t *testing.T) {
	t.Parallel()
	rendered := render(t, "apiKey.existingSecret=polylane-api-key")
	_, mainSection := containerSections(t, doc(t, rendered, "Deployment"))

	wantArgs := `args: ["tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run"]`
	if !strings.Contains(mainSection, wantArgs) {
		t.Errorf("cloudflared args are not exactly the default %s:\n%s", wantArgs, mainSection)
	}
	if strings.Contains(mainSection, "command:") {
		t.Errorf("cloudflared must not set command by default:\n%s", mainSection)
	}
	if !strings.Contains(mainSection, "value: /var/lib/polylane/state/tunnelToken") {
		t.Errorf("TUNNEL_TOKEN_FILE must point into the state mount:\n%s", mainSection)
	}
	if !strings.Contains(mainSection, "mountPath: /var/lib/polylane/state") {
		t.Errorf("cloudflared must mount the state volume:\n%s", mainSection)
	}
}

// TestTunnelCommandArgsOverride protects the kind e2e's stub-substitution
// path: tunnel.command/args must replace the cloudflared invocation.
func TestTunnelCommandArgsOverride(t *testing.T) {
	t.Parallel()
	rendered := render(t,
		"apiKey.existingSecret=polylane-api-key",
		"tunnel.command={/stub}",
		"tunnel.args={--ready-after,1s}",
	)
	_, mainSection := containerSections(t, doc(t, rendered, "Deployment"))

	for _, want := range []string{"command:", "- /stub", "- --ready-after", "- 1s"} {
		if !strings.Contains(mainSection, want) {
			t.Errorf("tunnel override missing %q:\n%s", want, mainSection)
		}
	}
	if strings.Contains(mainSection, "--no-autoupdate") {
		t.Errorf("default args must not leak through an override:\n%s", mainSection)
	}
}

func TestSchemaRequiresExactlyOneAPIKeySource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sets []string
	}{
		{"both set", []string{"apiKey.existingSecret=a", "polylane.apiKey=b"}},
		{"neither set", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := runHelmTemplate(t, tc.sets...)
			if err == nil {
				t.Fatalf("helm template must fail schema validation with %v", tc.sets)
			}
			if !strings.Contains(strings.ToLower(out), "schema") {
				t.Errorf("failure should come from the values schema, got:\n%s", out)
			}
		})
	}
}

func TestKubeconform(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sets []string
	}{
		{"defaults with existing secret", []string{"apiKey.existingSecret=polylane-api-key"}},
		{"convenience api key", []string{"polylane.apiKey=sk_test_123"}},
		{"all optional toggles", []string{
			"apiKey.existingSecret=polylane-api-key",
			"metrics.service.enabled=true",
			"metrics.serviceMonitor.enabled=true",
			"networkPolicy.enabled=true",
			"proxy.httpsProxy=http://proxy.corp:3128",
			"proxy.noProxy=10.0.0.0/8",
			"customCA.existingSecret=corp-ca",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered := render(t, tc.sets...)
			requireBinary(t, "kubeconform")
			cmd := exec.Command("kubeconform", "-strict", "-ignore-missing-schemas")
			cmd.Stdin = strings.NewReader(rendered)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("kubeconform: %v\n%s", err, out)
			}
		})
	}
}
