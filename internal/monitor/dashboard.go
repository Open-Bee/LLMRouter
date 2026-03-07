package monitor

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"llm-router/internal/model"
	"llm-router/internal/pool"
	"llm-router/internal/ratelimit"
	"llm-router/internal/util"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/rs/zerolog"
)

// maxLogLines is the max number of log lines to keep in the dashboard.
const maxLogLines = 500

// Dashboard provides a tview-based terminal monitoring panel.
type Dashboard struct {
	pool          *pool.BackendPool
	rateLimiter   *ratelimit.RateLimiter
	completionRPM *CompletionRPM
	stats         *util.RequestStats
	interval      time.Duration
	logger        zerolog.Logger
	startTime     time.Time

	app            *tview.Application
	summaryView    *tview.TextView
	backendsTable1 *tview.Table
	backendsTable2 *tview.Table
	logsView       *tview.TextView

	logRing *LogRing
}

// NewDashboard creates a new terminal Dashboard.
func NewDashboard(
	p *pool.BackendPool,
	rateLimiter *ratelimit.RateLimiter,
	completionRPM *CompletionRPM,
	stats *util.RequestStats,
	interval time.Duration,
	logger zerolog.Logger,
	logRing *LogRing,
) *Dashboard {
	return &Dashboard{
		pool:          p,
		rateLimiter:   rateLimiter,
		completionRPM: completionRPM,
		stats:         stats,
		interval:      interval,
		logger:        logger,
		startTime:     time.Now(),
		logRing:       logRing,
	}
}


// Run starts the tview application. Blocks until ctx is cancelled or user quits.
func (d *Dashboard) Run(ctx context.Context) {
	d.logger.Info().Msg("starting terminal dashboard")

	d.app = tview.NewApplication()

	// --- Create panels ---
	d.summaryView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(false)
	d.summaryView.SetBorder(true).SetTitle(" Summary ").
		SetBorderColor(tcell.ColorDarkCyan).
		SetTitleColor(tcell.ColorAqua)

	d.backendsTable1 = tview.NewTable().
		SetFixed(1, 0).
		SetSelectable(false, false)
	d.backendsTable1.SetBorder(true).SetTitle(" Backends [Part 1] ").
		SetBorderColor(tcell.ColorDarkCyan).
		SetTitleColor(tcell.ColorAqua)

	d.backendsTable2 = tview.NewTable().
		SetFixed(1, 0).
		SetSelectable(false, false)
	d.backendsTable2.SetBorder(true).SetTitle(" Backends [Part 2] ").
		SetBorderColor(tcell.ColorDarkCyan).
		SetTitleColor(tcell.ColorAqua)

	d.logsView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetMaxLines(maxLogLines)
	d.logsView.SetBorder(true).SetTitle(" Logs ").
		SetBorderColor(tcell.ColorDarkCyan).
		SetTitleColor(tcell.ColorAqua)

	// --- Layout ---
	// Top row: Summary (left) | Backends Part1 (mid) | Backends Part2 (right)
	topRow := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(d.summaryView, 0, 2, false).
		AddItem(d.backendsTable1, 0, 3, false).
		AddItem(d.backendsTable2, 0, 3, false)

	// Main: top panels + bottom logs
	mainLayout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(topRow, 0, 3, false).
		AddItem(d.logsView, 0, 1, false)

	d.app.SetRoot(mainLayout, true)

	// Keyboard: q or Ctrl-C to quit
	d.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Rune() == 'q' || event.Key() == tcell.KeyCtrlC {
			d.app.Stop()
			return nil
		}
		return event
	})

	// Background refresh goroutine
	go func() {
		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				d.app.Stop()
				return
			case <-ticker.C:
				d.app.QueueUpdateDraw(func() {
					d.refresh()
				})
			}
		}
	}()

	// Initial draw
	d.app.QueueUpdateDraw(func() {
		d.refresh()
	})

	if err := d.app.Run(); err != nil {
		d.logger.Error().Err(err).Msg("dashboard error")
	}
	d.logger.Info().Msg("dashboard stopped")
}

func (d *Dashboard) refresh() {
	d.refreshSummary()
	d.refreshBackends()
	d.refreshLogs()
}

// --- Summary Panel ---

func (d *Dashboard) refreshSummary() {
	var buf strings.Builder

	uptime := time.Since(d.startTime).Round(time.Second)
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	total := d.stats.Total()
	success := d.stats.Success()
	failures := d.stats.Failures()
	rate := float64(0)
	if total > 0 {
		rate = float64(success) / float64(total) * 100
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(&buf, " [yellow]%s[white] | Up: [bold]%s[white] | RPM: [bold]%d[white] | Tot: [bold]%d[white]\n\n",
		now, uptime, d.totalSendRPM(), total)

	// --- Service summary table ---
	fmt.Fprintf(&buf, " [aqua]%-12s %4s %4s %5s %5s %5s %6s %7s[white]\n",
		"Service", "Num", "OK", "Run", "Wait", "Conn", "RPM", "Total")

	type svcRow struct {
		name                                                 string
		num, ok, run, wait, conn, rpm                        int
		total                                                int64
	}
	svcMap := map[string]*svcRow{}

	backends := d.pool.AllBackends()
	completionRPMs := d.completionRPM.AllRPMs()

	for _, b := range backends {
		st := b.SourceType
		if st == "" {
			st = "other"
		}
		row, exists := svcMap[st]
		if !exists {
			row = &svcRow{name: st}
			svcMap[st] = row
		}
		row.num++
		if b.IsHealthy() {
			row.ok++
		}
		row.conn += int(b.ActiveConnections())
		row.rpm += d.rateLimiter.CurrentRPM(b.ID) + completionRPMs[b.ID]
		if m := b.GetLoadMetrics(); m != nil && m.Error == "" {
			row.run += m.RunningRequests
			row.wait += m.WaitingRequests
		}
		if bs := d.stats.GetBackendStats(b.ID); bs != nil {
			row.total += bs.Total.Load()
		}
	}

	svcNames := make([]string, 0, len(svcMap))
	for k := range svcMap {
		svcNames = append(svcNames, k)
	}
	sort.Strings(svcNames)

	var tNum, tOK, tRun, tWait, tConn, tRPM int
	var tTotal int64
	for _, name := range svcNames {
		r := svcMap[name]
		fmt.Fprintf(&buf, " %-12s %4d %4d %5d %5d %5d %6d %7d\n",
			r.name, r.num, r.ok, r.run, r.wait, r.conn, r.rpm, r.total)
		tNum += r.num
		tOK += r.ok
		tRun += r.run
		tWait += r.wait
		tConn += r.conn
		tRPM += r.rpm
		tTotal += r.total
	}
	fmt.Fprintf(&buf, " [bold]%-12s %4d %4d %5d %5d %5d %6d %7d[white]\n",
		"TOTAL", tNum, tOK, tRun, tWait, tConn, tRPM, tTotal)

	// --- Model summary table ---
	buf.WriteString("\n")
	fmt.Fprintf(&buf, " [aqua]%-20s %4s %4s %5s %5s %6s %7s[white]\n",
		"Model", "Num", "OK", "Run", "Wait", "RPM", "Total")

	models := d.pool.ModelNames()
	sort.Strings(models)

	for _, modelName := range models {
		all := d.pool.GetBackendsForModel(modelName)
		healthy := 0
		mRun, mWait, mRPM := 0, 0, 0
		var mTotal int64
		for _, b := range all {
			if b.IsHealthy() {
				healthy++
			}
			if m := b.GetLoadMetrics(); m != nil && m.Error == "" {
				mRun += m.RunningRequests
				mWait += m.WaitingRequests
			}
			mRPM += completionRPMs[b.ID]
			if bs := d.stats.GetBackendStats(b.ID); bs != nil {
				mTotal += bs.Total.Load()
			}
		}
		displayName := modelName
		if len(displayName) > 20 {
			displayName = displayName[:17] + "..."
		}
		fmt.Fprintf(&buf, " %-20s %4d %4d %5d %5d %6d %7d\n",
			displayName, len(all), healthy, mRun, mWait, mRPM, mTotal)
	}

	buf.WriteString("\n")
	fmt.Fprintf(&buf, " Success: [green]%d[white] | Fail: [red]%d[white] | Rate: [bold]%.1f%%[white]\n",
		success, failures, rate)
	fmt.Fprintf(&buf, " Goroutines: %d | Mem: %.1f MB\n",
		runtime.NumGoroutine(), float64(memStats.Alloc)/1024/1024)

	d.summaryView.SetText(buf.String())
}

func (d *Dashboard) totalSendRPM() int {
	total := 0
	for _, b := range d.pool.AllBackends() {
		total += d.rateLimiter.CurrentRPM(b.ID)
	}
	return total
}

// --- Backends Panels ---

func (d *Dashboard) refreshBackends() {
	backends := d.pool.AllBackends()
	sort.Slice(backends, func(i, j int) bool {
		if backends[i].ModelName != backends[j].ModelName {
			return backends[i].ModelName < backends[j].ModelName
		}
		return backends[i].ID < backends[j].ID
	})

	completionRPMs := d.completionRPM.AllRPMs()

	mid := len(backends) / 2
	if len(backends)%2 != 0 {
		mid++
	}

	part1 := backends[:mid]
	part2 := backends[mid:]

	d.fillBackendTable(d.backendsTable1, part1, completionRPMs, 0)
	d.fillBackendTable(d.backendsTable2, part2, completionRPMs, mid)
}

func (d *Dashboard) fillBackendTable(table *tview.Table, backends []*model.Backend, compRPMs map[string]int, startIdx int) {
	table.Clear()

	headers := []string{"#", "Type", "OK", "Run", "Wait", "Conn", "Pend", "RPM", "Total", "Model"}
	for i, h := range headers {
		cell := tview.NewTableCell(h).
			SetTextColor(tcell.ColorAqua).
			SetSelectable(false).
			SetAttributes(tcell.AttrBold)
		table.SetCell(0, i, cell)
	}

	for row, b := range backends {
		r := row + 1
		idx := startIdx + row + 1

		okStr := "[red]N[-]"
		if b.IsHealthy() {
			okStr = "[green]Y[-]"
		}

		srcType := b.SourceType
		if len(srcType) > 8 {
			srcType = srcType[:8]
		}

		run, wait := "-", "-"
		if m := b.GetLoadMetrics(); m != nil && m.Error == "" {
			run = fmt.Sprintf("%d", m.RunningRequests)
			wait = fmt.Sprintf("%d", m.WaitingRequests)
		}

		conns := fmt.Sprintf("%d", b.ActiveConnections())
		pending := fmt.Sprintf("%d", b.PendingRequests())
		rpm := fmt.Sprintf("%d", d.rateLimiter.CurrentRPM(b.ID)+compRPMs[b.ID])

		var totalReqs string
		if bs := d.stats.GetBackendStats(b.ID); bs != nil {
			totalReqs = fmt.Sprintf("%d", bs.Total.Load())
		} else {
			totalReqs = "0"
		}

		modelName := b.ModelName
		if len(modelName) > 18 {
			modelName = modelName[:15] + "..."
		}

		vals := []string{
			fmt.Sprintf("%d", idx),
			srcType,
			okStr,
			run,
			wait,
			conns,
			pending,
			rpm,
			totalReqs,
			modelName,
		}

		for col, v := range vals {
			cell := tview.NewTableCell(v).SetExpansion(1)
			table.SetCell(r, col, cell)
		}
	}
}

// --- Logs Panel ---

func (d *Dashboard) refreshLogs() {
	if d.logRing == nil {
		return
	}
	lines := d.logRing.Lines()
	d.logsView.SetText(strings.Join(lines, "\n"))
	d.logsView.ScrollToEnd()
}

// GetStats returns the request stats for external access.
func (d *Dashboard) GetStats() *util.RequestStats {
	return d.stats
}
