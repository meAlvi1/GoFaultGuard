package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/valyala/fasthttp"
	"godefaultguard/circuitbreaker"
)

// APIHandler handles REST requests for circuit breaker
type APIHandler struct {
	cb *circuitbreaker.CircuitBreaker
}

// NewAPIHandler creates a new API handler
func NewAPIHandler(cb *circuitbreaker.CircuitBreaker) *APIHandler {
	return &APIHandler{cb: cb}
}

// HandleRequest processes API requests
func (h *APIHandler) HandleRequest(ctx *fasthttp.RequestCtx) {
	if string(ctx.Path()) != "/execute" || string(ctx.Method()) != "POST" {
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		return
	}

	var req struct {
		Key string `json:"key"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		ctx.WriteString(err.Error())
		return
	}

	result, err := h.cb.Execute(context.Background(), req.Key, func() (string, error) {
		// Simulate external service call
		reqOut := fasthttp.AcquireRequest()
		respOut := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(reqOut)
		defer fasthttp.ReleaseResponse(respOut)

		reqOut.SetRequestURI(req.URL)
		reqOut.Header.SetMethod("GET")

		if err := fasthttp.Do(reqOut, respOut); err != nil {
			return "", err
		}
		return string(respOut.Body()), nil
	})

	if err != nil {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		ctx.WriteString(err.Error())
		return
	}

	ctx.SetContentType("application/json")
	ctx.WriteString(fmt.Sprintf(`{"result": "%s"}`, result))
}

func main() {
	config := circuitbreaker.Config{
		MaxFailures:    5,
		Timeout:        2 * time.Second,
		MaxRetries:     3,
		RetryBackoff:   100 * time.Millisecond,
		FallbackDBPath: "./fallback.db",
		ServiceName:    "external_service",
	}

	cb, err := circuitbreaker.NewCircuitBreaker(config)
	if err != nil {
		log.Fatalf("Failed to initialize circuit breaker: %v", err)
	}
	defer cb.Close()

	// Start Prometheus metrics server
	go cb.StartMetricsServer(":9090")

	// Start REST API server
	handler := NewAPIHandler(cb)
	log.Fatal(fasthttp.ListenAndServe(":8080", handler.HandleRequest))
}
