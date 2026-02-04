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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// mockSyslogNG creates a mock syslog-ng control socket for testing.
func mockSyslogNG(t testing.TB, response string) (string, func()) {
	if t != nil {
		t.Helper()
	}

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
					// Echo back the mock response
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

func TestCollector_FetchMetrics(t *testing.T) {
	expectedMetrics := `syslogng_input_events_total{id="s_local#0"} 42
syslogng_scratch_buffers_count 14
`

	socketPath, cleanup := mockSyslogNG(t, expectedMetrics)
	defer cleanup()

	cfg := &Config{
		SocketPath:    socketPath,
		ScrapeTimeout: 5 * time.Second,
		CacheTTL:      10 * time.Second,
	}

	reg := prometheus.NewRegistry()
	metrics := newExporterMetrics(reg)
	logger := setupLogger(&Config{LogLevel: "error"})

	collector := NewCollector(cfg, metrics, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, count, err := collector.fetchMetrics(ctx)
	if err != nil {
		t.Fatalf("fetchMetrics failed: %v", err)
	}

	if count != 2 {
		t.Errorf("expected 2 metrics, got %d", count)
	}

	if !strings.Contains(string(data), "syslogng_input_events_total") {
		t.Errorf("expected metrics to contain syslogng_input_events_total, got: %s", data)
	}
}

func TestCollector_Cache(t *testing.T) {
	socketPath, cleanup := mockSyslogNG(t, "syslogng_test 1\n")
	defer cleanup()

	cfg := &Config{
		SocketPath:    socketPath,
		ScrapeTimeout: 5 * time.Second,
		CacheTTL:      1 * time.Second,
	}

	reg := prometheus.NewRegistry()
	metrics := newExporterMetrics(reg)
	logger := setupLogger(&Config{LogLevel: "error"})

	collector := NewCollector(cfg, metrics, logger)

	ctx := context.Background()

	// First call - should miss cache
	_, err := collector.getMetrics(ctx)
	if err != nil {
		t.Fatalf("first getMetrics failed: %v", err)
	}

	// Second call - should hit cache
	_, err = collector.getMetrics(ctx)
	if err != nil {
		t.Fatalf("second getMetrics failed: %v", err)
	}

	// Wait for cache to expire
	time.Sleep(1100 * time.Millisecond)

	// Third call - should miss cache again
	_, err = collector.getMetrics(ctx)
	if err != nil {
		t.Fatalf("third getMetrics failed: %v", err)
	}
}

func TestServer_MetricsEndpoint(t *testing.T) {
	expectedMetrics := `syslogng_input_events_total{id="test"} 100
`
	socketPath, cleanup := mockSyslogNG(t, expectedMetrics)
	defer cleanup()

	cfg := &Config{
		ListenAddr:    ":0",
		SocketPath:    socketPath,
		ScrapeTimeout: 5 * time.Second,
		CacheTTL:      10 * time.Second,
	}

	reg := prometheus.NewRegistry()
	metrics := newExporterMetrics(reg)
	logger := setupLogger(&Config{LogLevel: "error"})

	collector := NewCollector(cfg, metrics, logger)
	server := NewServer(cfg, collector, metrics, logger)
	server.ready.Store(true)

	// Create test request
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()

	server.metricsHandler(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	if !strings.Contains(string(body), "syslogng_input_events_total") {
		t.Errorf("response should contain syslog-ng metrics: %s", body)
	}
}

func TestServer_HealthEndpoints(t *testing.T) {
	cfg := &Config{}
	server := &Server{config: cfg}
	server.ready.Store(true)

	tests := []struct {
		name     string
		path     string
		handler  func(http.ResponseWriter, *http.Request)
		wantCode int
		wantBody string
	}{
		{
			name:     "health",
			path:     "/health",
			handler:  server.healthHandler,
			wantCode: http.StatusOK,
			wantBody: "OK",
		},
		{
			name:     "ready when ready",
			path:     "/ready",
			handler:  server.readyHandler,
			wantCode: http.StatusOK,
			wantBody: "READY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			w := httptest.NewRecorder()

			tt.handler(w, req)

			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tt.wantCode {
				t.Errorf("expected status %d, got %d", tt.wantCode, resp.StatusCode)
			}

			if !strings.Contains(string(body), tt.wantBody) {
				t.Errorf("expected body to contain %q, got %q", tt.wantBody, body)
			}
		})
	}
}

func TestServer_ReadyWhenNotReady(t *testing.T) {
	cfg := &Config{}
	server := &Server{config: cfg}
	server.ready.Store(false)

	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()

	server.readyHandler(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", resp.StatusCode)
	}
}

func TestParseConfig(t *testing.T) {
	// Reset flags for testing
	os.Args = []string{
		"test",
		"--listen-address=:8080",
		"--socket-path=/test/socket.ctl",
		"--stats-with-legacy",
		"--cache-ttl=30s",
	}

	// Re-create flag set
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	cfg := parseConfig()

	if cfg.ListenAddr != ":8080" {
		t.Errorf("expected listen address :8080, got %s", cfg.ListenAddr)
	}

	if cfg.SocketPath != "/test/socket.ctl" {
		t.Errorf("expected socket path /test/socket.ctl, got %s", cfg.SocketPath)
	}

	if !cfg.WithLegacy {
		t.Error("expected with-legacy to be true")
	}

	if cfg.CacheTTL != 30*time.Second {
		t.Errorf("expected cache TTL 30s, got %v", cfg.CacheTTL)
	}
}

func BenchmarkCollector_GetMetrics_CacheHit(b *testing.B) {
	socketPath, cleanup := mockSyslogNG(nil, "syslogng_test 1\n")
	defer cleanup()

	cfg := &Config{
		SocketPath:    socketPath,
		ScrapeTimeout: 5 * time.Second,
		CacheTTL:      1 * time.Hour, // Long TTL to ensure cache hits
	}

	reg := prometheus.NewRegistry()
	metrics := newExporterMetrics(reg)
	logger := setupLogger(&Config{LogLevel: "error"})

	collector := NewCollector(cfg, metrics, logger)

	ctx := context.Background()

	// Prime the cache
	collector.getMetrics(ctx)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			collector.getMetrics(ctx)
		}
	})
}

func BenchmarkCollector_GetMetrics_CacheMiss(b *testing.B) {
	socketPath, cleanup := mockSyslogNG(nil, "syslogng_test 1\n")
	defer cleanup()

	cfg := &Config{
		SocketPath:    socketPath,
		ScrapeTimeout: 5 * time.Second,
		CacheTTL:      0, // No caching
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
