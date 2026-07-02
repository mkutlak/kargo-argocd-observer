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
requires. See [How it works](architecture.md) for the full design.

## Quick start

Install the Helm chart (published as an OCI artifact). Chart defaults are scoped for a
safe rollout: `observeMode=opt-in` (only annotated-and-opted-in Applications are
considered) and `dryRun=true` (intended Promotions are logged and emitted as Events,
never created):

```sh
helm install kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
  --namespace kargo-observer --create-namespace
```

Annotate one Application to opt it in:

```sh
kubectl annotate application <app> -n <namespace> kargo-observer.kutlak.cc/observe=true
```

Watch the logs and the Events emitted on Stages; once the Promotions it *would* create
match expectations, let it act for real:

```sh
helm upgrade kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
  --namespace kargo-observer --reuse-values --set dryRun=false
```

Once you trust the controller with everything (not just the Applications you opted in),
widen scope to every annotated Application, unless ignored:

```sh
helm upgrade kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
  --namespace kargo-observer --reuse-values --set observeMode=opt-out
```

## Documentation

| Document | Contents |
|---|---|
| [How it works](architecture.md) | Architecture, design constraints, RBAC, limitations |
| [Reference](reference.md) | Flags, metrics, Events, annotations, labels |
| [Chart values](https://github.com/mkutlak/kargo-argocd-observer/blob/main/charts/kargo-argocd-observer/README.md) | All Helm chart configuration options |
| [Development](https://github.com/mkutlak/kargo-argocd-observer/blob/main/docs/development.md) | Toolchain, quality gate, testing |
| [Releases](https://github.com/mkutlak/kargo-argocd-observer/blob/main/docs/release.md) | Application and chart release processes |

Source, issues, and releases live on
[GitHub](https://github.com/mkutlak/kargo-argocd-observer).
