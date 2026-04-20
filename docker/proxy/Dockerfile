FROM golang:1.22-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /dpivot ./cmd/dpivot

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /dpivot /dpivot

# Default environment (overridden by generate-injected values)
ENV DPIVOT_CONTROL_PORT=9900

EXPOSE 9900

ENTRYPOINT ["/dpivot", "proxy"]
