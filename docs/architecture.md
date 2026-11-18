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
   opt out with `kargo-observer.kutlak.cc/ignore: "true"`. Running with
   `--observe-mode=opt-in` inverts the default for everything else: only Applications
   additionally annotated `kargo-observer.kutlak.cc/observe: "true"` are watched, which
   is useful for scoping a rollout to a handful of Applications before trusting the
   controller with everything.
2. **Detect drift.** On reconcile, the controller reads the deployed image tags from the
   Application's `.status.summary.images` and compares them against the Stage's current
   view, `.status.freightHistory[0]`, restricted to the repositories subscribed to by the
   Stage's `requestedFreight` origin Warehouses. Equal tags short-circuit as a no-op —
   this is what keeps the controller loop-free. ArgoCD lists the images of live Pods,
   so during a rollout a repository appears with both the old and the new tag; such a
   repository is ignored (and logged) until it settles on one tag. When two of the Stage's origins
   carry the same repository, the Application is in sync if it deploys either tag.
3. **Find the matching Freight.** On drift, the controller lists `Freight` in the Stage's
   namespace and looks for one, from one of the Stage's Warehouses, that would move
   *only* what drifted: every image the Freight carries must be at the tag the
   Application deploys (or, for a repository the Application does not deploy, at the
   tag the Stage currently holds from that Warehouse — or from another origin when
   that Warehouse holds none), and at least one must be a drifted tag. Of several such
   Freight the newest wins. A Freight that carries the bumped tag next to a different
   tag for some other image is never promoted — that would deploy or roll back an image
   nobody touched.
4. **Promote.** A match produces a `Promotion` named `<stage>.<ulid>.<freight-prefix>`
   — exactly Kargo's own naming scheme, and deliberately so: Kargo's stage controller
   records a finished Promotion into freight history only when its name sorts lexically
   after `status.lastPromotion.name`, an ordering guaranteed by the embedded
   time-ordered ULID. The Promotion is labeled
   `app.kubernetes.io/managed-by=kargo-argocd-observer`,
   `kargo-observer.kutlak.cc/stage`, `kargo-observer.kutlak.cc/freight`, carrying the
   Stage's promotion template steps and variables — copied the same way Kargo's own API
   server builds Promotions, because Kargo's admission webhook rejects step-less
   Promotions rather than defaulting them from the Stage. The webhook then authorizes it,
   inflates task references, and stamps the Stage's shard. The Promotion is a real one:
   Kargo runs the Stage's steps. Because the chosen Freight's images equal what is
   already deployed, the image part of those steps is a no-op in git; anything else the
   Freight carries (git commits, charts) is not compared.
5. **Guard against repeats and races.** The controller does not create a Promotion
   - unless the Application is `Synced`. While it is `OutOfSync` the cluster is not
     running what git holds — typically a Kargo promotion ArgoCD has not applied yet
     (sync window, manual or failed sync) — and promoting to the images still running
     would undo that promotion. The Application is re-evaluated as soon as it becomes
     `Synced`;
   - while any Promotion for the Stage is unfinished, whoever created it;
   - for 5 minutes after the Stage's last Promotion finished — ArgoCD may still report
     the previous revision's images, and acting on that would promote the Stage straight
     back;
   - when another observed Application of the same Stage deploys a different tag for a
     drifted repository (`ApplicationsDisagree`) — the two would otherwise take turns
     promoting the Stage;
   - when its own most recent Promotion for the Stage was for the same Freight and
     failed, errored or was aborted — delete that Promotion to retry (picked up at the
     next resync). The verdict is dropped once the Stage has finished a newer Promotion,
     so going A → B → A works.

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
        find the newest Freight in the Stage namespace whose
        images equal the deployed tags and fix the drift
                │
        ┌───────┴────────┐
        │                │
   Freight found     no matching Freight
        │                │
        ▼                ▼
  create Promotion         Event: FreightMissing
  <stage>.<ulid>.<hash>    kargo_observer_freight_missing = 1
        │
        ▼
  Kargo admission webhook checks the `promote` verb,
  then executes the Promotion — the image part is a git
  no-op, since those tags are already deployed; Stage's
  Freight state updates
```

## Implementation notes

- **Unstructured clients only.** ArgoCD `Application` and Kargo `Stage`/`Freight`/
  `Warehouse`/`Promotion` objects are accessed via `unstructured.Unstructured` — the
  `argo-cd` and `kargo` Go modules are deliberately not imported. This keeps the
  dependency tree to `controller-runtime` + `client-go` + `apimachinery` and decouples
  the controller from Argo/Kargo internal API versioning.
- **Event-driven with a resync backstop.** The primary watch is on Applications
  (filtered by annotation, with updates reduced to annotation/image changes); Stage
  changes re-enqueue their Applications. Freight and Promotions are not watched: the
  cache resync (`--sync-period`, 10 minutes by default) re-reconciles every observed
  Application and is what picks up newly discovered Freight or a deleted Promotion.
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

## Trust model

The observer holds `promote` on every Stage, and by default it believes the
Application: the `authorized-stage` annotation picks the Stage and
`.status.summary.images` says what is deployed. Anyone who can create or annotate an
Application the observer watches can therefore have any *existing* Freight of that
Stage's Warehouses promoted to it, without holding `promote` themselves. Kargo's own
check runs the other way round (the Application authorizes the Stage), so it does not
close this.

Where the people who can edit Applications are not the people who own the Stages, run
with `--require-stage-consent` (chart: `requireStageConsent=true`). The Stage must then
name each Application allowed to drive it:

```yaml
metadata:
  annotations:
    kargo-observer.kutlak.cc/applications: "argocd/web-qa, argocd/api-qa"
```

An Application the Stage does not list is skipped with a log line and no Event.
Editing the annotation re-evaluates the Stage's Applications right away.

Not closed by the flag: the Promotion carries the Stage's own steps and runs with
Kargo's privileges, so whoever can edit a Stage's `promotionTemplate` gets those steps
executed once drift appears, without holding `promote`.

## Limitations

- One Promotion is created per reconcile, and one Freight comes from one Warehouse. A
  Stage with multiple Warehouse origins where repositories of several origins drift at
  the same time converges one origin at a time, each step waiting out the 5-minute
  settle period.
- `Synced` is ArgoCD's last comparison. Until ArgoCD refreshes after a Kargo push, the
  Application still reads `Synced` while running the old images; only the 5-minute
  settle wait covers that window. With refresh slowed or disabled and steps that do not
  wait for ArgoCD, the observer can still promote the Stage back.
- An Application that never reaches `Synced` (for example one resource that always
  shows a diff) is never acted on; the log says so each time drift is seen.
- A Pod that keeps an old tag around for good — a finished Job kept by its history
  limit, an evicted Pod — makes ArgoCD report the repository with two tags, and the
  observer ignores that repository until the Pod is gone. The log names the repository
  and its tags.
- Freight is matched on image tags only. A Warehouse that produces several Freight with
  identical images (differing in git commit or chart) gets the newest one.
- The matching Freight must already exist — if the hand-bumped tag has aged out of the
  Warehouse's discovery window, no Freight will match and the controller emits
  `FreightMissing` until a Warehouse discovers it (or a stable-line Warehouse is added).
- Old Promotions are garbage-collected by Kargo itself, not by this controller.
- Target-aware Stages (`spec.targets`, Kargo v1.12+) are promoted only through
  PromotionRequests, so the observer skips them with a `StageIsTargetAware` Event.
- Stages must define `promotionTemplate` steps — Kargo rejects Promotions for step-less
  (control-flow) Stages, so the observer skips them with a `StageHasNoPromotionSteps`
  Event.
- Requires the `kargo.akuity.io/authorized-stage` annotation on Applications — present
  wherever Kargo's ArgoCD integration is in use.
