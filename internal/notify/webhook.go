package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"llm-router/internal/model"
	"llm-router/internal/pool"

	"github.com/rs/zerolog"
)

// Notifier sends notifications to a WeChat Work webhook.
type Notifier struct {
	pool            *pool.BackendPool
	webhookURL      string
	client          *http.Client
	cooldown        time.Duration
	batchInterval   time.Duration
	reportActive    time.Duration
	reportIdle      time.Duration
	logger          zerolog.Logger

	mu              sync.Mutex
	lastNotify      map[string]time.Time // backend ID -> last alert time
	pendingAlerts   []alertEntry
	batchTimer      *time.Timer
	lastActivity    time.Time
	startTime       time.Time
}

type alertEntry struct {
	Backend   *model.Backend
	Healthy   bool
	Timestamp time.Time
}

type webhookMessage struct {
	MsgType  string          `json:"msgtype"`
	Markdown markdownContent `json:"markdown"`
}

type markdownContent struct {
	Content string `json:"content"`
}

// NewNotifier creates a new webhook Notifier.
func NewNotifier(
	p *pool.BackendPool,
	webhookURL string,
	cooldownSeconds, batchInterval, reportActive, reportIdle int,
	logger zerolog.Logger,
) *Notifier {
	return &Notifier{
		pool:          p,
		webhookURL:    webhookURL,
		client:        &http.Client{Timeout: 10 * time.Second},
		cooldown:      time.Duration(cooldownSeconds) * time.Second,
		batchInterval: time.Duration(batchInterval) * time.Second,
		reportActive:  time.Duration(reportActive) * time.Second,
		reportIdle:    time.Duration(reportIdle) * time.Second,
		logger:        logger.With().Str("component", "webhook").Logger(),
		lastNotify:    make(map[string]time.Time),
		lastActivity:  time.Now(),
		startTime:     time.Now(),
	}
}

// HandleStatusChange is called when a backend's health status changes.
func (n *Notifier) HandleStatusChange(backendID string, healthy bool, backend *model.Backend) {
	if n.webhookURL == "" {
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	n.lastActivity = time.Now()

	// Check cooldown for alerts
	if !healthy {
		if last, ok := n.lastNotify[backendID]; ok && time.Since(last) < n.cooldown {
			return
		}
		n.pendingAlerts = append(n.pendingAlerts, alertEntry{
			Backend:   backend,
			Healthy:   healthy,
			Timestamp: time.Now(),
		})
		// Start batch timer if not already running
		if n.batchTimer == nil {
			n.batchTimer = time.AfterFunc(n.batchInterval, n.flushAlerts)
		}
	} else {
		// Recovery notifications are sent immediately
		n.lastNotify[backendID] = time.Now()
		go n.sendRecovery(backend)
	}
}

// SendStartupReport sends the initial startup report.
func (n *Notifier) SendStartupReport(ctx context.Context) {
	if n.webhookURL == "" {
		return
	}

	backends := n.pool.AllBackends()
	healthy := 0
	for _, b := range backends {
		if b.IsHealthy() {
			healthy++
		}
	}

	models := n.pool.ModelNames()
	content := fmt.Sprintf("## 🚀 LLM Router Started\n\n"+
		"- **Total Backends**: %d\n"+
		"- **Healthy**: %d\n"+
		"- **Models**: %s\n"+
		"- **Time**: %s",
		len(backends), healthy,
		strings.Join(models, ", "),
		time.Now().Format("2006-01-02 15:04:05"))

	n.send(ctx, content)
}

// RunPeriodicReport starts the periodic report loop.
func (n *Notifier) RunPeriodicReport(ctx context.Context) {
	if n.webhookURL == "" {
		return
	}

	n.logger.Info().Msg("starting periodic webhook reports")
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	var lastReport time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			interval := n.reportIdle
			if time.Since(n.lastActivity) < n.reportActive {
				interval = n.reportActive
			}
			if time.Since(lastReport) >= interval {
				n.sendPeriodicReport(ctx)
				lastReport = time.Now()
			}
		}
	}
}

func (n *Notifier) flushAlerts() {
	n.mu.Lock()
	alerts := n.pendingAlerts
	n.pendingAlerts = nil
	n.batchTimer = nil
	n.mu.Unlock()

	if len(alerts) == 0 {
		return
	}

	var lines []string
	lines = append(lines, "## ⚠️ LLM Router Backend Health Alert\n")
	for _, a := range alerts {
		lines = append(lines, fmt.Sprintf("- **%s** → <font color=\"warning\">UNHEALTHY</font> at %s",
			a.Backend.ID, a.Timestamp.Format("15:04:05")))
		n.mu.Lock()
		n.lastNotify[a.Backend.ID] = time.Now()
		n.mu.Unlock()
	}

	content := strings.Join(lines, "\n")
	n.send(context.Background(), content)
}

func (n *Notifier) sendRecovery(b *model.Backend) {
	content := fmt.Sprintf("## ✅ LLM Router Backend Recovered\n\n"+
		"- **Backend**: %s\n"+
		"- **Time**: %s",
		b.ID, time.Now().Format("2006-01-02 15:04:05"))
	n.send(context.Background(), content)
}

func (n *Notifier) sendPeriodicReport(ctx context.Context) {
	backends := n.pool.AllBackends()
	healthy, unhealthy := 0, 0
	for _, b := range backends {
		if b.IsHealthy() {
			healthy++
		} else {
			unhealthy++
		}
	}

	models := n.pool.ModelNames()
	uptime := time.Since(n.startTime).Round(time.Second)

	var modelLines []string
	for _, m := range models {
		all := n.pool.GetBackendsForModel(m)
		h := 0
		for _, b := range all {
			if b.IsHealthy() {
				h++
			}
		}
		modelLines = append(modelLines, fmt.Sprintf("  - **%s**: %d/%d healthy", m, h, len(all)))
	}

	content := fmt.Sprintf("## 📊 LLM Router Status Report\n\n"+
		"- **Uptime**: %s\n"+
		"- **Total Backends**: %d (✅ %d / ❌ %d)\n"+
		"- **Models**:\n%s\n"+
		"- **Time**: %s",
		uptime, len(backends), healthy, unhealthy,
		strings.Join(modelLines, "\n"),
		time.Now().Format("2006-01-02 15:04:05"))

	n.send(ctx, content)
}

func (n *Notifier) send(ctx context.Context, content string) {
	msg := webhookMessage{
		MsgType:  "markdown",
		Markdown: markdownContent{Content: content},
	}

	body, err := json.Marshal(msg)
	if err != nil {
		n.logger.Error().Err(err).Msg("failed to marshal webhook message")
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
	if err != nil {
		n.logger.Error().Err(err).Msg("failed to create webhook request")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		n.logger.Error().Err(err).Msg("failed to send webhook")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		n.logger.Warn().Int("status", resp.StatusCode).Msg("webhook returned non-200")
	}
}
