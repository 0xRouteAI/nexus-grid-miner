# 矿工端接口说明 / Miner Interfaces

> 矿工端（agent）对外的全部接口：它连接什么、发什么、收什么、读哪些配置、出了问题怎么处理。
> 代码里的消息结构在 `agent-go/internal/protocol`，与本文件保持一致。

当前协议版本 `protocol_version = 2`，支持的模型 `bge-m3@1.0.0`（embedding）、`bge-reranker-v2-m3@1.0.0`（rerank）。
**一个节点 = 一张显卡 + 一个模型**：agent 只注册 `MODEL` 指定的那一个模型，填多个会拒绝启动。

---

## 0. 总览

矿工端**不监听任何端口**，所有连接都由它主动发起：

| # | 连接对象 | 接口 | 代码 |
|---|---|---|---|
| A | 网络服务器 | WebSocket `wss://<SERVER_URL>/ws/miner` | `internal/wsclient`、`internal/protocol` |
| B | 网络服务器 | HTTP `GET https://<SERVER_URL>/api/catalog` | `internal/catalogclient` |
| C | 本机 TEI | HTTP `/health`、`/info`、`/embed`、`/rerank` | `internal/engine` |
| D | 本机系统 | `nvidia-smi`、环境变量 | `internal/gpu`、`internal/config` |

```
                 NEXUS GRID 服务器
          /ws/miner (A)     /api/catalog (B)
                 ▲ wss           ▲ https
                 └──── agent ────┘
                    │ http (C)  │ http (C)
             TEI embeddings  TEI reranker
```

### 怎么连上服务器

只需要配置 `SERVER_URL`，填网络的域名即可，agent 会自动补全：

| 你填的 | 实际连接 |
|---|---|
| `nexus.example.com` | `wss://nexus.example.com/ws/miner` |
| `https://nexus.example.com` | `wss://nexus.example.com/ws/miner` |
| `wss://nexus.example.com/ws/miner` | 原样使用 |

模型目录地址由同一个域名推出：`https://nexus.example.com/api/catalog`。只写域名时默认走 TLS（`wss://`）。

---

## A. WebSocket：`/ws/miner`

### A.1 连接参数

| 项 | 值 |
|---|---|
| 编码 | 每个 WebSocket **文本帧**一个 JSON 对象 |
| 注册 | 连上后必须在 15 秒内发出 `register` |
| 心跳 | 每 `HEARTBEAT_SECS`（默认 20）秒发一次 `heartbeat`；服务器 Ping 时自动回 Pong |
| 失联 | 服务器 90 秒左右收不到任何消息会断开连接 |
| 发送队列 | 本地最多 128 帧，满了说明连接卡死，agent 主动断开 |
| 并发 | 同时最多执行 `MAX_CONCURRENT_TASKS`（默认 4）个任务 |

### A.2 消息一览

| 方向 | `type` | 作用 |
|---|---|---|
| 矿工 → 服务器 | `register` | 第一帧：身份、显卡、可服务的模型、签名 |
| 服务器 → 矿工 | `registered` | 注册成功 |
| 矿工 → 服务器 | `heartbeat` | 保活 |
| 服务器 → 矿工 | `task` | 一个推理任务 |
| 矿工 → 服务器 | `result` | 任务结果，每个任务恰好一个 |
| 服务器 → 矿工 | `error` | 错误；`fatal: true` 时随后断开 |
| 服务器 → 矿工 | `pong` | 应用层 pong，只记录 |

不认识的 `type` 和字段一律忽略，新旧版本靠这一点保持兼容。

### A.3 `register`

```json
{
  "type": "register",
  "protocol_version": 2,
  "wallet": "0x1234...5678",
  "gpu_model": "NVIDIA GeForce RTX 4090",
  "vram_mb": 24564,
  "models": ["bge-m3"],
  "model_versions": [
    {"slug": "bge-m3", "version": "1.0.0", "capability": "embedding",
     "backend": "tei-embed", "model_ref": "BAAI/bge-m3"}
  ],
  "agent_version": "0.1.0",
  "node_id": "gpu-box-01:12345678",
  "signature_challenge": "9f2c4d6e8a0b1c3d5e7f9a1b2c3d4e5f",
  "signature_issued_at": "2026-11-26T10:15:00Z",
  "signature": "0x8b3a...1c"
}
```

| 字段 | 来源 |
|---|---|
| `wallet` | `WALLET` |
| `gpu_model` / `vram_mb` | `nvidia-smi`，或 `GPU_MODEL` / `VRAM_MB` |
| `model_versions` | 模型目录（B）里 `MODEL` 对应的版本，按显存过滤，再去掉健康检查失败的 TEI；正常情况下只有一项 |
| `models` | 同上的模型名列表（兼容字段） |
| `agent_version` | `AGENT_VERSION` |
| `node_id` | `NODE_ID`，默认 `<主机名>:<钱包后 8 位>` |
| `signature*` | 用 `MINER_PRIVATE_KEY` 签名，见 A.4 |

### A.4 注册签名（EIP-191 `personal_sign`）

每次连接都生成新的 16 字节随机挑战，对下面这段文字签名（换行分隔，逐字节固定）：

```
NEXUS GRID miner registration
Domain: <MINER_SIGNATURE_DOMAIN，默认 nexus-grid>
Wallet: <小写 0x 地址>
Node ID: <node_id>
Challenge: <signature_challenge>
Issued At: <RFC 3339 UTC，精确到秒>
```

- 本机时钟偏差要在 5 分钟以内。
- 签名为 65 字节十六进制（`0x` + r + s + v，v = 27/28）。
- 启动时会先检查私钥是否属于 `WALLET`，不匹配直接退出。
- 私钥只签这段文字，不发交易，不需要余额。

### A.5 `registered`

```json
{"type": "registered", "protocol_version": 2, "reputation": 80, "heartbeat_secs": 20}
```

agent 读取 `protocol_version`、`reputation`、`heartbeat_secs`，其他字段忽略。

### A.6 `heartbeat`

```json
{"type": "heartbeat", "inflight": 1, "ts": 1763000020}
```

### A.7 `task`

```json
{"type": "task", "task_id": "0f4e2152-...", "capability": "embedding",
 "model": "bge-m3", "version": "1.0.0", "backend": "tei-embed",
 "payload": {...}, "deadline_ms": 25000}
```

- 带 `version` 时按 `model@version` **精确匹配**本机注册的模型；找不到，或 `capability`、`backend` 对不上，就返回失败，绝不换别的模型。
- 超时取 `deadline_ms`，没有则用 `INFERENCE_TIMEOUT_SECS`（默认 150 秒）。

**embedding**

```json
// payload
{"model": "bge-m3", "input": ["text one", "text two"], "encoding_format": "float"}
// result
{"embeddings": [[0.012, -0.044, ...]], "usage": {"prompt_tokens": 7, "total_tokens": 7}}
```

`input` 可以是字符串或字符串数组；每个输入对应一个向量，bge-m3 为 1024 维。

**rerank**

```json
// payload
{"model": "bge-reranker-v2-m3", "query": "...", "documents": ["...", "..."], "top_n": 2}
// result
{"results": [{"index": 0, "relevance_score": 0.98}, {"index": 1, "relevance_score": 0.001}]}
```

`index` 不越界、不重复；`relevance_score` 在 `[0, 1]`；不回传文档原文。

### A.8 `result`

```json
{"type": "result", "task_id": "0f4e2152-...", "ok": true, "latency_ms": 143, "result": {...}}
{"type": "result", "task_id": "0f4e2152-...", "ok": false, "latency_ms": 800, "error": "TEI /rerank returned HTTP 500"}
```

每个 `task_id` 恰好回复一次。连接断开时，在途任务会被取消，不会写到已关闭的连接上。

### A.9 `error`

```json
{"type": "error", "message": "wallet is banned", "fatal": true}
```

- `fatal: false`：记日志，连接继续。
- `fatal: true`：服务器随后断开；agent 重新拉取模型目录，按 1 → 10 分钟退避重试，不退出。

### A.10 结果校验

网络会对节点的结果做抽查，抽查任务和普通任务完全一样，无法区分。
**正确地在本地 GPU 上运行模型就会通过**；不要针对特定任务或文本做特殊处理，也不要把请求转发给其他服务。

---

## B. 模型目录：`GET /api/catalog`

- 时机：启动时一次，之后每 `CATALOG_REFRESH_SECS`（默认 300）秒一次；收到 fatal 错误后也会重新拉。
- 限制：10 秒超时，最大 1 MB。
- 拉不到时，退回内置的 `bge-m3@1.0.0`、`bge-reranker-v2-m3@1.0.0`。

agent 读取的字段：

```json
{
  "revision": 12,
  "models": [
    {
      "model":   {"slug": "bge-m3", "capability": "embedding"},
      "version": {"version": "1.0.0", "backend": "tei-embed", "model_ref": "BAAI/bge-m3",
                  "min_vram_mb": 4096, "recommended_vram_mb": 8192}
    }
  ]
}
```

过滤规则：slug 等于 `MODEL`，且 `min_vram_mb` ≤ 显存 − `VRAM_HEADROOM_MB`（默认 512）。
目录有变化时，agent 自动断开并按新的模型集合重新注册，**不需要重启**。

---

## C. 本机 TEI

| 调用 | 用途 | 时机 |
|---|---|---|
| `GET {TEI}/health` | 是否存活 | 启动时，之后每 30 秒 |
| `GET {TEI}/info` | 核对加载的模型是否为 `TEI_MODEL_ID` / `RERANK_MODEL_ID` | 启动时 |
| `POST {TEI}/embed` | `{"inputs": [...]}` | 每个 embedding 任务 |
| `POST {TEI}/rerank` | `{"query": "...", "texts": [...], "truncate": true}` | 每个 rerank 任务 |

某个 TEI 挂了，立即停止上报这个模型；恢复后自动加回。全部挂掉时断开连接，等恢复后再连。

---

## D. 配置项

| 变量 | 默认 | 说明 |
|---|---|---|
| `SERVER_URL` | **必填** | 网络域名，例如 `nexus.example.com` |
| `WALLET` | **必填** | 积分地址 |
| `MINER_PRIVATE_KEY` | **必填** | `WALLET` 的私钥，只用于签名 |
| `TEI_IMAGE` | **必填**（Docker 部署） | 和显卡匹配的 TEI 镜像，见 README 的对照表 |
| `COMPOSE_PROFILES` | **必填**（Docker 部署） | 这张卡跑的模型：`bge-m3` 或 `bge-reranker-v2-m3`，只能一个；只启动对应的推理容器，并作为 agent 的 `MODEL` |
| `GPU_DEVICE` | `0` | 这个节点使用的显卡编号（`nvidia-smi` 序号）；多卡机器每张卡一个节点 |
| `NODE_ID` | `<主机名>:<钱包后 8 位>` | 同一钱包的每台机器/每张卡各不相同；`[A-Za-z0-9-_.:]`，≤ 128 字节 |
| `MODEL` | **必填**（不用 Docker 时） | 这张卡跑的唯一模型；Docker 部署由 `COMPOSE_PROFILES` 自动带入。旧变量 `ENABLED_MODELS` 只填一个模型时仍兼容 |
| `GPU_MODEL` / `VRAM_MB` | 自动探测 | 探测失败时手动指定 |
| `MAX_CONCURRENT_TASKS` | `4` | 1–32 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `TEI_URL` / `RERANK_URL` | `http://embeddings:80` / `http://reranker:80` | TEI 地址（Docker 部署无需修改） |
| `TEI_HEALTH_URL` / `RERANK_HEALTH_URL` | `<URL>/health` | 健康检查地址 |
| `TEI_MODEL_ID` / `RERANK_MODEL_ID` | `BAAI/bge-m3` / `BAAI/bge-reranker-v2-m3` | 预期的模型 ID |
| `MODEL_ENDPOINTS` | 空 | 按模型覆盖地址：`bge-m3=http://host:port,...` |
| `REQUIRE_MODEL_INFO_MATCH` | `true` | 是否校验 TEI 加载的模型 |
| `VRAM_HEADROOM_MB` | `512` | 显存预留 |
| `CATALOG_URL` | 由 `SERVER_URL` 推出 | 一般不用改 |
| `CATALOG_REFRESH_SECS` | `300` | 30–86400 |
| `MINER_SIGNATURE_DOMAIN` | `nexus-grid` | 一般不用改 |
| `AGENT_VERSION` | `0.1.0` | 上报的版本号 |
| `HEARTBEAT_SECS` | `20` | 5–60 |
| `INFERENCE_TIMEOUT_SECS` | `150` | 10–1800 |
| `PROBE_TIMEOUT_MS` | `3000` | 300–30000 |

---

## E. 自动行为

| 情况 | agent 的反应 |
|---|---|
| 网络断开 | 1 秒 → 30 秒指数退避加随机抖动后重连，每次重新签名注册 |
| 服务器 fatal 拒绝 | 重新拉目录，1 → 10 分钟退避重试 |
| 模型目录变化 | 重建模型集合，重新注册 |
| TEI 挂了 / 恢复 | 30 秒内停止 / 恢复上报该模型 |
| 发送队列满 | 主动断开，任务交给其他节点 |
| 收到 Ctrl+C / SIGTERM | 取消在途任务，干净退出 |

---

## F. 给贡献者：修改协议

- 字段只加不改；需要改名时新增字段，旧字段保留一段时间。
- 两边都忽略不认识的字段和消息类型，不要破坏这一点。
- 签名原文（A.4）改动会让所有已部署的 agent 注册失败，不接受这类 PR，除非协议版本一起升级。
- 不兼容的改动必须提升 `protocol.ProtocolVersion`，并保证新 agent 连到旧服务器时还能工作。
- 提交前运行 `cd agent-go && go vet ./... && go test -race ./...`。
