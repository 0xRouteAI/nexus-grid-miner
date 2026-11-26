# Windows 原生运行（不用 Docker）

推荐用 Docker 运行（见 [README](README.md)）。如果你想在 Windows 上直接跑可执行文件，按下面做。

## 1. 先把 TEI 推理服务跑起来

agent 本身不做推理，它把任务转给本机的
[text-embeddings-inference](https://github.com/huggingface/text-embeddings-inference)（TEI）。
**一张显卡只跑一个模型。** 在 Windows 上请按 TEI 官方文档启动你选的那个模型，例如：

- `BAAI/bge-m3`，监听 `http://127.0.0.1:8080`；或者
- `BAAI/bge-reranker-v2-m3`，监听 `http://127.0.0.1:8080`

浏览器打开 `http://127.0.0.1:8080/health` 能返回，就说明起来了。

## 2. 编译 agent

需要 Go 1.24+：

```powershell
cd agent-go
go build -trimpath -o bin\nexus-miner-agent.exe .\cmd\agent
cd ..
```

## 3. 配置

```powershell
copy .env.example .env
```

用记事本编辑 `.env`：

```dotenv
SERVER_URL=nexus.example.com
WALLET=0x你的钱包地址
MINER_PRIVATE_KEY=你的私钥
MODEL=bge-m3
TEI_URL=http://127.0.0.1:8080
```

跑重排序模型时写 `MODEL=bge-reranker-v2-m3`，并把 `TEI_URL` 换成 `RERANK_URL=http://127.0.0.1:8080`。

## 4. 启动 / 停止

```powershell
.\start.ps1      # 读取 .env，后台启动，日志写到 logs\
.\stop.ps1       # 停止
```

日志里出现 `miner registered with server` 就表示已上线。

一台机器有多张卡时，每张卡各跑一个 TEI 和一个 agent，每个 agent 配不同的 `NODE_ID`、`MODEL` 和 TEI 地址。
