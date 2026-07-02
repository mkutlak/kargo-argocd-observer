---
paths:
  - "**/*.go"
---

## Go Controller Rules

- Unstructured clients only for external CRDs (ArgoCD `Application`, Kargo `Stage`/
  `Freight`/`Warehouse`/`Promotion`) — never import the `argo-cd` or `kargo` Go modules
- No third-party dependencies without explicit approval — prefer stdlib
- `CGO_ENABLED=0` for production builds
- All errors returned, not panicked
- Structured logging via `logf.FromContext` (controller-runtime's contextual logger),
  not `fmt.Println`/`log.Printf`
- Test files alongside source: `foo_test.go` next to `foo.go`
- Use `testing` stdlib plus the controller-runtime fake client; no live cluster, no
  `envtest`
- Run `mise run check` (fmt + vet + lint + test) before claiming done
