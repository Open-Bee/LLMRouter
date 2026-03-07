# LLM Router — 快速上手指南

[English](quick_start.md)

本指南涵盖 LLM Router 的安装、配置、部署和压测全流程。

---

## 目录

1. [环境安装](#1-环境安装)
2. [构建与启动](#2-构建与启动)
3. [配置详解](#3-配置详解)
4. [后端配置管理](#4-后端配置管理)
5. [路由策略](#5-路由策略)
6. [一键部署 vLLM/SGLang 后端](#6-一键部署-vllmsglang-后端)
7. [压力测试](#7-压力测试)
8. [Web 监控面板](#8-web-监控面板)
9. [与 DataStudio 集成](#9-与-datastudio-集成)
10. [Webhook 通知](#10-webhook-通知)
11. [日志配置](#11-日志配置)
12. [常见问题](#12-常见问题)

---

## 1. 环境安装

### 安装 Go

需要 Go 1.23+。如果环境中没有 Go，可使用内置脚本安装：

```bash
bash scripts/install_go.sh
```

该脚本会安装 Go 1.23.4 到 `/usr/local/go` 并配置 PATH 和国内代理 (`goproxy.cn`)。

### Python 依赖（部署/压测脚本）

```bash
pip install pyyaml aiohttp rich openai
```

---

## 2. 构建与启动

### 方式一：手动构建

```bash
go build -o llm-router ./cmd/router/
./llm-router -config config.yaml
```

### 方式二：一键启动

```bash
bash run.sh
```

`run.sh` 会自动检测源码变更、增量编译、释放占用端口并启动服务。

启动后两个服务同时运行：

| 服务 | 默认端口 | 说明 |
|------|----------|------|
| API 服务 | 8000 | OpenAI 兼容接口 |
| Web 监控面板 | 80 | 实时监控仪表板 |

### 命令行参数

```bash
./llm-router -config config.yaml -log-level debug -enabled-sources "self_deployed,openai"
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-config` | `config.yaml` | 主配置文件路径 |
| `-log-level` | 配置文件中的值 | 覆盖日志级别：`debug` / `info` / `warn` / `error` |
| `-enabled-sources` | 配置文件中的值 | 逗号分隔的启用来源类型 |

---

## 3. 配置详解

主配置文件 `config.yaml` 的完整说明：

```yaml
server:
  host: "0.0.0.0"
  port: 8000                 # API 监听端口

routing:
  strategy: "simple-shuffle" # 路由策略（见第 5 节）
  num_retries: 3             # 最大重试次数

http_client:
  request_timeout: 3600      # 后端请求超时（秒），适配 LLM 长时间生成
  connect_timeout: 10        # 连接超时（秒）
  max_idle_conns: 2000       # 最大空闲连接
  max_conns_per_host: 0      # 0 = 不限制
  idle_conn_timeout: 90      # 空闲连接超时（秒）

health_check:
  interval: 30               # 健康检查间隔（秒）
  timeout: 10                # 单次检查超时（秒）
  failure_threshold: 3       # 连续失败 N 次标记为不健康
  max_concurrency: 20        # 最大并发检查数

retry:
  base_delay_ms: 500         # 退避基础延迟
  max_delay_ms: 5000         # 最大退避延迟
  jitter_factor: 0.2         # 抖动因子
  failure_threshold: 3       # 连续转发失败 N 次标记为不健康

webhook:
  enabled: false
  url: ""                    # 企业微信 Webhook URL
  cooldown_seconds: 300      # 同一后端告警冷却时间
  batch_interval: 60         # 告警批量发送间隔
  report_interval_active: 3600   # 活跃期定期报告间隔
  report_interval_idle: 10800    # 空闲期定期报告间隔

monitor:
  collect_interval: 1        # Prometheus 指标采集间隔（秒）

backends:
  config_dir: "configs"      # 后端配置文件目录
  enabled_sources:           # 启用的来源类型
    - "openai"
    - "self_deployed"

dashboard:
  enabled: true
  refresh_interval: 2        # 面板刷新间隔（秒）
  web_port: 80               # Web 监控面板端口

log:
  level: "info"              # 日志级别
  dir: "logs"                # 日志文件目录，空字符串 = 不写文件
  max_size_mb: 100           # 单个日志文件最大大小
  max_backups: 10            # 保留旧日志文件数
  max_age_days: 7            # 旧日志最大保留天数
```

---

## 4. 后端配置管理

后端配置放在 `configs/` 目录，文件名前缀决定来源类型：

| 文件名模式 | 来源类型 |
|-----------|----------|
| `openai_*.yaml` | `openai` |
| `self_deployed_*.yaml` | `self_deployed` |

### 后端配置格式

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
      rpm_limit: null       # null = 无限制，整数 = 每分钟请求上限
```

### 配置热加载

修改 `configs/` 下的 YAML 文件后，Router 会自动检测变更并增量更新后端列表，无需重启。

热加载流程：
1. 检测文件修改时间变化
2. 解析新配置，与当前后端对比
3. 复用已有后端（保留运行时状态），新增后端执行健康检查
4. 按来源类型原子替换，不影响其他来源
5. 不健康的新后端后台重试（每 5 秒重试，最多约 2 分钟）

---

## 5. 路由策略

通过 `routing.strategy` 配置，支持三种策略：

| 策略 | 配置值 | 算法 | 适用场景 |
|------|--------|------|---------|
| **加权随机** | `simple-shuffle` | 按权重随机选择 | 通用场景（默认） |
| **最少连接** | `least-connections` | P2C（随机选 2 个，取连接数少的） | 后端性能差异大 |
| **最少等待** | `least-waiting` | P2C + Prometheus 指标（waiting + active 综合负载） | 需要精确调度 |

> `least-waiting` 策略需要后端暴露 Prometheus `/metrics` 端点（vLLM/SGLang 默认支持）。

### 错误分类与重试

Router 对转发错误进行分类处理：

| 错误类型 | 说明 | 是否重试 |
|---------|------|---------|
| 超时 | 后端响应超时 | 是 |
| 连接拒绝 | 连接被拒/DNS 失败 | 是 |
| 连接重置 | TCP 连接重置 | 是 |
| 5xx | 后端服务端错误 | 是（指数退避） |
| 4xx | 客户端错误 | 否（直接透传） |
| 客户端断连 | 请求方主动断开 | 否（立即中止） |

---

## 6. 一键部署 vLLM/SGLang 后端

`scripts/self_deploy.py` 支持在多个节点上一键部署 vLLM 或 SGLang 推理服务，并自动生成 Router 后端配置。

### 基本用法

```bash
# 使用环境变量指定节点
export NODE_IP_LIST="192.168.1.1,192.168.1.2,192.168.1.3"

python scripts/self_deploy.py \
    --python-cmd vllm_0.14.0 \
    --model-name Qwen/Qwen3-VL-235B-A22B-Instruct \
    --port 22005 \
    --tensor-parallel-size 8 \
    --backend vllm \
    --served-model-name Qwen3-VL-235B-A22B-Instruct
```

### 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--hosts` | `NODE_IP_LIST` 环境变量 | 逗号分隔的节点 IP 列表 |
| `--model-name` | （必填） | 模型名称或路径 |
| `--port` | `30596` | 起始端口号 |
| `--tensor-parallel-size` | （必填） | 每个服务的张量并行大小 |
| `--python-cmd` | `python` | Python 解释器（会映射为 conda 环境路径） |
| `--gpus-per-node` | `8` | 每个节点的 GPU 数量 |
| `--backend` | `vllm` | 推理后端：`vllm` 或 `sglang` |
| `--served-model-name` | 模型名最后一段 | 对外暴露的模型名称 |
| `--log-dir` | `./log_dir` | 日志目录 |
| `--health-check-timeout` | `3600` | 健康检查超时（秒） |

### 前置依赖

- **pdsh**：脚本通过 `pdsh`（并行分布式 Shell）在远程节点上执行命令，需要提前安装并配置好免密 SSH：
  ```bash
  # 安装 pdsh（Ubuntu/Debian）
  apt-get install pdsh
  # 安装 pdsh（CentOS/RHEL）
  yum install pdsh
  ```
  确保从部署机到所有目标节点的 SSH 免密登录已配置。

- **conda 环境**：`--python-cmd` 参数会被映射为 `/root/anaconda3/envs/{python_cmd}/bin/python`，因此目标节点需预先创建好对应的 conda 环境（如 `vllm_0.14.0`、`sglang_0.5.7`），并安装好 vLLM 或 SGLang。

### 多机节点指定方式

有两种方式指定部署目标节点：

**方式 1：环境变量 `NODE_IP_LIST`**

适用于集群调度系统（如 Slurm）自动注入节点列表的场景：

```bash
# 手动设置
export NODE_IP_LIST="192.168.1.1,192.168.1.2,192.168.1.3"

# 或者从 Slurm 获取（示例）
export NODE_IP_LIST=$(scontrol show hostnames $SLURM_JOB_NODELIST | paste -sd,)

python scripts/self_deploy.py \
    --python-cmd vllm_0.14.0 \
    --model-name Qwen/Qwen3-VL-235B-A22B-Instruct \
    --port 22005 \
    --tensor-parallel-size 8 \
    --backend vllm
```

**方式 2：`--hosts` 参数**

直接在命令行指定，优先级高于环境变量：

```bash
python scripts/self_deploy.py \
    --hosts "192.168.1.1,192.168.1.2,192.168.1.3" \
    --python-cmd vllm_0.14.0 \
    --model-name Qwen/Qwen3-VL-235B-A22B-Instruct \
    --port 22005 \
    --tensor-parallel-size 8 \
    --backend vllm
```

> IP 列表中的 `:<port>` 后缀会被自动忽略（如 `192.168.1.1:22` → `192.168.1.1`）。

### GPU 与端口自动分配

脚本根据 `--gpus-per-node` 和 `--tensor-parallel-size` 自动计算每个节点启动几个服务实例，以及每个实例使用哪些 GPU：

```
每节点服务数 = gpus_per_node / tensor_parallel_size
```

**示例**：3 个节点，每节点 8 GPU，`tensor-parallel-size=4`：

```
节点 192.168.1.1:
  服务 1: GPU 0,1,2,3 → 端口 22005
  服务 2: GPU 4,5,6,7 → 端口 22006

节点 192.168.1.2:
  服务 3: GPU 0,1,2,3 → 端口 22007
  服务 4: GPU 4,5,6,7 → 端口 22008

节点 192.168.1.3:
  服务 5: GPU 0,1,2,3 → 端口 22009
  服务 6: GPU 4,5,6,7 → 端口 22010
```

端口分配规则：
- 从 `--port` 指定的起始端口开始
- 通过远程 `lsof` 检测端口是否被占用，自动跳过已占用端口
- 端口号在所有节点间递增（非每节点重新开始）

### 部署流程

部署分为两个阶段：

**阶段 1**：异步启动所有服务
- 为每个节点分配可用端口（自动跳过已占用端口）
- 通过 `pdsh` 在远程节点启动 vLLM/SGLang，设置 `CUDA_VISIBLE_DEVICES` 指定 GPU
- 同节点多服务之间错开 3 秒避免 NCCL 竞争

**阶段 2**：轮询日志并验证启动
- 检测端口冲突 → 自动换端口重启（最多重试 5 次）
- 检测服务就绪 → 调用 `/v1/models` 验证模型可用
- 定期报告等待状态，超时未就绪的服务会被报告

### 部署完成后

脚本会自动：
1. 生成 `router_config_vllm.yaml`（独立的路由配置文件）
2. 增量更新 `configs/self_deployed_config.yaml`（按 `api_base` 去重，不覆盖已有配置）

Router 的配置热加载会自动检测到 `configs/self_deployed_config.yaml` 的变更并加载新后端。

### 更多示例

```bash
# 指定节点列表部署
python scripts/self_deploy.py \
    --hosts "192.168.1.100" \
    --python-cmd vllm_0.14.0 \
    --model-name Qwen/Qwen2.5-VL-32B-Instruct \
    --port 30010 \
    --tensor-parallel-size 4 \
    --backend vllm

# 部署 SGLang 后端
python scripts/self_deploy.py \
    --python-cmd sglang_0.5.7 \
    --model-name Qwen/Qwen3-VL-235B-A22B-Instruct \
    --port 22005 \
    --tensor-parallel-size 8 \
    --backend sglang

# 使用本地模型路径 + 自定义服务名
python scripts/self_deploy.py \
    --python-cmd vllm_0.14.0 \
    --model-name /path/to/model \
    --port 22005 \
    --tensor-parallel-size 4 \
    --backend vllm \
    --served-model-name Qwen3-235B-A22B-Instruct-2507-FP8
```

---

## 7. 压力测试

`scripts/benchmark.py` 是一个高性能压测工具，支持多进程 + 异步协程混合架构，可模拟万级并发。

### 基本用法

```bash
# 基础压测（1000 并发，持续运行，Ctrl+C 停止）
python scripts/benchmark.py --url http://localhost:8000 -c 1000

# 多进程压测（4 进程，10000 总并发）
python scripts/benchmark.py --url http://localhost:8000 -c 10000 -w 4

# 指定请求总数
python scripts/benchmark.py --url http://localhost:8000 -c 5000 -n 100000 -w 4

# 指定运行时长（60 秒）
python scripts/benchmark.py --url http://localhost:8000 -c 10000 -w 8 --duration 60
```

### 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--url`, `-u` | `http://localhost:8000` | Router 服务地址 |
| `--concurrency`, `-c` | `4096` | 并发数 |
| `--workers`, `-w` | `1` | 工作进程数（并发均匀分配） |
| `--requests`, `-n` | 无限 | 总请求数 |
| `--duration`, `-d` | 无限 | 运行时长（秒） |
| `--timeout`, `-t` | `600` | 单个请求超时（秒） |
| `--model`, `-m` | `Qwen3-235B-A22B-Instruct-2507-FP8` | 请求使用的模型名 |
| `--no-visual` | 关 | 禁用可视化，使用纯文本输出 |

### 输出指标

压测实时展示以下指标（安装 `rich` 后有可视化面板）：

- **QPS**：当前/平均每秒请求数
- **延迟分布**：平均 / P50 / P95 / P99 延迟
- **成功率**：请求成功百分比
- **趋势图表**：QPS、延迟、成功率的实时变化趋势

---

## 8. Web 监控面板

Router 内置两个 Web 页面，默认在端口 80 提供：

### 主仪表板 (`http://<host>:80/`)

实时展示：
- **系统概览**：运行时间、总请求数、成功/失败数、成功率、Goroutines、内存
- **来源汇总**：按 `self_deployed` / `openai` 分组统计
- **模型汇总**：按模型名分组的后端数量和请求统计
- **后端列表**：完整的后端表格（支持搜索过滤），包含状态、引擎类型、连接数、RPM、GPU 缓存等

### 图表页面 (`http://<host>:80/charts`)

提供多维度实时趋势图：
- 支持 **总览 / 按模型 / 按后端** 三种维度切换
- 支持 **RPM / Running / Waiting / Connections / Pending** 等指标切换
- 概览小图：RPM 总览、Running/Waiting、连接数、健康/不健康比例

### API 接口

Web 面板同时提供 JSON API：

| 路径 | 说明 |
|------|------|
| `/api/dashboard` | 完整的系统/请求/后端状态 JSON |
| `/api/history` | 最近 120 个时间序列数据点 |

---

## 9. 与 DataStudio 集成

LLM Router 作为 DataStudio 的 MLLM 请求路由层，典型使用方式：

### 步骤 1：启动 Router

```bash
cd tools/LLMRouter
bash run.sh
```

### 步骤 2：配置后端

编辑 `configs/self_deployed_config.yaml` 添加后端，或使用 `self_deploy.py` 自动部署并生成配置。

### 步骤 3：DataStudio 配置指向 Router

```python
# DataStudio 配置文件中
model = dict(
    model="Qwen3-VL-30B-A3B-Instruct",
    api_base="http://<router-host>",
    port=8000,
    thread_num=1024,
    return_dict=True,
    max_tokens=4096,
)
```

DataStudio 的所有 MLLM 请求（MLLMFilter、MLLMRewriter）都会通过 Router 自动分发到健康的后端实例，实现负载均衡和自动故障转移。

### 多机分布式场景

在多机分布式场景下（DataStudio 的 `multi_machine_deployment`），Router 尤其有用：
- 多台 DataStudio Worker 可共用同一个 Router 地址
- Router 负责跨节点的后端负载均衡
- 后端故障时自动切换，不影响数据处理流程

---

## 10. Webhook 通知

启用企业微信 Webhook 通知：

```yaml
webhook:
  enabled: true
  url: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY"
  cooldown_seconds: 300      # 同一后端告警冷却时间
  batch_interval: 60         # 告警聚合窗口
```

支持的通知类型：

| 类型 | 触发条件 | 说明 |
|------|---------|------|
| 启动报告 | 系统启动 | 后端总数、健康数、模型列表 |
| 故障告警 | 后端变为不健康 | 批量聚合后发送 |
| 恢复通知 | 后端恢复健康 | 立即发送 |
| 定期报告 | 定时触发 | 运行时间、后端状态、请求统计 |

---

## 11. 日志配置

```yaml
log:
  level: "info"          # debug | info | warn | error
  dir: "logs"            # 日志文件目录，空 = 不写文件
  max_size_mb: 100       # 单文件最大 100MB 后轮转
  max_backups: 10        # 保留 10 个旧文件
  max_age_days: 7        # 保留 7 天
```

日志同时输出到控制台和文件。启用追踪日志：

```bash
export LLM_ROUTER_TRACE=1
./llm-router -config config.yaml
```

追踪日志会记录每个请求的完整路由/转发/重试过程，写入独立文件。

---

## 12. 常见问题

### Q: 端口被占用怎么办？

`run.sh` 会自动释放占用的端口。手动释放：

```bash
lsof -ti:8000 | xargs kill -9  # 释放 API 端口
lsof -ti:80 | xargs kill -9    # 释放面板端口
```

### Q: 如何添加新的后端？

编辑 `configs/self_deployed_config.yaml`，在 `model_list` 中添加条目。Router 会自动热加载，无需重启。

### Q: 健康检查是怎样工作的？

Router 定期向每个后端的 `/v1/models` 发送 GET 请求。连续 `failure_threshold` 次失败后标记为不健康，恢复后自动重新启用。

### Q: 如何查看后端状态？

- 打开 Web 监控面板：`http://<host>:80/`
- 调用 API：`curl http://<host>:80/api/dashboard`

### Q: 如何使用 least-waiting 策略？

1. 确保后端是 vLLM 或 SGLang（它们默认暴露 `/metrics` 端点）
2. 配置 `routing.strategy: "least-waiting"`
3. Router 会自动从 `/metrics` 采集 running/waiting 等指标进行调度
