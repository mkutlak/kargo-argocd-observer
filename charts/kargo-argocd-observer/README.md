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
| `image.repository` | `ghcr.io/mkutlak/kargo-argocd-observer` | Controller image repository |
| `image.tag` | `""` | Image tag; defaults to `Chart.appVersion` |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Pull secrets for private registries |
| `replicaCount` | `2` | Replicas; active/standby via leader election |
| `dryRun` | `true` | Observe-only: log/emit Events instead of creating Promotions; flip to `false` to act |
| `observeMode` | `opt-in` | Scoped rollout: `opt-in` only acts on Applications annotated `kargo-observer.kutlak.cc/observe: "true"`; `opt-out` acts on all annotated Applications unless ignored |
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
