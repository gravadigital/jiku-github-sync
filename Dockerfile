# syntax=docker/dockerfile:1
# jiku-github-sync: static Go binary on distroless, non-root.

FROM golang:1.25-alpine AS build
WORKDIR /src
# go.sum is optional while the module has no dependencies.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/jiku-github-sync ./cmd/jiku-github-sync

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="jiku-github-sync" \
      org.opencontainers.image.description="Jiku <-> GitHub (Issues + Projects) connector" \
      org.opencontainers.image.vendor="Grava Digital"
COPY --from=build /out/jiku-github-sync /usr/local/bin/jiku-github-sync
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 CMD ["/usr/local/bin/jiku-github-sync", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/jiku-github-sync"]
CMD ["serve"]
