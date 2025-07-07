package circuitbreaker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sony/gobreaker"
)

// Metrics holds Prometheus metrics for the circuit breaker
type Metrics struct {
	State           *prometheus.GaugeVec
	Failures        *prometheus.CounterVec
	Retries         *prometheus.CounterVec
	Latency         *prometheus.HistogramVec
	FallbackSuccess *prometheus.CounterVec
}

// Config holds circuit breaker configuration
type Config struct {
	MaxFailures    uint32        // Threshold for opening circuit
	Timeout        time.Duration // Timeout for requests
	MaxRetries     int           // Number of retries
	RetryBackoff   time.Duration // Base backoff for retries
	FallbackDBPath string        // Path to SQLite fallback database
	ServiceName    string        // Service name for metrics
	MaxRequests    uint32        // Allowed requests in half-open state
	Interval       time.Duration // Time before transitioning from Open to Half-Open
}

// CircuitBreaker wraps gobreaker with retries and fallback
type CircuitBreaker struct {
	cb      *gobreaker.CircuitBreaker
	config  Config
	metrics Metrics
	db      *sql.DB
	mu      sync.Mutex
}

// NewCircuitBreaker initializes the circuit breaker with metrics and fallback
func NewCircuitBreaker(config Config) (*CircuitBreaker, error) {
	// Initialize Prometheus metrics
	metrics := Metrics{
		State: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "circuit_breaker_state",
			Help: "Current state (0=Closed, 1=Open, 2=Half-Open)",
		}, []string{"service"}),
		Failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "circuit_breaker_failures_total",
			Help: "Total failed requests",
		}, []string{"service"}),
		Retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "circuit_breaker_retries_total",
			Help: "Total retries attempted",
		}, []string{"service"}),
		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "circuit_breaker_latency_seconds",
			Help:    "Request latency",
			Buckets: prometheus.DefBuckets,
		}, []string{"service"}),
		FallbackSuccess: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "circuit_breaker_fallback_success_total",
			Help: "Total successful fallback responses",
		}, []string{"service"}),
	}

	// Register metrics safely
	for _, collector := range []prometheus.Collector{
		metrics.State, metrics.Failures, metrics.Retries, metrics.Latency, metrics.FallbackSuccess,
	} {
		if err := prometheus.Register(collector); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				return nil, fmt.Errorf("failed to register prometheus metric: %v", err)
			}
		}
	}

	// Initialize SQLite database
	db, err := sql.Open("sqlite3", config.FallbackDBPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open fallback database: %v", err)
	}
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS fallback_data (key TEXT PRIMARY KEY, value TEXT)")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create fallback table: %v", err)
	}

	// Initialize circuit breaker
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        config.ServiceName,
		MaxRequests: config.MaxRequests,
		Timeout:     config.Timeout,
		Interval:    config.Interval,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= config.MaxFailures
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			stateValue := float64(0)
			if to == gobreaker.StateOpen {
				stateValue = 1
			} else if to == gobreaker.StateHalfOpen {
				stateValue = 2
			}
			metrics.State.WithLabelValues(config.ServiceName).Set(stateValue)
		},
	})

	return &CircuitBreaker{
		cb:      cb,
		config:  config,
		metrics: metrics,
		db:      db,
	}, nil
}

// Execute runs a function with circuit breaker, retries, and fallback
func (c *CircuitBreaker) Execute(ctx context.Context, key string, fn func() (string, error)) (string, error) {
	start := time.Now()
	var lastErr error

	for i := 0; i <= c.config.MaxRetries; i++ {
		result, err := c.cb.Execute(func() (interface{}, error) {
			r, e := fn()
			fmt.Printf("Execute: fn returned result=%v, err=%v\n", r, e)
			return r, e
		})
		fmt.Printf("gobreaker.Execute returned result=%v, err=%v, state=%v\n", result, err, c.cb.State().String())
		if err == nil {
			c.metrics.Latency.WithLabelValues(c.config.ServiceName).Observe(time.Since(start).Seconds())
			c.mu.Lock()
			_, dbErr := c.db.ExecContext(ctx, "INSERT OR REPLACE INTO fallback_data (key, value) VALUES (?, ?)", key, result.(string))
			c.mu.Unlock()
			if dbErr != nil {
				fmt.Printf("Failed to store fallback: %v\n", dbErr)
			}
			return result.(string), nil
		}
		lastErr = err
		c.metrics.Failures.WithLabelValues(c.config.ServiceName).Inc()
		if i < c.config.MaxRetries {
			c.metrics.Retries.WithLabelValues(c.config.ServiceName).Inc()
			select {
			case <-time.After(c.config.RetryBackoff * time.Duration(1<<uint(i))):
			case <-ctx.Done():
				return "", fmt.Errorf("context cancelled: %v", ctx.Err())
			}
		}
	}

	// Try fallback
	c.mu.Lock()
	var value string
	err := c.db.QueryRowContext(ctx, "SELECT value FROM fallback_data WHERE key = ?", key).Scan(&value)
	c.mu.Unlock()
	fmt.Printf("Fallback query for key=%s returned value=%s, err=%v\n", key, value, err)
	if err == nil {
		c.metrics.FallbackSuccess.WithLabelValues(c.config.ServiceName).Inc()
		return value, nil
	}

	return "", fmt.Errorf("circuit breaker tripped and no fallback available: %v", lastErr)
}

// StartMetricsServer starts a Prometheus metrics endpoint
func (c *CircuitBreaker) StartMetricsServer(addr string) {
	http.Handle("/metrics", promhttp.Handler())
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Printf("metrics server error: %v", err)
	}
}

// Close shuts down the circuit breaker
func (c *CircuitBreaker) Close() {
	if err := c.db.Close(); err != nil {
		log.Printf("Failed to close database: %v", err)
	}
}
