package health

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"llm-router/internal/model"
	"llm-router/internal/pool"
	"llm-router/internal/util"

	"github.com/rs/zerolog"
)

// StatusChange represents a backend health status change event.
type StatusChange struct {
	Backend   *model.Backend
	Healthy   bool
	Timestamp time.Time
}

// StatusListener is called when a backend's health status changes.
type StatusListener func(change StatusChange)

// Checker performs asynchronous health checks on all backends.
type Checker struct {
	pool             *pool.BackendPool
	client           *http.Client
	interval         time.Duration
	timeout          time.Duration
	failureThreshold int
	maxConcurrency   int
	logger           zerolog.Logger
	listeners        []StatusListener
	listenersMu      sync.RWMutex
}

// NewChecker creates a new health Checker.
func NewChecker(
	p *pool.BackendPool,
	interval, timeout time.Duration,
	failureThreshold, maxConcurrency int,
	logger zerolog.Logger,
) *Checker {
	return &Checker{
		pool: p,
		client: &http.Client{
			Timeout: timeout,
		},
		interval:         interval,
		timeout:          timeout,
		failureThreshold: failureThreshold,
		maxConcurrency:   maxConcurrency,
		logger:           logger.With().Str("component", "health_checker").Logger(),
	}
}

// OnStatusChange registers a listener for backend health status changes.
func (c *Checker) OnStatusChange(listener StatusListener) {
	c.listenersMu.Lock()
	defer c.listenersMu.Unlock()
	c.listeners = append(c.listeners, listener)
}

// Run starts the periodic health check loop. Blocks until ctx is cancelled.
func (c *Checker) Run(ctx context.Context) {
	c.logger.Info().Dur("interval", c.interval).Msg("starting health checker")

	// Run initial check immediately
	c.CheckAll(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info().Msg("health checker stopped")
			return
		case <-ticker.C:
			c.CheckAll(ctx)
		}
	}
}

// CheckAll performs a single round of health checks on all backends.
func (c *Checker) CheckAll(ctx context.Context) {
	backends := c.pool.AllBackends()
	if len(backends) == 0 {
		return
	}

	sem := make(chan struct{}, c.maxConcurrency)
	var wg sync.WaitGroup

	for _, b := range backends {
		wg.Add(1)
		go func(backend *model.Backend) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			c.checkOne(ctx, backend)
		}(b)
	}

	wg.Wait()
}

// InitialCheckAll performs a startup health check that bypasses failureThreshold.
// Backends are first marked unhealthy; only those passing the check are restored.
// Returns the list of backends that remain unhealthy after the check.
func (c *Checker) InitialCheckAll(ctx context.Context) []*model.Backend {
	backends := c.pool.AllBackends()
	if len(backends) == 0 {
		return nil
	}

	// Mark all backends as unhealthy before the initial check
	for _, b := range backends {
		b.SetHealthy(false)
	}

	sem := make(chan struct{}, c.maxConcurrency)
	var wg sync.WaitGroup

	for _, b := range backends {
		wg.Add(1)
		go func(backend *model.Backend) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			c.initialCheckOne(ctx, backend)
		}(b)
	}

	wg.Wait()

	// Collect unhealthy backends
	var unhealthy []*model.Backend
	for _, b := range backends {
		if !b.IsHealthy() {
			unhealthy = append(unhealthy, b)
		}
	}
	return unhealthy
}

// initialCheckOne performs a single health check without failureThreshold.
// On success, the backend is marked healthy immediately.
// On failure, it stays unhealthy (no threshold accumulation needed).
func (c *Checker) initialCheckOne(ctx context.Context, b *model.Backend) {
	checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	url := util.ModelsURL(b.APIBase)
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, url, nil)
	if err != nil {
		c.logger.Warn().Str("backend", b.ID).Err(err).Msg("initial check failed: create request")
		return
	}

	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Warn().Str("backend", b.ID).Err(err).Msg("initial check failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Warn().Str("backend", b.ID).Int("status", resp.StatusCode).Msg("initial check failed: bad status")
		return
	}

	// Passed — mark healthy
	b.SetHealthy(true)
	b.ResetFailures()
	b.SetLastHealthCheck(time.Now())
	c.logger.Info().Str("backend", b.ID).Msg("initial check passed")
}

func (c *Checker) checkOne(ctx context.Context, b *model.Backend) {
	checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	url := util.ModelsURL(b.APIBase)
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, url, nil)
	if err != nil {
		c.handleFailure(b, fmt.Errorf("create request: %w", err))
		return
	}

	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.handleFailure(b, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.handleFailure(b, fmt.Errorf("status %d", resp.StatusCode))
		return
	}

	// Success
	wasUnhealthy := !b.IsHealthy()
	b.SetHealthy(true)
	b.ResetFailures()
	b.SetLastHealthCheck(time.Now())

	if wasUnhealthy {
		c.logger.Info().Str("backend", b.ID).Msg("backend recovered")
		c.notifyChange(StatusChange{Backend: b, Healthy: true, Timestamp: time.Now()})
	}
}

func (c *Checker) handleFailure(b *model.Backend, err error) {
	failures := b.IncrFailures()
	b.SetLastHealthCheck(time.Now())

	wasHealthy := b.IsHealthy()

	if failures >= int64(c.failureThreshold) {
		b.SetHealthy(false)
		if wasHealthy {
			c.logger.Warn().
				Str("backend", b.ID).
				Int64("consecutive_failures", failures).
				Err(err).
				Msg("backend marked unhealthy")
			c.notifyChange(StatusChange{Backend: b, Healthy: false, Timestamp: time.Now()})
		}
	} else {
		c.logger.Debug().
			Str("backend", b.ID).
			Int64("consecutive_failures", failures).
			Err(err).
			Msg("health check failed")
	}
}

func (c *Checker) notifyChange(change StatusChange) {
	c.listenersMu.RLock()
	defer c.listenersMu.RUnlock()
	for _, listener := range c.listeners {
		listener(change)
	}
}
