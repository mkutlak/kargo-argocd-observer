# Architecture

## The gap this controller closes

Kargo Stages only display the last Freight promoted *through Kargo*. When a developer
commits an image-tag bump directly to the GitOps repository and ArgoCD syncs it, the
cluster runs the new version while Kargo keeps showing the stale one — nothing told
Kargo's bookkeeping to move. There is no observed-state import in Kargo (≤ v1.10), and
the only supported way to move a Stage's state is to create a `Promotion`. That
constraint shapes the whole design: the controller *detects* drift and *creates
Promotions*; it never touches Stage status directly.

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
   `kargo-observer.mkutlak.github.io/stage`, `kargo-observer.mkutlak.github.io/freight`).
   Kargo's admission webhook authorizes and executes it like any other Promotion; because
   the tag is already live in git, the promotion changes only Kargo's bookkeeping.
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

## Implementation notes

- **Unstructured clients only.** ArgoCD `Application` and Kargo `Stage`/`Freight`/
  `Warehouse`/`Promotion` objects are accessed via `unstructured.Unstructured` — the
  `argo-cd` and `kargo` Go modules are deliberately not imported. This keeps the
  dependency tree to `controller-runtime` + `client-go` + `apimachinery` and decouples
  the controller from Argo/Kargo internal API versioning.
- **Event-driven with a resync backstop.** The primary watch is on Applications
  (filtered by annotation, with updates reduced to annotation/image changes); Stage
  changes re-enqueue their Applications; a 10-minute cache resync catches anything
  missed.
- **Leader election** makes multi-replica deployments safe; only the leader reconciles.

## RBAC

The controller needs:

- Read access to `argoproj.io` `Application` resources.
- Read access to `kargo.akuity.io` `Stage`, `Freight`, `Warehouse`, and `Promotion`
  resources.
- Create access to `kargo.akuity.io` `Promotion` resources.
- Kargo's virtual `promote` verb on `Stage` resources — this is enforced by Kargo's own
  admission webhook via a `SubjectAccessReview`, not by the Kubernetes API server, and
  must be granted separately from the standard `create` verb on `promotions`.

See the Helm chart's `clusterrole.yaml` template for the full
`ClusterRole`/`ClusterRoleBinding`.

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
