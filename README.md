# kargo-argocd-observer

A Kubernetes controller that closes a bookkeeping gap in [Kargo](https://kargo.io)
(≤ v1.10): Kargo Stages only display the last Freight promoted *through Kargo*, so when
someone commits an image-tag bump directly to the GitOps repository and ArgoCD syncs it,
the cluster runs the new version while Kargo keeps showing the stale one.

`kargo-argocd-observer` watches ArgoCD `Application` resources, detects when the
deployed image tag no longer matches the Stage's current Freight, finds the Freight that
corresponds to what is actually deployed, and creates a Kargo `Promotion` — the only
supported way to move a Stage's state — so Kargo converges on reality. The Promotion is
a **git no-op** (the tag is already in git); hand-bumps become auditable
`<stage>-observer-*` entries in promotion history instead of invisible drift.

It requires zero per-application configuration: it keys off the
`kargo.akuity.io/authorized-stage` annotation that Kargo's ArgoCD integration already
requires. See [docs/architecture.md](docs/architecture.md) for how it works.

## Quick start

Install the Helm chart (published as an OCI artifact) in observe-only mode:

```sh
helm install kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
  --namespace kargo-observer --create-namespace
```

Watch the logs and the Events emitted on Stages; once the Promotions it *would* create
match expectations, let it act for real:

```sh
helm upgrade kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
  --namespace kargo-observer --reuse-values --set dryRun=false
```

All chart values are documented in
[charts/kargo-argocd-observer/README.md](charts/kargo-argocd-observer/README.md).
Plain Kubernetes manifests are available in [`deploy/`](deploy/) as an alternative
(also shipping with `--dry-run=true`).

## Documentation

| Document | Contents |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How it works, design constraints, RBAC, limitations |
| [docs/reference.md](docs/reference.md) | Flags, metrics, Events, annotations, labels |
| [docs/development.md](docs/development.md) | Toolchain (mise), quality gate, repository layout, testing |
| [docs/release.md](docs/release.md) | Application and Helm chart release processes |

## License

See [LICENSE](LICENSE).
