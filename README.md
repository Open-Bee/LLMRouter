<p align="center">
  <img src="https://img.shields.io/badge/🚀-LLM%20Router-00D4AA?style=for-the-badge&labelColor=000000" alt="LLM Router Logo" height="60"/>
</p>

<p align="center">
  <strong>High-performance OpenAI API-compatible reverse proxy router for LLM backends</strong>
</p>

<p align="center">
  <em>Part of the <a href="https://github.com/Open-Bee/DataStudio">DataStudio</a> ecosystem — intelligent request routing for LLM inference services</em>
</p>

---

<p align="center">
  <a href="https://github.com/Open-Bee/LLMRouter"><img src="https://img.shields.io/github/stars/Open-Bee/LLMRouter?style=social" alt="GitHub Stars"/></a>
  <a href="https://github.com/Open-Bee/LLMRouter/blob/main/LICENSE"><img src="https://img.shields.io/badge/License-Apache%202.0-blue.svg" alt="License"/></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go&logoColor=white" alt="Go"/></a>
  <a href="https://www.python.org/"><img src="https://img.shields.io/badge/Python-3.8%2B-3776AB?logo=python&logoColor=white" alt="Python"/></a>
</p>

<p align="center">
  <a href="docs/readme_zh.md">简体中文</a> | <a href="docs/quick_start.md">Quick Start</a>
</p>

---

## Introduction

**LLM Router** is an OpenAI API-compatible reverse proxy router built with Go, providing LLM backend management for [DataStudio](https://github.com/Open-Bee/DataStudio). It supports intelligent load balancing, sliding-window RPM rate limiting, async health checking, automatic failover, and hot-reloading of configurations.

> LLM Router is a companion tool to DataStudio (located in `tools/LLMRouter/`). By pointing `model.api_base` in DataStudio's config to the Router address, all requests are automatically routed to multiple healthy backend instances.

## Features

| Feature | Description |
|---------|-------------|
| **Zero-copy forwarding** | Only extracts the `model` field with gjson — no full JSON parsing |
| **COW backend pool** | Lock-free reads (`atomic.Value`), Copy-on-Write for writes |
| **Three routing strategies** | Weighted random / Least connections (P2C) / Least waiting (P2C + Prometheus) |
| **RPM rate limiting** | Sliding window counter, atomic pre-deduction, per-backend granularity |
| **Async health checks** | Concurrent probing of `/v1/models`, marks unhealthy after N consecutive failures |
| **Hot config reload** | Watches YAML file changes, incremental backend updates without restart |
| **Smart retry** | Error classification (timeout / 5xx / connection refused), exponential backoff + jitter |
| **Web monitoring dashboard** | Real-time backend status, RPM, load metrics, trend charts |
| **Prometheus metrics collection** | Auto-collects running/waiting/GPU cache metrics from vLLM/SGLang |
| **Webhook notifications** | Failure/recovery/periodic reports pushed to WeCom (Enterprise WeChat) |
| **Multi-node deployment tool** | One-click vLLM/SGLang deployment to multiple nodes with auto-generated router configs |
| **Benchmarking tool** | Multi-process + async coroutines, supports 10K+ concurrency with live visualization |

## Project Structure

```
go_router/
├── cmd/router/             # Entry point
├── internal/
│   ├── config/             # Config loading & defaults
│   ├── handler/            # HTTP request handlers
│   ├── health/             # Health checks & config hot-reload
│   ├── model/              # Data models (Backend, Metrics, etc.)
│   ├── monitor/            # Monitoring (Prometheus collector, Web dashboard, TUI dashboard)
│   ├── notify/             # Webhook notifications (WeCom)
│   ├── pool/               # COW backend pool management
│   ├── ratelimit/          # Sliding window RPM rate limiter
│   ├── router/             # Core router (forwarding, retry, error classification)
│   ├── routing/            # Routing strategies (shuffle / least-connections / least-waiting)
│   └── util/               # Utilities (URL, stats, Prometheus parsing)
├── configs/                # Backend config files
│   ├── openai_config.yaml
│   └── self_deployed_config.yaml
├── scripts/
│   ├── self_deploy.py      # Multi-node vLLM/SGLang deployment tool
│   ├── benchmark.py        # High-performance benchmarking tool
│   └── install_go.sh       # Go environment installation script
├── config.yaml             # Main configuration file
├── run.sh                  # One-click launch script
└── docs/
    ├── quick_start.md      # Quick start guide (English)
    ├── quick_start_zh.md   # 快速上手指南 (中文)
    └── readme_zh.md        # 中文 README
```

## Quick Start

### Prerequisites

- Go 1.23+ (use `scripts/install_go.sh` to install)
- Python 3.8+ (for deployment and benchmarking scripts)

### Build & Run

```bash
# Build
go build -o llm-router ./cmd/router/

# Run
./llm-router -config config.yaml
```

Or use the one-click launch script:

```bash
bash run.sh
```

After startup:
- **API service**: `http://0.0.0.0:8000`
- **Web dashboard**: `http://0.0.0.0:80`

### Integration with DataStudio

Point `model.api_base` in your DataStudio config to the Router:

```python
model = dict(
    model="Qwen3-VL-30B-A3B-Instruct",
    api_base="http://<router-host>",
    port=8000,
    thread_num=1024,
    return_dict=True,
)
```

The Router automatically distributes requests to all healthy backend instances.

> For detailed usage, see the **[Quick Start Guide](docs/quick_start.md)** ([中文](docs/quick_start_zh.md)).

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/chat/completions` | Route and forward chat completion requests |
| GET | `/v1/models` | Return registered model list (OpenAI-compatible format) |
| GET | `/health` | Router health check |

## Configuration

### Main Config (`config.yaml`)

```yaml
server:
  host: "0.0.0.0"
  port: 8000

routing:
  strategy: "simple-shuffle"   # simple-shuffle | least-connections | least-waiting
  num_retries: 3

health_check:
  interval: 30
  timeout: 10
  failure_threshold: 3

dashboard:
  enabled: true
  web_port: 80

backends:
  config_dir: "configs"
  enabled_sources:
    - "openai"
    - "self_deployed"
```

### Backend Config (`configs/` directory)

Each `source_type` corresponds to a YAML file (filename prefix determines the type):

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
      rpm_limit: null   # null = unlimited
```

## CLI Arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `-config` | `config.yaml` | Path to main config file |
| `-log-level` | from config | Override log level: debug / info / warn / error |
| `-enabled-sources` | from config | Comma-separated enabled source types |

## Related Projects

| Project | Description | Link |
|---------|-------------|------|
| **DataStudio** | Config-driven multimodal data processing pipeline | [GitHub](https://github.com/Open-Bee/DataStudio) |
| **DataVis** | Web-based multimodal data visualization & analysis | [GitHub](https://github.com/uyzhang/DataVis) |
| **Honey-Data-15M** | 15M high-quality QA pairs produced by DataStudio | [HuggingFace](https://huggingface.co/datasets/Open-Bee/Honey-Data-15M) |
| **Bee** | Fully open-source MLLM project | [Project Page](https://open-bee.github.io/) |

## License

[Apache License 2.0](LICENSE)
