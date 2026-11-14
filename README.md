# nvidia-playgroud-go

NVIDIA Build Playground 的 Go 客户端和本地代理。支持多模型路由、hCaptcha PoW token 池，以及 OpenAI Chat Completions、OpenAI Responses 和 Claude Messages 接口。

默认模型为 `moonshotai/kimi-k3_1-pro`。项目依赖上游匿名 Playground 接口，上游调整可能导致模型或接口不可用。

## 快速开始

需要 Go 1.26。启动本地代理：

```bash
go run ./cmd/serve -auto -addr :8080
```

调用 OpenAI Chat Completions：

```bash
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"moonshotai/kimi-k3_1-pro","messages":[{"role":"user","content":"Hi"}],"stream":true}'
```

可用端点：

| 端点 | 用途 |
|------|------|
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | OpenAI Responses |
| `POST /v1/messages` | Claude Messages，需要 `anthropic-version` 请求头 |
| `GET /v1/models` | 当前模型列表 |
| `GET /healthz` | 健康检查和 token 池状态 |

代理默认不校验入站 API key，请仅监听可信网络，不要直接暴露到公网。

## 模型

- 默认模型：`moonshotai/kimi-k3_1-pro`。
- `z-ai/glm-5.2` 和 `moonshotai/kimi-k3` 已被上游移除，不要再作为默认模型使用。
- 请求体中的 `model` 用于选择注册表中的模型；未知模型返回 `unknown model`。
- 启动时加载本地缓存和编译期快照，然后刷新模型目录。默认每 6 小时刷新一次，失败时保留现有注册表。
- 刷新成功后写入 `models_cache.json`，下次启动可离线加载上次成功的目录。

查看当前模型：

```bash
curl http://localhost:8080/v1/models
```

常用模型参数：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-model-refresh` | `6h` | 目录刷新间隔；`0` 仅启动时刷新，负数关闭刷新 |
| `-model-cache` | `models_cache.json` | 模型目录缓存路径，空值关闭缓存 |
| `-catalog-cookie` | 空 | 可选的目录页 Cookie，例如已有的 `aws-waf-token` |

重新抓取模型快照：

```bash
python3 scripts/scrape_playground_models.py > scripts/playground_models.json
```

抓取后按输出更新 `internal/models/registry.go`。可用以下命令验证实时抓取：

```bash
MODEL_LIVE=1 go test ./internal/models -run TestLiveFetchSmoke -v
```

## 验证码

验证码求解与 `nvidia-playground.mjs` 的 minter 同一条路径：刮 `api.js` 拿版本，`POST checksiteconfig`（不带 `hl`），在 v8go 里调用 `hsw(jwt, {href, ardata:null})`，由 hsw 自己采集指纹并算出 `n`。提交体是 msgpack `{v, sitekey, host, hl, motionData:"{}", n, e:null}`，经 `hsw(1)` 加密后以 `msgpack([JSON.stringify(c), ext18(密文)])` 发到 `getcaptcha`。只接受 `octet-stream` 答案，解密后只读 `generated_pass_UUID`。不预造指纹，也不走明文参数变体或二次 claim。

hCaptcha 的 HTTP 仍走 Chrome 指纹的 uTLS（ALPN 固定 HTTP/1.1）。下载 `hsw.js` 时带 widget HTML referer，失败会先拉 HTML 再试，并校验 JWT 里的 SRI。

- token 单次有效，池中 token 默认 90 秒后丢弃。
- token 来源优先级：请求头 `nv-captcha-token`、`-captcha` 一次性参数、`-auto` token 池。
- 池为空时，请求最多等待 `-captcha-wait`（默认 30 秒），随后返回 503。
- 上游返回 captcha 相关错误时，代理会更换 token 重试，最多尝试 3 次。
- 无请求 3 分钟后停止后台求解，下次请求到达时重新启动；`-pool-idle=0` 可关闭此行为。

单独获取 token：

```bash
go run ./cmd/captchapow
go run ./cmd/captchapow -v
```

## 命令行工具

| 命令 | 用途 |
|------|------|
| `cmd/serve` | 本地多格式代理 |
| `cmd/predict` | 单发预测请求，可指定 function-id |
| `cmd/captchapow` | 生成 hCaptcha token |
| `cmd/streambench` | SSE 时序和并发测试 |
| `cmd/example` | Go 客户端示例 |

```bash
go run ./cmd/predict -prompt "ping"
go run ./cmd/predict -list
go run ./cmd/predict -model minimaxai/minimax-m3 -v
go run ./cmd/streambench -auto -prompt "Count from 1 to 20."
```

需要指定注册表外的 NVCF function 时，可使用 `-function-id` 和 `-slug`：

```bash
go run ./cmd/predict -model acme/new-llm -slug new-llm -function-id <uuid>
```

## 配置

常用 serve 参数：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:8080` | 监听地址 |
| `-auto` | `false` | 启用验证码 token 池 |
| `-pool-size` | `12` | 池中保留的可用 token 数；需 ≥ `-pool-workers`，否则多余求解器不会运行 |
| `-pool-workers` | `3` | 并发求解器数量；建议 ≤ CPU 数 − 1 |
| `-captcha-wait` | `30s` | 等待 token 的最长时间，超时返回 503 |
| `-max-inflight` | `9` | 最大并发上游流，`0` 表示不限制 |
| `-coalesce-ms` | `16` | SSE 内容合并窗口，`0` 关闭 |
| `-proxy` | 空 | PoW、模型目录和 WAF token 使用的代理 |

完整参数见 `go run ./cmd/serve -h`。

环境变量：

| 变量 | 作用 |
|------|------|
| `CHROME_PROXY` | PoW 求解、模型目录抓取和 WAF token 使用的代理，等同于 `-proxy` |
| `HTTP_PROXY` / `HTTPS_PROXY` | 标准 Go 代理环境变量 |

predict API 始终直连；`-proxy` 和 `CHROME_PROXY` 只作用于 PoW 求解、模型目录抓取和 WAF token 获取。

### 出口轮换（WARP）

`cmd/warpsocks` 把多个 SOCKS5 出口（例如多个 WARP 隧道）合成一个 SOCKS5 代理，每条新 TCP 连接换一个出口。求解器在单次求解内保持一条连接、失败后重连，因此每一次求解固定落在一个出口上，重试则换到下一条隧道：

```bash
# 每个出口一个 WARP 客户端，例如 usque
usque socks -c cfg1.json -b 127.0.0.1 -p 4001 &
usque socks -c cfg2.json -b 127.0.0.1 -p 4002 &

# 合成为一条轮换代理
go run ./cmd/warpsocks -exits 127.0.0.1:4001,127.0.0.1:4002 -addr 127.0.0.1:1080

# 全部 hCaptcha 流量走轮换出口
go run ./cmd/serve -auto -proxy socks5://127.0.0.1:1080
```

`-exits` 也可用 `WARPSOCKS_EXITS` 环境变量设置；`-v` 打印每条连接实际使用的出口。本命令不创建隧道，只做选择与转发。

## Go 客户端

仓库 module 名为 `nvidia-playgroud-go`。在仓库内使用：

```go
import nvidia "nvidia-playgroud-go"

client := nvidia.New(
    nvidia.WithCaptchaToken("P1_..."),
    nvidia.WithModel("moonshotai/kimi-k3_1-pro"),
)
resp, err := client.Chat(ctx, messages)
```

token 单次有效。需要指定其他模型时，使用 `nvidia.WithModel("publisher/slug")`；需要固定 NVCF 调用目标时，使用 `WithModelInfo`。

## Docker

```bash
docker compose up --build
```

或使用已发布镜像：

```bash
docker run --rm -p 8080:8080 ghcr.io/6kmfi6hp/nvidia-playgroud-go:latest
```

镜像不包含 Chromium。健康检查路径为 `GET /healthz`。反向代理流式接口时请关闭 buffering，并将 read timeout 设置为至少 120 秒。

## 部署到 Render（保活）

Render 免费版 web service 在 **15 分钟没有入站请求**后休眠（spin down），下一次请求要等约 50 秒冷启动。上游 API 本身没问题，只是实例被停掉了。仓库提供三条保活路径，任选其一：

1. `render.yaml` —— Blueprint 部署描述（Docker、`plan: free`、`healthCheckPath: /healthz`、区域 `singapore`）。注意 `healthCheckPath` 只用于判断部署是否健康，**不阻止**休眠。
2. `scripts/keepalive.workflow.yml` —— GitHub Actions 的**模板**，每 5 分钟 `curl` 一次 `/healthz`。它故意放在 `.github/workflows/` 之外：推送该目录需要带 `workflow` scope 的 token，当前凭据没有，GitHub 会直接拒绝。要启用就复制过去（或直接在网页编辑器里新建）：

   ```bash
   mkdir -p .github/workflows
   cp scripts/keepalive.workflow.yml .github/workflows/keepalive.yml
   ```

   然后在 Settings → Secrets and variables → Actions → Variables 里加 `KEEPALIVE_URL`（例如 `https://xxx.onrender.com`）。注意 GitHub 的定时任务是最佳努力，高峰期可能晚 10–20 分钟才启动，所以它只能当兜底，最可靠的还是第 1 种外部监控。
3. `scripts/keepalive.sh` —— 在任何常开机器上跑一个循环 ping，适合漏 ping 时兜底：

```bash
./scripts/keepalive.sh --once https://xxx.onrender.com   # 单次自检，非 0 退出码即失败
KEEPALIVE_LOG=keepalive.log nohup ./scripts/keepalive.sh https://xxx.onrender.com &
```

脚本默认 600 秒一次、单次超时 90 秒（冷启动可能占掉近 60 秒）、失败后 20 秒快速重试一次；`KEEPALIVE_INTERVAL` / `KEEPALIVE_TIMEOUT` / `KEEPALIVE_PATH` 可覆盖。

ping 的目标是 `/healthz`：它由进程内直接应答，只报告 token 池计数，不打上游 NVIDIA 接口，所以既便宜也不会触发限流。

几点注意：

- 免费额度是每个 workspace 每月 750 instance hours，一个 7×24 常驻服务就要用掉约 730 小时，几乎没有余量给其他服务。
- `-keep-alive` 参数是另一件事：它定时 HEAD 上游 predict 主机，只为保住进程内的 TCP 连接（省掉 300–500ms 重连），不产生入站流量，因此**不能**阻止 Render 休眠。
- 进程内自 ping 也不能当保活：实例休眠后进程已经停了，定时器没有机会把自己叫醒。保活必须在外部发起。
- 如果服务是反复重启或崩溃而不是"睡过去"，那是 OOM 或上游失败，加 ping 治不了，先看 Render 的 Logs。

## 项目结构

```text
cmd/serve/            本地代理
cmd/predict/          单发 predict 请求
cmd/captchapow/       hCaptcha PoW token 工具
cmd/streambench/      SSE 和并发测试
internal/captcha/     token 池
internal/captchatls/  uTLS Chrome 指纹传输
internal/hcaptcha/    hCaptcha 请求编排
internal/hcaptchapow/ PoW 算法
internal/hsw/         v8go 运行器
internal/models/      模型注册表和目录刷新
internal/provider/    上游请求与响应适配
scripts/              模型目录抓取和实验脚本
runs/                 历史实验记录
```

## 开发

```bash
go build ./...
go vet ./...
go test ./...
```

可选的实时目录冒烟测试：

```bash
MODEL_LIVE=1 go test ./internal/models -run TestLiveFetchSmoke -v
```

推送 `v*` tag 会触发二进制和 Docker 镜像发布。支持 linux/amd64、linux/arm64 和 darwin/arm64；v8go 的预编译库限制了其他平台的发布。

历史实验记录见 `runs/`。
