# Development

## Architecture

One single-replica Deployment (`strategy: Recreate` — the schema caps
replicas at 1, so two agents can never fight over the same tunnel
credentials), two containers:

- **agent** (this repo) runs as a native sidecar (an initContainer
  with `restartPolicy: Always`): it starts first, and its startupProbe
  gates cloudflared so the tunnel only comes up after registration has
  persisted credentials.
- **cloudflared** (the official image, unmodified) runs the tunnel and
  is the shim's only path to the outside world.

### Registration and the state Secret

On first boot the agent makes one registration call to the platform
(`internal/platform`), identifying the cluster by its `kube-system`
namespace UID. The platform provisions a Cloudflare Tunnel and returns
the credentials the pod needs: the tunnel token and hostname, plus the
shim secret the platform authenticates with. The agent persists all of
it into a pre-created state Secret; cloudflared reads the tunnel token
from that Secret's volume — a whole-dir mount, no subPath, so kubelet
propagates updates without a pod restart — and connects out.

The chart pre-creates the Secret empty because the agent's RBAC is
`get`/`update`/`patch` pinned to that one `resourceNames` entry: it
can never create (or read) any other Secret. See
`internal/kube/statestore.go`.

Warm restarts load the state Secret and make **zero** platform calls —
complete persisted state is served as-is, and registration re-runs
only when state is absent or credentials were rejected. Steady state
involves no polling and no control channel beyond the tunnel itself.

Registration failures split into a retry taxonomy
(`internal/platform/retry.go`): transient answers (429, 5xx, network)
retry forever with capped full-jitter backoff; terminal rejections
(400, 401, 403, 426) exit nonzero so a bad API key surfaces as
CrashLoopBackOff — the deliberate operator signal for "this install
needs a human".

### The shim

`internal/shim` is the security boundary: a pass-through proxy in
front of the kube API, bound only to loopback (config validation
refuses anything else) and reached only through the tunnel. Every
request runs an ordered, deny-first policy — auth, local health,
method, path hygiene, allowed roots (`/version`, `/api...`,
`/apis...`), subresource denylist (`exec`, `attach`, `portforward`,
`proxy`, ...), `secrets` paths denied outright, query validation
(`watch`/`follow` rejected), connection-upgrade rejection — and the
first failing rule decides the response with a fixed status code.
Surviving requests are re-issued upstream as fresh GETs with a nil
body and an allowlisted header set, with the ServiceAccount token
attached per request, so writes, exec, watches, and protocol upgrades
cannot transit the shim by construction. The package documentation in
`internal/shim/shim.go` is the reference.

### Credential rotation

When the platform rotates the tunnel and shim secrets, the old
connector is rejected at Cloudflare's edge: cloudflared's `/ready`
fails (typically restarting its container once). The agent watches
that readiness endpoint (`internal/tunnel`); once it has been down for
the failure grace (~3 minutes, jittered so a fleet never acts in
lockstep), the agent re-registers idempotently and rewrites the state
Secret. cloudflared picks up the propagated token and the pod
converges without restarting: same pod, cloudflared restart count +1,
`/readyz` back to 200 within a few minutes, the new shim secret
working through the tunnel and the old one answering 401.

Rotation is a platform-side operation, so the full loop cannot be
driven from this repo. The pieces are unit-tested (`internal/tunnel`,
`internal/agent`); the end-to-end recovery is observable against a
live `task e2e:cf` cluster when a rotation occurs.

### Health and failure semantics

The agent's ops listener (`:8081`) serves `/startupz`, `/healthz`, and
`/readyz`; Prometheus metrics are on `:9090`; the agent polls
cloudflared's metrics endpoint (`:2000/ready`) to watch tunnel health
cross-container. Liveness is process health only — it never depends on
the tunnel or the platform, so a Cloudflare or Polylane outage
surfaces as NotReady, never as a crash loop. The startup probe budget
is deliberately long (1 hour) so cold-boot registration can ride out a
platform outage in backoff without the kubelet faking the
CrashLoopBackOff signal reserved for rejected credentials.

## Toolchain

Everything is pinned with [Hermit](https://cashapp.github.io/hermit/) —
no system-wide installs:

```sh
. bin/activate-hermit
```

That puts pinned `go`, `task`, `golangci-lint`, `helm`, `kubeconform`,
`kind`, `kubectl`, `goreleaser`, and `yamlfmt` on your PATH. CI runs the
same versions via `cashapp/activate-hermit`.

## Task targets

| Target | What it does |
|---|---|
| `task do` | All quality checks: generate, format, lint, test, build |
| `task init` | Download Go module dependencies |
| `task format` (`f`) | `go fmt` + yamlfmt (charts/ excluded) |
| `task lint` (`l`) | golangci-lint + chart lint/schema validation + workflow audit |
| `task lint:chart` | `helm lint` + kubeconform over three value permutations |
| `task lint:workflows` | zizmor security audit of `.github` (workflows + vendored actions) |
| `task test` (`t`) | Go tests |
| `task test:full` | Tests with `-race -cover` (what CI runs) |
| `task test:chart` | Chart render tests (`./test/chart/...`) |
| `task build` | `dist/polylane-k8s` + `dist/fakeplatform`, CGO off, version-stamped |
| `task e2e` | kind-based end-to-end tests |
| `task e2e:cf` | Manual UAT against real Cloudflare + a real platform |
| `task clean` | Remove build artifacts and the test cache |

## End-to-end tests (kind)

```sh
task e2e
```

Runs `go test -tags e2e ./test/e2e/...`: creates a kind cluster, loads
the agent and fakeplatform images, installs the chart, and asserts the
full boot story (register → state Secret → shim policy → warm restart
makes zero platform calls). Requirements:

- A working Docker daemon (`kind` is hermit-pinned; Docker is not).
- ~20 minutes of budget on a cold image cache (the task sets the test
  timeout accordingly).

## Real-Cloudflare UAT (`task e2e:cf`)

The kind e2e stubs the platform. Before a release that touches
registration or tunnel behavior, run the manual UAT that drives a REAL
registration and proxies through Cloudflare's edge:

```sh
export POLYLANE_E2E_API_KEY=...       # key scoped cloud_accounts:write
export POLYLANE_E2E_PLATFORM_URL=...  # platform base URL
task e2e:cf
```

This creates real tunnels in the associated Cloudflare account and a
real cloud account in the target workspace. Point it at a Polylane
workspace set aside for testing — never a customer one — and
disconnect the test cluster in the console afterwards.

The credential-rotation loop (see "Credential rotation" above) is the
one behavior this suite cannot drive, since rotation happens platform
side. If a rotation is triggered while the `task e2e:cf` cluster is
still running, verify the convergence contract described there.

## Release flow

1. Merge conventional commits to `main`.
2. release-please maintains a release PR that bumps the version
   (`internal/buildinfo/buildinfo.go`, `charts/polylane-k8s/Chart.yaml`)
   and the changelog.
3. Merging that PR creates the `vX.Y.Z` tag and GitHub release; then:
   - the goreleaser workflow attaches binary archives + checksums and a
     keyless cosign signature over `checksums.txt`,
   - the containers workflow publishes multi-arch images to
     `ghcr.io/coreplanelabs/polylane-k8s`, cosign-signed by digest,
   - the release workflow pushes the chart to
     `oci://ghcr.io/coreplanelabs/charts` and cosign-signs its digest.

Verification commands for all three live in the README ("Verifying
release artifacts").

The release workflow authenticates release-please with the org's
release bot (a GitHub App) whose credentials are loaded from 1Password
at runtime via `1password/load-secrets-action`. The only GitHub secret
needed is `OP_SERVICE_ACCOUNT_TOKEN` — provision it before the first
release.

## Renovate posture

`config:best-practices` with automerge for GitHub Actions and
minor/patch updates. Two deliberate choices:

- The cloudflared sidecar image is tag+digest pinned in the chart
  values; its bumps ride the minor/patch automerge, so a cloudflared
  CVE fix ships days after upstream with no agent release.
- The chart's own image (`ghcr.io/coreplanelabs/polylane-k8s`) is
  ignored by the helm-values manager: its tag is deliberately empty and
  follows the chart's `appVersion`, which release-please owns.
