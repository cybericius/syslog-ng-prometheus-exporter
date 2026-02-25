package main

import (
	"context"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// mockSyslogNG creates a mock syslog-ng control socket for testing.
func mockSyslogNG(t testing.TB, response string) (string, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "sng-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	socketPath := filepath.Join(dir, "syslog-ng.ctl")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("failed to create socket: %v", err)
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}

			listener.(*net.UnixListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
			conn, err := listener.Accept()
			if err != nil {
				continue
			}

			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				n, _ := c.Read(buf)
				if n > 0 {
					c.Write([]byte(response + ".\n"))
				}
			}(conn)
		}
	}()

	cleanup := func() {
		close(done)
		listener.Close()
		wg.Wait()
		os.RemoveAll(dir)
	}

	return socketPath, cleanup
}

// newTestCollector creates a Collector with a fresh prometheus registry.
func newTestCollector(t *testing.T, socketPath string, cacheTTL time.Duration) (*Collector, *exporterMetrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := newExporterMetrics(reg)
	cfg := &Config{
		SocketPath:    socketPath,
		ScrapeTimeout: 5 * time.Second,
		CacheTTL:      cacheTTL,
	}
	logger := setupLogger(&Config{LogLevel: "error"})
	return NewCollector(cfg, m, logger), m, reg
}

// newTestServer creates a Server backed by a mock socket with a fresh registry.
func newTestServer(t *testing.T, socketPath string) (*Server, *prometheus.Registry) {
	t.Helper()
	collector, metrics, reg := newTestCollector(t, socketPath, 10*time.Second)
	cfg := &Config{
		ListenAddr:    ":0",
		SocketPath:    socketPath,
		ScrapeTimeout: 5 * time.Second,
		CacheTTL:      10 * time.Second,
	}
	logger := setupLogger(&Config{LogLevel: "error"})
	srv := NewServer(cfg, collector, metrics, logger)
	return srv, reg
}

// getCounterValue extracts the float64 value from a prometheus Counter.
func getCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	m := &dto.Metric{}
	c.(prometheus.Metric).Write(m)
	return m.GetCounter().GetValue()
}

// getGaugeValue extracts the float64 value from a prometheus Gauge.
func getGaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	m := &dto.Metric{}
	g.(prometheus.Metric).Write(m)
	return m.GetGauge().GetValue()
}

// ---------------------------------------------------------------------------
// flagToEnv
// ---------------------------------------------------------------------------

func TestFlagToEnv(t *testing.T) {
	tests := []struct {
		flag string
		want string
	}{
		{"listen-address", "SYSLOGNG_EXPORTER_LISTEN_ADDRESS"},
		{"socket-path", "SYSLOGNG_EXPORTER_SOCKET_PATH"},
		{"cache-ttl", "SYSLOGNG_EXPORTER_CACHE_TTL"},
		{"stats-with-legacy", "SYSLOGNG_EXPORTER_STATS_WITH_LEGACY"},
		{"log-level", "SYSLOGNG_EXPORTER_LOG_LEVEL"},
		{"log-format", "SYSLOGNG_EXPORTER_LOG_FORMAT"},
		{"scrape-timeout", "SYSLOGNG_EXPORTER_SCRAPE_TIMEOUT"},
		{"health-check", "SYSLOGNG_EXPORTER_HEALTH_CHECK"},
	}

	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			got := flagToEnv(tt.flag)
			if got != tt.want {
				t.Errorf("flagToEnv(%q) = %q, want %q", tt.flag, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseConfig
// ---------------------------------------------------------------------------

func TestParseConfig_Defaults(t *testing.T) {
	os.Args = []string{"test"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	cfg := parseConfig()

	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
	}
	if cfg.SocketPath != defaultSocketPath {
		t.Errorf("SocketPath = %q, want %q", cfg.SocketPath, defaultSocketPath)
	}
	if cfg.WithLegacy {
		t.Error("WithLegacy should default to false")
	}
	if cfg.ScrapeTimeout != defaultScrapeTimeout {
		t.Errorf("ScrapeTimeout = %v, want %v", cfg.ScrapeTimeout, defaultScrapeTimeout)
	}
	if cfg.CacheTTL != defaultCacheTTL {
		t.Errorf("CacheTTL = %v, want %v", cfg.CacheTTL, defaultCacheTTL)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "text")
	}
}

func TestParseConfig_Flags(t *testing.T) {
	os.Args = []string{
		"test",
		"--listen-address=:8080",
		"--socket-path=/test/socket.ctl",
		"--stats-with-legacy",
		"--cache-ttl=30s",
		"--scrape-timeout=20s",
		"--log-level=debug",
		"--log-format=json",
	}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	cfg := parseConfig()

	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":8080")
	}
	if cfg.SocketPath != "/test/socket.ctl" {
		t.Errorf("SocketPath = %q, want %q", cfg.SocketPath, "/test/socket.ctl")
	}
	if !cfg.WithLegacy {
		t.Error("expected WithLegacy to be true")
	}
	if cfg.CacheTTL != 30*time.Second {
		t.Errorf("CacheTTL = %v, want 30s", cfg.CacheTTL)
	}
	if cfg.ScrapeTimeout != 20*time.Second {
		t.Errorf("ScrapeTimeout = %v, want 20s", cfg.ScrapeTimeout)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "json")
	}
}

func TestParseConfig_EnvVars(t *testing.T) {
	t.Setenv("SYSLOGNG_EXPORTER_LISTEN_ADDRESS", ":7777")
	t.Setenv("SYSLOGNG_EXPORTER_SOCKET_PATH", "/env/socket.ctl")
	t.Setenv("SYSLOGNG_EXPORTER_CACHE_TTL", "45s")
	t.Setenv("SYSLOGNG_EXPORTER_LOG_LEVEL", "debug")
	t.Setenv("SYSLOGNG_EXPORTER_LOG_FORMAT", "json")
	t.Setenv("SYSLOGNG_EXPORTER_SCRAPE_TIMEOUT", "30s")
	t.Setenv("SYSLOGNG_EXPORTER_STATS_WITH_LEGACY", "true")

	os.Args = []string{"test"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	cfg := parseConfig()

	if cfg.ListenAddr != ":7777" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":7777")
	}
	if cfg.SocketPath != "/env/socket.ctl" {
		t.Errorf("SocketPath = %q, want %q", cfg.SocketPath, "/env/socket.ctl")
	}
	if cfg.CacheTTL != 45*time.Second {
		t.Errorf("CacheTTL = %v, want 45s", cfg.CacheTTL)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "json")
	}
	if cfg.ScrapeTimeout != 30*time.Second {
		t.Errorf("ScrapeTimeout = %v, want 30s", cfg.ScrapeTimeout)
	}
	if !cfg.WithLegacy {
		t.Error("expected WithLegacy to be true via env var")
	}
}

func TestParseConfig_FlagOverridesEnv(t *testing.T) {
	t.Setenv("SYSLOGNG_EXPORTER_LISTEN_ADDRESS", ":7777")
	t.Setenv("SYSLOGNG_EXPORTER_SOCKET_PATH", "/env/socket.ctl")

	os.Args = []string{"test", "--listen-address=:9999"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	cfg := parseConfig()

	if cfg.ListenAddr != ":9999" {
		t.Errorf("CLI flag should override env var: ListenAddr = %q, want %q", cfg.ListenAddr, ":9999")
	}
	// Env var should still apply for flags not set on CLI
	if cfg.SocketPath != "/env/socket.ctl" {
		t.Errorf("Env var should apply when flag not set: SocketPath = %q, want %q", cfg.SocketPath, "/env/socket.ctl")
	}
}

// ---------------------------------------------------------------------------
// setupLogger
// ---------------------------------------------------------------------------

func TestSetupLogger_Levels(t *testing.T) {
	levels := []string{"debug", "info", "warn", "warning", "error", "unknown"}
	for _, level := range levels {
		t.Run(level, func(t *testing.T) {
			logger := setupLogger(&Config{LogLevel: level})
			if logger == nil {
				t.Fatal("setupLogger returned nil")
			}
		})
	}
}

func TestSetupLogger_Formats(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			logger := setupLogger(&Config{LogLevel: "info", LogFormat: format})
			if logger == nil {
				t.Fatal("setupLogger returned nil")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NewCollector
// ---------------------------------------------------------------------------

func TestNewCollector_Command(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newExporterMetrics(reg)
	logger := setupLogger(&Config{LogLevel: "error"})

	t.Run("without legacy", func(t *testing.T) {
		c := NewCollector(&Config{}, m, logger)
		if string(c.command) != "STATS PROMETHEUS\n" {
			t.Errorf("command = %q, want %q", c.command, "STATS PROMETHEUS\n")
		}
	})

	t.Run("with legacy", func(t *testing.T) {
		c := NewCollector(&Config{WithLegacy: true}, m, logger)
		if string(c.command) != "STATS PROMETHEUS WITH_LEGACY\n" {
			t.Errorf("command = %q, want %q", c.command, "STATS PROMETHEUS WITH_LEGACY\n")
		}
	})
}

// ---------------------------------------------------------------------------
// Collector: fetchMetrics
// ---------------------------------------------------------------------------

func TestCollector_FetchMetrics(t *testing.T) {
	expectedMetrics := `# HELP syslogng_input_events_total Total input events
syslogng_input_events_total{id="s_local#0"} 42
syslogng_scratch_buffers_count 14
`
	socketPath, cleanup := mockSyslogNG(t, expectedMetrics)
	defer cleanup()

	collector, _, _ := newTestCollector(t, socketPath, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, count, err := collector.fetchMetrics(ctx)
	if err != nil {
		t.Fatalf("fetchMetrics failed: %v", err)
	}

	// Comment lines should not be counted
	if count != 2 {
		t.Errorf("metric count = %d, want 2 (comments excluded)", count)
	}

	output := string(data)
	if !strings.Contains(output, "syslogng_input_events_total") {
		t.Errorf("output missing syslogng_input_events_total:\n%s", output)
	}
	if !strings.Contains(output, "syslogng_scratch_buffers_count") {
		t.Errorf("output missing syslogng_scratch_buffers_count:\n%s", output)
	}
	// Comment lines should still be in the output data
	if !strings.Contains(output, "# HELP") {
		t.Errorf("output missing comment lines:\n%s", output)
	}
}

func TestCollector_FetchMetrics_ConnectionError(t *testing.T) {
	collector, _, _ := newTestCollector(t, "/nonexistent/socket.ctl", 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, _, err := collector.fetchMetrics(ctx)
	if err == nil {
		t.Fatal("expected error for nonexistent socket, got nil")
	}
	if !strings.Contains(err.Error(), "connect") {
		t.Errorf("error should mention connect: %v", err)
	}
}

func TestCollector_FetchMetrics_Reconnects(t *testing.T) {
	socketPath, cleanup := mockSyslogNG(t, "syslogng_test 1\n")
	defer cleanup()

	collector, metrics, _ := newTestCollector(t, socketPath, 10*time.Second)

	ctx := context.Background()

	// First fetch establishes connection
	_, _, err := collector.fetchMetrics(ctx)
	if err != nil {
		t.Fatalf("first fetch failed: %v", err)
	}

	reconnects := getCounterValue(t, metrics.socketReconnects)
	if reconnects != 1 {
		t.Errorf("reconnects = %v, want 1 after first connection", reconnects)
	}

	// Force disconnect
	collector.disconnect()

	// Next fetch should reconnect (mock closes conn after each response, so
	// the previous connection is dead; disconnect ensures conn is nil)
	_, _, err = collector.fetchMetrics(ctx)
	if err != nil {
		t.Fatalf("fetch after disconnect failed: %v", err)
	}

	reconnects = getCounterValue(t, metrics.socketReconnects)
	if reconnects != 2 {
		t.Errorf("reconnects = %v, want 2 after reconnection", reconnects)
	}
}

func TestCollector_FetchMetrics_EmptyResponse(t *testing.T) {
	// Mock returns just the terminator
	socketPath, cleanup := mockSyslogNG(t, "")
	defer cleanup()

	collector, _, _ := newTestCollector(t, socketPath, 10*time.Second)

	ctx := context.Background()
	data, count, err := collector.fetchMetrics(ctx)
	if err != nil {
		t.Fatalf("fetchMetrics failed: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 for empty response", count)
	}
	if len(data) != 0 {
		t.Errorf("data = %q, want empty", data)
	}
}

// ---------------------------------------------------------------------------
// Collector: scrape
// ---------------------------------------------------------------------------

func TestCollector_Scrape_UpdatesMetrics(t *testing.T) {
	socketPath, cleanup := mockSyslogNG(t, "syslogng_test 1\nsyslogng_test2 2\n")
	defer cleanup()

	collector, metrics, _ := newTestCollector(t, socketPath, 10*time.Second)

	ctx := context.Background()
	err := collector.scrape(ctx)
	if err != nil {
		t.Fatalf("scrape failed: %v", err)
	}

	if v := getGaugeValue(t, metrics.up); v != 1 {
		t.Errorf("up = %v, want 1", v)
	}
	if v := getGaugeValue(t, metrics.metricsCount); v != 2 {
		t.Errorf("metricsCount = %v, want 2", v)
	}
	if v := getCounterValue(t, metrics.cacheMisses); v != 1 {
		t.Errorf("cacheMisses = %v, want 1", v)
	}
	if v := getCounterValue(t, metrics.scrapeErrors); v != 0 {
		t.Errorf("scrapeErrors = %v, want 0", v)
	}

	// Cache should be populated
	cached := collector.cache.Load()
	if cached == nil {
		t.Fatal("cache should be populated after scrape")
	}
	if cached.count != 2 {
		t.Errorf("cached count = %d, want 2", cached.count)
	}
}

func TestCollector_Scrape_ErrorUpdatesMetrics(t *testing.T) {
	collector, metrics, _ := newTestCollector(t, "/nonexistent/socket.ctl", 10*time.Second)

	ctx := context.Background()
	err := collector.scrape(ctx)
	if err == nil {
		t.Fatal("expected scrape error, got nil")
	}

	if v := getGaugeValue(t, metrics.up); v != 0 {
		t.Errorf("up = %v, want 0 after error", v)
	}
	if v := getCounterValue(t, metrics.scrapeErrors); v != 1 {
		t.Errorf("scrapeErrors = %v, want 1", v)
	}
}

// ---------------------------------------------------------------------------
// Collector: getMetrics / cache
// ---------------------------------------------------------------------------

func TestCollector_Cache_HitAndMiss(t *testing.T) {
	socketPath, cleanup := mockSyslogNG(t, "syslogng_test 1\n")
	defer cleanup()

	collector, metrics, _ := newTestCollector(t, socketPath, 1*time.Second)

	ctx := context.Background()

	// First call - cache miss
	_, err := collector.getMetrics(ctx)
	if err != nil {
		t.Fatalf("first getMetrics failed: %v", err)
	}
	if v := getCounterValue(t, metrics.cacheMisses); v != 1 {
		t.Errorf("cacheMisses = %v, want 1", v)
	}

	// Second call - cache hit
	_, err = collector.getMetrics(ctx)
	if err != nil {
		t.Fatalf("second getMetrics failed: %v", err)
	}
	if v := getCounterValue(t, metrics.cacheHits); v != 1 {
		t.Errorf("cacheHits = %v, want 1", v)
	}

	// Wait for cache to expire
	time.Sleep(1100 * time.Millisecond)

	// Third call - cache expired, scrape triggers reconnection via new
	// connection (mock closes connections after each response). The stale
	// cache may be returned if the old conn fails, but we should still
	// get data without error.
	data, err := collector.getMetrics(ctx)
	if err != nil {
		t.Fatalf("third getMetrics failed: %v", err)
	}
	if len(data) == 0 {
		t.Error("third getMetrics returned empty data")
	}
}

func TestCollector_GetMetrics_StaleCacheFallback(t *testing.T) {
	socketPath, cleanup := mockSyslogNG(t, "syslogng_test 42\n")

	collector, _, _ := newTestCollector(t, socketPath, 0) // TTL=0 means always expired

	ctx := context.Background()

	// Prime the cache
	data, err := collector.getMetrics(ctx)
	if err != nil {
		cleanup()
		t.Fatalf("prime failed: %v", err)
	}
	if !strings.Contains(string(data), "42") {
		cleanup()
		t.Fatalf("prime data = %q, want to contain '42'", data)
	}

	// Kill the mock socket so next scrape fails
	cleanup()

	// Next call should return stale cache (no error)
	data, err = collector.getMetrics(ctx)
	if err != nil {
		t.Fatalf("stale cache fallback should not error: %v", err)
	}
	if !strings.Contains(string(data), "42") {
		t.Errorf("stale data = %q, want to contain '42'", data)
	}
}

func TestCollector_GetMetrics_NoCacheReturnsError(t *testing.T) {
	collector, _, _ := newTestCollector(t, "/nonexistent/socket.ctl", 10*time.Second)

	ctx := context.Background()
	_, err := collector.getMetrics(ctx)
	if err == nil {
		t.Fatal("expected error when no cache and socket unreachable")
	}
}

func TestCollector_GetMetrics_ThunderingHerd(t *testing.T) {
	// Use a mock that counts how many connections are made
	var connCount atomic.Int32

	dir, err := os.MkdirTemp("", "sng-herd-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	socketPath := filepath.Join(dir, "syslog-ng.ctl")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			listener.(*net.UnixListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
			conn, err := listener.Accept()
			if err != nil {
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				connCount.Add(1)
				buf := make([]byte, 1024)
				n, _ := c.Read(buf)
				if n > 0 {
					// Add a small delay to simulate work
					time.Sleep(50 * time.Millisecond)
					c.Write([]byte("syslogng_test 1\n.\n"))
				}
			}(conn)
		}
	}()
	defer func() {
		close(done)
		listener.Close()
		wg.Wait()
	}()

	collector, _, _ := newTestCollector(t, socketPath, 0) // TTL=0 so every call is a miss

	ctx := context.Background()

	// Launch many concurrent goroutines
	var clientWg sync.WaitGroup
	const goroutines = 20
	for i := 0; i < goroutines; i++ {
		clientWg.Add(1)
		go func() {
			defer clientWg.Done()
			collector.getMetrics(ctx)
		}()
	}
	clientWg.Wait()

	// With thundering herd prevention, the number of actual scrapes should
	// be far fewer than the number of goroutines.
	// The first goroutine acquires scrapeMu, the rest wait and then get the
	// refreshed cache. With TTL=0 and 50ms delay, we might get a few scrapes
	// but definitely not 20.
	count := int(connCount.Load())
	if count >= goroutines {
		t.Errorf("thundering herd not prevented: %d connections for %d goroutines", count, goroutines)
	}
	t.Logf("thundering herd: %d connections for %d goroutines", count, goroutines)
}

// ---------------------------------------------------------------------------
// Collector: disconnect
// ---------------------------------------------------------------------------

func TestCollector_Disconnect(t *testing.T) {
	socketPath, cleanup := mockSyslogNG(t, "syslogng_test 1\n")
	defer cleanup()

	collector, _, _ := newTestCollector(t, socketPath, 10*time.Second)

	ctx := context.Background()

	// Establish a connection
	_, _, err := collector.fetchMetrics(ctx)
	if err != nil {
		t.Fatalf("fetchMetrics failed: %v", err)
	}

	// Disconnect should not panic
	collector.disconnect()

	// conn should be nil after disconnect
	collector.connMu.Lock()
	isNil := collector.conn == nil
	collector.connMu.Unlock()
	if !isNil {
		t.Error("conn should be nil after disconnect")
	}

	// Double disconnect should be safe
	collector.disconnect()
}

// ---------------------------------------------------------------------------
// HTTP handlers: /metrics
// ---------------------------------------------------------------------------

func TestServer_MetricsEndpoint(t *testing.T) {
	expectedMetrics := `syslogng_input_events_total{id="test"} 100
`
	socketPath, cleanup := mockSyslogNG(t, expectedMetrics)
	defer cleanup()

	srv, _ := newTestServer(t, socketPath)
	srv.ready.Store(true)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()

	srv.metricsHandler(w, req)
	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	output := string(body)
	if !strings.Contains(output, "syslogng_input_events_total") {
		t.Errorf("response missing syslog-ng metrics:\n%s", output)
	}
}

func TestServer_MetricsEndpoint_Error(t *testing.T) {
	srv, _ := newTestServer(t, "/nonexistent/socket.ctl")
	srv.ready.Store(true)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()

	srv.metricsHandler(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestServer_MetricsEndpoint_IncludesExporterMetrics(t *testing.T) {
	socketPath, cleanup := mockSyslogNG(t, "syslogng_test 1\n")
	defer cleanup()

	// This test uses the DefaultRegisterer so exporter metrics show up
	// via DefaultGatherer in metricsHandler. We need a real registry wired
	// to the default gatherer. Since other tests use isolated registries,
	// we just check that the "# Exporter metrics" header is in the output.
	srv, _ := newTestServer(t, socketPath)
	srv.ready.Store(true)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()

	srv.metricsHandler(w, req)
	body, _ := io.ReadAll(w.Result().Body)

	if !strings.Contains(string(body), "# Exporter metrics") {
		t.Errorf("response missing exporter metrics section:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers: /health, /health/live
// ---------------------------------------------------------------------------

func TestServer_HealthHandler(t *testing.T) {
	srv := &Server{config: &Config{}}

	for _, path := range []string{"/health", "/health/live"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()

			srv.healthHandler(w, req)
			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", ct)
			}
			if !strings.Contains(string(body), "OK") {
				t.Errorf("body = %q, want to contain 'OK'", body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers: /ready, /health/ready
// ---------------------------------------------------------------------------

func TestServer_ReadyHandler_Ready(t *testing.T) {
	srv := &Server{config: &Config{}}
	srv.ready.Store(true)

	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()

	srv.readyHandler(w, req)
	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if !strings.Contains(string(body), "READY") {
		t.Errorf("body = %q, want to contain 'READY'", body)
	}
}

func TestServer_ReadyHandler_NotReady(t *testing.T) {
	srv := &Server{config: &Config{}}
	srv.ready.Store(false)

	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()

	srv.readyHandler(w, req)
	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain on 503", ct)
	}
	if !strings.Contains(string(body), "NOT READY") {
		t.Errorf("body = %q, want to contain 'NOT READY'", body)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers: / (root)
// ---------------------------------------------------------------------------

func TestServer_RootHandler(t *testing.T) {
	srv := &Server{config: &Config{}}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	srv.rootHandler(w, req)
	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	output := string(body)
	if !strings.Contains(output, "syslog-ng") {
		t.Errorf("body missing 'syslog-ng':\n%s", output)
	}
	if !strings.Contains(output, "/metrics") {
		t.Errorf("body missing link to /metrics:\n%s", output)
	}
}

func TestServer_RootHandler_404(t *testing.T) {
	srv := &Server{config: &Config{}}

	for _, path := range []string{"/unknown", "/foo/bar", "/metrics/"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()

			srv.rootHandler(w, req)
			resp := w.Result()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status for %s = %d, want 404", path, resp.StatusCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Server lifecycle: Start and Shutdown
// ---------------------------------------------------------------------------

func TestServer_StartAndShutdown(t *testing.T) {
	socketPath, cleanup := mockSyslogNG(t, "syslogng_test 1\n")
	defer cleanup()

	srv, _ := newTestServer(t, socketPath)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	// Wait for server to become ready
	deadline := time.After(3 * time.Second)
	for {
		if srv.ready.Load() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("server did not become ready in time")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Trigger shutdown
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("server returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down in time")
	}

	// After shutdown, ready should be false
	if srv.ready.Load() {
		t.Error("ready should be false after shutdown")
	}
}

// ---------------------------------------------------------------------------
// Full integration: HTTP server with mux routing
// ---------------------------------------------------------------------------

func TestServer_Routing(t *testing.T) {
	socketPath, cleanup := mockSyslogNG(t, "syslogng_test 1\n")
	defer cleanup()

	srv, _ := newTestServer(t, socketPath)
	srv.ready.Store(true)

	// Build the mux the same way Start() does
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.rootHandler)
	mux.HandleFunc("/metrics", srv.metricsHandler)
	mux.HandleFunc("/health", srv.healthHandler)
	mux.HandleFunc("/health/live", srv.healthHandler)
	mux.HandleFunc("/ready", srv.readyHandler)
	mux.HandleFunc("/health/ready", srv.readyHandler)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	tests := []struct {
		path     string
		wantCode int
		wantBody string
	}{
		{"/", http.StatusOK, "syslog-ng"},
		{"/metrics", http.StatusOK, "syslogng_test"},
		{"/health", http.StatusOK, "OK"},
		{"/health/live", http.StatusOK, "OK"},
		{"/ready", http.StatusOK, "READY"},
		{"/health/ready", http.StatusOK, "READY"},
		{"/nonexistent", http.StatusNotFound, ""},
	}

	client := &http.Client{Timeout: 5 * time.Second}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := client.Get(ts.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantCode {
				t.Errorf("GET %s status = %d, want %d", tt.path, resp.StatusCode, tt.wantCode)
			}

			if tt.wantBody != "" {
				body, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(body), tt.wantBody) {
					t.Errorf("GET %s body = %q, want to contain %q", tt.path, body, tt.wantBody)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkCollector_GetMetrics_CacheHit(b *testing.B) {
	socketPath, cleanup := mockSyslogNG(b, "syslogng_test 1\n")
	defer cleanup()

	cfg := &Config{
		SocketPath:    socketPath,
		ScrapeTimeout: 5 * time.Second,
		CacheTTL:      1 * time.Hour,
	}

	reg := prometheus.NewRegistry()
	metrics := newExporterMetrics(reg)
	logger := setupLogger(&Config{LogLevel: "error"})

	collector := NewCollector(cfg, metrics, logger)

	ctx := context.Background()
	collector.getMetrics(ctx)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			collector.getMetrics(ctx)
		}
	})
}

func BenchmarkCollector_GetMetrics_CacheMiss(b *testing.B) {
	socketPath, cleanup := mockSyslogNG(b, "syslogng_test 1\n")
	defer cleanup()

	cfg := &Config{
		SocketPath:    socketPath,
		ScrapeTimeout: 5 * time.Second,
		CacheTTL:      0,
	}

	reg := prometheus.NewRegistry()
	metrics := newExporterMetrics(reg)
	logger := setupLogger(&Config{LogLevel: "error"})

	collector := NewCollector(cfg, metrics, logger)

	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		collector.getMetrics(ctx)
	}
}
