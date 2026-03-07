#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
LLM Router High-Performance Benchmark Script (Multi-Process Version)

A sustained benchmarking tool supporting tens of thousands of concurrent requests
to test router server performance.

Features:
- Multi-process + async coroutine hybrid architecture, fully utilizing multi-core CPUs
- Each subprocess runs an independent asyncio event loop to avoid single-process bottleneck
- Main process handles statistics aggregation and visualization
- Real-time QPS, latency distribution, success rate statistics
- Supports sustained benchmarking mode
- Configurable request parameters, concurrency, and process count
- Graceful signal handling and resource cleanup

Usage Examples:
    # Basic benchmark (1000 concurrency, continuous run, single process by default)
    python benchmark.py --url http://localhost:8000 --concurrency 1000

    # Multi-process benchmark (4 processes, 10000 total concurrency)
    python benchmark.py --url http://localhost:8000 -c 10000 -w 4

    # Specify request count
    python benchmark.py --url http://localhost:8000 -c 5000 -n 100000 -w 4

    # High concurrency benchmark (10000 concurrency, 8 processes)
    python benchmark.py --url http://localhost:8000 -c 10000 -w 8 --duration 300
"""

import argparse
import asyncio
import json
import multiprocessing
import random
import signal
import sys
import time
from collections import deque
from dataclasses import dataclass, field
from multiprocessing import Process, Value, Array
from multiprocessing.connection import Connection
from typing import Any, Dict, List, Optional

import aiohttp

try:
    from rich.console import Console
    from rich.live import Live
    from rich.table import Table
    from rich.panel import Panel
    from rich.layout import Layout
    from rich.text import Text
    from rich import box

    RICH_AVAILABLE = True
except ImportError:
    RICH_AVAILABLE = False
    Console = None
    Live = None


# ============================================================================
# Configuration Constants
# ============================================================================

DEFAULT_CONCURRENCY = 4096
DEFAULT_WORKERS = 1
DEFAULT_TIMEOUT = 600
STATS_INTERVAL = 1  # Stats output interval (seconds)
LATENCY_WINDOW_SIZE = 10000  # Latency sampling window size
QPS_WINDOW_SIZE = 5  # QPS sliding window size (seconds)
HISTORY_SIZE = 60  # History data points (for charts)

# Test message template
TEST_MESSAGES = [
    [{"role": "user", "content": "Tell me a long story"}],
]


# ============================================================================
# Shared Statistics (for inter-process communication)
# ============================================================================


class SharedStats:
    """Shared memory based statistics for subprocess-to-main process reporting."""

    def __init__(self):
        self.total_requests = Value("q", 0)
        self.successful_requests = Value("q", 0)
        self.failed_requests = Value("q", 0)
        self.total_latency_ms = Value("d", 0.0)
        self.min_latency_ms = Value("d", float("inf"))
        self.max_latency_ms = Value("d", 0.0)

        self._latency_buf_size = LATENCY_WINDOW_SIZE
        self._latency_buf = Array("d", self._latency_buf_size)
        self._latency_write_idx = Value("q", 0)
        self._latency_count = Value("q", 0)

    def record_success(self, latency_ms: float) -> None:
        with self.total_requests.get_lock():
            self.total_requests.value += 1
        with self.successful_requests.get_lock():
            self.successful_requests.value += 1
        with self.total_latency_ms.get_lock():
            self.total_latency_ms.value += latency_ms
        with self.min_latency_ms.get_lock():
            if latency_ms < self.min_latency_ms.value:
                self.min_latency_ms.value = latency_ms
        with self.max_latency_ms.get_lock():
            if latency_ms > self.max_latency_ms.value:
                self.max_latency_ms.value = latency_ms
        with self._latency_write_idx.get_lock():
            idx = self._latency_write_idx.value % self._latency_buf_size
            self._latency_buf[idx] = latency_ms
            self._latency_write_idx.value += 1
        with self._latency_count.get_lock():
            if self._latency_count.value < self._latency_buf_size:
                self._latency_count.value += 1

    def record_failure(self) -> None:
        with self.total_requests.get_lock():
            self.total_requests.value += 1
        with self.failed_requests.get_lock():
            self.failed_requests.value += 1

    def snapshot(self) -> dict:
        count = min(self._latency_count.value, self._latency_buf_size)
        latencies = [self._latency_buf[i] for i in range(count)] if count > 0 else []
        return {
            "total_requests": self.total_requests.value,
            "successful_requests": self.successful_requests.value,
            "failed_requests": self.failed_requests.value,
            "total_latency_ms": self.total_latency_ms.value,
            "min_latency_ms": self.min_latency_ms.value,
            "max_latency_ms": self.max_latency_ms.value,
            "latencies": latencies,
        }


# ============================================================================
# Statistics Data Structure (main process, for visualization)
# ============================================================================


@dataclass
class BenchmarkStats:
    """Benchmark statistics (main process side, for display)"""

    total_requests: int = 0
    successful_requests: int = 0
    failed_requests: int = 0
    total_latency_ms: float = 0.0
    min_latency_ms: float = float("inf")
    max_latency_ms: float = 0.0
    latencies: list = field(default_factory=list)
    error_counts: Dict[str, int] = field(default_factory=dict)
    start_time: float = 0.0
    last_report_time: float = 0.0
    last_report_requests: int = 0

    # For delta-based QPS calculation
    _prev_total: int = 0
    _qps_samples: deque = field(default_factory=lambda: deque(maxlen=QPS_WINDOW_SIZE))

    # History data (for visualization)
    qps_history: deque = field(default_factory=lambda: deque(maxlen=HISTORY_SIZE))
    latency_history: deque = field(default_factory=lambda: deque(maxlen=HISTORY_SIZE))
    success_rate_history: deque = field(
        default_factory=lambda: deque(maxlen=HISTORY_SIZE)
    )

    def update_from_shared(self, snapshot: dict) -> None:
        """Update statistics from shared memory snapshot."""
        self.total_requests = snapshot["total_requests"]
        self.successful_requests = snapshot["successful_requests"]
        self.failed_requests = snapshot["failed_requests"]
        self.total_latency_ms = snapshot["total_latency_ms"]
        self.min_latency_ms = snapshot["min_latency_ms"]
        self.max_latency_ms = snapshot["max_latency_ms"]
        self.latencies = snapshot["latencies"]

    def update_from_error_pipes(self, error_pipes: list) -> None:
        """Read error info from error pipes"""
        for pipe in error_pipes:
            while pipe.poll():
                try:
                    error_key = pipe.recv()
                    self.error_counts[error_key] = self.error_counts.get(error_key, 0) + 1
                except (EOFError, OSError):
                    break

    def get_percentile(self, p: float) -> float:
        """Calculate percentile latency."""
        if not self.latencies:
            return 0.0
        sorted_latencies = sorted(self.latencies)
        idx = int(len(sorted_latencies) * p / 100)
        idx = min(idx, len(sorted_latencies) - 1)
        return sorted_latencies[idx]

    def get_current_qps(self) -> float:
        """Calculate current QPS (based on sampling delta)"""
        now_total = self.total_requests
        delta = now_total - self._prev_total
        self._prev_total = now_total
        self._qps_samples.append(delta)
        if not self._qps_samples:
            return 0.0
        return sum(self._qps_samples) / len(self._qps_samples)

    def get_overall_qps(self) -> float:
        """Calculate overall QPS."""
        elapsed = time.time() - self.start_time
        if elapsed <= 0:
            return 0.0
        return self.total_requests / elapsed

    def update_report_checkpoint(self) -> None:
        """Update report checkpoint"""
        self.last_report_time = time.time()
        self.last_report_requests = self.total_requests

    def record_history(
        self, qps: float, avg_latency: float, success_rate: float
    ) -> None:
        """Record history data."""
        self.qps_history.append(qps)
        self.latency_history.append(avg_latency)
        self.success_rate_history.append(success_rate)


# ============================================================================
# Subprocess Worker
# ============================================================================


class _WorkerProcess:
    """Benchmark worker running in a subprocess."""

    def __init__(
        self,
        worker_id: int,
        endpoint: str,
        concurrency: int,
        total_requests: Optional[int],
        duration: Optional[int],
        timeout: int,
        model: str,
        shared_stats: SharedStats,
        error_pipe: Connection,
        stop_event: multiprocessing.Event,
    ):
        self.worker_id = worker_id
        self.endpoint = endpoint
        self.concurrency = concurrency
        self.total_requests = total_requests
        self.duration = duration
        self.timeout = timeout
        self.model = model
        self.shared_stats = shared_stats
        self.error_pipe = error_pipe
        self.stop_event = stop_event
        self.running = False
        self._request_counter: int = 0
        self._ramp_up_time: float = max(1.0, self.concurrency / 1000.0)
        self._request_bodies = self._build_request_bodies()

    def _build_request_bodies(self) -> List[bytes]:
        bodies = []
        for messages in TEST_MESSAGES:
            body = {
                "model": self.model,
                "messages": messages,
                "temperature": 2.0,
                "max_tokens": 8192,
            }
            bodies.append(json.dumps(body).encode("utf-8"))
        return bodies

    async def _make_request(self) -> bool:
        if not self.running or self.stop_event.is_set():
            return False
        start_time = time.perf_counter()
        try:
            body = random.choice(self._request_bodies)
            async with self.session.post(
                self.endpoint,
                data=body,
                headers={"Content-Type": "application/json"},
                timeout=aiohttp.ClientTimeout(total=self.timeout),
            ) as response:
                await response.read()
                latency_ms = (time.perf_counter() - start_time) * 1000
                if response.status == 200:
                    self.shared_stats.record_success(latency_ms)
                else:
                    self.shared_stats.record_failure()
                    self._send_error(f"HTTP {response.status}")
        except asyncio.TimeoutError:
            self.shared_stats.record_failure()
            self._send_error("Timeout")
        except aiohttp.ClientError as e:
            self.shared_stats.record_failure()
            self._send_error(f"ClientError: {type(e).__name__}: {e}")
        except Exception as e:
            self.shared_stats.record_failure()
            self._send_error(f"Error: {type(e).__name__}")
        return True

    def _send_error(self, error: str) -> None:
        try:
            self.error_pipe.send(error[:120])
        except (BrokenPipeError, OSError):
            pass

    async def _coroutine_worker(self, coro_id: int) -> None:
        startup_delay = (coro_id / self.concurrency) * self._ramp_up_time
        await asyncio.sleep(startup_delay)
        while self.running and not self.stop_event.is_set():
            if self.total_requests is not None:
                num = self._request_counter
                self._request_counter += 1
                if num >= self.total_requests:
                    break
            if not await self._make_request():
                break

    async def _check_stop_event(self) -> None:
        while self.running:
            await asyncio.sleep(0.5)
            if self.stop_event.is_set():
                self.running = False
                break

    async def run_async(self) -> None:
        self.running = True
        self._request_counter = 0
        connector = aiohttp.TCPConnector(
            limit=0, limit_per_host=0, ttl_dns_cache=300,
            enable_cleanup_closed=True, force_close=False, keepalive_timeout=30,
        )
        async with aiohttp.ClientSession(connector=connector) as session:
            self.session = session
            workers = [asyncio.create_task(self._coroutine_worker(i)) for i in range(self.concurrency)]
            stop_checker = asyncio.create_task(self._check_stop_event())
            duration_task = None
            if self.duration:
                async def _stop_after():
                    await asyncio.sleep(self.duration)
                    self.running = False
                duration_task = asyncio.create_task(_stop_after())
            try:
                await asyncio.gather(*workers, return_exceptions=True)
            except asyncio.CancelledError:
                pass
            finally:
                self.running = False
                stop_checker.cancel()
                if duration_task and not duration_task.done():
                    duration_task.cancel()
                await asyncio.gather(stop_checker, return_exceptions=True)
                if duration_task:
                    await asyncio.gather(duration_task, return_exceptions=True)
        try:
            self.error_pipe.close()
        except Exception:
            pass


def _worker_process_entry(
    worker_id, endpoint, concurrency, total_requests, duration,
    timeout, model, shared_stats, error_pipe, stop_event,
) -> None:
    """Subprocess entry function."""
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    worker = _WorkerProcess(
        worker_id=worker_id, endpoint=endpoint, concurrency=concurrency,
        total_requests=total_requests, duration=duration, timeout=timeout,
        model=model, shared_stats=shared_stats, error_pipe=error_pipe,
        stop_event=stop_event,
    )
    asyncio.run(worker.run_async())


# ============================================================================
# Main Process Benchmark Controller
# ============================================================================


class Benchmark:
    """Multi-process benchmark controller."""

    def __init__(
        self,
        base_url: str,
        concurrency: int = DEFAULT_CONCURRENCY,
        num_workers: int = DEFAULT_WORKERS,
        total_requests: Optional[int] = None,
        duration: Optional[int] = None,
        timeout: int = DEFAULT_TIMEOUT,
        model: str = "default",
        no_visual: bool = False,
    ):
        self.base_url = base_url.rstrip("/")
        self.endpoint = f"{self.base_url}/v1/chat/completions"
        self.concurrency = concurrency
        self.num_workers = num_workers
        self.total_requests = total_requests
        self.duration = duration
        self.timeout = timeout
        self.model = model

        self.shared_stats = SharedStats()
        self.stats = BenchmarkStats()
        self.running = False

        self.processes: List[Process] = []
        self.error_pipes: List[Connection] = []
        self.stop_event = multiprocessing.Event()

        self._per_worker_concurrency = self._distribute(concurrency, num_workers)
        self._per_worker_requests = (
            self._distribute(total_requests, num_workers) if total_requests else [None] * num_workers
        )

        self.use_rich = RICH_AVAILABLE and not no_visual
        self.console = Console() if self.use_rich else None

        if self.use_rich:
            terminal_height = self.console.height or 40
            self._fixed_height = max(terminal_height - 2, 20)
            self._layout = Layout(size=self._fixed_height)
            self._layout.split_column(
                Layout(name="header", size=3),
                Layout(name="body"),
                Layout(name="footer", size=3),
            )
            self._layout["body"].split_row(
                Layout(name="left"),
                Layout(name="right"),
            )

    @staticmethod
    def _distribute(total: int, n: int) -> List[int]:
        """Distribute total as evenly as possible into n parts."""
        base = total // n
        remainder = total % n
        return [base + (1 if i < remainder else 0) for i in range(n)]

    def _start_workers(self) -> None:
        for i in range(self.num_workers):
            parent_conn, child_conn = multiprocessing.Pipe(duplex=False)
            self.error_pipes.append(parent_conn)
            p = Process(
                target=_worker_process_entry,
                args=(
                    i, self.endpoint, self._per_worker_concurrency[i],
                    self._per_worker_requests[i], self.duration, self.timeout,
                    self.model, self.shared_stats, child_conn, self.stop_event,
                ),
                daemon=True,
            )
            p.start()
            child_conn.close()
            self.processes.append(p)

    def _stop_workers(self) -> None:
        self.stop_event.set()
        for p in self.processes:
            p.join(timeout=10)
            if p.is_alive():
                p.terminate()
                p.join(timeout=5)
        for pipe in self.error_pipes:
            try:
                pipe.close()
            except Exception:
                pass

    def _collect_stats(self) -> None:
        snapshot = self.shared_stats.snapshot()
        self.stats.update_from_shared(snapshot)
        self.stats.update_from_error_pipes(self.error_pipes)

    def _all_workers_done(self) -> bool:
        return all(not p.is_alive() for p in self.processes)

    @staticmethod
    def _interruptible_sleep(seconds: float, check_fn, interval: float = 0.1) -> None:
        """Interruptible sleep that checks check_fn every interval, exits early if True."""
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            if check_fn():
                return
            time.sleep(min(interval, deadline - time.monotonic()))

    def _should_stop(self) -> bool:
        return not self.running or self._all_workers_done()

    def _stats_reporter(self) -> None:
        """Stats reporter - periodically output statistics (main process sync call)."""
        self.stats.start_time = time.time()
        self.stats.last_report_time = self.stats.start_time

        while not self._should_stop():
            self._interruptible_sleep(STATS_INTERVAL, self._should_stop)
            if self._should_stop():
                break
            self._collect_stats()
            self._print_stats()
            self.stats.update_report_checkpoint()

    def _stats_reporter_rich(self) -> None:
        """Stats reporter - Rich visualization version (main process sync call)."""
        self.stats.start_time = time.time()
        self.stats.last_report_time = self.stats.start_time

        self._collect_stats()
        self._update_rich_layout()

        with Live(
            self._layout,
            console=self.console,
            refresh_per_second=1,
            screen=True,
            auto_refresh=True,
            vertical_overflow="crop",
        ) as live:
            self.console.show_cursor(False)
            while not self._should_stop():
                self._interruptible_sleep(STATS_INTERVAL, self._should_stop)
                if self._should_stop():
                    break
                self._collect_stats()
                self._update_rich_layout()
                self.stats.update_report_checkpoint()

    def _create_sparkline(self, data: deque, height: int = 8, width: int = 60) -> str:
        """Create a simple ASCII sparkline chart."""
        if not data or len(data) < 2:
            return " " * width

        # Normalize data to 0-height range
        data_list = list(data)
        min_val = min(data_list)
        max_val = max(data_list)

        if max_val - min_val < 0.001:
            return "─" * width

        # Draw using Unicode block characters
        blocks = [" ", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"]

        # Sample data points to fit width
        step = max(1, len(data_list) // width)
        sampled = [data_list[i] for i in range(0, len(data_list), step)][:width]

        result = ""
        for val in sampled:
            normalized = (val - min_val) / (max_val - min_val)
            block_idx = int(normalized * (len(blocks) - 1))
            result += blocks[block_idx]

        return result

    def _update_rich_layout(self) -> None:
        """Update persistent Layout child content (avoid rebuilding to prevent flicker)."""
        s = self.stats
        current_qps = s.get_current_qps()
        overall_qps = s.get_overall_qps()
        success_rate = (
            (s.successful_requests / s.total_requests * 100)
            if s.total_requests > 0
            else 0
        )
        avg_latency = (
            (s.total_latency_ms / s.successful_requests)
            if s.successful_requests > 0
            else 0
        )

        p50 = s.get_percentile(50)
        p95 = s.get_percentile(95)
        p99 = s.get_percentile(99)

        elapsed = time.time() - s.start_time

        s.record_history(current_qps, avg_latency, success_rate)

        # Header info
        header_text = Text()
        header_text.append(f"🚀 LLM Router Benchmark ", style="bold cyan")
        header_text.append(f"| Runtime: {elapsed:.1f}s ", style="yellow")
        header_text.append(f"| Concurrency: {self.concurrency} ", style="green")
        header_text.append(f"| Workers: {self.num_workers}", style="blue")
        self._layout["header"].update(Panel(header_text, border_style="cyan"))

        # Left side - statistics table
        stats_table = Table(show_header=False, box=None, padding=(0, 2))
        stats_table.add_column("Metric", style="cyan", width=20)
        stats_table.add_column("Value", style="green")

        stats_table.add_row("📊 Total Requests", f"{s.total_requests:,}")
        stats_table.add_row("✅ Successful", f"{s.successful_requests:,}")
        stats_table.add_row("❌ Failed", f"{s.failed_requests:,}")
        stats_table.add_row("📈 Success Rate", f"{success_rate:.2f}%")
        stats_table.add_row("", "")
        stats_table.add_row("⚡ Current QPS", f"{current_qps:.1f}")
        stats_table.add_row("📊 Average QPS", f"{overall_qps:.1f}")
        stats_table.add_row("", "")
        stats_table.add_row("⏱️  Avg Latency", f"{avg_latency:.1f} ms")
        stats_table.add_row("📉 P50 Latency", f"{p50:.1f} ms")
        stats_table.add_row("📉 P95 Latency", f"{p95:.1f} ms")
        stats_table.add_row("📉 P99 Latency", f"{p99:.1f} ms")

        self._layout["left"].update(
            Panel(stats_table, title="📊 Real-time Stats", border_style="green")
        )

        # Right side - charts
        charts_text = Text()

        # QPS chart
        charts_text.append("QPS Trend:\n", style="bold yellow")
        if len(s.qps_history) > 1:
            qps_chart = self._create_sparkline(s.qps_history, height=8, width=50)
            charts_text.append(qps_chart + "\n", style="yellow")
            charts_text.append(
                f"  Min: {min(s.qps_history):.1f}  Max: {max(s.qps_history):.1f}\n\n",
                style="dim",
            )
        else:
            charts_text.append("  Waiting for data...\n\n", style="dim")

        # Latency chart
        charts_text.append("Latency Trend (ms):\n", style="bold magenta")
        if len(s.latency_history) > 1:
            latency_chart = self._create_sparkline(
                s.latency_history, height=8, width=50
            )
            charts_text.append(latency_chart + "\n", style="magenta")
            charts_text.append(
                f"  Min: {min(s.latency_history):.1f}  Max: {max(s.latency_history):.1f}\n\n",
                style="dim",
            )
        else:
            charts_text.append("  Waiting for data...\n\n", style="dim")

        # Success rate chart
        charts_text.append("Success Rate Trend (%):\n", style="bold green")
        if len(s.success_rate_history) > 1:
            success_chart = self._create_sparkline(
                s.success_rate_history, height=8, width=50
            )
            charts_text.append(success_chart + "\n", style="green")
            charts_text.append(
                f"  Min: {min(s.success_rate_history):.1f}  Max: {max(s.success_rate_history):.1f}",
                style="dim",
            )
        else:
            charts_text.append("  Waiting for data...", style="dim")

        self._layout["right"].update(
            Panel(charts_text, title="📈 Trend Charts", border_style="magenta")
        )

        # Footer - error info
        footer_text = Text()
        alive_count = sum(1 for p in self.processes if p.is_alive())
        footer_text.append(f"Workers: {alive_count}/{self.num_workers} ", style="blue")
        if s.error_counts:
            footer_text.append("| ⚠️  Errors: ", style="bold red")
            error_items = sorted(s.error_counts.items(), key=lambda x: -x[1])[:3]
            footer_text.append(
                " | ".join([f"{err}: {cnt}" for err, cnt in error_items]), style="red"
            )
        else:
            footer_text.append("| ✨ Running normally", style="bold green")

        self._layout["footer"].update(Panel(footer_text, border_style="blue"))

    def _print_stats(self) -> None:
        """Print statistics."""
        s = self.stats
        current_qps = s.get_current_qps()
        overall_qps = s.get_overall_qps()
        success_rate = (
            (s.successful_requests / s.total_requests * 100)
            if s.total_requests > 0
            else 0
        )
        avg_latency = (
            (s.total_latency_ms / s.successful_requests)
            if s.successful_requests > 0
            else 0
        )

        p50 = s.get_percentile(50)
        p95 = s.get_percentile(95)
        p99 = s.get_percentile(99)

        elapsed = time.time() - s.start_time

        # Record history data (for non-rich mode)
        s.record_history(current_qps, avg_latency, success_rate)

        print(
            f"[{elapsed:6.1f}s] "
            f"Reqs: {s.total_requests:8d} | "
            f"QPS: {current_qps:7.1f} (avg: {overall_qps:7.1f}) | "
            f"Success: {success_rate:5.1f}% | "
            f"Latency(ms) avg:{avg_latency:7.1f} p50:{p50:7.1f} p95:{p95:7.1f} p99:{p99:7.1f} | "
            f"Failed: {s.failed_requests}"
        )

    def _print_final_report(self) -> None:
        """Print final report."""
        s = self.stats
        elapsed = time.time() - s.start_time
        overall_qps = s.get_overall_qps()
        success_rate = (
            (s.successful_requests / s.total_requests * 100)
            if s.total_requests > 0
            else 0
        )
        avg_latency = (
            (s.total_latency_ms / s.successful_requests)
            if s.successful_requests > 0
            else 0
        )

        print("\n" + "=" * 80)
        print("Benchmark Report")
        print("=" * 80)
        print(f"Target:          {self.endpoint}")
        print(f"Concurrency:     {self.concurrency}")
        print(f"Workers:         {self.num_workers}")
        print(f"Duration:        {elapsed:.2f}s")
        print("-" * 80)
        print(f"Total Requests:  {s.total_requests}")
        print(f"Successful:      {s.successful_requests}")
        print(f"Failed:          {s.failed_requests}")
        print(f"Success Rate:    {success_rate:.2f}%")
        print("-" * 80)
        print(f"Avg QPS:         {overall_qps:.2f}")
        print(f"Avg Latency:     {avg_latency:.2f} ms")
        if s.min_latency_ms != float("inf"):
            print(f"Min Latency:     {s.min_latency_ms:.2f} ms")
            print(f"Max Latency:     {s.max_latency_ms:.2f} ms")
        print(f"P50 Latency:     {s.get_percentile(50):.2f} ms")
        print(f"P95 Latency:     {s.get_percentile(95):.2f} ms")
        print(f"P99 Latency:     {s.get_percentile(99):.2f} ms")

        if s.error_counts:
            print("-" * 80)
            print("Error Statistics:")
            for error, count in sorted(s.error_counts.items(), key=lambda x: -x[1])[
                :10
            ]:
                print(f"  {error}: {count}")
        print("=" * 80)

    def run(self) -> None:
        """Run benchmark (multi-process)."""
        if self.use_rich:
            print(f"🚀 Starting benchmark (visual mode)...")
        else:
            print(f"Starting benchmark...")
        print(f"  Target: {self.endpoint}")
        print(f"  Concurrency: {self.concurrency}")
        print(f"  Workers: {self.num_workers}")
        if self.total_requests:
            print(f"  Total requests: {self.total_requests}")
        if self.duration:
            print(f"  Duration: {self.duration}s")
        concurrency_info = ", ".join(
            f"P{i}={c}" for i, c in enumerate(self._per_worker_concurrency)
        )
        print(f"  Concurrency distribution: [{concurrency_info}]")
        if not self.use_rich:
            print("  Tip: Install rich for visual mode (pip install rich)")
        print("-" * 80)

        self.running = True
        self._start_workers()

        try:
            if self.use_rich:
                self._stats_reporter_rich()
            else:
                self._stats_reporter()
        except KeyboardInterrupt:
            pass
        finally:
            self.running = False
            self._stop_workers()
            # Final stats collection
            self._collect_stats()

        self._print_final_report()

    def stop(self) -> None:
        """Stop benchmark."""
        print("\nStopping benchmark...")
        self.running = False
        self.stop_event.set()


# ============================================================================
# Main Function
# ============================================================================


def parse_args() -> argparse.Namespace:
    """Parse command line arguments."""
    parser = argparse.ArgumentParser(
        description="LLM Router High-Performance Benchmark Tool (Multi-Process)",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Basic benchmark (1000 concurrency, continuous run, Ctrl+C to stop)
  python benchmark.py --url http://localhost:8000 -c 1000

  # Multi-process benchmark (4 processes, 10000 total concurrency)
  python benchmark.py --url http://localhost:8000 -c 10000 -w 4

  # Send specified number of requests
  python benchmark.py --url http://localhost:8000 -c 5000 -n 100000 -w 4

  # Run for specified duration (seconds)
  python benchmark.py --url http://localhost:8000 -c 10000 -w 8 --duration 60
        """,
    )

    parser.add_argument(
        "--url",
        "-u",
        default="http://localhost:8000",
        help="Router server address (e.g., http://localhost:8000)",
    )
    parser.add_argument(
        "--concurrency",
        "-c",
        type=int,
        default=DEFAULT_CONCURRENCY,
        help=f"Concurrency level (default: {DEFAULT_CONCURRENCY})",
    )
    parser.add_argument(
        "--workers",
        "-w",
        type=int,
        default=DEFAULT_WORKERS,
        help=f"Number of worker processes, concurrency distributed evenly (default: {DEFAULT_WORKERS})",
    )
    parser.add_argument(
        "--requests",
        "-n",
        type=int,
        default=None,
        help="Total number of requests (runs continuously if not set)",
    )
    parser.add_argument(
        "--duration",
        "-d",
        type=int,
        default=None,
        help="Duration in seconds (runs continuously if not set)",
    )
    parser.add_argument(
        "--timeout",
        "-t",
        type=int,
        default=DEFAULT_TIMEOUT,
        help=f"Request timeout in seconds (default: {DEFAULT_TIMEOUT})",
    )
    parser.add_argument(
        "--model",
        "-m",
        default="Qwen3-235B-A22B-Instruct-2507-FP8",
        help="Model name (default: Qwen3-235B-A22B-Instruct-2507-FP8)",
    )
    parser.add_argument(
        "--no-visual",
        action="store_true",
        help="Disable visual mode, use plain text output",
    )

    return parser.parse_args()


def main() -> None:
    """Main entry point."""
    args = parse_args()

    if sys.version_info < (3, 8):
        print("Error: Python 3.8+ required")
        sys.exit(1)

    benchmark = Benchmark(
        base_url=args.url,
        concurrency=args.concurrency,
        num_workers=args.workers,
        total_requests=args.requests,
        duration=args.duration,
        timeout=args.timeout,
        model=args.model,
        no_visual=args.no_visual,
    )

    # Signal handling: first Ctrl+C graceful stop, second force exit
    _interrupt_count = 0

    def _signal_handler(signum, frame):
        nonlocal _interrupt_count
        _interrupt_count += 1
        if _interrupt_count >= 2:
            print("\nForce exit...")
            # Ensure child process cleanup
            benchmark.stop_event.set()
            for p in benchmark.processes:
                if p.is_alive():
                    p.terminate()
            import os
            os._exit(1)
        benchmark.stop()

    signal.signal(signal.SIGINT, _signal_handler)
    signal.signal(signal.SIGTERM, _signal_handler)

    benchmark.run()


if __name__ == "__main__":
    main()
