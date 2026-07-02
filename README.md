# kargo-argocd-observer

A Kubernetes controller that closes a bookkeeping gap in [Kargo](https://kargo.io) (≤ v1.10):
Kargo Stages only display the last Freight promoted *through Kargo*. When a developer
commits an image-tag bump directly to the GitOps repository and ArgoCD syncs it — a
routine "hand-bump" for a hotfix or an out-of-band QA push — the cluster is running the
new version but Kargo keeps showing the stale one, because nothing told Kargo's
bookkeeping to move. `kargo-argocd-observer` watches ArgoCD `Application` resources,
detects when the deployed image tag no longer matches the Stage's current Freight, finds
the Freight that corresponds to what is actually deployed, and creates a Kargo
`Promotion` — the only supported way to move a Stage's state — so Kargo converges on
reality. The resulting Promotion is a **git no-op**: the tag is already in the GitOps
repository, so only Kargo's internal state changes. Hand-bumps become auditable
`<stage>-observer-*` entries in the Stage's promotion history instead of invisible drift.

## How it works

1. **Watch.** The controller watches ArgoCD `Application` resources carrying the
   annotation `kargo.akuity.io/authorized-stage: "<namespace>:<stage>"` — the same
   annotation Kargo's ArgoCD integration already requires before a Stage may manage an
   Application. If Kargo promotes to your Applications today, the annotation is already
   there, so there is zero additional per-application configuration. An `Application` can
   opt out with `kargo-observer.mkutlak.github.io/ignore: "true"`.
2. **Detect drift.** On reconcile, the controller reads the deployed image tags from the
   Application's `.status.summary.images` and compares them against the Stage's current
   view, `.status.freightHistory[0]`, restricted to the repositories subscribed to by the
   Stage's `requestedFreight` origin Warehouses. Equal tags short-circuit as a no-op —
   this is what keeps the controller loop-free.
3. **Find the matching Freight.** On drift, the controller lists `Freight` in the Stage's
   namespace and looks for one whose images match *all* of the drifted repo:tag pairs.
4. **Promote.** A match produces a `Promotion` (`generateName: <stage>-observer-`,
   labeled `app.kubernetes.io/managed-by=kargo-argocd-observer`,
   `kargo-observer.mkutlak.github.io/stage`, `kargo-observer.mkutlak.github.io/freight`). Kargo's admission
   webhook authorizes and executes it like any other Promotion; because the tag is
   already live in git, the promotion changes only Kargo's bookkeeping.
5. **Guard against repeats.** The controller skips reconciliation while the Stage has a
   Promotion in flight, and will not recreate a Promotion for a Freight it already tried
   and failed for — delete the failed Promotion to retry.

```
ArgoCD Application (kargo.akuity.io/authorized-stage annotation)
        │
        │ reconcile
        ▼
compare deployed image tags (.status.summary.images)
   against Stage's current Freight (.status.freightHistory[0])
        │
        ├── tags match ────────────────────────────► no-op, already converged
        │
        └── tags differ (drift)
                │
                ▼
        find Freight in the Stage namespace whose images
        match ALL drifted repo:tag pairs
                │
        ┌───────┴────────┐
        │                │
   Freight found     no matching Freight
        │                │
        ▼                ▼
  create Promotion    Event: FreightMissing
  <stage>-observer-*  kargo_observer_freight_missing = 1
        │
        ▼
  Kargo admission webhook checks the `promote` verb,
  then executes the Promotion — a git no-op, since the
  tag is already deployed; Stage's Freight state updates
```

## Flags

| Flag | Default | Description |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Address the Prometheus metrics endpoint binds to |
| `--health-probe-bind-address` | `:8081` | Address the liveness/readiness probes bind to |
| `--leader-elect` | `false` | Enable leader election for multi-replica deployments |
| `--dry-run` | `false` | Log and emit Events instead of creating Promotions |

## Metrics

| Metric | Labels | Description |
|---|---|---|
| `kargo_observer_promotions_created_total` | `namespace`, `stage` | Counter of Promotions created by the controller |
| `kargo_observer_freight_missing` | `namespace`, `stage` | Gauge; `1` when a deployed tag has no matching Freight — usually the tag aged out of the Warehouse discovery window. Fix by adding a stable-line Warehouse |
| `kargo_observer_promotion_create_errors_total` | `namespace`, `stage` | Counter of failed Promotion create calls |

## Events

Emitted on the `Stage` object:

| Event | Meaning |
|---|---|
| `PromotionCreated` | A Promotion was created to converge the Stage on the deployed tag |
| `DryRunPromotionSkipped` | Drift detected; no Promotion created because `--dry-run` is set |
| `FreightMissing` | No Freight matches the deployed tag |
| `PromotionPreviouslyFailed` | The controller already tried and failed to promote this Freight; delete the failed Promotion to retry |
| `PromotionCreateFailed` | The Promotion create call itself failed |

## Annotations

| Annotation | Applies to | Meaning |
|---|---|---|
| `kargo.akuity.io/authorized-stage: "<namespace>:<stage>"` | `Application` | Links the Application to its Kargo Stage; required by Kargo's own ArgoCD integration, so it is already present wherever Kargo manages the Application |
| `kargo-observer.mkutlak.github.io/ignore: "true"` | `Application` | Opts the Application out of observation |

## RBAC

The controller needs:

- Read access to `argoproj.io` `Application` resources.
- Read access to `kargo.akuity.io` `Stage`, `Freight`, `Warehouse`, and `Promotion`
  resources.
- Create access to `kargo.akuity.io` `Promotion` resources.
- Kargo's virtual `promote` verb on `Stage` resources — this is enforced by Kargo's own
  admission webhook via a `SubjectAccessReview`, not by the Kubernetes API server, and
  must be granted separately from the standard `create` verb on `promotions`.

See `deploy/` for the full `ClusterRole`/`ClusterRoleBinding`.

## Deployment

### Helm

A Helm chart lives in `charts/kargo-argocd-observer` and is published as an OCI
artifact:

```sh
helm install kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
  --namespace kargo-observer --create-namespace
```

Or straight from a local checkout:

```sh
helm install kargo-argocd-observer ./charts/kargo-argocd-observer \
  --namespace kargo-observer --create-namespace
```

The chart defaults to observe-only mode; once the intended promotions in the logs
match expectations, let it act for real:

```sh
helm upgrade kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
  --namespace kargo-observer --reuse-values --set dryRun=false
```

See `charts/kargo-argocd-observer/README.md` for the full list of values.

### Plain manifests

Plain Kubernetes manifests live in `deploy/`: namespace `kargo-observer`, a 2-replica
leader-elected `Deployment`, `ServiceAccount`/RBAC, `Service`, `ServiceMonitor`, and a
`PrometheusRule` for `kargo_observer_freight_missing` and Promotion-create failures. The
image is `ghcr.io/mkutlak/kargo-argocd-observer`.

The manifests ship with `--dry-run=true` by default. Recommended rollout:

1. Apply `deploy/` and watch the controller's logs and the Events it emits on Stages —
   confirm the Promotions it *would* create match expectations.
2. Once satisfied, flip `--dry-run` to `false` (e.g. via a Kustomize patch or Helm value
   in the consuming GitOps repo) and let it create real Promotions.

## Development

Toolchain is managed with [mise](https://mise.jdx.dev); run `mise install` once, then:

```
mise run check         # fmt + vet + lint + test — full quality gate
mise run test           # go test -race ./...
mise run build           # build the observer binary
mise run docker:build    # build the container image
```

Run `mise tasks` for the complete list. See `CLAUDE.md` for architecture and agent
routing.

## Release

`main` is released with [semantic-release](https://semantic-release.gitbook.io/) driven
by conventional commits. Each release publishes a multi-arch (`linux/amd64`,
`linux/arm64`) image to GHCR with build provenance and an SBOM attestation.

## Limitations

- One Promotion is created per reconcile. A Stage with multiple Warehouse origins where
  several repositories drift at the same time converges over several reconciles, not
  instantly.
- The matching Freight must already exist — if the hand-bumped tag has aged out of the
  Warehouse's discovery window, no Freight will match and the controller emits
  `FreightMissing` until a Warehouse discovers it (or a stable-line Warehouse is added).
- Old Promotions are garbage-collected by Kargo itself, not by this controller.
- Requires the `kargo.akuity.io/authorized-stage` annotation on Applications — present
  wherever Kargo's ArgoCD integration is in use.
