# kargo-argocd-observer Helm chart

Installs [kargo-argocd-observer](https://github.com/mkutlak/kargo-argocd-observer) — a
Kubernetes controller that reconciles Kargo Stage state with what ArgoCD actually
deployed. See the [repository README](https://github.com/mkutlak/kargo-argocd-observer#readme)
for how the controller works.

## Install

From the OCI registry:

```sh
helm install kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
  --namespace kargo-observer --create-namespace
```

From a local checkout:

```sh
helm install kargo-argocd-observer ./charts/kargo-argocd-observer \
  --namespace kargo-observer --create-namespace
```

The chart installs scoped to a safe rollout: `observeMode=opt-in` (only Applications
annotated `kargo-observer.kutlak.cc/observe: "true"` are considered) and `dryRun=true`
(intended Promotions are logged and emitted as Events, never created). Recommended
rollout:

1. Install with the chart defaults and annotate one Application with
   `kargo-observer.kutlak.cc/observe: "true"`.
2. Watch the logs and Stage Events for the Promotions it *would* create.
3. Once they match expectations, let it act for real:

   ```sh
   helm upgrade kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
     --namespace kargo-observer --reuse-values --set dryRun=false
   ```
4. When you're confident the controller should watch every annotated Application
   (unless ignored), widen scope:

   ```sh
   helm upgrade kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
     --namespace kargo-observer --reuse-values --set observeMode=opt-out
   ```

## Values

| Key | Default | Description |
|---|---|---|
| `nameOverride` | `""` | Override the chart name |
| `fullnameOverride` | `""` | Override the fully qualified resource name |
| `commonLabels` | `{}` | Labels merged onto every resource this chart creates |
| `commonAnnotations` | `{}` | Annotations merged onto every resource's metadata; resource-specific annotations (e.g. `service.annotations`) win on key conflicts |
| `image.repository` | `ghcr.io/mkutlak/kargo-argocd-observer` | Controller image repository |
| `image.tag` | `""` | Image tag; defaults to `Chart.appVersion` |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Pull secrets for private registries |
| `replicaCount` | `2` | Replicas; active/standby via leader election |
| `dryRun` | `true` | Observe-only: log/emit Events instead of creating Promotions; flip to `false` to act |
| `observeMode` | `opt-in` | Scoped rollout: `opt-in` only acts on Applications annotated `kargo-observer.kutlak.cc/observe: "true"`; `opt-out` acts on all annotated Applications unless ignored |
| `leaderElect` | `true` | Enable leader election (required with >1 replica) |
| `syncPeriod` | `10m` | Periodic resync backstop — how often all Applications are re-reconciled; must be positive |
| `extraArgs` | `[]` | Extra command-line arguments appended after the templated ones |
| `logging.level` | `""` | `--zap-log-level`; omitted when empty |
| `logging.encoder` | `""` | `--zap-encoder`; omitted when empty |
| `logging.devMode` | `""` | `--zap-devel`; omitted when empty |
| `serviceAccount.create` | `true` | Create the ServiceAccount |
| `serviceAccount.name` | `""` | ServiceAccount name; defaults to the chart fullname |
| `serviceAccount.annotations` | `{}` | Extra ServiceAccount annotations |
| `serviceAccount.automountServiceAccountToken` | `true` | Automount the ServiceAccount token into the pod |
| `rbac.create` | `true` | Create the ClusterRole/ClusterRoleBinding |
| `rbac.extraRules` | `[]` | Extra PolicyRules appended to the ClusterRole |
| `rbac.leaderElection.create` | `true` | Create the namespaced leader-election Role/RoleBinding (independent of `rbac.create`) |
| `resources` | requests `50m`/`64Mi`, limits `500m`/`256Mi` | Container resources |
| `podAnnotations` | `{}` | Extra pod annotations |
| `podLabels` | `{}` | Extra pod labels |
| `podSecurityContext` | nonroot UID/GID `65532`, seccomp `RuntimeDefault` | Pod security context; numeric IDs let kubelet verify `runAsNonRoot` against the distroless image |
| `containerSecurityContext` | no privilege escalation, read-only rootfs, all capabilities dropped | Container security context |
| `nodeSelector` | `{}` | Node selector |
| `tolerations` | `[]` | Tolerations |
| `affinity` | `{}` | Affinity rules |
| `topologySpreadConstraints` | `[]` | Topology spread constraints, e.g. to spread the active/standby pair across zones |
| `priorityClassName` | `""` | Pod priority class |
| `terminationGracePeriodSeconds` | unset | Pod termination grace period |
| `revisionHistoryLimit` | unset | Deployment revision history limit |
| `strategy` | `{}` | Deployment update strategy (RollingUpdate maxSurge/maxUnavailable) |
| `extraEnvVars` | `[]` | Extra container env vars |
| `envFrom` | `[]` | Extra container `envFrom` sources |
| `extraVolumes` | `[]` | Extra pod volumes, e.g. a custom CA bundle |
| `extraVolumeMounts` | `[]` | Extra container volume mounts |
| `livenessProbe.path` | `/healthz` | Liveness probe path |
| `livenessProbe.port` | `health` | Liveness probe port |
| `livenessProbe.initialDelaySeconds` / `periodSeconds` / `timeoutSeconds` / `failureThreshold` / `successThreshold` | unset | Liveness probe timing; only rendered when set |
| `readinessProbe.path` | `/readyz` | Readiness probe path |
| `readinessProbe.port` | `health` | Readiness probe port |
| `readinessProbe.initialDelaySeconds` / `periodSeconds` / `timeoutSeconds` / `failureThreshold` / `successThreshold` | unset | Readiness probe timing; only rendered when set |
| `startupProbe` | `{}` | Startup probe; disabled unless a field is set. `path`/`port` default to `/healthz`/`health` |
| `service.type` | `ClusterIP` | Service type |
| `service.annotations` | `{}` | Extra Service annotations, e.g. `prometheus.io/scrape` |
| `service.labels` | `{}` | Extra Service labels |
| `metrics.port` | `8080` | Container port for `--metrics-bind-address` |
| `metrics.service.port` | `8080` | Metrics Service port (defaults to the same value as `metrics.port`) |
| `health.port` | `8081` | Container port for `--health-probe-bind-address` |
| `podDisruptionBudget.enabled` | `true` | Create a PodDisruptionBudget for the active/standby pair |
| `podDisruptionBudget.minAvailable` | `1` | Minimum available pods; mutually exclusive with `maxUnavailable` |
| `podDisruptionBudget.maxUnavailable` | unset | Overrides `minAvailable` when set |
| `metrics.serviceMonitor.enabled` | `false` | Create a ServiceMonitor (requires Prometheus Operator CRDs) |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval |
| `metrics.serviceMonitor.scrapeTimeout` | `""` | Per-scrape timeout; omitted when empty |
| `metrics.serviceMonitor.relabelings` | `[]` | Target relabelings |
| `metrics.serviceMonitor.metricRelabelings` | `[]` | Metric relabelings |
| `metrics.serviceMonitor.namespaceSelector` | `{}` | Restrict scraped Service namespaces |
| `metrics.serviceMonitor.honorLabels` | `false` | Honor labels scraped from the target over Prometheus's own |
| `metrics.serviceMonitor.additionalLabels` | `{}` | Extra ServiceMonitor labels, e.g. to match your Prometheus Operator's `serviceMonitorSelector` |
| `metrics.prometheusRule.enabled` | `false` | Create a PrometheusRule with the controller's alerts |
| `metrics.prometheusRule.additionalLabels` | `{}` | Extra PrometheusRule labels |
| `metrics.prometheusRule.freightMissing.{enabled,for,threshold,severity}` | `true`, `15m`, `0`, `warning` | `KargoObserverFreightMissing` alert |
| `metrics.prometheusRule.promotionCreateErrors.{enabled,window,threshold,severity,for}` | `true`, `30m`, `0`, `warning`, unset | `KargoObserverPromotionCreateErrors` alert |
| `metrics.prometheusRule.absent.{enabled,for,severity}` | `true`, `15m`, `warning` | `KargoObserverAbsent` alert |
| `metrics.prometheusRule.extraRules` | `[]` | Custom alerting rules appended to the same rule group |

**Follow-ups needing a Go change (not configurable from this chart):** the 10-minute
controller cache resync period, and true namespace-scoped RBAC (`rbac.extraRules` only
lets you extend/restrict the existing cluster-wide grant, not scope watches to specific
namespaces).
