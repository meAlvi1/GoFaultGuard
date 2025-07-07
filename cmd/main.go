package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/meAlvi1/GoFaultGuard/circuitbreaker"
)

func main() {
	cfg := circuitbreaker.Config{
		MaxFailures:    2,
		Timeout:        2 * time.Second,
		MaxRetries:     1,
		RetryBackoff:   100 * time.Millisecond,
		FallbackDBPath: "fallback_demo.db",
		ServiceName:    "demo_service",
		MaxRequests:    1,
		Interval:       1 * time.Second,
	}

	cb, err := circuitbreaker.NewCircuitBreaker(cfg)
	if err != nil {
		log.Fatalf("Failed to create circuit breaker: %v", err)
	}
	defer cb.Close()

	// Start Prometheus metrics server in background
	go cb.StartMetricsServer(":8080")
	fmt.Println("Prometheus metrics available at http://localhost:8080/metrics")

	ctx := context.Background()
	key := "demo_key"

	// Simulate a successful call
	result, err := cb.Execute(ctx, key, func() (string, error) {
		return "Hello, world!", nil
	})
	fmt.Printf("First call result: %v, err: %v\n", result, err)

	// Simulate a failing call (to trigger fallback)
	result, err = cb.Execute(ctx, key, func() (string, error) {
		return "", fmt.Errorf("simulated failure")
	})
	fmt.Printf("Second call (should fallback) result: %v, err: %v\n", result, err)

	// Wait so you can visit /metrics
	fmt.Println("Press Ctrl+C to exit...")
	select {}
}
