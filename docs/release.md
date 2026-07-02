# Release

## Application

`main` is released with [semantic-release](https://semantic-release.gitbook.io/) driven
by conventional commits (`.releaserc.yml`): `fix:` bumps patch, `feat:` bumps minor,
`BREAKING CHANGE:`/`!` bumps major. On each release the pipeline
(`.github/workflows/release.yml`):

1. Runs the race-enabled test suite.
2. Tags `v<version>`, generates `CHANGELOG.md`, and creates the GitHub release.
3. Builds and pushes a multi-arch (`linux/amd64`, `linux/arm64`) image to
   `ghcr.io/mkutlak/kargo-argocd-observer` with semver tags, build provenance, and an
   SBOM attestation.

Commits touching only `*.md`, `docs/**`, `deploy/**`, `.claude/**`, or `charts/**` do
not trigger an application release.

## Helm chart

The chart is versioned independently via `charts/kargo-argocd-observer/Chart.yaml`.
When a push to `main` touches `charts/**`, the chart workflow
(`.github/workflows/release-chart.yml`):

1. Reads `.version` from `Chart.yaml` and skips if the `helm-v<version>` tag already
   exists — so releasing a new chart version means bumping `version:` in `Chart.yaml`.
2. Packages the chart and pushes it as an OCI artifact to
   `oci://ghcr.io/mkutlak/charts/kargo-argocd-observer`.
3. Tags the commit `helm-v<version>`.

Bump the chart's `appVersion` when a new application release should become the chart's
default image tag.
