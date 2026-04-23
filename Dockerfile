# Use BUILDPLATFORM so the build tools always run on the host arch (fast).
# TARGETARCH / TARGETOS are injected by docker buildx for the output image.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" \
    -o /dpivot ./cmd/dpivot

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /dpivot /dpivot

# Default environment (overridden by generate-injected values)
ENV DPIVOT_CONTROL_PORT=9900

EXPOSE 9900

ENTRYPOINT ["/dpivot", "proxy"]
