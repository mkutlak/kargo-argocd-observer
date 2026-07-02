<!-- Last reviewed: 2026-07-02. Quarterly or after major dependency upgrades. -->

# kargo-argocd-observer

Kubernetes controller that reconciles Kargo Stage state with what ArgoCD has actually
deployed — so hand-committed image-tag bumps show up in Kargo instead of leaving Stages
stale. See `README.md` for the full design.

## Tech Stack

- Go (see `go.mod` for version) — `controller-runtime` + `client-go` + `apimachinery`
- **Unstructured clients only** for ArgoCD `Application` and Kargo `Stage`/`Freight`/
  `Warehouse`/`Promotion` CRDs — no `argo-cd` or `kargo` Go module imports. This keeps
  `go.mod` light and decoupled from Argo/Kargo's internal API versioning.
- golangci-lint v2
- `CGO_ENABLED=0` static builds; distroless/static-nonroot runtime image

## Essential Commands

Run `mise tasks` for the full task list. The toolchain (Go, golangci-lint) is provisioned
with `mise install`. Key commands:

```
# Quality gate
mise run check          # fmt + vet + lint + test

# Dev
mise run build           # build the observer binary
mise run build-static    # static CGO_ENABLED=0 build (matches Docker)
mise run run             # build + run locally

# Test
mise run test            # go test -race ./...
mise run test-cover      # test with HTML coverage report

# Lint / format
mise run fmt
mise run vet
mise run lint
mise run tidy            # go mod tidy

# Build & Release
mise run docker:build    # build the container image
mise run clean           # remove build artifacts
```

## Architecture

| Path | Description |
|---|---|
| `cmd/observer/` | Manager entrypoint — flag parsing, leader election, health probes, metrics registration |
| `internal/controller/` | Application reconciler and annotation predicate; unstructured helpers for reading deployed images, matching Freight, and building Promotions. Tests alongside source |
| `internal/version/` | `Version`/`BuildDate`/`BuildRef` vars, injected at link time via `-ldflags` |
| `deploy/` | Plain Kubernetes manifests — namespace, ServiceAccount/RBAC, Deployment, Service, ServiceMonitor, PrometheusRule |

## Agent Routing

- Controller/reconciler changes → `executor` (sonnet); Go review → `code-reviewer` (opus)
- Kubernetes manifests/RBAC (`deploy/`) → `executor` (sonnet); architecture → `architect` (opus)
- Bug investigation → `debugger` (sonnet) first, then `executor`
- `controller-runtime`/`client-go`/ArgoCD/Kargo API docs → `document-specialist` with Context7 MCP

## Development Instructions

- Delegate specialized or tool-heavy work to the most appropriate agent.
- Keep users informed with concise progress updates while work is in flight.
- Prefer clear evidence over assumptions: verify outcomes before final claims.
- Choose the lightest-weight path that preserves quality (direct action, or agent).
- Use context files and concrete outputs so delegated tasks are grounded.
- Consult official documentation before implementing with SDKs, frameworks, or APIs - context7 MCP.

## Testing

- Write tests first (TDD) — make them fail, then implement.
- Only update existing tests with explicit permission.
- Use `testing` stdlib plus the controller-runtime fake client
  (`sigs.k8s.io/controller-runtime/pkg/client/fake`) — hermetic, no live cluster or
  `envtest` required.
- Test files alongside source: `foo_test.go` next to `foo.go`.
