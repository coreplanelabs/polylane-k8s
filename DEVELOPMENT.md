# Development

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
| `task lint` (`l`) | golangci-lint + chart lint/schema validation |
| `task lint:chart` | `helm lint` + kubeconform over three value permutations |
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
export POLYLANE_E2E_PLATFORM_URL=...  # e.g. a staging platform
task e2e:cf
```

This creates real tunnels in the associated Cloudflare account; use a
staging platform account, never a customer one.

### Rotation-loop validation (manual, needs a platform operator)

The credential-rotation loop (COR-32 §1: platform rotates → edge rejects
the connector → cloudflared restarts → agent re-registers → converges)
cannot be driven from this repo alone: rotation is an explicit platform
op that rotates BOTH the tunnel secret and the shim secret. With the
`task e2e:cf` cluster still running:

1. Have a platform operator trigger the rotate op for the test account
   (`rotateTunnelSecret` + shim secret, the §4.2 composite).
2. Watch the pod: cloudflared's `/ready` goes 503 and it restarts;
   within the agent's ~3 min grace it re-registers (idempotently) and
   rewrites the state Secret.
3. Assert convergence, no pod restart:
   `kubectl -n polylane-cf-e2e get pod` (same pod, cloudflared restart
   count +1) and the agent's `/readyz` back to 200 within ~1–3 min.
4. Confirm the new shim secret works through the tunnel and the old one
   answers 401.

## Release flow

1. Merge conventional commits to `main`.
2. release-please maintains a release PR that bumps the version
   (`internal/buildinfo/buildinfo.go`, `charts/polylane-k8s/Chart.yaml`)
   and the changelog.
3. Merging that PR creates the `vX.Y.Z` tag and GitHub release; then:
   - the goreleaser workflow attaches binary archives + checksums,
   - the containers workflow publishes multi-arch images to
     `ghcr.io/coreplanelabs/polylane-k8s`,
   - the release workflow pushes the chart to
     `oci://ghcr.io/coreplanelabs/charts`.

The release workflow authenticates release-please with the org bot app
(`COREPLANE_BOT_CLIENT_ID` / `COREPLANE_BOT_PRIVATE_SIGNING_KEY` org
secrets) — provision those before the first release.

## Renovate posture

`config:best-practices` with automerge for GitHub Actions and
minor/patch updates. Two deliberate choices:

- The cloudflared sidecar image is tag+digest pinned in the chart
  values; its bumps ride the minor/patch automerge, so a cloudflared
  CVE fix ships days after upstream with no agent release.
- The chart's own image (`ghcr.io/coreplanelabs/polylane-k8s`) is
  ignored by the helm-values manager: its tag is deliberately empty and
  follows the chart's `appVersion`, which release-please owns.
