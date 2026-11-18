# Security policy

## Supported versions

Only the latest release (image and Helm chart) receives security fixes.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private reporting instead:
**Security → Report a vulnerability** on
<https://github.com/mkutlak/kargo-argocd-observer/security/advisories/new>.

Include the affected version, what an attacker needs (which RBAC, which objects they
must be able to edit) and what they gain. The controller's trust boundaries are
described in [docs/architecture.md](docs/architecture.md#trust-model).

## Verifying a release

Release images carry a signed build provenance attestation. Verify it with the image
tag, which has no `v` prefix (e.g. `0.6.0`):

```sh
gh attestation verify oci://ghcr.io/mkutlak/kargo-argocd-observer:<version> \
  --repo mkutlak/kargo-argocd-observer \
  --signer-workflow mkutlak/kargo-argocd-observer/.github/workflows/release.yml
```

The images also carry a BuildKit SBOM; it is not signed.
