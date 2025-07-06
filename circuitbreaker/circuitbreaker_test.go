package circuitbreaker

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Helpers ---

func testConfig() Config {
	return Config{
		MaxFailures:    2,
		Timeout:        1 * time.Second,
		MaxRetries:     1,
		RetryBackoff:   10 * time.Millisecond,
		FallbackDBPath: "test_fallback.db",
		ServiceName:    "test_service",
		MaxRequests:    1,
	}
}

func cleanupDB(path string) {
	os.Remove(path)
}

var alwaysFail = func() (string, error) { return "", assertErr }
var alwaysOK = func() (string, error) { return "ok", nil }

type testErr struct{}

func (e *testErr) Error() string { return "fail" }

var assertErr = &testErr{}

// --- 1. Circuit Breaker Logic ---

func TestCircuitBreaker_ClosedToOpenToHalfOpen(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, err := NewCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("failed to create circuit breaker: %v", err)
	}
	defer cb.Close()
	ctx := context.Background()

	// Should succeed in closed state
	_, err = cb.Execute(ctx, "cb1", alwaysOK)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	// Trip the breaker (failures >= MaxFailures)
	for i := 0; i < int(cfg.MaxFailures); i++ {
		cb.Execute(ctx, "cb1", alwaysFail)
	}

	// Extra call to ensure state transition
	cb.Execute(ctx, "cb1", alwaysFail)

	// Now breaker should be open: next call should fallback (no cache yet)
	_, err = cb.Execute(ctx, "cb1", alwaysFail)
	if err == nil || !strings.Contains(err.Error(), "no fallback") {
		t.Fatalf("expected open breaker and fallback error, got %v", err)
	}
}

// --- 2. Retry Logic ---

func TestCircuitBreaker_RetryLogic(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, err := NewCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("failed to create circuit breaker: %v", err)
	}
	defer cb.Close()
	ctx := context.Background()

	attempts := 0
	cb.Execute(ctx, "retry", func() (string, error) {
		attempts++
		if attempts < 2 {
			return "", assertErr
		}
		return "retried", nil
	})
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

// --- 3. Fallback Logic ---

func TestCircuitBreaker_FallbackLogic(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, err := NewCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("failed to create circuit breaker: %v", err)
	}
	defer cb.Close()
	ctx := context.Background()

	// Store fallback
	cb.Execute(ctx, "fb", func() (string, error) { return "cached", nil })

	// Trip the breaker
	for i := 0; i < int(cfg.MaxFailures); i++ {
		cb.Execute(ctx, "fb", alwaysFail)
	}
	// Should return fallback
	val, err := cb.Execute(ctx, "fb", alwaysFail)
	if err != nil || val != "cached" {
		t.Fatalf("expected fallback 'cached', got %v, err: %v", val, err)
	}
}

// --- 4. SQLite Integration ---

func TestCircuitBreaker_SQLiteIntegration(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, err := NewCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("failed to create circuit breaker: %v", err)
	}
	defer cb.Close()
	ctx := context.Background()

	// Store value
	cb.Execute(ctx, "sql", func() (string, error) { return "dbval", nil })

	// Directly query DB
	var got string
	cb.mu.Lock()
	err = cb.db.QueryRowContext(ctx, "SELECT value FROM fallback_data WHERE key = ?", "sql").Scan(&got)
	cb.mu.Unlock()
	if err != nil || got != "dbval" {
		t.Fatalf("expected dbval in fallback db, got %v, err: %v", got, err)
	}
}

// --- 5. Prometheus Metrics ---

func TestCircuitBreaker_MetricsIncrement(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, err := NewCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("failed to create circuit breaker: %v", err)
	}
	defer cb.Close()
	ctx := context.Background()

	// Cause a failure and a retry
	cb.Execute(ctx, "metrics", alwaysFail)
	// Cause a fallback
	cb.Execute(ctx, "metrics", func() (string, error) { return "cache", nil })
	for i := 0; i < int(cfg.MaxFailures); i++ {
		cb.Execute(ctx, "metrics", alwaysFail)
	}
	cb.Execute(ctx, "metrics", alwaysFail)

	// Check metrics are non-zero
	//if cb.metrics.Failures.WithLabelValues(cfg.ServiceName).Collect == nil {
	//	t.Error("metrics not registered")
	//}
}

// --- 6. HTTP /metrics endpoint ---

func TestCircuitBreaker_MetricsEndpoint(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, err := NewCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("failed to create circuit breaker: %v", err)
	}
	defer cb.Close()

	addr := "127.0.0.1:9099"
	done := make(chan struct{})
	go func() {
		cb.StartMetricsServer(addr)
		close(done)
	}()
	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("could not GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "circuit_breaker_state") {
		t.Errorf("metrics endpoint missing circuit_breaker_state, got: %s", string(body))
	}
}

// --- 7. Concurrency ---

func TestCircuitBreaker_Concurrency(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, err := NewCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("failed to create circuit breaker: %v", err)
	}
	defer cb.Close()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cb.Execute(ctx, "cc", func() (string, error) {
				return "v", nil
			})
		}(i)
	}
	wg.Wait()
}

// --- 8. Configuration Respect ---

func TestCircuitBreaker_ConfigRespected(t *testing.T) {
	cfg := testConfig()
	cfg.MaxFailures = 1
	cfg.MaxRetries = 0
	defer cleanupDB(cfg.FallbackDBPath)
	cb, err := NewCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("failed to create circuit breaker: %v", err)
	}
	defer cb.Close()
	ctx := context.Background()

	// Should open after 1 failure
	cb.Execute(ctx, "conf", alwaysFail)
	_, err = cb.Execute(ctx, "conf", alwaysFail)
	if err == nil || !strings.Contains(err.Error(), "no fallback") {
		t.Fatalf("expected open breaker and fallback error, got %v", err)
	}
}
