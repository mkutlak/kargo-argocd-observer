# syntax=docker/dockerfile:1

###### Go Build Stage
# --platform=$BUILDPLATFORM keeps this stage native on the CI runner and
# cross-compiles via GOOS/GOARCH — without it, the arm64 half of a multi-arch
# build runs the whole Go toolchain under QEMU emulation (20-45 min vs ~2 min).
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION=dev
ARG BUILD_DATE=unknown
ARG BUILD_REF=unknown

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w \
      -X github.com/mkutlak/kargo-argocd-observer/internal/version.Version=${BUILD_VERSION} \
      -X github.com/mkutlak/kargo-argocd-observer/internal/version.BuildDate=${BUILD_DATE} \
      -X github.com/mkutlak/kargo-argocd-observer/internal/version.BuildRef=${BUILD_REF}" \
    -o /out/observer ./cmd/observer

###### Final Image
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/observer /observer

USER nonroot:nonroot

ENTRYPOINT ["/observer"]
