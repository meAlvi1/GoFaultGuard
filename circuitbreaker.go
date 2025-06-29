package circuitbreaker

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sony/gobreaker"
	_ "github.com/mattn/go-sqlite3"
	"github.com/valyala/fasthttp"
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
	_ = prometheus.Register(metrics.State)
	_ = prometheus.Register(metrics.Failures)
	_ = prometheus.Register(metrics.Retries)
	_ = prometheus.Register(metrics.Latency)
	_ = prometheus.Register(metrics.FallbackSuccess)

	// Initialize SQLite fallback database
	db, err := sql.Open("sqlite3", config.FallbackDBPath)
	if err != nil {
		return nil, errors.Join(err, errors.New("failed to open fallback database"))
	}
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS fallback_data (key TEXT PRIMARY KEY, value TEXT)")
	if err != nil {
		return nil, errors.Join(err, errors.New("failed to create fallback table"))
	}

	// Initialize circuit breaker
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        config.ServiceName,
		MaxRequests: config.MaxFailures,
		Timeout:     config.Timeout,
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

	// Retry logic with exponential backoff
	for i := 0; i <= c.config.MaxRetries; i++ {
		result, err := c.cb.Execute(func() (interface{}, error) {
			return fn()
		})
		if err == nil {
			c.metrics.Latency.WithLabelValues(c.config.ServiceName).Observe(time.Since(start).Seconds())
			return result.(string), nil
		}
		lastErr = err
		c.metrics.Failures.WithLabelValues(c.config.ServiceName).Inc()
		c.metrics.Retries.WithLabelValues(c.config.ServiceName).Inc()
		if i < c.config.MaxRetries {
			time.Sleep(c.config.RetryBackoff * time.Duration(1<<uint(i)))
		}
	}

	// Fallback to SQLite
	c.mu.Lock()
	defer c.mu.Unlock()
	var value string
	err := c.db.QueryRow("SELECT value FROM fallback_data WHERE key = ?", key).Scan(&value)
	if err == nil {
		c.metrics.FallbackSuccess.WithLabelValues(c.config.ServiceName).Inc()
		return value, nil
	}

	// Store result in fallback if successful
	result, err := fn()
	if err == nil {
		_, dbErr = c.db.Exec("INSERT OR REPLACE INTO fallback_data (key, value) VALUES (?, ?)", key, result)
		if dbErr != nil {
			log.Printf("Failed to store fallback: %v", dbErr)
		}
		c.metrics.Latency.WithLabelValues(c.config.ServiceName).Observe(time.Since(start).Seconds())
		return result, nil
	}

	return "", errors.Join(lastErr, errors.New("circuit breaker tripped and no fallback available"))
}

// StartMetricsServer starts a Prometheus metrics endpoint
func (c *CircuitBreaker) StartMetricsServer(addr string) {
	fasthttp.ListenAndServe(addr, promhttp.Handler().ServeHTTP)
}

// Close shuts down the circuit breaker
func (c *CircuitBreaker) Close() {
	c.db.Close()
}
