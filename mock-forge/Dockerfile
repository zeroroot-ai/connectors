# Stage 1: build a static binary. The build context is the root of the repo.
FROM ghcr.io/zeroroot-ai/mirror/golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder

# GOTOOLCHAIN=local makes a go.mod that names a newer Go fail the build
# instead of downloading a toolchain (.github#22).
ARG GOTOOLCHAIN=local
ENV GOTOOLCHAIN=${GOTOOLCHAIN}
ARG VERSION=dev

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY mock-forge ./mock-forge
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" -o /out/server ./mock-forge

# Stage 2: distroless static image, non-root. This is a TEST image. It holds
# no credential and it checks none.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS runtime
LABEL ai.zeroroot.image-class="test" \
      org.opencontainers.image.description="TEST IMAGE. A stateful mock of the GitHub and Prometheus APIs for the fix loop exit test. It checks no credential. Never run it in production."
COPY --from=builder /out/server /server
COPY LICENSE /licenses/LICENSE
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/server"]
