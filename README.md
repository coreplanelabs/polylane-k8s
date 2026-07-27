# polylane-k8s

The in-cluster agent that connects a Kubernetes cluster to the
Polylane platform. It is read-only and reaches the platform through an
outbound-only Cloudflare Tunnel.

## Requirements

- Kubernetes >= 1.29
- Helm >= 3.8
- A Polylane API key scoped `cloud_accounts:write`
- Outbound egress from the pod: HTTPS to the Polylane API
  (`api.polylane.com` by default) and cloudflared's connection to
  Cloudflare's edge (see Cloudflare's "Tunnel with firewall"
  documentation for ports and IP ranges). The chart's `proxy.*` values
  apply to the agent's platform calls only; cloudflared does not use
  them.

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

The Secret holds the API key under the key `api-key` (configurable via
`apiKey.secretKey`); `config.cluster_name` is the name shown in the
Polylane console. Once the pod is Ready, the cluster shows as Live in
the console.

Upgrade with `helm upgrade` at any time. To remove the agent, run
`helm uninstall polylane-k8s --namespace polylane`, then disconnect the
cluster in the console.

## Configuration

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
`config.example.yaml` for the agent's config file.

## Verifying release artifacts

Images (`ghcr.io/coreplanelabs/polylane-k8s`), the Helm chart
(`oci://ghcr.io/coreplanelabs/charts/polylane-k8s`), and the release
binaries are signed with [cosign](https://docs.sigstore.dev) keyless
signatures tied to this repository's GitHub Actions identity.

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

## Security

The agent's RBAC grants `get`/`list` over an enumerated resource list,
with no access to Secrets. The full security model is documented in
[DEVELOPMENT.md](DEVELOPMENT.md); report vulnerabilities per
[SECURITY.md](SECURITY.md).

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) for the architecture, task
targets, e2e instructions, and the release flow.

## License

[MIT](LICENSE).
