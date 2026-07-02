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

The chart installs in observe-only mode (`dryRun=true`). Watch the logs and Stage
Events, then let the controller create real Promotions:

```sh
helm upgrade kargo-argocd-observer oci://ghcr.io/mkutlak/charts/kargo-argocd-observer \
  --namespace kargo-observer --reuse-values --set dryRun=false
```

## Values

| Key | Default | Description |
|---|---|---|
| `nameOverride` | `""` | Override the chart name |
| `fullnameOverride` | `""` | Override the fully qualified resource name |
| `image.repository` | `ghcr.io/mkutlak/kargo-argocd-observer` | Controller image repository |
| `image.tag` | `""` | Image tag; defaults to `Chart.appVersion` |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Pull secrets for private registries |
| `replicaCount` | `2` | Replicas; active/standby via leader election |
| `dryRun` | `true` | Observe-only: log/emit Events instead of creating Promotions; flip to `false` to act |
| `leaderElect` | `true` | Enable leader election (required with >1 replica) |
| `extraArgs` | `[]` | Extra command-line arguments for the controller |
| `serviceAccount.create` | `true` | Create the ServiceAccount |
| `serviceAccount.name` | `""` | ServiceAccount name; defaults to the chart fullname |
| `rbac.create` | `true` | Create ClusterRole/Binding and leader-election Role/Binding |
| `resources` | requests `50m`/`64Mi`, limits `500m`/`256Mi` | Container resources |
| `podAnnotations` | `{}` | Extra pod annotations |
| `podLabels` | `{}` | Extra pod labels |
| `nodeSelector` | `{}` | Node selector |
| `tolerations` | `[]` | Tolerations |
| `affinity` | `{}` | Affinity rules |
| `priorityClassName` | `""` | Pod priority class |
| `metrics.service.port` | `8080` | Metrics Service port |
| `metrics.serviceMonitor.enabled` | `false` | Create a ServiceMonitor (requires Prometheus Operator CRDs) |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval |
| `metrics.serviceMonitor.additionalLabels` | `{}` | Extra ServiceMonitor labels |
| `metrics.prometheusRule.enabled` | `false` | Create a PrometheusRule with the controller's alerts |
| `metrics.prometheusRule.additionalLabels` | `{}` | Extra PrometheusRule labels |
