package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"llm-router/internal/model"
	"llm-router/internal/monitor"
	"llm-router/internal/pool"
	"llm-router/internal/ratelimit"
	"llm-router/internal/routing"
	"llm-router/internal/util"

	"github.com/rs/zerolog"
)

// ErrorType classifies backend errors for retry decisions.
type ErrorType int

const (
	ErrTimeout       ErrorType = iota // backend response timeout
	ErrConnRefused                    // TCP connection refused / DNS failure
	ErrConnReset                      // connection reset during request
	ErrBackend5xx                     // backend returned 5xx
	ErrBackend4xx                     // backend returned 4xx (non-retryable)
	ErrNoBackend                      // no healthy backend available
	ErrClientGone                     // client disconnected / context canceled (non-retryable)
)

// Manager is the core routing coordinator. It implements handler.Router.
type Manager struct {
	pool          *pool.BackendPool
	strategy      routing.Strategy
	rateLimiter   *ratelimit.RateLimiter
	completionRPM *monitor.CompletionRPM
	stats         *util.RequestStats
	client        *http.Client
	logger        zerolog.Logger
	trace         zerolog.Logger // per-request trace; defaults to Nop, enabled via LLM_ROUTER_TRACE

	// Retry configuration
	maxRetries       int
	baseDelay        time.Duration
	maxDelay         time.Duration
	jitterFactor     float64
	failureThreshold int
}

// ManagerOption is a functional option for Manager.
type ManagerOption func(*Manager)

// WithMaxRetries sets the max retry count.
func WithMaxRetries(n int) ManagerOption {
	return func(m *Manager) { m.maxRetries = n }
}

// WithBackoff sets the backoff parameters.
func WithBackoff(baseMs, maxMs int, jitter float64) ManagerOption {
	return func(m *Manager) {
		m.baseDelay = time.Duration(baseMs) * time.Millisecond
		m.maxDelay = time.Duration(maxMs) * time.Millisecond
		m.jitterFactor = jitter
	}
}

// WithFailureThreshold sets the consecutive failure threshold for marking unhealthy.
func WithFailureThreshold(n int) ManagerOption {
	return func(m *Manager) { m.failureThreshold = n }
}

// NewManager creates a new routing Manager.
func NewManager(
	p *pool.BackendPool,
	strategy routing.Strategy,
	rateLimiter *ratelimit.RateLimiter,
	completionRPM *monitor.CompletionRPM,
	stats *util.RequestStats,
	client *http.Client,
	logger zerolog.Logger,
	opts ...ManagerOption,
) *Manager {
	mgr := &Manager{
		pool:             p,
		strategy:         strategy,
		rateLimiter:      rateLimiter,
		completionRPM:    completionRPM,
		stats:            stats,
		client:           client,
		logger:           logger.With().Str("component", "router_manager").Logger(),
		trace:            zerolog.Nop(),
		maxRetries:       3,
		baseDelay:        500 * time.Millisecond,
		maxDelay:         5 * time.Second,
		jitterFactor:     0.2,
		failureThreshold: 3,
	}
	for _, opt := range opts {
		opt(mgr)
	}

	// Enable per-request trace logging via LLM_ROUTER_TRACE env var.
	// Value is a directory path; trace logs are written to <dir>/trace_<timestamp>.log.
	// Set LLM_ROUTER_TRACE=logs to write alongside normal logs.
	if traceDir := os.Getenv("LLM_ROUTER_TRACE"); traceDir != "" {
		if err := os.MkdirAll(traceDir, 0755); err == nil {
			traceFile := filepath.Join(traceDir,
				fmt.Sprintf("trace_%s.log", time.Now().Format("2006-01-02_1504")))
			f, err := os.OpenFile(traceFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err == nil {
				mgr.trace = zerolog.New(f).With().Timestamp().Logger()
				mgr.logger.Info().Str("trace_file", traceFile).Msg("per-request trace logging enabled")
			}
		}
	}

	return mgr
}

// Route implements handler.Router. It routes a chat completion request to a backend.
func (m *Manager) Route(w http.ResponseWriter, r *http.Request, modelName string, body []byte) {
	ctx := r.Context()

	// Step 1: Check if model exists
	if !m.pool.HasModel(modelName) {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"model '%s' not found","type":"invalid_request_error"}}`, modelName), http.StatusNotFound)
		return
	}

	rctx := routing.NewContext(modelName, m.maxRetries)

	for rctx.CanRetry() {
		// Check if client already disconnected before attempting
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Step 2: Get healthy backends
		candidates := m.pool.GetHealthyBackends(modelName)
		if len(candidates) == 0 {
			// Short wait and retry once (give scanner/health checker time)
			if rctx.AttemptNum == 0 {
				select {
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
					http.Error(w, `{"error":{"message":"request cancelled","type":"server_error"}}`, http.StatusGatewayTimeout)
					return
				}
				candidates = m.pool.GetHealthyBackends(modelName)
			}
			if len(candidates) == 0 {
				http.Error(w, `{"error":{"message":"no healthy backends available","type":"server_error"}}`, http.StatusServiceUnavailable)
				return
			}
		}

		// Step 3: RPM filter
		available := m.filterByRPM(candidates)
		if len(available) == 0 {
			// All backends RPM-limited, wait briefly and check again
			select {
			case <-time.After(1 * time.Second):
			case <-ctx.Done():
				http.Error(w, `{"error":{"message":"request cancelled","type":"server_error"}}`, http.StatusGatewayTimeout)
				return
			}
			available = m.filterByRPM(candidates)
			if len(available) == 0 {
				http.Error(w, `{"error":{"message":"all backends rate limited","type":"server_error"}}`, http.StatusTooManyRequests)
				return
			}
		}

		// Step 4: Prefer unattempted backends
		preferred := rctx.FilterUnattempted(available)

		// Step 5: Strategy selection
		backend := m.strategy.Select(preferred)
		if backend == nil {
			http.Error(w, `{"error":{"message":"no backend selected","type":"server_error"}}`, http.StatusServiceUnavailable)
			return
		}

		// Step 6: Record RPM before sending (strict enforcement)
		if !m.rateLimiter.Allow(backend.ID, backend.RPMLimit) {
			// This backend hit limit between filter and now, retry loop
			rctx.MarkAttempted(backend)
			continue
		}

		// Step 7: Forward request
		rctx.MarkAttempted(backend)
		start := time.Now()
		errType, statusCode, err := m.forward(ctx, w, r, backend, body)

		if err == nil {
			// Success
			latency := time.Since(start)
			backend.ResetFailures()
			m.completionRPM.Record(backend.ID)
			m.stats.RecordSuccess(backend.ID, latency)
			m.trace.Debug().
				Str("backend", backend.ID).
				Str("model", modelName).
				Dur("latency", latency).
				Msg("request forwarded successfully")
			return
		}

		// Step 8: Handle error

		// Client gone: don't retry, don't count as backend failure
		if errType == ErrClientGone {
			m.trace.Debug().
				Str("backend", backend.ID).
				Str("model", modelName).
				Msg("client disconnected, aborting retries")
			return
		}
		m.trace.Debug().
			Str("backend", backend.ID).
			Str("model", modelName).
			Int("attempt", rctx.AttemptNum).
			Err(err).
			Msg("request failed")
		m.stats.RecordFailure(backend.ID, fmt.Sprintf("%d", errType))

		// Non-retryable: 4xx errors
		if errType == ErrBackend4xx {
			// Response already written by forward(), or we write it now
			if statusCode > 0 {
				return // already written
			}
			http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error"}}`, err.Error()), http.StatusBadGateway)
			return
		}

		// Exponential backoff for 5xx errors
		if errType == ErrBackend5xx {
			delay := m.backoffDelay(rctx.AttemptNum)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				http.Error(w, `{"error":{"message":"request cancelled","type":"server_error"}}`, http.StatusGatewayTimeout)
				return
			}
		}
		// Other retryable errors: retry immediately with different backend
	}

	// All retries exhausted
	http.Error(w, `{"error":{"message":"all retries exhausted","type":"server_error"}}`, http.StatusBadGateway)
}

// filterByRPM removes backends that have reached their RPM limit.
func (m *Manager) filterByRPM(candidates []*model.Backend) []*model.Backend {
	filtered := make([]*model.Backend, 0, len(candidates))
	for _, b := range candidates {
		if !m.rateLimiter.IsLimited(b.ID, b.RPMLimit) {
			filtered = append(filtered, b)
		}
	}
	return filtered
}

// forward sends the request to the selected backend and streams the response back.
// Returns error type, status code, and error (nil on success).
func (m *Manager) forward(
	ctx context.Context,
	w http.ResponseWriter,
	originalReq *http.Request,
	backend *model.Backend,
	body []byte,
) (ErrorType, int, error) {
	targetURL := util.ChatCompletionsURL(backend.APIBase)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return ErrConnRefused, 0, fmt.Errorf("create request: %w", err)
	}

	// Copy relevant headers
	req.Header.Set("Content-Type", "application/json")
	if backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}
	// Forward original headers that may be needed
	if v := originalReq.Header.Get("Accept"); v != "" {
		req.Header.Set("Accept", v)
	}

	backend.IncrConnections()
	defer backend.DecrConnections()

	backend.IncrPending()
	resp, err := m.client.Do(req)
	backend.DecrPending()
	if err != nil {
		return classifyError(err), 0, err
	}
	defer resp.Body.Close()

	// 4xx: non-retryable, transparently forward to client
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		copyHeaders(w, resp)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return ErrBackend4xx, resp.StatusCode, fmt.Errorf("backend returned %d", resp.StatusCode)
	}

	// 5xx: retryable
	if resp.StatusCode >= 500 {
		// Read the error body but don't write to client (will retry)
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return ErrBackend5xx, resp.StatusCode, fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(errBody))
	}

	// Success: stream response back to client (zero-copy)
	copyHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)

	// Use a buffer for efficient streaming
	buf := make([]byte, 32*1024)
	_, err = io.CopyBuffer(w, resp.Body, buf)
	if err != nil {
		m.trace.Debug().Err(err).Str("backend", backend.ID).Msg("error streaming response")
	}

	// Flush if the writer supports it (for SSE/streaming)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	return 0, resp.StatusCode, nil
}

// classifyError determines the error type from an HTTP client error.
func classifyError(err error) ErrorType {
	if err == nil {
		return 0
	}

	// Check for client disconnection (context canceled)
	if errors.Is(err, context.Canceled) {
		return ErrClientGone
	}

	// Check for timeout
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}

	// Check for connection refused
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" {
			return ErrConnRefused
		}
	}

	// Check for connection reset
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return ErrConnReset
	}

	// Check for DNS errors
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ErrConnRefused
	}

	// Check error message patterns
	errMsg := err.Error()
	if strings.Contains(errMsg, "connection refused") {
		return ErrConnRefused
	}
	if strings.Contains(errMsg, "connection reset") || strings.Contains(errMsg, "broken pipe") {
		return ErrConnReset
	}
	if strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "deadline") {
		return ErrTimeout
	}

	return ErrConnReset // default to retryable
}

// backoffDelay calculates exponential backoff with jitter.
func (m *Manager) backoffDelay(attempt int) time.Duration {
	delay := float64(m.baseDelay) * math.Pow(2, float64(attempt-1))
	if delay > float64(m.maxDelay) {
		delay = float64(m.maxDelay)
	}
	jitter := delay * m.jitterFactor * (rand.Float64()*2 - 1)
	result := time.Duration(delay + jitter)
	if result < 0 {
		result = 0
	}
	return result
}

// copyHeaders copies response headers from the backend response to the client response.
func copyHeaders(w http.ResponseWriter, resp *http.Response) {
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
}
