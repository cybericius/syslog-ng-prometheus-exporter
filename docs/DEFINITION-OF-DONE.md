# Definition of Done

Copyright (c) 2024-2026 Akos Zalavary. All rights reserved.
See LICENSE for terms.

Checklist for all changes to the syslog-ng Prometheus exporter.

## Code Quality

- [ ] Go builds clean: `go build ./...`
- [ ] No new lint warnings: `golangci-lint run ./...`
- [ ] Tests pass with race detector: `go test -race -count=1 ./...`
- [ ] Test coverage does not decrease

## Handler Checklist

- [ ] Proper HTTP status codes (200 for success, 404 for unknown paths, 503 for unavailable)
- [ ] Content-Type header set on all responses (including error responses)
- [ ] Error responses use consistent format
- [ ] Scrape timeout applied via context

## Metrics Checklist

- [ ] All self-instrumentation metrics update correctly on success and failure paths
- [ ] Cache hit/miss counters increment accurately
- [ ] `up` gauge reflects actual syslog-ng reachability
- [ ] Socket reconnect counter tracks reconnections
- [ ] Histogram (scrape duration) is exposed on `/metrics` endpoint

## Configuration

- [ ] All flags configurable via `SYSLOGNG_EXPORTER_` environment variables
- [ ] CLI flags take precedence over environment variables
- [ ] Defaults are sensible for production use

## Security

- [ ] No secrets or credentials in logs, metrics, or error messages
- [ ] Graceful degradation on socket errors (stale cache, not crash)
- [ ] Timeouts on all socket and HTTP operations
