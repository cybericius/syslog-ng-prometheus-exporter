# Build stage
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo 'dev')" \
    -o syslog-ng-prometheus-exporter .

# Runtime stage
FROM scratch

# Import certificates and timezone data
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy binary
COPY --from=builder /build/syslog-ng-prometheus-exporter /syslog-ng-prometheus-exporter

# Expose metrics port
EXPOSE 9577

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/syslog-ng-prometheus-exporter", "-health-check"]

USER 65534:65534

ENTRYPOINT ["/syslog-ng-prometheus-exporter"]
