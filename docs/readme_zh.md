<p align="center">
  <img src="https://img.shields.io/badge/🚀-LLM%20Router-00D4AA?style=for-the-badge&labelColor=000000" alt="LLM Router Logo" height="60"/>
</p>

<h1 align="center">LLM Router</h1>

<p align="center">
  <strong>OpenAI API 兼容的高性能反向代理路由器</strong>
</p>

<p align="center">
  <em><a href="https://github.com/Open-Bee/DataStudio">DataStudio</a> 生态系统的一部分 — 为 LLM 推理服务提供智能请求路由</em>
</p>

---

<p align="center">
  <a href="https://github.com/Open-Bee/DataStudio"><img src="https://img.shields.io/github/stars/Open-Bee/DataStudio?style=social" alt="GitHub Stars"/></a>
  <a href="https://github.com/Open-Bee/DataStudio/blob/main/LICENSE"><img src="https://img.shields.io/badge/License-Apache%202.0-blue.svg" alt="License"/></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go&logoColor=white" alt="Go"/></a>
  <a href="https://www.python.org/"><img src="https://img.shields.io/badge/Python-3.8%2B-3776AB?logo=python&logoColor=white" alt="Python"/></a>
</p>

<p align="center">
  <a href="../README.md">English</a> | <a href="quick_start_zh.md">快速上手</a> | <a href="quick_start.md">Quick Start</a>
</p>

---

## 简介

**LLM Router** 是一个基于 Go 开发的 OpenAI API 兼容反向代理路由器，为 [DataStudio](https://github.com/Open-Bee/DataStudio) 提供 LLM 后端管理能力。支持智能负载均衡、滑动窗口 RPM 限流、异步健康检查、自动故障转移和配置热加载。

> LLM Router 是 DataStudio 的辅助工具，位于 `tools/LLMRouter/` 目录下。DataStudio 通过配置 `model.api_base` 指向 Router 地址，即可将请求自动路由到多个后端实例。

```
                     ┌─────────────────────────────────────────────┐
                     │             DataStudio 生态系统              │
                     │                                             │
  DataStudio ──▶ LLM Router ──▶ vLLM / SGLang / OpenAI 后端
  (过滤与重写)     (负载均衡、故障转移、限流)
                     │                                             │
                     └─────────────────────────────────────────────┘
```

## 核心特性

| 特性 | 说明 |
|------|------|
| **零拷贝转发** | 请求体仅用 gjson 提取 model 字段，不做完整 JSON 解析 |
| **COW 后端池** | 读路径无锁（`atomic.Value`），写路径 Copy-on-Write |
| **三种路由策略** | 加权随机 / 最少连接数 (P2C) / 最少等待 (P2C + Prometheus) |
| **RPM 限流** | 滑动窗口计数，发送前原子扣减，per-backend 粒度 |
| **异步健康检查** | 并发探测 `/v1/models`，连续 N 次失败标记不健康 |
| **配置热加载** | 监听 YAML 文件变更，增量更新后端，不中断服务 |
| **智能重试** | 错误分类（超时/5xx/连接拒绝），指数退避 + 抖动 |
| **Web 监控面板** | 实时展示后端状态、RPM、负载指标、趋势图表 |
| **Prometheus 指标采集** | 自动从 vLLM/SGLang 后端采集 running/waiting/GPU 缓存等指标 |
| **Webhook 通知** | 故障/恢复/定期报告推送到企业微信 |
| **多后端部署工具** | 一键部署 vLLM/SGLang 到多节点，自动生成路由配置 |
| **压力测试工具** | 多进程 + 异步协程，支持万级并发，实时可视化 |

## 项目结构

```
go_router/
├── cmd/router/             # 程序入口
├── internal/
│   ├── config/             # 配置加载与默认值
│   ├── handler/            # HTTP 请求处理器
│   ├── health/             # 健康检查 & 配置热加载
│   ├── model/              # 数据模型（Backend, Metrics 等）
│   ├── monitor/            # 监控系统（Prometheus 采集、Web 面板、TUI 面板）
│   ├── notify/             # Webhook 通知（企业微信）
│   ├── pool/               # COW 后端池管理
│   ├── ratelimit/          # 滑动窗口 RPM 限流
│   ├── router/             # 核心路由管理器（转发、重试、错误分类）
│   ├── routing/            # 路由策略（shuffle / least-connections / least-waiting）
│   └── util/               # 工具函数（URL、统计、Prometheus 解析）
├── configs/                # 后端配置文件
│   ├── openai_config.yaml
│   └── self_deployed_config.yaml
├── scripts/
│   ├── self_deploy.py      # 多节点 vLLM/SGLang 部署工具
│   ├── benchmark.py        # 高性能压力测试工具
│   └── install_go.sh       # Go 环境安装脚本
├── config.yaml             # 主配置文件
├── run.sh                  # 一键启动脚本
└── docs/
    ├── quick_start.md      # Quick start guide (English)
    ├── quick_start_zh.md   # 快速上手指南 (中文)
    └── readme_zh.md        # 中文 README
```

## 快速开始

### 环境要求

- Go 1.23+（可使用 `scripts/install_go.sh` 安装）
- Python 3.8+（用于部署和压测脚本）

### 构建与运行

```bash
# 构建
go build -o llm-router ./cmd/router/

# 运行
./llm-router -config config.yaml
```

或使用一键启动脚本：

```bash
bash run.sh
```

启动后：
- **API 服务**：`http://0.0.0.0:8000`
- **Web 监控面板**：`http://0.0.0.0:80`

### 与 DataStudio 配合使用

在 DataStudio 配置中，将 `model.api_base` 指向 Router 地址：

```python
model = dict(
    model="Qwen3-VL-30B-A3B-Instruct",
    api_base="http://<router-host>",
    port=8000,
    thread_num=1024,
    return_dict=True,
)
```

Router 会自动将请求分发到所有健康的后端实例。

> 详细使用方法请参考 **[快速上手指南](quick_start_zh.md)**。

## API 接口

| 方法 | 路径 | 功能 |
|------|------|------|
| POST | `/v1/chat/completions` | 路由转发 chat completion 请求 |
| GET  | `/v1/models` | 返回已注册模型列表（OpenAI 兼容格式） |
| GET  | `/health` | 路由器健康检查 |

## 配置概览

### 主配置文件 (`config.yaml`)

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

### 后端配置 (`configs/` 目录)

每种 `source_type` 对应一个 YAML 文件（文件名前缀决定类型）：

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
      rpm_limit: null   # null = 无限制
```

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-config` | `config.yaml` | 主配置文件路径 |
| `-log-level` | 配置文件中的值 | 日志级别覆盖：debug / info / warn / error |
| `-enabled-sources` | 配置文件中的值 | 逗号分隔的启用来源类型 |

## 相关项目

| 项目 | 说明 | 链接 |
|------|------|------|
| **DataStudio** | 配置驱动的多模态数据处理流水线 | [GitHub](https://github.com/Open-Bee/DataStudio) |
| **DataVis** | 多模态数据可视化与分析平台 | [GitHub](https://github.com/uyzhang/DataVis) |
| **Honey-Data-15M** | DataStudio 生产的 1500 万高质量 QA 对 | [HuggingFace](https://huggingface.co/datasets/Open-Bee/Honey-Data-15M) |
| **Bee** | 全开源多模态大语言模型项目 | [项目主页](https://open-bee.github.io/) |

## 许可证

[Apache License 2.0](../LICENSE)
