# polylane-k8s

A deliberately minimal, read-only in-cluster agent that connects a
Kubernetes cluster to the Polylane platform over an outbound-only
Cloudflare Tunnel. No inbound ports, no LoadBalancer, no Ingress — and
no cluster credentials ever leave the cluster.

## How it works

One pod, two containers:

- **agent** (this repo, a native sidecar): registers the cluster and
  serves a loopback-only, GET-only proxy (the "shim") in front of the
  kube API.
- **cloudflared** (the official Cloudflare image, unmodified): runs the
  tunnel that is the shim's only path to the outside world.

On first boot the agent makes one bootstrap call to the platform:
`POST /v1/cloud_accounts/kubernetes/agent/register` with the cluster's
identity (the `kube-system` namespace UID). The platform provisions a
tunnel and returns a tunnel token, hostname, and a shim secret. The
agent persists all of it into a pre-created state Secret; cloudflared
reads the tunnel token from that Secret's volume and connects out.

Warm restarts load the state Secret and make **zero** platform calls.
Steady state involves no polling and no control channel beyond the
tunnel itself.

## Security model

- **Read-only shim**: loopback-only listener, GET-only, with an ordered
  9-rule policy — per-request shared-secret auth, method allowlist,
  strict path validation (only `/version`, `/api...`, `/apis...`),
  subresource denylist (`exec`, `attach`, `portforward`, `proxy`, ...),
  `secrets` paths denied outright, `watch`/`follow` rejected,
  connection upgrades rejected, request bodies never forwarded.
- **RBAC**: the ClusterRole grants `get`/`list` only, over an
  enumerated resource list — `secrets` is deliberately absent. The only
  Secret access is a namespaced Role scoped by `resourceNames` to the
  agent's own state Secret.
- **Exposure**: the shim listens on 127.0.0.1 by construction (the
  agent refuses anything else) and is reachable only through the
  Cloudflare Tunnel. The pod's cluster-facing surface is health probes
  and Prometheus metrics.
- **Credentials**: the ServiceAccount token stays in the cluster; the
  platform authenticates to the shim with a rotatable shim secret, and
  the shim attaches the token upstream per request.

## Install

```sh
helm install polylane oci://ghcr.io/coreplanelabs/charts/polylane-k8s \
  --namespace polylane --create-namespace \
  --set apiKey.existingSecret=polylane-api-key \
  --set config.cluster_name=prod-us-east
```

`apiKey.existingSecret` names a Secret holding the Polylane API key
under the key `api-key` (configurable via `apiKey.secretKey`). For
quick trials, `--set polylane.apiKey=...` templates the Secret for you
— exactly one of the two must be set.

## Requirements

- Kubernetes >= 1.29 (native sidecar containers)
- Outbound HTTPS from the pod to the Polylane API and Cloudflare's edge
  (proxy and custom-CA settings are available in the chart values)

## Configuration

- `config.example.yaml` — the full commented reference for the agent's
  config file. Validate with `polylane-k8s config validate`.
- `charts/polylane-k8s/values.yaml` — chart values; `config` is
  rendered verbatim as the agent's config file.

## Development

```sh
. bin/activate-hermit
task do
```

See `DEVELOPMENT.md` for the task targets, e2e instructions, and the
release flow.

## Releases

release-please cuts the tag and notes from conventional commits;
GoReleaser attaches binary archives; CI publishes multi-arch images to
`ghcr.io/coreplanelabs/polylane-k8s` and the chart to
`oci://ghcr.io/coreplanelabs/charts`.

### Verifying release artifacts

Everything CI publishes is signed with [cosign](https://docs.sigstore.dev)
keyless signatures tied to this repository's GitHub Actions identity.

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

## License

[MIT](LICENSE).
