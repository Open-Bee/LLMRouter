# LLM Router — Quick Start Guide

[中文版](quick_start_zh.md)

This guide covers the full workflow of installation, configuration, deployment, and benchmarking for LLM Router.

---

## Table of Contents

1. [Installation](#1-installation)
2. [Build & Run](#2-build--run)
3. [Configuration Reference](#3-configuration-reference)
4. [Backend Configuration](#4-backend-configuration)
5. [Routing Strategies](#5-routing-strategies)
6. [One-Click vLLM/SGLang Deployment](#6-one-click-vllmsglang-deployment)
7. [Benchmarking](#7-benchmarking)
8. [Web Monitoring Dashboard](#8-web-monitoring-dashboard)
9. [DataStudio Integration](#9-datastudio-integration)
10. [Webhook Notifications](#10-webhook-notifications)
11. [Logging Configuration](#11-logging-configuration)
12. [FAQ](#12-faq)

---

## 1. Installation

### Install Go

Go 1.23+ is required. If Go is not installed, use the built-in script:

```bash
bash scripts/install_go.sh
```

This installs Go 1.23.4 to `/usr/local/go` and configures PATH.

### Python Dependencies (Deployment & Benchmark Scripts)

```bash
pip install pyyaml aiohttp rich openai
```

---

## 2. Build & Run

### Option 1: Manual Build

```bash
go build -o llm-router ./cmd/router/
./llm-router -config config.yaml
```

### Option 2: One-Click Launch

```bash
bash run.sh
```

`run.sh` automatically detects source changes, performs incremental builds, releases occupied ports, and starts the service.

After startup, two services run simultaneously:

| Service | Default Port | Description |
|---------|-------------|-------------|
| API Service | 8000 | OpenAI-compatible API |
| Web Dashboard | 80 | Real-time monitoring dashboard |

### CLI Arguments

```bash
./llm-router -config config.yaml -log-level debug -enabled-sources "self_deployed,openai"
```

| Argument | Default | Description |
|----------|---------|-------------|
| `-config` | `config.yaml` | Path to main config file |
| `-log-level` | from config | Override log level: `debug` / `info` / `warn` / `error` |
| `-enabled-sources` | from config | Comma-separated enabled source types |

---

## 3. Configuration Reference

Full reference for the main config file `config.yaml`:

```yaml
server:
  host: "0.0.0.0"
  port: 8000                 # API listening port

routing:
  strategy: "simple-shuffle" # Routing strategy (see Section 5)
  num_retries: 3             # Max retries

http_client:
  request_timeout: 3600      # Backend request timeout (seconds), suitable for long LLM generation
  connect_timeout: 10        # Connection timeout (seconds)
  max_idle_conns: 2000       # Max idle connections
  max_conns_per_host: 0      # 0 = unlimited
  idle_conn_timeout: 90      # Idle connection timeout (seconds)

health_check:
  interval: 30               # Health check interval (seconds)
  timeout: 10                # Single check timeout (seconds)
  failure_threshold: 3       # Mark unhealthy after N consecutive failures
  max_concurrency: 20        # Max concurrent checks
  prune_unhealthy_on_start: false  # Remove unhealthy backends from config files on startup

retry:
  base_delay_ms: 500         # Base backoff delay
  max_delay_ms: 5000         # Max backoff delay
  jitter_factor: 0.2         # Jitter factor
  failure_threshold: 3       # Mark unhealthy after N consecutive forwarding failures

webhook:
  enabled: false
  url: ""                    # WeCom (Enterprise WeChat) webhook URL
  cooldown_seconds: 300      # Alert cooldown per backend
  batch_interval: 60         # Alert batch aggregation window
  report_interval_active: 3600   # Periodic report interval when active
  report_interval_idle: 10800    # Periodic report interval when idle

monitor:
  collect_interval: 1        # Prometheus metrics collection interval (seconds)

backends:
  config_dir: "configs"      # Backend config file directory
  enabled_sources:           # Enabled source types
    - "openai"
    - "self_deployed"

dashboard:
  enabled: true
  refresh_interval: 2        # Dashboard refresh interval (seconds)
  web_port: 80               # Web dashboard port

log:
  level: "info"              # Log level
  dir: "logs"                # Log file directory; empty = no file logging
  max_size_mb: 100           # Max size per log file
  max_backups: 10            # Number of old log files to keep
  max_age_days: 7            # Max days to retain old log files
```

---

## 4. Backend Configuration

Backend configs are stored in the `configs/` directory. The filename prefix determines the source type:

| Filename Pattern | Source Type |
|-----------------|-------------|
| `openai_*.yaml` | `openai` |
| `self_deployed_*.yaml` | `self_deployed` |

### Backend Config Format

```yaml
# configs/self_deployed_config.yaml
model_list:
  - model_name: "Qwen3-VL-30B-A3B-Instruct"
    litellm_params:
      model: "openai/Qwen3-VL-30B-A3B-Instruct"
      api_base: "http://10.0.0.1:8000/v1"
      api_key: "dummy"
      supports_vision: true
      weight: 1.0
      source_type: "self_deployed"
      rpm_limit: null       # null = unlimited, integer = max requests per minute
```

### Hot Reload

After modifying YAML files under `configs/`, the Router automatically detects changes and incrementally updates the backend list — no restart required.

Hot reload process:
1. Detects file modification time changes
2. Parses new config, compares with current backends
3. Reuses existing backends (preserves runtime state), health-checks new backends
4. Atomically replaces backends by source type, without affecting other sources
5. Unhealthy new backends are retried in the background (every 5s, up to ~2 minutes)

---

## 5. Routing Strategies

Configure via `routing.strategy`. Three strategies are supported:

| Strategy | Config Value | Algorithm | Use Case |
|----------|-------------|-----------|----------|
| **Weighted Random** | `simple-shuffle` | Random selection weighted by backend weight | General use (default) |
| **Least Connections** | `least-connections` | P2C (pick 2 random, choose one with fewer connections) | Backends with varying performance |
| **Least Waiting** | `least-waiting` | P2C + Prometheus metrics (waiting + active combined load) | Precise scheduling needed |

> The `least-waiting` strategy requires backends to expose a Prometheus `/metrics` endpoint (vLLM/SGLang support this by default).

### Error Classification & Retry

The Router classifies forwarding errors and handles them accordingly:

| Error Type | Description | Retry? |
|-----------|-------------|--------|
| Timeout | Backend response timeout | Yes |
| Connection refused | Connection refused / DNS failure | Yes |
| Connection reset | TCP connection reset | Yes |
| 5xx | Backend server error | Yes (exponential backoff) |
| 4xx | Client error | No (pass through directly) |
| Client disconnect | Requester disconnected | No (abort immediately) |

---

## 6. One-Click vLLM/SGLang Deployment

`scripts/self_deploy.py` supports one-click deployment of vLLM or SGLang inference services across multiple nodes, with automatic Router backend config generation.

### Basic Usage

```bash
# Specify nodes via environment variable
export NODE_IP_LIST="192.168.1.1,192.168.1.2,192.168.1.3"

python scripts/self_deploy.py \
    --python-cmd vllm_0.14.0 \
    --model-name Qwen/Qwen3-VL-235B-A22B-Instruct \
    --port 22005 \
    --tensor-parallel-size 8 \
    --backend vllm \
    --served-model-name Qwen3-VL-235B-A22B-Instruct
```

### Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `--hosts` | `NODE_IP_LIST` env var | Comma-separated node IP list |
| `--model-name` | (required) | Model name or path |
| `--port` | `30596` | Starting port number |
| `--tensor-parallel-size` | (required) | Tensor parallelism per service |
| `--python-cmd` | `python` | Python interpreter (mapped to conda env path) |
| `--gpus-per-node` | `8` | Number of GPUs per node |
| `--backend` | `vllm` | Inference backend: `vllm` or `sglang` |
| `--served-model-name` | last segment of model name | Externally exposed model name |
| `--log-dir` | `./log_dir` | Log directory |
| `--health-check-timeout` | `3600` | Health check timeout (seconds) |

### Prerequisites

- **pdsh**: The script uses `pdsh` (Parallel Distributed Shell) to execute commands on remote nodes. Install it and configure passwordless SSH:
  ```bash
  # Ubuntu/Debian
  apt-get install pdsh
  # CentOS/RHEL
  yum install pdsh
  ```
  Ensure passwordless SSH login is configured from the deployment machine to all target nodes.

- **conda environment**: The `--python-cmd` parameter is mapped to `/root/anaconda3/envs/{python_cmd}/bin/python`, so target nodes must have the corresponding conda environment pre-created (e.g., `vllm_0.14.0`, `sglang_0.5.7`) with vLLM or SGLang installed.

### Multi-Node Specification

Two ways to specify deployment target nodes:

**Method 1: `NODE_IP_LIST` environment variable**

Suitable for cluster schedulers (e.g., Slurm) that auto-inject node lists:

```bash
# Manual setup
export NODE_IP_LIST="192.168.1.1,192.168.1.2,192.168.1.3"

# Or from Slurm
export NODE_IP_LIST=$(scontrol show hostnames $SLURM_JOB_NODELIST | paste -sd,)

python scripts/self_deploy.py \
    --python-cmd vllm_0.14.0 \
    --model-name Qwen/Qwen3-VL-235B-A22B-Instruct \
    --port 22005 \
    --tensor-parallel-size 8 \
    --backend vllm
```

**Method 2: `--hosts` parameter**

Specify directly on the command line (takes priority over env var):

```bash
python scripts/self_deploy.py \
    --hosts "192.168.1.1,192.168.1.2,192.168.1.3" \
    --python-cmd vllm_0.14.0 \
    --model-name Qwen/Qwen3-VL-235B-A22B-Instruct \
    --port 22005 \
    --tensor-parallel-size 8 \
    --backend vllm
```

> The `:<port>` suffix in IP lists is automatically stripped (e.g., `192.168.1.1:22` → `192.168.1.1`).

### GPU & Port Auto-Assignment

The script automatically calculates how many service instances to start per node and which GPUs each instance uses, based on `--gpus-per-node` and `--tensor-parallel-size`:

```
Services per node = gpus_per_node / tensor_parallel_size
```

**Example**: 3 nodes, 8 GPUs per node, `tensor-parallel-size=4`:

```
Node 192.168.1.1:
  Service 1: GPU 0,1,2,3 → Port 22005
  Service 2: GPU 4,5,6,7 → Port 22006

Node 192.168.1.2:
  Service 3: GPU 0,1,2,3 → Port 22007
  Service 4: GPU 4,5,6,7 → Port 22008

Node 192.168.1.3:
  Service 5: GPU 0,1,2,3 → Port 22009
  Service 6: GPU 4,5,6,7 → Port 22010
```

Port assignment rules:
- Starts from the port specified by `--port`
- Uses remote `lsof` to detect port occupancy, automatically skips occupied ports
- Port numbers increment across all nodes (not reset per node)

### Deployment Process

Deployment has two phases:

**Phase 1**: Async launch of all services
- Allocates available ports for each node (auto-skips occupied ports)
- Launches vLLM/SGLang on remote nodes via `pdsh`, setting `CUDA_VISIBLE_DEVICES` to specify GPUs
- Staggers services on the same node by 3 seconds to avoid NCCL contention

**Phase 2**: Log polling and startup verification
- Detects port conflicts → automatically picks a new port and restarts (up to 5 retries)
- Detects service readiness → calls `/v1/models` to verify model availability
- Periodically reports waiting status; services that don't become ready are reported

### After Deployment

The script automatically:
1. Generates `router_config_vllm.yaml` (standalone router config file)
2. Incrementally updates `configs/self_deployed_config.yaml` (deduplicates by `api_base`, does not overwrite existing configs)

The Router's hot reload automatically detects changes to `configs/self_deployed_config.yaml` and loads new backends.

### More Examples

```bash
# Deploy with specified hosts
python scripts/self_deploy.py \
    --hosts "192.168.1.100" \
    --python-cmd vllm_0.14.0 \
    --model-name Qwen/Qwen2.5-VL-32B-Instruct \
    --port 30010 \
    --tensor-parallel-size 4 \
    --backend vllm

# Deploy SGLang backend
python scripts/self_deploy.py \
    --python-cmd sglang_0.5.7 \
    --model-name Qwen/Qwen3-VL-235B-A22B-Instruct \
    --port 22005 \
    --tensor-parallel-size 8 \
    --backend sglang

# Use local model path + custom served name
python scripts/self_deploy.py \
    --python-cmd vllm_0.14.0 \
    --model-name /path/to/model \
    --port 22005 \
    --tensor-parallel-size 4 \
    --backend vllm \
    --served-model-name Qwen3-235B-A22B-Instruct-2507-FP8
```

---

## 7. Benchmarking

`scripts/benchmark.py` is a high-performance benchmarking tool with a multi-process + async coroutine hybrid architecture, capable of simulating 10K+ concurrent requests.

### Basic Usage

```bash
# Basic benchmark (1000 concurrency, runs until Ctrl+C)
python scripts/benchmark.py --url http://localhost:8000 -c 1000

# Multi-process benchmark (4 processes, 10000 total concurrency)
python scripts/benchmark.py --url http://localhost:8000 -c 10000 -w 4

# Specify total number of requests
python scripts/benchmark.py --url http://localhost:8000 -c 5000 -n 100000 -w 4

# Specify duration (60 seconds)
python scripts/benchmark.py --url http://localhost:8000 -c 10000 -w 8 --duration 60
```

### Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `--url`, `-u` | `http://localhost:8000` | Router service address |
| `--concurrency`, `-c` | `4096` | Concurrency level |
| `--workers`, `-w` | `1` | Number of worker processes (concurrency evenly distributed) |
| `--requests`, `-n` | unlimited | Total number of requests |
| `--duration`, `-d` | unlimited | Run duration (seconds) |
| `--timeout`, `-t` | `600` | Per-request timeout (seconds) |
| `--model`, `-m` | `Qwen3-235B-A22B-Instruct-2507-FP8` | Model name for requests |
| `--no-visual` | off | Disable visualization, use plain text output |

### Output Metrics

The benchmark displays the following metrics in real-time (with visual panel when `rich` is installed):

- **QPS**: Current/average requests per second
- **Latency distribution**: Mean / P50 / P95 / P99 latency
- **Success rate**: Request success percentage
- **Trend charts**: Real-time QPS, latency, and success rate trends

---

## 8. Web Monitoring Dashboard

The Router includes two built-in web pages, served by default on port 80:

### Main Dashboard (`http://<host>:80/`)

Displays in real-time:
- **System overview**: Uptime, total requests, success/failure counts, success rate, Goroutines, memory
- **Source summary**: Statistics grouped by `self_deployed` / `openai`
- **Model summary**: Backend counts and request statistics grouped by model name
- **Backend list**: Complete backend table (searchable), showing status, engine type, connections, RPM, GPU cache, etc.

### Charts Page (`http://<host>:80/charts`)

Provides multi-dimensional real-time trend charts:
- Supports switching between **Overview / By Model / By Backend** dimensions
- Supports switching between **RPM / Running / Waiting / Connections / Pending** metrics
- Overview thumbnails: RPM overview, Running/Waiting, connections, healthy/unhealthy ratio

### API Endpoints

The web dashboard also provides JSON APIs:

| Path | Description |
|------|-------------|
| `/api/dashboard` | Full system/request/backend status JSON |
| `/api/history` | Recent 120 time-series data points |

---

## 9. DataStudio Integration

LLM Router serves as the MLLM request routing layer for DataStudio. Typical usage:

### Step 1: Start the Router

```bash
cd tools/LLMRouter
bash run.sh
```

### Step 2: Configure Backends

Edit `configs/self_deployed_config.yaml` to add backends, or use `self_deploy.py` for automatic deployment and config generation.

### Step 3: Point DataStudio to the Router

```python
# In DataStudio config
model = dict(
    model="Qwen3-VL-30B-A3B-Instruct",
    api_base="http://<router-host>",
    port=8000,
    thread_num=1024,
    return_dict=True,
    max_tokens=4096,
)
```

All MLLM requests from DataStudio (MLLMFilter, MLLMRewriter) are automatically routed through the Router to healthy backend instances, enabling load balancing and automatic failover.

### Multi-Machine Distributed Scenarios

In multi-machine distributed scenarios (DataStudio's `multi_machine_deployment`), the Router is especially useful:
- Multiple DataStudio Workers can share the same Router address
- The Router handles cross-node backend load balancing
- Backend failures are handled transparently without disrupting data processing

---

## 10. Webhook Notifications

Enable WeCom (Enterprise WeChat) webhook notifications:

```yaml
webhook:
  enabled: true
  url: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY"
  cooldown_seconds: 300      # Alert cooldown per backend
  batch_interval: 60         # Alert aggregation window
```

Supported notification types:

| Type | Trigger | Description |
|------|---------|-------------|
| Startup report | System startup | Total backends, healthy count, model list |
| Failure alert | Backend becomes unhealthy | Sent in batches |
| Recovery notice | Backend recovers | Sent immediately |
| Periodic report | Timed trigger | Uptime, backend status, request statistics |

---

## 11. Logging Configuration

```yaml
log:
  level: "info"          # debug | info | warn | error
  dir: "logs"            # Log file directory; empty = no file logging
  max_size_mb: 100       # Max 100MB per file before rotation
  max_backups: 10        # Keep 10 old files
  max_age_days: 7        # Retain for 7 days
```

Logs are output to both console and file. Enable trace logging:

```bash
export LLM_ROUTER_TRACE=1
./llm-router -config config.yaml
```

Trace logging records the complete routing/forwarding/retry process for each request, written to a separate file.

---

## 12. FAQ

### Q: What if the port is occupied?

`run.sh` automatically releases occupied ports. Manual release:

```bash
lsof -ti:8000 | xargs kill -9  # Release API port
lsof -ti:80 | xargs kill -9    # Release dashboard port
```

### Q: How to add new backends?

Edit `configs/self_deployed_config.yaml` and add entries to `model_list`. The Router will auto hot-reload without restart.

### Q: How does health checking work?

The Router periodically sends GET requests to each backend's `/v1/models` endpoint. After `failure_threshold` consecutive failures, the backend is marked unhealthy and automatically re-enabled upon recovery.

### Q: How to view backend status?

- Open the Web dashboard: `http://<host>:80/`
- Call the API: `curl http://<host>:80/api/dashboard`

### Q: How to use the least-waiting strategy?

1. Ensure backends are vLLM or SGLang (they expose `/metrics` by default)
2. Set `routing.strategy: "least-waiting"`
3. The Router will automatically collect running/waiting metrics from `/metrics` for scheduling
