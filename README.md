# polylane-k8s

The agent you install in a Kubernetes cluster to connect it to the
Polylane platform. It is deliberately minimal and read-only: after a
one-time registration it serves cluster state to the platform over an
outbound-only Cloudflare Tunnel. No inbound ports, no LoadBalancer, no
Ingress — and no cluster credentials ever leave the cluster.

## What runs in your cluster

One single-replica Deployment, two containers:

- **agent** (this repo): registers the cluster on first boot, persists
  the returned tunnel credentials into a Secret in its own namespace,
  and serves a loopback-only, GET-only proxy (the "shim") in front of
  the kube API.
- **cloudflared** (the official Cloudflare image, unmodified): runs
  the tunnel that is the shim's only path to the outside world.

Steady state involves no polling and no control channel beyond the
tunnel itself. Restarts and upgrades reuse the persisted credentials —
a warm boot makes zero platform calls.

## Security posture

- **Read-only by construction.** The shim binds to 127.0.0.1 (the
  agent refuses anything else) and forwards GETs only. Requests
  touching Secrets, sensitive subresources (`exec`, `attach`,
  `portforward`, `proxy`, ...), watches, and connection upgrades are
  refused, and request bodies are never forwarded.
- **Enumerated RBAC.** The ClusterRole grants `get`/`list` over an
  enumerated resource list — no wildcards, no `watch`, and no access
  to Secrets (a chart test enforces it). The agent's one write grant
  is a namespaced Role pinned by `resourceNames` to its own state
  Secret.
- **No inbound exposure.** Everything reaches the shim through the
  tunnel; the pod's only cluster-facing surface is health probes and
  Prometheus metrics. An optional NetworkPolicy
  (`networkPolicy.enabled`) locks that down too.
- **Credentials stay in the cluster.** The ServiceAccount token is
  never sent to the platform — the shim attaches it upstream per
  request. The platform authenticates to the shim with a rotatable
  secret. Token automount is off; the agent's kube identity is a
  short-lived projected token.
- **Hardened containers.** Both run non-root with read-only root
  filesystems, all capabilities dropped, and the RuntimeDefault
  seccomp profile.

## Requirements

- Kubernetes >= 1.29 (the chart uses native sidecar containers)
- Helm >= 3.8 (OCI registry support)
- A Polylane API key scoped `cloud_accounts:write`
- Outbound egress from the pod: HTTPS to the Polylane API
  (`api.polylane.com` by default) and cloudflared's connection to
  Cloudflare's edge — see Cloudflare's "Tunnel with firewall"
  documentation for its ports and IP ranges. The chart's `proxy.*`
  values cover the agent's platform calls only; cloudflared does not
  use them.

## Install

Create a namespace and an API key Secret, then install the chart:

```sh
kubectl create namespace polylane
kubectl --namespace polylane create secret generic polylane-api-key \
  --from-literal=api-key=<your Polylane API key>

helm install polylane-k8s oci://ghcr.io/coreplanelabs/charts/polylane-k8s \
  --namespace polylane \
  --set apiKey.existingSecret=polylane-api-key \
  --set config.cluster_name=prod-us-east
```

`apiKey.existingSecret` names a Secret holding the API key under the
key `api-key` (configurable via `apiKey.secretKey`). For quick trials,
`--set polylane.apiKey=...` templates the Secret for you — exactly one
of the two must be set.

`config.cluster_name` is the name shown in the Polylane console; leave
it unset and the platform names the cluster for you.

The install notes print the exact commands to watch the rollout and
tail the agent logs. If the rollout stalls, the logs say why: a
rejected API key is a terminal error (the pod enters
CrashLoopBackOff); network trouble retries forever with backoff.

Upgrading is safe at any time — warm restarts reuse the persisted
credentials. To remove the agent, `helm uninstall polylane-k8s
--namespace polylane`, then disconnect the cluster in the Polylane
console.

## Configuration

The chart's `config` value is the agent's config file, rendered
verbatim. What an installer actually sets:

| Value | What it does |
|---|---|
| `config.cluster_name` | Display name in the Polylane console |
| `config.distribution` | Optional cluster flavor: `eks`, `gke`, `aks`, ... |
| `proxy.httpsProxy`, `proxy.noProxy` | Corporate egress proxy for the agent's platform calls |
| `customCA.existingSecret`, `customCA.key` | Extra CA bundle for TLS-intercepting middleboxes |
| `metrics.service.enabled`, `metrics.serviceMonitor.enabled` | Prometheus scraping |
| `networkPolicy.enabled` | Restrict pod ingress to probes and metrics |
| `resources`, `tunnel.resources` | Container resources |

Full references: `charts/polylane-k8s/values.yaml` for the chart,
`config.example.yaml` for the agent's config file (validate one with
`polylane-k8s config validate <file>`).

## Verifying release artifacts

CI publishes multi-arch images to `ghcr.io/coreplanelabs/polylane-k8s`,
the chart to `oci://ghcr.io/coreplanelabs/charts/polylane-k8s`, and
binary archives on GitHub releases. Everything is signed with
[cosign](https://docs.sigstore.dev) keyless signatures tied to this
repository's GitHub Actions identity.

```sh
# Container image (any tag; the signature binds to the digest)
cosign verify ghcr.io/coreplanelabs/polylane-k8s:<tag> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/coreplanelabs/polylane-k8s/\.github/workflows/containers\.yaml@'

# Helm chart
cosign verify ghcr.io/coreplanelabs/charts/polylane-k8s:<version> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/coreplanelabs/polylane-k8s/\.github/workflows/release\.yaml@'

# Binary archives: verify checksums.txt, then the archives against it
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/coreplanelabs/polylane-k8s/\.github/workflows/goreleaser\.yaml@' \
  && sha256sum --check --ignore-missing checksums.txt
```

## Reporting a vulnerability

See [SECURITY.md](SECURITY.md). Please do not open a public issue for
anything you believe is exploitable.

## Development

```sh
. bin/activate-hermit
task do
```

See [DEVELOPMENT.md](DEVELOPMENT.md) for the architecture, task
targets, e2e instructions, and the release flow.

## License

[MIT](LICENSE).
