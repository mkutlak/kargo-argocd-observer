# syntax=docker/dockerfile:1

###### Go Build Stage
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG BUILD_VERSION=dev
ARG BUILD_DATE=unknown
ARG BUILD_REF=unknown

RUN CGO_ENABLED=0 go build \
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
