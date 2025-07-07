package circuitbreaker

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sony/gobreaker"
)

func testConfig() Config {
	return Config{
		MaxFailures:    2,
		Timeout:        500 * time.Millisecond,
		MaxRetries:     1,
		RetryBackoff:   10 * time.Millisecond,
		FallbackDBPath: "test_fallback.db",
		ServiceName:    "test_service",
		MaxRequests:    1,
		Interval:       200 * time.Millisecond,
	}
}

func cleanupDB(path string) {
	os.Remove(path)
}

func alwaysFail() (string, error) { return "", errTest }
func alwaysOK() (string, error)   { return "ok", nil }

var errTest = &testErr{}

type testErr struct{}

func (e *testErr) Error() string { return "fail" }

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
	res, err := cb.Execute(ctx, "cb1", alwaysOK)
	t.Logf("Closed state: result=%v, err=%v", res, err)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	// Trip the breaker
	for i := 0; i < int(cfg.MaxFailures); i++ {
		res, err := cb.Execute(ctx, "cb1", alwaysFail)
		t.Logf("Trip %d: result=%v, err=%v", i+1, res, err)
	}
	res, err = cb.Execute(ctx, "cb1", alwaysFail) // ensure state transition
	t.Logf("After tripping: result=%v, err=%v", res, err)

	// Should now be open, and fallback should return the cached value ("ok")
	res, err = cb.Execute(ctx, "cb1", alwaysFail)
	t.Logf("Breaker open: result=%v, err=%v", res, err)
	if err != nil || res != "ok" {
		t.Fatalf("expected fallback value 'ok', got result=%v, err=%v", res, err)
	}

	// Wait for Open to Half-Open transition
	time.Sleep(cfg.Timeout + 50*time.Millisecond)
	state := cb.cb.State()
	t.Logf("Breaker state after wait: %v", state)
	if state != gobreaker.StateHalfOpen {
		t.Fatalf("expected state Half-Open, got %v", state)
	}
}

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
	cb.Execute(ctx, "fb", alwaysFail)

	// Should return fallback
	val, err := cb.Execute(ctx, "fb", alwaysFail)
	if err != nil || val != "cached" {
		t.Fatalf("expected fallback 'cached', got %v, err: %v", val, err)
	}
}

func TestCircuitBreaker_RetryLogic(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, _ := NewCircuitBreaker(cfg)
	defer cb.Close()
	ctx := context.Background()

	attempts := 0
	cb.Execute(ctx, "retry", func() (string, error) {
		attempts++
		if attempts < 2 {
			return "", errTest
		}
		return "retried", nil
	})
	if attempts != 2 {
		t.Fatalf("expected 2 attempts (1+1 retry), got %d", attempts)
	}
}

func TestCircuitBreaker_SQLiteIntegration(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, _ := NewCircuitBreaker(cfg)
	defer cb.Close()
	ctx := context.Background()

	cb.mu.Lock()
	_, err := cb.db.ExecContext(ctx, "INSERT INTO fallback_data (key, value) VALUES (?, ?)", "sql", "val")
	cb.mu.Unlock()
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	var got string
	cb.mu.Lock()
	err = cb.db.QueryRowContext(ctx, "SELECT value FROM fallback_data WHERE key = ?", "sql").Scan(&got)
	cb.mu.Unlock()
	if err != nil || got != "val" {
		t.Fatalf("expected val, got %v, err: %v", got, err)
	}
}

func TestCircuitBreaker_PrometheusMetrics(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, _ := NewCircuitBreaker(cfg)
	defer cb.Close()
	ctx := context.Background()

	cb.Execute(ctx, "metrics", alwaysFail)
	count := testutil.ToFloat64(cb.metrics.Failures.WithLabelValues(cfg.ServiceName))
	if count == 0 {
		t.Fatalf("expected failures metric to increment, got %v", count)
	}
}

func TestCircuitBreaker_Concurrency(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, _ := NewCircuitBreaker(cfg)
	defer cb.Close()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cb.Execute(ctx, "conc", alwaysOK)
		}()
	}
	wg.Wait()
}

func TestCircuitBreaker_ConfigRespected(t *testing.T) {
	cfg := testConfig()
	cfg.MaxFailures = 1
	cfg.MaxRetries = 0
	defer cleanupDB(cfg.FallbackDBPath)
	cb, _ := NewCircuitBreaker(cfg)
	defer cb.Close()
	ctx := context.Background()

	cb.Execute(ctx, "conf", alwaysFail)
	_, err := cb.Execute(ctx, "conf", alwaysFail)
	if err == nil || !strings.Contains(err.Error(), "no fallback") {
		t.Fatalf("expected open breaker and fallback error, got %v", err)
	}
}

func TestCircuitBreaker_ErrorHandling(t *testing.T) {
	cfg := testConfig()
	defer cleanupDB(cfg.FallbackDBPath)
	cb, _ := NewCircuitBreaker(cfg)
	defer cb.Close()
	ctx := context.Background()

	// Function error
	_, err := cb.Execute(ctx, "err", alwaysFail)
	if err == nil {
		t.Fatalf("expected error from alwaysFail, got nil")
	}

	// Simulate DB error by closing DB
	cb.db.Close()
	_, err = cb.Execute(ctx, "err2", alwaysOK)
	if err == nil {
		t.Fatalf("expected error due to closed DB, got nil")
	}
}
