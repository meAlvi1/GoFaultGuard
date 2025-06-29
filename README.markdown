# GoFaultGuard: Fault-Tolerant Circuit Breaker Plugin

**GoFaultGuard** is a lightweight, fault-tolerant circuit breaker plugin written in Go, designed for critical systems requiring resilience against unreliable external services. It provides configurable retries, a local SQLite fallback database, and Prometheus/Grafana integration for observability, eliminating cloud reliance. The plugin exposes a REST API for seamless integration with any framework (e.g., Django, Spring, Express, Laravel).

## Features

- **Circuit Breaker**: Built on `sony/gobreaker` for robust fault tolerance.
- **Retries**: Configurable retry counts with exponential backoff.
- **Fallback Database**: Local SQLite storage for data recovery during failures.
- **Observability**: Prometheus metrics (state, failures, retries, latency) visualized in Grafana.
- **Cloud Independence**: Runs locally or in containers without cloud dependencies.
- **Framework-Agnostic**: REST API integrates with Python, Java, JavaScript, PHP, and more.
- **Go 1.25 Optimized**: Leverages `errors.Join`, `sync.OnceFunc`, and garbage collection improvements.

## Installation

1. **Clone the Repository**:
   ```bash
   git clone https://github.com/yourusername/gofaultguard.git
   cd gofaultguard
   ```

2. **Install Dependencies**:
   ```bash
   go mod tidy
   ```

3. **Build and Run**:
   ```bash
   go build -o gofaultguard
   ./gofaultguard
   ```

4. **Run with Docker**:
   ```bash
   docker build -t gofaultguard .
   docker run -p 8080:8080 -p 9090:9090 gofaultguard
   ```

## Usage

### Configuration

Edit the `main.go` configuration to set retries, fallback database path, and service name:

```go
config := circuitbreaker.Config{
    MaxFailures:    5,                    // Open circuit after 5 failures
    Timeout:        2 * time.Second,      // Request timeout
    MaxRetries:     3,                    // Retry attempts
    RetryBackoff:   100 * time.Millisecond, // Base backoff
    FallbackDBPath: "./fallback.db",      // SQLite database path
    ServiceName:    "external_service",   // Metrics label
}
```

### API Endpoint

Send POST requests to `/execute` with a JSON payload:

```json
{
    "key": "unique-request-id",
    "url": "https://external-service.com/api"
}
```

Example with Python (Django):

```python
import requests

response = requests.post("http://localhost:8080/execute", json={"key": "req1", "url": "https://api.example.com"})
print(response.json())  # {"result": "response-data"}
```

### Prometheus/Grafana

1. **Prometheus**: Configure to scrape `http://localhost:9090/metrics`.
   ```yaml
   scrape_configs:
     - job_name: "gofaultguard"
       static_configs:
         - targets: ["localhost:9090"]
   ```

2. **Grafana**: Add Prometheus as a data source and create dashboards for:
   - Circuit breaker state (`circuit_breaker_state`).
   - Failure and retry rates (`circuit_breaker_failures_total`, `circuit_breaker_retries_total`).
   - Latency histograms (`circuit_breaker_latency_seconds`).

## Project Structure

```
gofaultguard/
├── circuitbreaker/   # Core circuit breaker logic
│   └── circuitbreaker.go
├── main.go           # REST API server
├── Dockerfile        # Containerization
├── go.mod            # Dependencies
└── README.md         # This file
```

## Metrics

- `circuit_breaker_state{service}`: Gauge (0=Closed, 1=Open, 2=Half-Open).
- `circuit_breaker_failures_total{service}`: Counter for failed requests.
- `circuit_breaker_retries_total{service}`: Counter for retry attempts.
- `circuit_breaker_latency_seconds{service}`: Histogram for request latency.
- `circuit_breaker_fallback_success_total{service}`: Counter for successful fallbacks.

## Contributing

Contributions are welcome! Please open an issue or submit a pull request on [GitHub](https://github.com/meAlvi1/GoFaultGuard).

## License

MIT License. See [LICENSE](https://github.com/meAlvi1/GoFaultGuard/blob/main/LICENSE) for details.
