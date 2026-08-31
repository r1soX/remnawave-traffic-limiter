# Keep this aligned with go.mod.  These tags are available on Docker Hub and
# are intentionally conservative for repeatable production builds.
FROM golang:1.22-alpine3.20 AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bin/remnawave-traffic-limiter ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata su-exec \
    && addgroup -S limiter \
    && adduser -S -D -H -G limiter limiter \
    && mkdir -p /data \
    && chown limiter:limiter /data
WORKDIR /app
COPY --from=builder /bin/remnawave-traffic-limiter /app/remnawave-traffic-limiter
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod 0755 /app/docker-entrypoint.sh
ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:${PORT}/health || exit 1
ENTRYPOINT ["/app/docker-entrypoint.sh"]
