# Development

## Toolchain

The toolchain (Go, golangci-lint, helm, yq) is managed with
[mise](https://mise.jdx.dev). Run `mise install` once, then `mise tasks` for the full
task list. Key commands:

```
# Quality gate
mise run check           # fmt + vet + lint + test

# Dev
mise run build           # build the observer binary
mise run build-static    # static CGO_ENABLED=0 build (matches Docker)
mise run run             # build + run locally

# Test
mise run test            # go test -race ./...
mise run test-cover      # test with HTML coverage report

# Helm chart
mise run helm:lint
mise run helm:template

# Build
mise run docker:build    # build the container image
```

## Repository layout

| Path | Description |
|---|---|
| `cmd/observer/` | Manager entrypoint — flag parsing, leader election, health probes, metrics registration |
| `internal/controller/` | Application reconciler and annotation predicate; unstructured helpers for reading deployed images, matching Freight, and building Promotions |
| `internal/version/` | `Version`/`BuildDate`/`BuildRef` vars, injected at link time via `-ldflags` |
| `charts/kargo-argocd-observer/` | Helm chart (published as an OCI artifact) |
| `docs/` | This documentation |

## Testing

- Tests use the Go `testing` stdlib plus the controller-runtime fake client
  (`sigs.k8s.io/controller-runtime/pkg/client/fake`) — hermetic, no live cluster or
  `envtest` required.
- Test files live alongside source: `foo_test.go` next to `foo.go`.
- Run `mise run check` before opening a pull request.
