# polylane-k8s

The in-cluster agent that connects a Kubernetes cluster to the
Polylane platform through an outbound-only Cloudflare Tunnel. Kubernetes
API access is read-only; optional service routes connect HTTP APIs inside
the cluster through the same tunnel.

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
| `config.services` | Explicit service and HTTP route allowlist, empty by default |
| `proxy.httpsProxy`, `proxy.noProxy` | Corporate egress proxy for the agent's platform calls |
| `customCA.existingSecret`, `customCA.key` | Extra CA bundle for TLS-intercepting middleboxes |
| `metrics.service.enabled`, `metrics.serviceMonitor.enabled` | Prometheus scraping |
| `networkPolicy.enabled` | Restrict pod ingress to probes and metrics |
| `resources`, `tunnel.resources` | Container resources |

Full references: `charts/polylane-k8s/values.yaml` for the chart,
`config.example.yaml` for the agent's config file.

### Internal HTTP services

To make an internal Grafana API reachable through the agent, add this to
your Helm values and upgrade the agent:

```yaml
config:
  services:
    - id: grafana
      namespace: monitoring
      name: grafana
      port: 80
      scheme: http
      routes:
        - method: GET
          path_prefix: /api/
        - method: POST
          path: /api/ds/query
```

Use the Kubernetes Service's `port`, which can differ from Grafana's Pod
port. Only ClusterIP Services with a Pod selector and a matching TCP port
are accepted. The agent must be allowed to reach the service by the
cluster's NetworkPolicies. Configure `scheme: https` when the service
uses TLS; its certificate must be trusted by the agent and valid for
`<name>.<namespace>.svc`. The chart's `customCA` bundle is available for
private CAs. An optional `base_path: /grafana` supports an installation
under a subpath.

The tunnel endpoint accepts:

- `GET /services`: catalog of configured services and their allowed routes.
- `GET /services/grafana/api/search`: dashboard search for this example.
- `POST /services/grafana/api/ds/query`: Grafana data-source queries.

Every request requires `x-polylane-shim-key`. Application requests also
carry the application's credentials in `Authorization`, such as
`Bearer <Grafana service-account token>`. Use a Grafana token with only the
permissions needed for the configured routes. HTTP methods alone do not
establish whether an application's operation is read-only.

Routes allow an exact `path` or a `path_prefix` ending in `/`, paired with
one HTTP method. The proxy strips `/services/<id>` and prepends `base_path`
before forwarding. Redirects and protocol upgrades are rejected; request
bodies are limited to 1 MiB and response bodies to 32 MiB, with a 100-second
request deadline. Service configuration changes take effect when the
agent restarts; `helm upgrade` rolls the Pod when its config changes.

This is the agent-side API. Selecting a Kubernetes service in the Polylane
Grafana integration requires platform support. Browser sessions and
Grafana Live WebSockets are outside this API's scope.

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
