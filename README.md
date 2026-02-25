# syslog-ng Prometheus Exporter

A high-performance, production-grade Prometheus exporter for syslog-ng metrics.

## Features

- **Background metric collection** - Fetches metrics on configurable intervals, not per-request
- **Atomic caching** - Lock-free reads during Prometheus scrapes
- **Persistent connection** - Reuses Unix socket with automatic reconnection
- **Self-instrumentation** - Exposes exporter health metrics
- **Graceful shutdown** - Proper signal handling for containers
- **Kubernetes-ready** - Health and readiness probes
- **Single binary** - No runtime dependencies, deploys anywhere
- **IPv4/IPv6 support** - Proper address parsing

## Performance Comparison

| Metric | Python Exporter | Go Exporter |
|--------|-----------------|-------------|
| Memory usage | ~30-50 MB | ~8-12 MB |
| Startup time | ~200ms | <10ms |
| Requests/sec | ~100 | ~10,000+ |
| Socket connections/scrape | 1 (always new) | 0 (cached) |
| CPU per scrape | High (blocking) | Minimal |

## Installation

### From Source

```bash
# Clone and build
git clone https://github.com/cybericius/syslog-ng-prometheus-exporter
cd syslog-ng-prometheus-exporter
make build

# Run
./syslog-ng-prometheus-exporter --socket-path=/var/lib/syslog-ng/syslog-ng.ctl
```

### Docker

```bash
# Build
docker build -t syslog-ng-prometheus-exporter .

# Run (mount syslog-ng socket)
docker run -d \
  -p 9577:9577 \
  -v /var/lib/syslog-ng/syslog-ng.ctl:/var/lib/syslog-ng/syslog-ng.ctl:ro \
  syslog-ng-prometheus-exporter
```

### Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: syslog-ng-prometheus-exporter
spec:
  replicas: 1
  selector:
    matchLabels:
      app: syslog-ng-prometheus-exporter
  template:
    metadata:
      labels:
        app: syslog-ng-prometheus-exporter
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9577"
    spec:
      containers:
      - name: syslog-ng-prometheus-exporter
        image: syslog-ng-prometheus-exporter:latest
        args:
        - --socket-path=/var/lib/syslog-ng/syslog-ng.ctl
        - --cache-ttl=15s
        ports:
        - containerPort: 9577
          name: metrics
        livenessProbe:
          httpGet:
            path: /health/live
            port: 9577
          initialDelaySeconds: 5
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health/ready
            port: 9577
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          limits:
            memory: 32Mi
            cpu: 100m
          requests:
            memory: 16Mi
            cpu: 10m
        volumeMounts:
        - name: syslog-socket
          mountPath: /var/lib/syslog-ng
          readOnly: true
      volumes:
      - name: syslog-socket
        hostPath:
          path: /var/lib/syslog-ng
          type: Directory
```

## Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--listen-address` | `:9577` | Address to listen on |
| `--socket-path` | `/var/lib/syslog-ng/syslog-ng.ctl` | Path to syslog-ng control socket |
| `--stats-with-legacy` | `false` | Include legacy statistics |
| `--scrape-timeout` | `10s` | Timeout for scraping syslog-ng |
| `--cache-ttl` | `10s` | Cache TTL for metrics |
| `--log-level` | `info` | Log level (debug, info, warn, error) |
| `--log-format` | `text` | Log format (text, json) |
| `--health-check` | `false` | Run health check against running instance and exit |

### Environment Variables

All flags can be set via environment variables with `SYSLOGNG_EXPORTER_` prefix:

```bash
SYSLOGNG_EXPORTER_LISTEN_ADDRESS=:9577
SYSLOGNG_EXPORTER_SOCKET_PATH=/run/syslog-ng.ctl
SYSLOGNG_EXPORTER_CACHE_TTL=15s
```

## Endpoints

| Endpoint | Description |
|----------|-------------|
| `/` | Landing page with links |
| `/metrics` | Combined syslog-ng + exporter metrics |
| `/exporter-metrics` | Exporter self-metrics only |
| `/health` | Health check (always 200 if running) |
| `/health/live` | Liveness probe (Kubernetes) |
| `/health/ready` | Readiness probe (Kubernetes) |

## Exporter Metrics

The exporter exposes its own metrics under the `syslogng_exporter_` namespace:

| Metric | Type | Description |
|--------|------|-------------|
| `syslogng_exporter_up` | Gauge | Whether syslog-ng is reachable (0/1) |
| `syslogng_exporter_scrape_duration_seconds` | Histogram | Time to scrape syslog-ng |
| `syslogng_exporter_scrape_errors_total` | Counter | Total scrape errors |
| `syslogng_exporter_cache_hits_total` | Counter | Cache hits |
| `syslogng_exporter_cache_misses_total` | Counter | Cache misses (fresh scrapes) |
| `syslogng_exporter_last_scrape_timestamp_seconds` | Gauge | Unix timestamp of last scrape |
| `syslogng_exporter_metrics_count` | Gauge | Number of metrics from syslog-ng |
| `syslogng_exporter_socket_reconnects_total` | Counter | Socket reconnection count |

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                      Go Exporter                                │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  ┌──────────────┐    ┌─────────────────────────────────────┐   │
│  │ HTTP Server  │    │         Collector                   │   │
│  │              │    │                                     │   │
│  │ GET /metrics ├───►│  ┌─────────────────────────────┐   │   │
│  │              │    │  │    Atomic Cache              │   │   │
│  │              │◄───┤  │    (sync/atomic.Pointer)     │   │   │
│  │              │    │  │                              │   │   │
│  └──────────────┘    │  │  - metrics []byte            │   │   │
│                      │  │  - timestamp time.Time       │   │   │
│                      │  │  - count int                 │   │   │
│                      │  └──────────────▲──────────────┘   │   │
│                      │                 │                   │   │
│                      │  ┌──────────────┴──────────────┐   │   │
│                      │  │  Background Scraper         │   │   │
│                      │  │  (on-demand with TTL)       │   │   │
│                      │  └──────────────┬──────────────┘   │   │
│                      │                 │                   │   │
│                      └─────────────────┼───────────────────┘   │
│                                        │                       │
│  ┌─────────────────────────────────────▼───────────────────┐   │
│  │              Persistent Unix Socket                     │   │
│  │              (with auto-reconnect)                      │   │
│  └─────────────────────────────────────┬───────────────────┘   │
│                                        │                       │
└────────────────────────────────────────┼───────────────────────┘
                                         │
                                         ▼
                              ┌──────────────────────┐
                              │   syslog-ng          │
                              │   Control Socket     │
                              │   (.ctl)             │
                              └──────────────────────┘
```

## Caching Strategy

The exporter uses a cache-on-demand strategy:

1. **On scrape request**: Check if cache is valid (within TTL)
2. **Cache hit**: Return immediately (lock-free atomic read)
3. **Cache miss**: Fetch from syslog-ng, update cache, return
4. **On error**: Return stale cache if available, log warning

This ensures:
- Multiple Prometheus instances don't hammer syslog-ng
- Fast response times for scrapes
- Graceful degradation on errors

## Systemd Service

```ini
[Unit]
Description=syslog-ng Prometheus Exporter
After=syslog-ng.service
Wants=syslog-ng.service

[Service]
Type=simple
User=nobody
Group=nogroup
ExecStart=/usr/local/bin/syslog-ng-prometheus-exporter \
    --socket-path=/var/lib/syslog-ng/syslog-ng.ctl \
    --log-format=json
Restart=always
RestartSec=5

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadOnlyPaths=/
ReadWritePaths=/var/lib/syslog-ng

[Install]
WantedBy=multi-user.target
```

## Requirements

- syslog-ng OSE 4.1+ or PE 7.0.34+ (with `STATS PROMETHEUS` support)
- Access to syslog-ng control socket

## License

Source-Available License - free to use with syslog-ng OSE only. Use with syslog-ng PE or commercial derivatives requires written permission from the copyright holder. See [LICENSE](LICENSE) for details.
