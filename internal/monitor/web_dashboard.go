package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"

	"llm-router/internal/pool"
	"llm-router/internal/ratelimit"
	"llm-router/internal/util"

	"github.com/rs/zerolog"
)

// WebDashboard serves a web-based monitoring dashboard over HTTP.
type WebDashboard struct {
	pool          *pool.BackendPool
	rateLimiter   *ratelimit.RateLimiter
	completionRPM *CompletionRPM
	stats         *util.RequestStats
	logger        zerolog.Logger
	startTime     time.Time
	addr          string

	// time-series history (ring buffer, last N samples)
	historyMu sync.Mutex
	history   []historyPoint
}

type historyPoint struct {
	Time       string `json:"time"`
	TotalRPM   int    `json:"total_rpm"`
	CompRPM    int    `json:"comp_rpm"`
	Running    int    `json:"running"`
	Waiting    int    `json:"waiting"`
	Healthy    int    `json:"healthy"`
	Unhealthy  int    `json:"unhealthy"`
	Conns      int64  `json:"conns"`
	Pending    int64  `json:"pending"`

	// Per-backend breakdown
	Backends map[string]backendHistoryPoint `json:"backends"`
	// Per-model breakdown
	Models map[string]modelHistoryPoint `json:"models"`
}

type backendHistoryPoint struct {
	SendRPM  int   `json:"send_rpm"`
	CompRPM  int   `json:"comp_rpm"`
	Running  int   `json:"running"`
	Waiting  int   `json:"waiting"`
	Conns    int64 `json:"conns"`
	Pending  int64 `json:"pending"`
	Healthy  bool  `json:"healthy"`
}

type modelHistoryPoint struct {
	SendRPM  int `json:"send_rpm"`
	CompRPM  int `json:"comp_rpm"`
	Running  int `json:"running"`
	Waiting  int `json:"waiting"`
	Conns    int64 `json:"conns"`
	Healthy  int `json:"healthy"`
	Total    int `json:"total"`
}

const maxHistoryPoints = 120 // 2 minutes at 1s interval

// NewWebDashboard creates a new WebDashboard.
func NewWebDashboard(
	p *pool.BackendPool,
	rateLimiter *ratelimit.RateLimiter,
	completionRPM *CompletionRPM,
	stats *util.RequestStats,
	addr string,
	logger zerolog.Logger,
) *WebDashboard {
	return &WebDashboard{
		pool:          p,
		rateLimiter:   rateLimiter,
		completionRPM: completionRPM,
		stats:         stats,
		addr:          addr,
		logger:        logger.With().Str("component", "web_dashboard").Logger(),
		startTime:     time.Now(),
	}
}

// Run starts the web dashboard HTTP server. Blocks until ctx is cancelled.
func (w *WebDashboard) Run(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handleIndex)
	mux.HandleFunc("/charts", w.handleCharts)
	mux.HandleFunc("/api/dashboard", w.handleAPI)
	mux.HandleFunc("/api/history", w.handleHistory)

	// start history collection goroutine
	go w.collectHistory(ctx)

	srv := &http.Server{
		Addr:    w.addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	w.logger.Info().Str("addr", w.addr).Msg("web dashboard listening")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		w.logger.Error().Err(err).Msg("web dashboard server error")
	}
}

// --- JSON API types ---

type dashboardData struct {
	System   systemInfo      `json:"system"`
	Requests requestInfo     `json:"requests"`
	Summary  []summaryInfo   `json:"summary"`
	Models   []modelInfo     `json:"models"`
	Backends []backendInfo   `json:"backends"`
}

type systemInfo struct {
	Uptime     string `json:"uptime"`
	Goroutines int    `json:"goroutines"`
	MemoryMB   float64 `json:"memory_mb"`
	Time       string `json:"time"`
}

type requestInfo struct {
	Total       int64   `json:"total"`
	Success     int64   `json:"success"`
	Failures    int64   `json:"failures"`
	SuccessRate float64 `json:"success_rate"`
}

type summaryInfo struct {
	SourceType     string `json:"source_type"`
	Total          int    `json:"total"`
	Healthy        int    `json:"healthy"`
	TotalConns     int64  `json:"total_conns"`
	TotalPending   int64  `json:"total_pending"`
	TotalSendRPM   int    `json:"total_send_rpm"`
	TotalCompRPM   int    `json:"total_comp_rpm"`
	TotalRunning   int    `json:"total_running"`
	TotalWaiting   int    `json:"total_waiting"`
	TotalCompleted int64  `json:"total_completed"`
}

type modelInfo struct {
	Name           string `json:"name"`
	Healthy        int    `json:"healthy"`
	Total          int    `json:"total"`
	Running        int    `json:"running"`
	Waiting        int    `json:"waiting"`
	CompRPM        int    `json:"comp_rpm"`
	TotalCompleted int64  `json:"total_completed"`
}

type backendInfo struct {
	ID               string   `json:"id"`
	ModelName        string   `json:"model_name"`
	Healthy          bool     `json:"healthy"`
	Engine           string   `json:"engine"`
	SourceType       string   `json:"source_type"`
	Connections      int64    `json:"connections"`
	Pending          int64    `json:"pending"`
	SendRPM          int      `json:"send_rpm"`
	CompRPM          int      `json:"comp_rpm"`
	RPMLimit         *int     `json:"rpm_limit"`
	Running          *int     `json:"running"`
	Waiting          *int     `json:"waiting"`
	GPUCache         *float64 `json:"gpu_cache"`
	PromptTokens     *float64 `json:"prompt_tokens"`
	GenerationTokens *float64 `json:"generation_tokens"`
	TotalRequests    int64    `json:"total_requests"`
	Completed        int64    `json:"completed"`
}

func (w *WebDashboard) handleAPI(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Access-Control-Allow-Origin", "*")

	data := w.collectData()
	json.NewEncoder(rw).Encode(data)
}

func (w *WebDashboard) handleHistory(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Access-Control-Allow-Origin", "*")

	w.historyMu.Lock()
	pts := make([]historyPoint, len(w.history))
	copy(pts, w.history)
	w.historyMu.Unlock()

	json.NewEncoder(rw).Encode(pts)
}

func (w *WebDashboard) collectHistory(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			backends := w.pool.AllBackends()
			completionRPMs := w.completionRPM.AllRPMs()

			var totalRPM, compRPM, running, waiting, healthy, unhealthy int
			var conns, pending int64

			backendPts := make(map[string]backendHistoryPoint)
			modelAgg := make(map[string]*modelHistoryPoint)

			for _, b := range backends {
				bHealthy := b.IsHealthy()
				if bHealthy {
					healthy++
				} else {
					unhealthy++
				}
				bConns := b.ActiveConnections()
				conns += bConns
				bPending := b.PendingRequests()
				pending += bPending
				bSendRPM := w.rateLimiter.CurrentRPM(b.ID)
				totalRPM += bSendRPM
				bCompRPM := completionRPMs[b.ID]
				compRPM += bCompRPM

				var bRunning, bWaiting int
				if m := b.GetLoadMetrics(); m != nil && m.Error == "" {
					bRunning = m.RunningRequests
					bWaiting = m.WaitingRequests
					running += bRunning
					waiting += bWaiting
				}

				backendPts[b.ID] = backendHistoryPoint{
					SendRPM: bSendRPM,
					CompRPM: bCompRPM,
					Running: bRunning,
					Waiting: bWaiting,
					Conns:   bConns,
					Pending: bPending,
					Healthy: bHealthy,
				}

				mp, ok := modelAgg[b.ModelName]
				if !ok {
					mp = &modelHistoryPoint{}
					modelAgg[b.ModelName] = mp
				}
				mp.SendRPM += bSendRPM
				mp.CompRPM += bCompRPM
				mp.Running += bRunning
				mp.Waiting += bWaiting
				mp.Conns += bConns
				mp.Total++
				if bHealthy {
					mp.Healthy++
				}
			}

			modelPts := make(map[string]modelHistoryPoint, len(modelAgg))
			for k, v := range modelAgg {
				modelPts[k] = *v
			}

			pt := historyPoint{
				Time:      time.Now().Format("15:04:05"),
				TotalRPM:  totalRPM,
				CompRPM:   compRPM,
				Running:   running,
				Waiting:   waiting,
				Healthy:   healthy,
				Unhealthy: unhealthy,
				Conns:     conns,
				Pending:   pending,
				Backends:  backendPts,
				Models:    modelPts,
			}

			w.historyMu.Lock()
			w.history = append(w.history, pt)
			if len(w.history) > maxHistoryPoints {
				w.history = w.history[len(w.history)-maxHistoryPoints:]
			}
			w.historyMu.Unlock()
		}
	}
}

func (w *WebDashboard) handleCharts(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(rw, chartsHTML)
}

func (w *WebDashboard) collectData() dashboardData {
	// System
	uptime := time.Since(w.startTime).Round(time.Second)
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	sys := systemInfo{
		Uptime:     uptime.String(),
		Goroutines: runtime.NumGoroutine(),
		MemoryMB:   float64(memStats.Alloc) / 1024 / 1024,
		Time:       time.Now().Format("15:04:05"),
	}

	// Requests
	total := w.stats.Total()
	success := w.stats.Success()
	failures := w.stats.Failures()
	rate := float64(0)
	if total > 0 {
		rate = float64(success) / float64(total) * 100
	}
	req := requestInfo{
		Total:       total,
		Success:     success,
		Failures:    failures,
		SuccessRate: rate,
	}

	// Models
	modelNames := w.pool.ModelNames()
	sort.Strings(modelNames)
	completionRPMs := w.completionRPM.AllRPMs()
	allBackendStats := w.stats.AllBackendStats()

	var models []modelInfo
	for _, name := range modelNames {
		all := w.pool.GetBackendsForModel(name)
		healthy := 0
		totalRunning, totalWaiting := 0, 0
		modelCompRPM := 0
		var modelCompleted int64
		for _, b := range all {
			if b.IsHealthy() {
				healthy++
			}
			if m := b.GetLoadMetrics(); m != nil && m.Error == "" {
				totalRunning += m.RunningRequests
				totalWaiting += m.WaitingRequests
			}
			modelCompRPM += completionRPMs[b.ID]
			if bs := allBackendStats[b.ID]; bs != nil {
				modelCompleted += bs.Success.Load()
			}
		}
		models = append(models, modelInfo{
			Name:           name,
			Healthy:        healthy,
			Total:          len(all),
			Running:        totalRunning,
			Waiting:        totalWaiting,
			CompRPM:        modelCompRPM,
			TotalCompleted: modelCompleted,
		})
	}

	// Summary by source type
	allBackends := w.pool.AllBackends()
	summaryMap := make(map[string]*summaryInfo)
	for _, b := range allBackends {
		st := b.SourceType
		if st == "" {
			st = "unknown"
		}
		si, ok := summaryMap[st]
		if !ok {
			si = &summaryInfo{SourceType: st}
			summaryMap[st] = si
		}
		si.Total++
		if b.IsHealthy() {
			si.Healthy++
		}
		si.TotalConns += b.ActiveConnections()
		si.TotalPending += b.PendingRequests()
		si.TotalSendRPM += w.rateLimiter.CurrentRPM(b.ID)
		si.TotalCompRPM += completionRPMs[b.ID]
		if m := b.GetLoadMetrics(); m != nil && m.Error == "" {
			si.TotalRunning += m.RunningRequests
			si.TotalWaiting += m.WaitingRequests
		}
		if bs := allBackendStats[b.ID]; bs != nil {
			si.TotalCompleted += bs.Success.Load()
		}
	}
	summaryOrder := []string{"self_deployed", "openai"}
	var summary []summaryInfo
	added := make(map[string]bool)
	for _, st := range summaryOrder {
		if si, ok := summaryMap[st]; ok {
			summary = append(summary, *si)
			added[st] = true
		}
	}
	for st, si := range summaryMap {
		if !added[st] {
			summary = append(summary, *si)
		}
	}

	// Backends
	sort.Slice(allBackends, func(i, j int) bool {
		if allBackends[i].ModelName != allBackends[j].ModelName {
			return allBackends[i].ModelName < allBackends[j].ModelName
		}
		return allBackends[i].ID < allBackends[j].ID
	})

	completionRPMs2 := w.completionRPM.AllRPMs()
	var backends []backendInfo
	for _, b := range allBackends {
		engine := string(b.GetEngineType())
		// If engine is "unknown", use source_type as a better display value
		if engine == "unknown" || engine == "" {
			if b.SourceType != "" {
				engine = b.SourceType
			}
		}
		bi := backendInfo{
			ID:          b.ID,
			ModelName:   b.ModelName,
			Healthy:     b.IsHealthy(),
			Engine:      engine,
			SourceType:  b.SourceType,
			Connections: b.ActiveConnections(),
			Pending:     b.PendingRequests(),
			SendRPM:     w.rateLimiter.CurrentRPM(b.ID),
			CompRPM:     completionRPMs2[b.ID],
			RPMLimit:    b.RPMLimit,
		}
		if m := b.GetLoadMetrics(); m != nil && m.Error == "" {
			bi.Running = &m.RunningRequests
			bi.Waiting = &m.WaitingRequests
			bi.GPUCache = &m.GPUCacheUsage
			bi.PromptTokens = &m.PromptTokens
			bi.GenerationTokens = &m.GenerationTokens
		}
		if bs := allBackendStats[b.ID]; bs != nil {
			bi.TotalRequests = bs.Total.Load()
			bi.Completed = bs.Success.Load()
		}
		backends = append(backends, bi)
	}

	return dashboardData{
		System:   sys,
		Requests: req,
		Summary:  summary,
		Models:   models,
		Backends: backends,
	}
}

func (w *WebDashboard) handleIndex(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(rw, indexHTML)
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>LLM Router Dashboard</title>
<style>
  @import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap');
  :root {
    --bg: #0a0f1e;
    --surface: #111827;
    --surface2: #1e293b;
    --surface3: #283548;
    --border: rgba(99,118,150,0.28);
    --border-hover: rgba(99,118,150,0.55);
    --text: #e8ecf4;
    --text-dim: #8b98b0;
    --text-muted: #5a6680;
    --accent: #38bdf8;
    --accent-dim: rgba(56,189,248,0.12);
    --green: #34d399;
    --green-dim: rgba(52,211,153,0.12);
    --red: #fb7185;
    --red-dim: rgba(251,113,133,0.12);
    --yellow: #fbbf24;
    --yellow-dim: rgba(251,191,36,0.12);
    --blue: #60a5fa;
    --blue-dim: rgba(96,165,250,0.12);
    --purple: #a78bfa;
    --purple-dim: rgba(167,139,250,0.12);
    --glow: 0 0 20px rgba(56,189,248,0.08);
    --card-shadow: 0 1px 3px rgba(0,0,0,0.3), 0 1px 2px rgba(0,0,0,0.2);
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: 'Inter', -apple-system, BlinkMacSystemFont, sans-serif;
    background: var(--bg);
    color: var(--text);
    min-height: 100vh;
    line-height: 1.5;
  }
  .container { max-width: 1440px; margin: 0 auto; padding: 20px 28px 40px; }

  /* Header */
  .header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 16px 0 20px;
    margin-bottom: 24px;
    border-bottom: 1px solid var(--border);
  }
  .header-left { display: flex; align-items: center; gap: 14px; }
  .logo {
    width: 36px; height: 36px;
    background: linear-gradient(135deg, var(--accent), var(--purple));
    border-radius: 10px;
    display: flex; align-items: center; justify-content: center;
    font-weight: 700; font-size: 16px; color: #fff;
    box-shadow: 0 2px 12px rgba(56,189,248,0.25);
  }
  .header-title { font-size: 20px; font-weight: 700; color: var(--text); }
  .header-subtitle { font-size: 12px; color: var(--text-dim); margin-top: 1px; }
  .header-right { display: flex; align-items: center; gap: 12px; }
  .nav-pill {
    display: inline-flex; align-items: center; gap: 6px;
    padding: 6px 14px; border-radius: 8px;
    font-size: 13px; font-weight: 500;
    text-decoration: none; color: var(--text-dim);
    border: 1px solid transparent;
    transition: all 0.15s ease;
  }
  .nav-pill:hover { color: var(--text); background: var(--surface2); }
  .nav-pill.active {
    color: var(--accent); background: var(--accent-dim);
    border-color: rgba(56,189,248,0.2);
  }
  .live-badge {
    display: inline-flex; align-items: center; gap: 6px;
    padding: 5px 12px; border-radius: 20px;
    font-size: 11px; font-weight: 600; color: var(--green);
    background: var(--green-dim); border: 1px solid rgba(52,211,153,0.2);
    letter-spacing: 0.3px;
  }
  .live-dot {
    width: 6px; height: 6px; border-radius: 50%;
    background: var(--green);
    animation: pulse-glow 2s ease-in-out infinite;
  }
  @keyframes pulse-glow {
    0%, 100% { opacity: 1; box-shadow: 0 0 4px rgba(52,211,153,0.6); }
    50% { opacity: 0.4; box-shadow: none; }
  }
  .clock { font-family: 'JetBrains Mono', monospace; font-size: 12px; color: var(--text-muted); }

  /* Stat Cards */
  .stats-row {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(155px, 1fr));
    gap: 12px;
    margin-bottom: 24px;
  }
  .stat-card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 16px 18px;
    position: relative;
    overflow: hidden;
    transition: border-color 0.2s, box-shadow 0.2s;
  }
  .stat-card:hover { border-color: var(--border-hover); box-shadow: var(--glow); }
  .stat-card .label {
    font-size: 11px; font-weight: 600;
    text-transform: uppercase; letter-spacing: 0.6px;
    color: var(--text-muted); margin-bottom: 8px;
  }
  .stat-card .value {
    font-family: 'JetBrains Mono', monospace;
    font-size: clamp(14px, 2.2vw, 24px); font-weight: 700;
    line-height: 1;
    word-break: break-all;
    overflow-wrap: anywhere;
  }
  .stat-card .value.green { color: var(--green); }
  .stat-card .value.red { color: var(--red); }
  .stat-card .value.blue { color: var(--blue); }
  .stat-card .value.accent { color: var(--accent); }
  .stat-card .value.purple { color: var(--purple); }
  .stat-card::after {
    content: ''; position: absolute; top: 0; right: 0;
    width: 60px; height: 60px; border-radius: 50%;
    opacity: 0.04; pointer-events: none;
  }
  .stat-card.c-green::after { background: var(--green); }
  .stat-card.c-red::after { background: var(--red); }
  .stat-card.c-blue::after { background: var(--blue); }
  .stat-card.c-accent::after { background: var(--accent); }
  .stat-card.c-purple::after { background: var(--purple); }

  /* Sections */
  .section { margin-bottom: 28px; }
  .section-header {
    display: flex; align-items: center; justify-content: space-between;
    margin-bottom: 14px;
  }
  .section-title {
    font-size: 14px; font-weight: 600; color: var(--text);
    display: flex; align-items: center; gap: 8px;
  }
  .section-title .icon {
    width: 28px; height: 28px; border-radius: 8px;
    display: flex; align-items: center; justify-content: center;
    font-size: 14px;
  }
  .section-title .icon.purple { background: var(--purple-dim); }
  .section-title .icon.blue { background: var(--blue-dim); }
  .section-title .icon.accent { background: var(--accent-dim); }
  .section-count {
    font-size: 11px; font-weight: 500; color: var(--text-muted);
    background: var(--surface2); padding: 3px 10px; border-radius: 12px;
  }

  /* Summary Cards */
  .summary-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
    gap: 12px;
  }
  .summary-card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 18px;
    transition: border-color 0.2s, box-shadow 0.2s;
  }
  .summary-card:hover { border-color: var(--border-hover); box-shadow: var(--glow); }
  .summary-card .source-label {
    display: inline-flex; align-items: center; gap: 6px;
    font-size: 12px; font-weight: 700;
    text-transform: uppercase; letter-spacing: 0.8px;
    margin-bottom: 14px; color: var(--purple);
  }
  .summary-card .source-label .dot {
    width: 8px; height: 8px; border-radius: 3px;
    background: var(--purple);
  }
  .summary-metrics {
    display: grid; grid-template-columns: repeat(4, 1fr);
    gap: 10px 14px;
    overflow: hidden;
  }
  .summary-metric { display: flex; flex-direction: column; min-width: 0; }
  .summary-metric .sm-label { font-size: 10px; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.5px; margin-bottom: 2px; }
  .summary-metric .sm-value { font-family: 'JetBrains Mono', monospace; font-size: clamp(12px, 1.5vw, 15px); font-weight: 600; color: var(--text); overflow: hidden; text-overflow: ellipsis; }

  /* Model Cards */
  .models-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
    gap: 12px;
  }
  .model-card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 18px;
    transition: border-color 0.2s, box-shadow 0.2s;
  }
  .model-card:hover { border-color: var(--border-hover); box-shadow: var(--glow); }
  .model-card .model-header {
    display: flex; align-items: flex-start; justify-content: space-between;
    margin-bottom: 14px;
  }
  .model-card .model-name {
    font-weight: 600; font-size: 14px; color: var(--accent);
    word-break: break-all; line-height: 1.4;
  }
  .health-badge {
    display: inline-flex; align-items: center; gap: 4px;
    padding: 3px 10px; border-radius: 20px;
    font-size: 11px; font-weight: 600; white-space: nowrap;
  }
  .health-badge.all-healthy { background: var(--green-dim); color: var(--green); border: 1px solid rgba(52,211,153,0.2); }
  .health-badge.partial { background: var(--yellow-dim); color: var(--yellow); border: 1px solid rgba(251,191,36,0.2); }
  .health-badge.all-down { background: var(--red-dim); color: var(--red); border: 1px solid rgba(251,113,133,0.2); }
  .model-metrics {
    display: grid; grid-template-columns: repeat(4, 1fr); gap: 8px;
    overflow: hidden;
  }
  .model-metric { display: flex; flex-direction: column; min-width: 0; }
  .model-metric .mm-label { font-size: 10px; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.4px; margin-bottom: 2px; }
  .model-metric .mm-value { font-family: 'JetBrains Mono', monospace; font-size: clamp(11px, 1.4vw, 14px); font-weight: 600; color: var(--text); overflow: hidden; text-overflow: ellipsis; }

  /* Table */
  .table-container {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    overflow: hidden;
  }
  .table-scroll { overflow-x: auto; }
  table { width: 100%; border-collapse: collapse; font-size: 13px; table-layout: auto; }
  thead th {
    text-align: left; padding: 12px 16px;
    font-size: 10px; font-weight: 700;
    text-transform: uppercase; letter-spacing: 0.8px;
    color: var(--text-muted);
    background: rgba(30,41,59,0.5);
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
    position: sticky; top: 0; z-index: 1;
  }
  tbody td {
    padding: 11px 16px;
    border-bottom: 1px solid rgba(99,118,150,0.1);
    white-space: nowrap;
    font-variant-numeric: tabular-nums;
  }
  tbody tr { transition: background 0.1s; }
  tbody tr:nth-child(even) { background: rgba(17,24,39,0.3); }
  tbody tr:hover { background: rgba(56,189,248,0.04); }
  tbody tr:last-child td { border-bottom: none; }

  .status-chip {
    display: inline-flex; align-items: center; gap: 5px;
    padding: 3px 10px; border-radius: 6px;
    font-size: 11px; font-weight: 600;
  }
  .status-chip.healthy { background: var(--green-dim); color: var(--green); }
  .status-chip.down { background: var(--red-dim); color: var(--red); }
  .status-chip .sdot {
    width: 6px; height: 6px; border-radius: 50%;
  }
  .status-chip.healthy .sdot { background: var(--green); box-shadow: 0 0 6px rgba(52,211,153,0.5); }
  .status-chip.down .sdot { background: var(--red); box-shadow: 0 0 6px rgba(251,113,133,0.5); }

  .engine-tag {
    display: inline-block; padding: 2px 8px; border-radius: 4px;
    font-size: 11px; font-weight: 600;
    background: var(--surface3); color: var(--text-dim);
  }
  .source-tag {
    display: inline-block; padding: 2px 8px; border-radius: 4px;
    font-size: 11px; font-weight: 500;
    background: var(--purple-dim); color: var(--purple);
  }

  .gpu-bar-track {
    width: 64px; height: 6px;
    background: var(--surface3);
    border-radius: 3px;
    display: inline-block; vertical-align: middle;
    margin-right: 8px; overflow: hidden;
  }
  .gpu-bar-fill {
    height: 100%; border-radius: 3px;
    transition: width 0.3s ease;
  }
  .gpu-bar-fill.low { background: var(--green); }
  .gpu-bar-fill.mid { background: var(--yellow); }
  .gpu-bar-fill.high { background: var(--red); }
  .gpu-pct { font-family: 'JetBrains Mono', monospace; font-size: 12px; color: var(--text-dim); }

  .backend-id-cell {
    max-width: 280px; overflow: hidden; text-overflow: ellipsis;
    white-space: nowrap; display: block;
    font-family: 'JetBrains Mono', monospace; font-size: 12px;
    color: var(--text-dim);
  }
  .num-cell { font-family: 'JetBrains Mono', monospace; font-size: 13px; }

  .filter-bar {
    display: flex; align-items: center; gap: 10px;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border);
  }
  .filter-bar input {
    flex: 1; background: var(--bg); color: var(--text);
    border: 1px solid var(--border); border-radius: 8px;
    padding: 7px 12px; font-size: 13px; outline: none;
    font-family: 'Inter', sans-serif;
    transition: border-color 0.15s;
  }
  .filter-bar input::placeholder { color: var(--text-muted); }
  .filter-bar input:focus { border-color: var(--accent); }
  .filter-bar .count { font-size: 12px; color: var(--text-muted); white-space: nowrap; }

  /* Responsive */
  @media (max-width: 768px) {
    .container { padding: 12px 14px; }
    .header { flex-direction: column; gap: 12px; text-align: center; }
    .header-right { justify-content: center; }
    .stats-row { grid-template-columns: repeat(2, 1fr); }
    .models-grid { grid-template-columns: 1fr; }
    .summary-grid { grid-template-columns: 1fr; }
    .model-metrics { grid-template-columns: repeat(2, 1fr); }
  }
</style>
</head>
<body>
<div class="container">

<div class="header">
  <div class="header-left">
    <div class="logo">R</div>
    <div>
      <div class="header-title">LLM Router</div>
      <div class="header-subtitle" id="system-info">Loading...</div>
    </div>
  </div>
  <div class="header-right">
    <nav style="display:flex;gap:4px;">
      <a href="/" class="nav-pill active">Dashboard</a>
      <a href="/charts" class="nav-pill">Charts</a>
    </nav>
    <div class="live-badge"><span class="live-dot"></span> LIVE</div>
    <div class="clock" id="clock"></div>
  </div>
</div>

<div class="stats-row" id="stats-grid"></div>

<div class="section">
  <div class="section-header">
    <div class="section-title"><span class="icon purple">&#128202;</span> Backend Summary</div>
  </div>
  <div class="summary-grid" id="summary-grid"></div>
</div>

<div class="section">
  <div class="section-header">
    <div class="section-title"><span class="icon blue">&#129302;</span> Models</div>
    <span class="section-count" id="model-count"></span>
  </div>
  <div class="models-grid" id="models-grid"></div>
</div>

<div class="section">
  <div class="section-header">
    <div class="section-title"><span class="icon accent">&#128421;</span> Backends</div>
    <span class="section-count" id="backend-count"></span>
  </div>
  <div class="table-container">
    <div class="filter-bar">
      <input type="text" id="filter-input" placeholder="Filter backends by name, model, source...">
      <span class="count" id="filter-count"></span>
    </div>
    <div class="table-scroll">
      <table>
        <thead>
          <tr>
            <th>Backend</th>
            <th>Model</th>
            <th>Source</th>
            <th>Status</th>
            <th>Engine</th>
            <th>Conns</th>
            <th>Pending</th>
            <th>Send RPM</th>
            <th>Comp RPM</th>
            <th>RPM Limit</th>
            <th>Running</th>
            <th>Waiting</th>
            <th>Total Req</th>
            <th>Completed</th>
            <th>GPU Cache</th>
            <th>Prompt Tok</th>
            <th>Gen Tok</th>
          </tr>
        </thead>
        <tbody id="backends-tbody"></tbody>
      </table>
    </div>
  </div>
</div>

</div>

<script>
const REFRESH_MS = 1000;
let lastData = null;

async function fetchData() {
  try {
    const resp = await fetch('/api/dashboard');
    return await resp.json();
  } catch (e) {
    console.error('Fetch error:', e);
    return null;
  }
}

function renderStats(data) {
  const grid = document.getElementById('stats-grid');
  const rate = data.requests.success_rate.toFixed(1);
  const rateColor = parseFloat(rate) >= 99 ? 'green' : parseFloat(rate) >= 90 ? 'accent' : 'red';
  const rateCard = parseFloat(rate) >= 99 ? 'c-green' : parseFloat(rate) >= 90 ? 'c-accent' : 'c-red';
  grid.innerHTML =
    '<div class="stat-card c-accent"><div class="label">Uptime</div><div class="value accent">' + data.system.uptime + '</div></div>' +
    '<div class="stat-card c-blue"><div class="label">Total Requests</div><div class="value blue">' + data.requests.total.toLocaleString() + '</div></div>' +
    '<div class="stat-card c-green"><div class="label">Success</div><div class="value green">' + data.requests.success.toLocaleString() + '</div></div>' +
    '<div class="stat-card c-red"><div class="label">Failures</div><div class="value red">' + data.requests.failures.toLocaleString() + '</div></div>' +
    '<div class="stat-card ' + rateCard + '"><div class="label">Success Rate</div><div class="value ' + rateColor + '">' + rate + '%</div></div>' +
    '<div class="stat-card c-purple"><div class="label">Goroutines</div><div class="value purple">' + data.system.goroutines + '</div></div>' +
    '<div class="stat-card c-accent"><div class="label">Memory</div><div class="value accent">' + data.system.memory_mb.toFixed(1) + ' MB</div></div>';
}

function renderSystemInfo(data) {
  document.getElementById('system-info').textContent = 'Uptime ' + data.system.uptime + '  \u00b7  ' + data.system.time;
  document.getElementById('clock').textContent = data.system.time;
}

function renderSummary(data) {
  const grid = document.getElementById('summary-grid');
  if (!data.summary || data.summary.length === 0) {
    grid.innerHTML = '<div style="color:var(--text-muted);padding:20px">No backend sources configured</div>';
    return;
  }
  grid.innerHTML = data.summary.map(function(s) {
    return '<div class="summary-card">' +
      '<div class="source-label"><span class="dot"></span>' + escapeHtml(s.source_type) + '</div>' +
      '<div class="summary-metrics">' +
        '<div class="summary-metric"><span class="sm-label">Backends</span><span class="sm-value">' + s.healthy + '/' + s.total + '</span></div>' +
        '<div class="summary-metric"><span class="sm-label">Conns</span><span class="sm-value">' + s.total_conns.toLocaleString() + '</span></div>' +
        '<div class="summary-metric"><span class="sm-label">Pending</span><span class="sm-value">' + s.total_pending.toLocaleString() + '</span></div>' +
        '<div class="summary-metric"><span class="sm-label">Completed</span><span class="sm-value">' + (s.total_completed || 0).toLocaleString() + '</span></div>' +
        '<div class="summary-metric"><span class="sm-label">Send RPM</span><span class="sm-value">' + s.total_send_rpm.toLocaleString() + '</span></div>' +
        '<div class="summary-metric"><span class="sm-label">Comp RPM</span><span class="sm-value">' + s.total_comp_rpm.toLocaleString() + '</span></div>' +
        '<div class="summary-metric"><span class="sm-label">Running</span><span class="sm-value">' + s.total_running.toLocaleString() + '</span></div>' +
        '<div class="summary-metric"><span class="sm-label">Waiting</span><span class="sm-value">' + s.total_waiting.toLocaleString() + '</span></div>' +
      '</div></div>';
  }).join('');
}

function renderModels(data) {
  const grid = document.getElementById('models-grid');
  var mc = document.getElementById('model-count');
  if (!data.models || data.models.length === 0) {
    grid.innerHTML = '<div style="color:var(--text-muted);padding:20px">No models registered</div>';
    mc.textContent = '0';
    return;
  }
  mc.textContent = data.models.length + ' model' + (data.models.length > 1 ? 's' : '');
  grid.innerHTML = data.models.map(function(m) {
    var bc = 'all-healthy';
    if (m.healthy === 0) bc = 'all-down';
    else if (m.healthy < m.total) bc = 'partial';
    return '<div class="model-card">' +
      '<div class="model-header">' +
        '<div class="model-name">' + escapeHtml(m.name) + '</div>' +
        '<span class="health-badge ' + bc + '">' + m.healthy + '/' + m.total + '</span>' +
      '</div>' +
      '<div class="model-metrics">' +
        '<div class="model-metric"><span class="mm-label">Running</span><span class="mm-value">' + m.running.toLocaleString() + '</span></div>' +
        '<div class="model-metric"><span class="mm-label">Waiting</span><span class="mm-value">' + m.waiting.toLocaleString() + '</span></div>' +
        '<div class="model-metric"><span class="mm-label">Comp RPM</span><span class="mm-value">' + m.comp_rpm.toLocaleString() + '</span></div>' +
        '<div class="model-metric"><span class="mm-label">Completed</span><span class="mm-value">' + m.total_completed.toLocaleString() + '</span></div>' +
      '</div></div>';
  }).join('');
}

function extractApiBase(id) {
  var idx = id.indexOf('@');
  return idx >= 0 ? id.substring(idx + 1) : id;
}

var filterText = '';
document.getElementById('filter-input').addEventListener('input', function(e) {
  filterText = e.target.value.toLowerCase();
  if (lastData) renderBackends(lastData);
});

function renderBackends(data) {
  var tbody = document.getElementById('backends-tbody');
  var bcEl = document.getElementById('backend-count');
  var fcEl = document.getElementById('filter-count');
  if (!data.backends || data.backends.length === 0) {
    tbody.innerHTML = '<tr><td colspan="17" style="text-align:center;color:var(--text-muted);padding:32px">No backends available</td></tr>';
    bcEl.textContent = '0';
    fcEl.textContent = '';
    return;
  }
  var filtered = data.backends;
  if (filterText) {
    filtered = data.backends.filter(function(b) {
      var s = (b.id + ' ' + b.model_name + ' ' + b.source_type + ' ' + b.engine).toLowerCase();
      return s.indexOf(filterText) >= 0;
    });
  }
  bcEl.textContent = data.backends.length + ' backend' + (data.backends.length > 1 ? 's' : '');
  fcEl.textContent = filterText ? (filtered.length + ' / ' + data.backends.length) : '';

  tbody.innerHTML = filtered.map(function(b) {
    var chipClass = b.healthy ? 'healthy' : 'down';
    var chipText = b.healthy ? 'UP' : 'DOWN';
    var rpmLimit = b.rpm_limit !== null && b.rpm_limit !== undefined ? b.rpm_limit : '\u221e';
    var running = b.running !== null && b.running !== undefined ? b.running : '-';
    var waiting = b.waiting !== null && b.waiting !== undefined ? b.waiting : '-';
    var apiBase = extractApiBase(b.id);
    var gpuCell = '<span style="color:var(--text-muted)">-</span>';
    if (b.gpu_cache !== null && b.gpu_cache !== undefined) {
      var pct = b.gpu_cache.toFixed(1);
      var pctNorm = b.gpu_cache / 100;
      var barClass = pctNorm < 0.5 ? 'low' : pctNorm < 0.8 ? 'mid' : 'high';
      gpuCell = '<span class="gpu-bar-track"><span class="gpu-bar-fill ' + barClass + '" style="width:' + pct + '%"></span></span><span class="gpu-pct">' + pct + '%</span>';
    }
    return '<tr>' +
      '<td><span class="backend-id-cell" title="' + escapeHtml(b.id) + '">' + escapeHtml(apiBase) + '</span></td>' +
      '<td>' + escapeHtml(b.model_name) + '</td>' +
      '<td><span class="source-tag">' + escapeHtml(b.source_type) + '</span></td>' +
      '<td><span class="status-chip ' + chipClass + '"><span class="sdot"></span>' + chipText + '</span></td>' +
      '<td><span class="engine-tag">' + escapeHtml(b.engine) + '</span></td>' +
      '<td class="num-cell">' + b.connections.toLocaleString() + '</td>' +
      '<td class="num-cell">' + b.pending.toLocaleString() + '</td>' +
      '<td class="num-cell">' + b.send_rpm.toLocaleString() + '</td>' +
      '<td class="num-cell">' + b.comp_rpm.toLocaleString() + '</td>' +
      '<td class="num-cell">' + rpmLimit + '</td>' +
      '<td class="num-cell">' + running + '</td>' +
      '<td class="num-cell">' + waiting + '</td>' +
      '<td class="num-cell">' + (b.total_requests || 0).toLocaleString() + '</td>' +
      '<td class="num-cell">' + (b.completed || 0).toLocaleString() + '</td>' +
      '<td>' + gpuCell + '</td>' +
      '<td class="num-cell">' + formatTokens(b.prompt_tokens) + '</td>' +
      '<td class="num-cell">' + formatTokens(b.generation_tokens) + '</td>' +
    '</tr>';
  }).join('');
}

function escapeHtml(str) {
  var div = document.createElement('div');
  div.textContent = str;
  return div.innerHTML;
}

function formatTokens(val) {
  if (val === null || val === undefined) return '<span style="color:var(--text-muted)">-</span>';
  if (val >= 1e9) return (val / 1e9).toFixed(2) + 'B';
  if (val >= 1e6) return (val / 1e6).toFixed(2) + 'M';
  if (val >= 1e3) return (val / 1e3).toFixed(1) + 'K';
  return Math.round(val).toLocaleString();
}

async function refresh() {
  var data = await fetchData();
  if (!data) return;
  lastData = data;
  renderSystemInfo(data);
  renderStats(data);
  renderSummary(data);
  renderModels(data);
  renderBackends(data);
}

refresh();
setInterval(refresh, REFRESH_MS);
</script>
</body>
</html>`

const chartsHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>LLM Router Charts</title>
<style>
  @import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap');
  :root {
    --bg: #0a0f1e;
    --surface: #111827;
    --surface2: #1e293b;
    --surface3: #283548;
    --border: rgba(99,118,150,0.28);
    --border-hover: rgba(99,118,150,0.55);
    --text: #e8ecf4;
    --text-dim: #8b98b0;
    --text-muted: #5a6680;
    --accent: #38bdf8;
    --accent-dim: rgba(56,189,248,0.12);
    --green: #34d399;
    --green-dim: rgba(52,211,153,0.12);
    --red: #fb7185;
    --red-dim: rgba(251,113,133,0.12);
    --yellow: #fbbf24;
    --blue: #60a5fa;
    --purple: #a78bfa;
    --purple-dim: rgba(167,139,250,0.12);
    --glow: 0 0 20px rgba(56,189,248,0.08);
    --card-shadow: 0 1px 3px rgba(0,0,0,0.3), 0 1px 2px rgba(0,0,0,0.2);
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: 'Inter', -apple-system, BlinkMacSystemFont, sans-serif;
    background: var(--bg);
    color: var(--text);
    min-height: 100vh;
    line-height: 1.5;
  }
  .container { max-width: 1440px; margin: 0 auto; padding: 20px 28px 40px; }

  /* Header */
  .header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 16px 0 20px;
    margin-bottom: 24px;
    border-bottom: 1px solid var(--border);
  }
  .header-left { display: flex; align-items: center; gap: 14px; }
  .logo {
    width: 36px; height: 36px;
    background: linear-gradient(135deg, var(--accent), var(--purple));
    border-radius: 10px;
    display: flex; align-items: center; justify-content: center;
    font-weight: 700; font-size: 16px; color: #fff;
    box-shadow: 0 2px 12px rgba(56,189,248,0.25);
  }
  .header-title { font-size: 20px; font-weight: 700; color: var(--text); }
  .header-subtitle { font-size: 12px; color: var(--text-dim); margin-top: 1px; }
  .header-right { display: flex; align-items: center; gap: 12px; }
  .nav-pill {
    display: inline-flex; align-items: center; gap: 6px;
    padding: 6px 14px; border-radius: 8px;
    font-size: 13px; font-weight: 500;
    text-decoration: none; color: var(--text-dim);
    border: 1px solid transparent;
    transition: all 0.15s ease;
  }
  .nav-pill:hover { color: var(--text); background: var(--surface2); }
  .nav-pill.active {
    color: var(--accent); background: var(--accent-dim);
    border-color: rgba(56,189,248,0.2);
  }
  .live-badge {
    display: inline-flex; align-items: center; gap: 6px;
    padding: 5px 12px; border-radius: 20px;
    font-size: 11px; font-weight: 600; color: var(--green);
    background: var(--green-dim); border: 1px solid rgba(52,211,153,0.2);
    letter-spacing: 0.3px;
  }
  .live-dot {
    width: 6px; height: 6px; border-radius: 50%;
    background: var(--green);
    animation: pulse-glow 2s ease-in-out infinite;
  }
  @keyframes pulse-glow {
    0%, 100% { opacity: 1; box-shadow: 0 0 4px rgba(52,211,153,0.6); }
    50% { opacity: 0.4; box-shadow: none; }
  }
  .clock { font-family: 'JetBrains Mono', monospace; font-size: 12px; color: var(--text-muted); }

  /* Controls */
  .controls {
    display: flex;
    align-items: center;
    gap: 16px;
    margin-bottom: 24px;
    flex-wrap: wrap;
  }
  .control-group {
    display: flex; align-items: center; gap: 8px;
  }
  .control-label {
    font-size: 11px; font-weight: 600;
    text-transform: uppercase; letter-spacing: 0.6px;
    color: var(--text-muted);
  }
  .pill-group {
    display: flex; gap: 4px;
  }
  .pill-btn {
    padding: 5px 14px; border-radius: 8px;
    font-size: 12px; font-weight: 500;
    color: var(--text-dim); cursor: pointer;
    border: 1px solid var(--border);
    background: transparent;
    transition: all 0.15s ease;
    font-family: 'Inter', sans-serif;
  }
  .pill-btn:hover { color: var(--text); background: var(--surface2); border-color: var(--border-hover); }
  .pill-btn.active {
    color: var(--accent); background: var(--accent-dim);
    border-color: rgba(56,189,248,0.3);
  }

  /* Charts */
  .charts-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(480px, 1fr));
    gap: 16px;
  }
  .chart-card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 20px;
    transition: border-color 0.2s, box-shadow 0.2s;
  }
  .chart-card:hover { border-color: var(--border-hover); box-shadow: var(--glow); }
  .chart-card h3 {
    font-size: 12px; font-weight: 600;
    text-transform: uppercase; letter-spacing: 0.6px;
    color: var(--text-muted); margin-bottom: 14px;
  }
  .chart-container {
    position: relative;
    height: 220px;
  }
  canvas { width: 100% !important; height: 100% !important; }

  .tooltip-box {
    position: absolute; display: none;
    background: var(--surface2); color: var(--text);
    border: 1px solid var(--border-hover);
    border-radius: 8px; padding: 8px 12px;
    font-size: 12px; pointer-events: none;
    z-index: 10; white-space: nowrap;
    box-shadow: 0 4px 16px rgba(0,0,0,0.4);
  }
  .tooltip-box .tt-time {
    font-family: 'JetBrains Mono', monospace;
    font-size: 10px; color: var(--text-muted);
    margin-bottom: 4px;
  }
  .tooltip-box .tt-row {
    display: flex; align-items: center; gap: 6px;
    margin: 2px 0;
  }
  .tooltip-box .tt-dot {
    width: 8px; height: 8px; border-radius: 3px; flex-shrink: 0;
  }
  .tooltip-box .tt-name { color: var(--text-dim); }
  .tooltip-box .tt-val {
    font-family: 'JetBrains Mono', monospace;
    font-weight: 600; margin-left: auto; padding-left: 12px;
  }

  @media (max-width: 768px) {
    .container { padding: 12px 14px; }
    .header { flex-direction: column; gap: 12px; text-align: center; }
    .header-right { justify-content: center; }
    .charts-grid { grid-template-columns: 1fr; }
    .controls { flex-direction: column; align-items: flex-start; }
  }
</style>
</head>
<body>
<div class="container">

<div class="header">
  <div class="header-left">
    <div class="logo">R</div>
    <div>
      <div class="header-title">LLM Router</div>
      <div class="header-subtitle">Real-time Charts</div>
    </div>
  </div>
  <div class="header-right">
    <nav style="display:flex;gap:4px;">
      <a href="/" class="nav-pill">Dashboard</a>
      <a href="/charts" class="nav-pill active">Charts</a>
    </nav>
    <div class="live-badge"><span class="live-dot"></span> LIVE</div>
    <div class="clock" id="clock"></div>
  </div>
</div>

<div class="controls">
  <div class="control-group">
    <span class="control-label">Dimension</span>
    <div class="pill-group" id="dim-pills">
      <button class="pill-btn active" data-dim="total">Total</button>
      <button class="pill-btn" data-dim="model">By Model</button>
      <button class="pill-btn" data-dim="backend">By Backend</button>
    </div>
  </div>
  <div class="control-group">
    <span class="control-label">Metric</span>
    <div class="pill-group" id="met-pills">
      <button class="pill-btn active" data-met="send_rpm" data-label="Send RPM">Send RPM</button>
      <button class="pill-btn" data-met="comp_rpm" data-label="Comp RPM">Comp RPM</button>
      <button class="pill-btn" data-met="running" data-label="Running">Running</button>
      <button class="pill-btn" data-met="waiting" data-label="Waiting">Waiting</button>
      <button class="pill-btn" data-met="conns" data-label="Connections">Conns</button>
      <button class="pill-btn" data-met="pending" data-label="Pending">Pending</button>
    </div>
  </div>
</div>

<div class="charts-grid">
  <div class="chart-card" style="grid-column: 1 / -1; position: relative;">
    <h3 id="main-chart-title">Send RPM &mdash; Total</h3>
    <div class="chart-container" style="height: 340px;"><canvas id="chart-main"></canvas></div>
    <div class="tooltip-box" id="tooltip-main"></div>
  </div>
  <div class="chart-card" style="position:relative;">
    <h3>RPM Overview</h3>
    <div class="chart-container"><canvas id="chart-rpm"></canvas></div>
    <div class="tooltip-box" id="tooltip-rpm"></div>
  </div>
  <div class="chart-card" style="position:relative;">
    <h3>Running / Waiting</h3>
    <div class="chart-container"><canvas id="chart-queue"></canvas></div>
    <div class="tooltip-box" id="tooltip-queue"></div>
  </div>
  <div class="chart-card" style="position:relative;">
    <h3>Connections</h3>
    <div class="chart-container"><canvas id="chart-conns"></canvas></div>
    <div class="tooltip-box" id="tooltip-conns"></div>
  </div>
  <div class="chart-card" style="position:relative;">
    <h3>Healthy / Unhealthy</h3>
    <div class="chart-container"><canvas id="chart-health"></canvas></div>
    <div class="tooltip-box" id="tooltip-health"></div>
  </div>
</div>

</div>

<script>
var PALETTE = [
  '#38bdf8','#34d399','#fbbf24','#fb7185','#a78bfa','#60a5fa',
  '#fb923c','#c084fc','#22d3ee','#f472b6','#e879f9','#a3e635',
  '#facc15','#818cf8','#2dd4bf','#84cc16','#f87171','#4ade80'
];

function LineChart(canvasId, tooltipId) {
  this.canvas = document.getElementById(canvasId);
  this.ctx = this.canvas.getContext('2d');
  this.tooltipEl = document.getElementById(tooltipId);
  this.seriesData = {};
  this.times = [];
  this.colors = {};
  this.colorIdx = 0;
  this.hoverIdx = -1;
  this._bindHover();
}

LineChart.prototype.getColor = function(name) {
  if (!this.colors[name]) {
    this.colors[name] = PALETTE[this.colorIdx % PALETTE.length];
    this.colorIdx++;
  }
  return this.colors[name];
};

LineChart.prototype.update = function(times, seriesData) {
  this.times = times;
  this.seriesData = seriesData;
  this.draw();
};

LineChart.prototype._bindHover = function() {
  var self = this;
  this.canvas.addEventListener('mousemove', function(e) {
    var rect = self.canvas.parentElement.getBoundingClientRect();
    var mx = e.clientX - rect.left;
    var W = rect.width;
    var PAD_L = 55, PAD_R = 14;
    var plotW = W - PAD_L - PAD_R;
    var n = self.times.length;
    if (n < 2 || mx < PAD_L || mx > PAD_L + plotW) {
      self.hoverIdx = -1;
      if (self.tooltipEl) self.tooltipEl.style.display = 'none';
      self.draw();
      return;
    }
    var ratio = (mx - PAD_L) / plotW;
    self.hoverIdx = Math.round(ratio * (n - 1));
    self.draw();
    if (self.tooltipEl) self._showTooltip(e, rect);
  });
  this.canvas.addEventListener('mouseleave', function() {
    self.hoverIdx = -1;
    if (self.tooltipEl) self.tooltipEl.style.display = 'none';
    self.draw();
  });
};

LineChart.prototype._showTooltip = function(e, rect) {
  var idx = this.hoverIdx;
  var names = Object.keys(this.seriesData);
  if (idx < 0 || names.length === 0) { this.tooltipEl.style.display = 'none'; return; }
  var html = '<div class="tt-time">' + (this.times[idx] || '') + '</div>';
  for (var s = 0; s < names.length; s++) {
    var nm = names[s];
    var val = this.seriesData[nm][idx];
    if (val === undefined) val = 0;
    var c = this.getColor(nm);
    html += '<div class="tt-row"><span class="tt-dot" style="background:' + c + '"></span>' +
      '<span class="tt-name">' + nm + '</span><span class="tt-val">' + val + '</span></div>';
  }
  this.tooltipEl.innerHTML = html;
  this.tooltipEl.style.display = 'block';
  var tx = e.clientX - rect.left + 16;
  var ty = e.clientY - rect.top - 10;
  if (tx + this.tooltipEl.offsetWidth > rect.width - 8) tx = e.clientX - rect.left - this.tooltipEl.offsetWidth - 16;
  if (ty < 0) ty = 4;
  this.tooltipEl.style.left = tx + 'px';
  this.tooltipEl.style.top = ty + 'px';
};

LineChart.prototype.draw = function() {
  var c = this.ctx;
  var rect = this.canvas.parentElement.getBoundingClientRect();
  this.canvas.width = rect.width * devicePixelRatio;
  this.canvas.height = rect.height * devicePixelRatio;
  c.scale(devicePixelRatio, devicePixelRatio);
  var W = rect.width, H = rect.height;
  var PAD_L = 55, PAD_B = 28, PAD_T = 20, PAD_R = 14;
  var plotW = W - PAD_L - PAD_R;
  var plotH = H - PAD_T - PAD_B;
  var n = this.times.length;

  c.clearRect(0, 0, W, H);

  var seriesNames = Object.keys(this.seriesData);
  if (n === 0 || seriesNames.length === 0) {
    c.fillStyle = '#5a6680';
    c.font = '13px Inter, sans-serif';
    c.textAlign = 'center';
    c.fillText('Waiting for data...', W / 2, H / 2);
    c.setTransform(1,0,0,1,0,0);
    return;
  }

  // legend (top)
  c.font = '11px Inter, sans-serif';
  c.textAlign = 'left';
  var lx = PAD_L;
  for (var s = 0; s < seriesNames.length; s++) {
    var lc = this.getColor(seriesNames[s]);
    c.fillStyle = lc;
    c.beginPath();
    c.moveTo(lx + 2, 4); c.lineTo(lx + 10, 4); c.lineTo(lx + 10, 12); c.lineTo(lx + 2, 12); c.closePath();
    c.fill();
    c.fillStyle = '#8b98b0';
    var lbl = seriesNames[s].length > 24 ? seriesNames[s].substring(0, 22) + '..' : seriesNames[s];
    c.fillText(lbl, lx + 14, 12);
    lx += c.measureText(lbl).width + 28;
    if (lx > W - 40) break;
  }

  // compute max
  var maxVal = 1;
  for (var si = 0; si < seriesNames.length; si++) {
    var arr = this.seriesData[seriesNames[si]];
    for (var vi = 0; vi < arr.length; vi++) {
      if (arr[vi] > maxVal) maxVal = arr[vi];
    }
  }
  maxVal = Math.ceil(maxVal * 1.15);
  if (maxVal === 0) maxVal = 1;

  // grid
  c.strokeStyle = 'rgba(99,118,150,0.15)';
  c.lineWidth = 0.5;
  c.fillStyle = '#5a6680';
  c.font = '10px JetBrains Mono, monospace';
  c.textAlign = 'right';
  var gridLines = 5;
  for (var gi = 0; gi <= gridLines; gi++) {
    var gy = PAD_T + plotH - (plotH * gi / gridLines);
    c.beginPath(); c.moveTo(PAD_L, gy); c.lineTo(PAD_L + plotW, gy); c.stroke();
    c.fillText(Math.round(maxVal * gi / gridLines), PAD_L - 6, gy + 3);
  }

  // hover crosshair
  if (this.hoverIdx >= 0 && this.hoverIdx < n) {
    var hx = PAD_L + (this.hoverIdx / Math.max(n - 1, 1)) * plotW;
    c.strokeStyle = 'rgba(255,255,255,0.15)';
    c.lineWidth = 1;
    c.setLineDash([4, 4]);
    c.beginPath(); c.moveTo(hx, PAD_T); c.lineTo(hx, PAD_T + plotH); c.stroke();
    c.setLineDash([]);
  }

  // draw lines with gradient fill
  for (var li = 0; li < seriesNames.length; li++) {
    var name = seriesNames[li];
    var vals = this.seriesData[name];
    var color = this.getColor(name);

    // gradient fill
    c.beginPath();
    for (var pi = 0; pi < n; pi++) {
      var px = PAD_L + (pi / Math.max(n - 1, 1)) * plotW;
      var py = PAD_T + plotH - (vals[pi] / maxVal) * plotH;
      if (pi === 0) c.moveTo(px, py); else c.lineTo(px, py);
    }
    c.lineTo(PAD_L + ((n - 1) / Math.max(n - 1, 1)) * plotW, PAD_T + plotH);
    c.lineTo(PAD_L, PAD_T + plotH);
    c.closePath();
    var grad = c.createLinearGradient(0, PAD_T, 0, PAD_T + plotH);
    grad.addColorStop(0, color.replace(')', ',0.15)').replace('rgb', 'rgba'));
    grad.addColorStop(1, color.replace(')', ',0.01)').replace('rgb', 'rgba'));
    c.fillStyle = grad;
    c.fill();

    // line
    c.strokeStyle = color;
    c.lineWidth = 2;
    c.lineJoin = 'round';
    c.beginPath();
    for (var qi = 0; qi < n; qi++) {
      var qx = PAD_L + (qi / Math.max(n - 1, 1)) * plotW;
      var qy = PAD_T + plotH - (vals[qi] / maxVal) * plotH;
      if (qi === 0) c.moveTo(qx, qy); else c.lineTo(qx, qy);
    }
    c.stroke();

    // hover dot
    if (this.hoverIdx >= 0 && this.hoverIdx < n) {
      var dx = PAD_L + (this.hoverIdx / Math.max(n - 1, 1)) * plotW;
      var dy = PAD_T + plotH - (vals[this.hoverIdx] / maxVal) * plotH;
      c.fillStyle = '#0a0f1e';
      c.beginPath(); c.arc(dx, dy, 5, 0, Math.PI * 2); c.fill();
      c.fillStyle = color;
      c.beginPath(); c.arc(dx, dy, 3.5, 0, Math.PI * 2); c.fill();
    }
  }

  // x-axis labels
  c.fillStyle = '#5a6680';
  c.font = '9px JetBrains Mono, monospace';
  c.textAlign = 'center';
  var labelEvery = Math.max(1, Math.floor(n / 10));
  for (var xi = 0; xi < n; xi += labelEvery) {
    var xx = PAD_L + (xi / Math.max(n - 1, 1)) * plotW;
    c.fillText(this.times[xi] || '', xx, H - 6);
  }

  c.setTransform(1,0,0,1,0,0);
};

var chartMain = new LineChart('chart-main', 'tooltip-main');
var chartRPM = new LineChart('chart-rpm', 'tooltip-rpm');
var chartQueue = new LineChart('chart-queue', 'tooltip-queue');
var chartConns = new LineChart('chart-conns', 'tooltip-conns');
var chartHealth = new LineChart('chart-health', 'tooltip-health');

var latestData = [];
var curDim = 'total';
var curMet = 'send_rpm';
var curMetLabel = 'Send RPM';

function extractApiBase(id) {
  var idx = id.indexOf('@');
  return idx >= 0 ? id.substring(idx + 1) : id;
}

// pill button handlers
document.getElementById('dim-pills').addEventListener('click', function(e) {
  var btn = e.target.closest('.pill-btn');
  if (!btn) return;
  var pills = this.querySelectorAll('.pill-btn');
  for (var i = 0; i < pills.length; i++) pills[i].classList.remove('active');
  btn.classList.add('active');
  curDim = btn.getAttribute('data-dim');
  updateMainChart();
});
document.getElementById('met-pills').addEventListener('click', function(e) {
  var btn = e.target.closest('.pill-btn');
  if (!btn) return;
  var pills = this.querySelectorAll('.pill-btn');
  for (var i = 0; i < pills.length; i++) pills[i].classList.remove('active');
  btn.classList.add('active');
  curMet = btn.getAttribute('data-met');
  curMetLabel = btn.getAttribute('data-label');
  updateMainChart();
});

function getDimLabel() {
  if (curDim === 'total') return 'Total';
  if (curDim === 'model') return 'By Model';
  return 'By Backend';
}

function updateMainChart() {
  if (!latestData || latestData.length === 0) return;

  document.getElementById('main-chart-title').textContent = curMetLabel + ' \u2014 ' + getDimLabel();
  var times = latestData.map(function(p) { return p.time; });
  var series = {};

  if (curDim === 'total') {
    var metricMap = {
      'send_rpm': 'total_rpm',
      'comp_rpm': 'comp_rpm',
      'running': 'running',
      'waiting': 'waiting',
      'conns': 'conns',
      'pending': 'pending'
    };
    var key = metricMap[curMet];
    series['Total'] = latestData.map(function(p) { return p[key] || 0; });
  } else if (curDim === 'model') {
    var allModels = {};
    for (var mi = 0; mi < latestData.length; mi++) {
      var pt = latestData[mi];
      if (pt.models) {
        var mk = Object.keys(pt.models);
        for (var mj = 0; mj < mk.length; mj++) allModels[mk[mj]] = true;
      }
    }
    var modelNames = Object.keys(allModels);
    for (var mn = 0; mn < modelNames.length; mn++) {
      var model = modelNames[mn];
      series[model] = latestData.map(function(pp) {
        if (!pp.models || !pp.models[model]) return 0;
        return pp.models[model][curMet] || 0;
      });
    }
  } else {
    var allBackends = {};
    for (var bi = 0; bi < latestData.length; bi++) {
      var bpt = latestData[bi];
      if (bpt.backends) {
        var bk = Object.keys(bpt.backends);
        for (var bj = 0; bj < bk.length; bj++) allBackends[bk[bj]] = true;
      }
    }
    var backendIds = Object.keys(allBackends);
    for (var bn = 0; bn < backendIds.length; bn++) {
      var bid = backendIds[bn];
      var label = extractApiBase(bid);
      series[label] = latestData.map(function(pp) {
        if (!pp.backends || !pp.backends[bid]) return 0;
        return pp.backends[bid][curMet] || 0;
      });
    }
  }

  chartMain.update(times, series);
}

function updateOverviewCharts() {
  if (!latestData || latestData.length === 0) return;
  var times = latestData.map(function(p) { return p.time; });

  chartRPM.update(times, {
    'Send RPM': latestData.map(function(p) { return p.total_rpm || 0; }),
    'Comp RPM': latestData.map(function(p) { return p.comp_rpm || 0; })
  });
  chartQueue.update(times, {
    'Running': latestData.map(function(p) { return p.running || 0; }),
    'Waiting': latestData.map(function(p) { return p.waiting || 0; })
  });
  chartConns.update(times, {
    'Connections': latestData.map(function(p) { return p.conns || 0; }),
    'Pending': latestData.map(function(p) { return p.pending || 0; })
  });
  chartHealth.update(times, {
    'Healthy': latestData.map(function(p) { return p.healthy || 0; }),
    'Unhealthy': latestData.map(function(p) { return p.unhealthy || 0; })
  });
}

function updateClock() {
  var now = new Date();
  var h = now.getHours().toString(); if (h.length < 2) h = '0' + h;
  var m = now.getMinutes().toString(); if (m.length < 2) m = '0' + m;
  var s = now.getSeconds().toString(); if (s.length < 2) s = '0' + s;
  document.getElementById('clock').textContent = h + ':' + m + ':' + s;
}

async function refresh() {
  try {
    var resp = await fetch('/api/history');
    latestData = await resp.json();
    if (!latestData) return;
    updateMainChart();
    updateOverviewCharts();
    updateClock();
  } catch(e) { console.error(e); }
}

refresh();
setInterval(refresh, 1000);
window.addEventListener('resize', function() {
  chartMain.draw();
  chartRPM.draw();
  chartQueue.draw();
  chartConns.draw();
  chartHealth.draw();
});
</script>
</body>
</html>`
