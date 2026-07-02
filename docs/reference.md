# Reference

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
| `kargo-observer.kutlak.cc/ignore: "true"` | `Application` | Opts the Application out of observation |

## Labels set on created Promotions

| Label | Value |
|---|---|
| `app.kubernetes.io/managed-by` | `kargo-argocd-observer` |
| `kargo-observer.kutlak.cc/stage` | The target Stage name |
| `kargo-observer.kutlak.cc/freight` | The promoted Freight name |
