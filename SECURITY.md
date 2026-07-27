# Security policy

## Reporting a vulnerability

Report suspected vulnerabilities to **security@coreplane.ai**, or via
GitHub's private vulnerability reporting (Security → "Report a
vulnerability"). Please do not open a public issue for anything you
believe is exploitable.

Include what you can — affected version or commit, reproduction steps,
and impact. You will get an acknowledgment within 3 business days and
updates as we triage, fix, and disclose.

We support good-faith research: testing against your own clusters and
accounts is welcome, and we will not pursue action over reports made in
good faith. Do not test against clusters or accounts you do not own.

## Supported versions

Security fixes land on the latest release only. Upgrading is designed to
be safe at any time: warm restarts reuse persisted credentials and make
zero platform calls.

## Scope

- The agent (`polylane-k8s`), especially the read-only shim's policy
  layers: auth, path hygiene, the subresource and Secrets denylists,
  watch/upgrade rejection.
- The Helm chart: RBAC grants, securityContexts, Secret handling.
- The release pipeline and published artifacts (images, chart,
  binaries) — all cosign-signed; verification commands are in the
  README under "Verifying release artifacts".

The Polylane platform itself is out of scope for this repository;
platform reports go to the same address.
