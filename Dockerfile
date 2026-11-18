###### Go Build Stage
# Base images are pinned by digest (the tag is only a label for humans and
# for Dependabot, which bumps both).
# --platform=$BUILDPLATFORM keeps this stage native on the CI runner and
# cross-compiles via GOOS/GOARCH — without it, the arm64 half of a multi-arch
# build runs the whole Go toolchain under QEMU emulation (20-45 min vs ~2 min).
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION=dev
ARG BUILD_DATE=unknown
ARG BUILD_REF=unknown

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w \
      -X github.com/mkutlak/kargo-argocd-observer/internal/version.Version=${BUILD_VERSION} \
      -X github.com/mkutlak/kargo-argocd-observer/internal/version.BuildDate=${BUILD_DATE} \
      -X github.com/mkutlak/kargo-argocd-observer/internal/version.BuildRef=${BUILD_REF}" \
    -o /out/observer ./cmd/observer

###### Final Image
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931

COPY --from=build /out/observer /observer

# Numeric UID/GID, not the symbolic "nonroot" the base image's own USER would
# otherwise be overwritten with — kubelet cannot resolve a username to a UID, so
# a symbolic USER makes `runAsNonRoot: true` fail admission with "image has
# non-numeric user (nonroot), cannot verify user is non-root".
USER 65532:65532

ENTRYPOINT ["/observer"]
