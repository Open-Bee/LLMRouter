#!/usr/bin/env bash
set -euo pipefail

# ── Configuration ─────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Ports
ROUTER_PORT="${ROUTER_PORT:-8000}"
DASHBOARD_PORT="${DASHBOARD_PORT:-80}"

# Paths
CONFIG_FILE="${CONFIG_FILE:-config.yaml}"
CONFIGS_DIR="${CONFIGS_DIR:-configs}"

# ── Release occupied ports ────────────────────────────────────────────────────
release_ports() {
    echo "[run.sh] Releasing occupied ports..."

    local ports=("$ROUTER_PORT" "$DASHBOARD_PORT")

    for port in "${ports[@]}"; do
        [ -z "$port" ] && continue
        pids_on_port=$(lsof -ti :"$port" 2>/dev/null || true)
        if [ -n "$pids_on_port" ]; then
            echo "[run.sh]   Killing processes on port $port: $pids_on_port"
            echo "$pids_on_port" | xargs kill -9 2>/dev/null || true
        fi
    done
    sleep 1
}

# ── Cleanup on exit ──────────────────────────────────────────────────────────
PIDS=()
cleanup() {
    echo "[run.sh] Shutting down all services..."
    for pid in "${PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
        fi
    done
    wait 2>/dev/null || true
    echo "[run.sh] All services stopped."
}
trap cleanup EXIT INT TERM

# ── Build and start Go router ─────────────────────────────────────────────────
start_go_router() {
    echo "[run.sh] Starting Go LLM Router (API port $ROUTER_PORT, Dashboard port $DASHBOARD_PORT)..."

    # Build if binary doesn't exist or source is newer
    if [ ! -f llm-router ] || [ "$(find cmd internal -name '*.go' -newer llm-router 2>/dev/null | head -1)" ]; then
        echo "[run.sh]   Building llm-router..."
        go build -buildvcs=false -o llm-router ./cmd/router
    fi

    ./llm-router \
        --config "$CONFIG_FILE" \
        "$@" &
    PIDS+=($!)
}

# ── Main ─────────────────────────────────────────────────────────────────────
echo "============================================"
echo "  LLM Router — Launcher"
echo "============================================"
echo ""

release_ports
start_go_router "$@"

echo ""
echo "[run.sh] Services started:"
echo "  - LLM Router API:      http://0.0.0.0:${ROUTER_PORT}"
echo "  - Web Dashboard:       http://0.0.0.0:${DASHBOARD_PORT}"
echo ""
echo "[run.sh] Press Ctrl+C to stop all services."

# Wait for all background processes
wait
