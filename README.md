# companion-go

[中文](#%E4%B8%AD%E6%96%87) | [English](#english)

---

## 中文

自托管的 Telegram 机器人，目标是在有限的上下文下让大模型的人设保持连续：用本地检索式长期记忆和风格锚点代替整段历史回放。Go 编写，**零第三方依赖**，单个静态二进制，换模型后记忆可延续，同时提供 OpenAI 兼容 API。
后端可以是任意 OpenAI 兼容接口（DeepSeek API、本地 llama.cpp / vLLM 的 Qwen 等）；记忆全部存在本地，按检索注入，而不是每轮回放全部历史。

### 特性
- **通用后端**：`LLM_BASE_URL + LLM_API_KEY + LLM_MODEL`；对话、后台记忆整理、读图、嵌入可分别配置。
- **长期记忆（检索式）**：每轮只注入 top-K 相关记忆 + 核心档案 + 滚动摘要 + 最近若干条原文。
- **记忆自我优化**：每 N 轮抽取（仅用户亲口所述）→ 新旧去重/更新（旧条目标记为“已取代”，不删除）→ 空闲时滚动摘要与整理（归档陈旧低重要度、合并重复、重写核心档案，整理前自动备份）。
- **学习进度表**：记录知识点状态与反复出现的错误模式，后续对话中会引用。
- **换模型可延续**（见下）。
- **读图**：发图片即可（需要视觉模型）。
- 数据全在 `/data`（JSONL + 快照），无数据库服务。

### 快速开始（Docker）
```bash
cp .env.example .env     # 填 TELEGRAM_TOKEN（@BotFather 给的 bot token）、LLM_*、EMBED_*
docker compose up -d --build
docker compose logs -f companion
```
`ALLOWED_USER_IDS` 先留空：给 bot 发一句话，它会回你的数字 ID，填回 `.env` 后 `docker compose up -d`。
没有入站端口（Telegram 长轮询，仅出站）。

### HTTP API：把它当“本地模型”接任意前端
设 `API_ADDR=:8088` 和 `API_KEY=<随机长串>` 后，服务同时提供 OpenAI 兼容接口（此时 `TELEGRAM_TOKEN` 可省略，纯 API 模式）：

| 接口 | 说明 |
|---|---|
| `POST /v1/chat/completions` | 模型 `companion`：带记忆/人设；**只取最后一条 user 消息**，前端发来的历史和 system 一律忽略（历史、记忆由服务端管理）。支持 `stream`。 |
| 同上，模型 `companion-raw` | 纯透传到对话后端，不带记忆、不存储。把前端的“标题/标签生成”等辅助任务指向它，避免污染记忆。 |
| `GET /v1/models` | 列出以上两个模型 |
| `GET /v1/memory?q=` · `DELETE /v1/memory/{id}` | 查看/检索、删除记忆（前端可做记忆面板） |
| `GET /healthz` | 无需鉴权 |

- 前端里填：Base URL `http://<host>:8088/v1`，Key = `API_KEY`，模型 `companion`。
- 默认会话与 Telegram 白名单里最小的用户 ID 共用，因此**网页端和 Telegram 共享同一份记忆与历史**；用请求头 `X-Companion-Chat: <名字>`（或 `user` 字段）可开独立会话。
- 图片：支持 `image_url` 的 base64 `data:` URL；**不会代为抓取远程 URL**。
- 流式是“生成完整回复后分块回放”（保证记忆写入一致），首字延迟等于整条回复的生成时间。
- 默认只绑定本机回环（compose 里 `API_BIND`）；对外暴露请放在反向代理/VPN 后并启用 TLS。

#### 工具调用（前端抓图等）
`companion` 模型支持 OpenAI `tools`：前端在请求里声明自己的工具（如 `capture_screen`、`fetch_image`），模型需要时返回 `finish_reason: "tool_calls"`；前端执行后，把 `assistant(tool_calls)` 和 `tool` 结果消息接在同一条用户消息后面再发一次，服务端继续这一轮。
- 工具结果里的图片：放进 `tool` 消息的 `content`（`image_url` 的 base64 `data:` URL 数组，或直接给 `data:image/...` 字符串），服务端用读图模型转写成文字再交给对话模型。
- 这一轮的工具往返**不写入长期历史**，只存用户原话和最终回复。
- 每轮工具调用上限 `MAX_TOOL_ROUNDS`（默认 4），到限后强制模型直接作答。
- 对话模型需支持函数调用；DeepSeek 开启思考时，带 tools 的请求必须回传 `reasoning_content`（服务端会原样转发/返回，前端需照做）。**建议聊天保持关闭思考。**
- 限制：若同一次回复里混合了服务端内置工具和前端工具，会整体交给前端，模型之前的内置工具结果不会出现在下一次请求里。

#### 联网搜索（可选）
`SEARCH_ENABLED=true` 后，模型自带 `web_search` / `web_fetch` 两个工具，由服务端调用 DeepSeek 的 Search/Fetch API（约 5 元/千次成功请求，失败不计费）。Telegram 和 API 都可用。搜索词和 URL 会发送给该服务。

### 用量与费用
每次调用的 token 用量（含 DeepSeek 的缓存命中/未命中）按“对话/后台整理/读图”累计保存。`/status` 显示次数、输入、缓存命中率、输出；设置 `PRICE_*` 后还会显示估算费用（不区分高峰/空闲时段，仅作参考）。`/balance` 查询 DeepSeek 余额。
最近原文窗口按批裁剪（滚动摘要在 125% 时一次裁到 60%），而不是每轮丢一条，以保持提示前缀稳定、提高缓存命中；`RECENT_TOKENS` 是目标值，实际窗口在约 60%–125% 之间波动。

### 在受限环境中构建
项目没有任何第三方模块，构建不会下载依赖：
```bash
go build -trimpath -o companion ./cmd/companion     # 只需要 Go ≥ 1.22 工具链
make test                                           # go test -race ./...
```
- Docker 能拉到 `golang` 镜像：`docker compose build`（可用 `--build-arg GO_IMAGE=... BASE_IMAGE=...` 指向内部镜像仓库）。
- 拉不到 golang 镜像：`make release` 在别处交叉编译，再 `docker build -f Dockerfile.prebuilt .`（基础镜像仅需 distroless/static 或 scratch）。
- 不用 Docker：直接运行二进制，`DATA_DIR` 指向任意可写目录。

### Docker 嵌套 / 访问宿主服务
- 数据用命名卷 `companion-data`，不依赖宿主路径，嵌套时行为一致。
- 不需要 `docker.sock`、特权或端口；容器以非 root、只读根文件系统、`cap_drop: ALL` 运行。
- 本地模型在宿主上时用 `host.docker.internal`；**DinD 里它指向外层容器**，请改用局域网 IP。嵌套层通常拿不到 GPU，模型服务建议跑在宿主上。
- 可选 compose profile：`local-embed`（CPU 嵌入）、`local-llm`（llama.cpp Vulkan）。镜像 tag 和启动参数需按你的 llama.cpp 版本核对。

### 换模型后记忆如何延续
- 记忆是**纯文本**（附 `by_model` 记录是谁写的），与对话模型无关：把 `LLM_*` 从 DeepSeek 改成 Qwen，记忆、档案、摘要、学习进度原样可用。
- 向量只和**嵌入模型**绑定。若改了 `EMBED_MODEL`，旧向量会被识别为不匹配：期间自动退化为关键词检索，并在启动时（`AUTO_REEMBED=true`）后台重新计算；也可手动 `/reembed`。
- 迁移/备份：`companion export > mem.json`（不含向量）；在新实例 `companion import mem.json`（需配置 `EMBED_*`，导入时重算向量）。
- **记忆连续 ≠ 人格连续**：不同模型语气不同。用 `/pin` 把你喜欢的回复固定为“风格示例”，会放进系统提示，帮助新模型对齐语气；`persona.md` 也在数据目录里，可直接编辑。

### 命令
`/memory [关键词]` `/balance` `/forget <id>` `/edit <id> <内容>` `/profile [set …]` `/learning` `/pin` `/pins` `/unpin <n>` `/reembed` `/consolidate` `/status` `/help`

### 配置
见 `.env.example`。常用：`RECENT_TOKENS`（最近原文预算）、`MEMORY_TOKENS`、`TOP_K`、`MIN_SIM`、`EXTRACT_EVERY`、`IDLE_MINUTES`、`CONSOLIDATE_HOURS`。
DeepSeek V4 默认开启思考，聊天建议 `LLM_EXTRA_BODY={"thinking":{"type":"disabled"}}`（`.env.example` 已默认写入；该字段依据官方文档，未实测）。`deepseek-flash` 支持读图，`deepseek-v4-pro` 不支持。
Qwen3 系列关闭思考：`LLM_EXTRA_BODY={"chat_template_kwargs":{"enable_thinking":false}}`。
注意：用 shell `source` 加载 `.env` 时，JSON 值要用单引号包起来；Docker Compose 不会去掉引号，那里用不带引号的写法。

### 隐私
- 只响应白名单用户；token 不写日志（错误信息已剥离 URL）。
- 日志不记录消息内容。
- `/forget` 会从记忆库永久删除（立即压缩）；但**原始聊天日志** `messages.jsonl` 不受影响，需要时请手动清理。
- 使用云端 API 时，对话与待抽取内容会发送给对应服务商。

### 已验证 / 未验证
已验证（在开发沙箱中）：工具调用往返（含工具结果里的图片转写、历史不含工具噪声）、服务端内置搜索循环与轮数上限、用量/缓存命中解析与 `/status`、提示前缀跨轮稳定、余额解析（均用模拟服务）；HTTP API（鉴权、只取最后一条用户消息、会话共享与隔离、SSE 格式、raw 透传不入库、拒绝远程图片 URL）；`gofmt`、`go vet`、`go test -race`（store 重放/压缩/硬删除/导入导出、LLM 重试与 extra body、嵌入批处理、检索与换嵌入模型后的重嵌入、提示词布局、token 不泄露、bot 白名单与命令）；静态编译 linux/amd64 与 arm64；用 mock Telegram+LLM 端到端跑通真实二进制（收消息→回复→后台抽取记忆→export→healthcheck→SIGTERM 正常退出）；`docker compose config` 通过；`docker build` 已由使用者在本机构建成功。

**未验证**：真实 DeepSeek 的 Search/Fetch、`/user/balance` 鉴权、`thinking` 字段与 tool_calls 的实际响应；对真实 Telegram 服务器的联调；真实 DeepSeek / llama.cpp 的响应细节（DeepSeek 模型名请以官方文档为准）；可选 profile 里 llama.cpp 的镜像 tag 与参数；读图在真实视觉模型上的效果；长期（数周）记忆质量需要你实际使用后评估。

### 许可
MIT

---

## English

A self-hosted Telegram bot that aims to keep an LLM persona consistent under a limited context window, using local retrieval-based long-term memory and style anchors instead of replaying the whole history. Written in Go with **zero third-party dependencies**, shipped as a single static binary. Memory survives model swaps, and the service also exposes an OpenAI-compatible API.
The backend can be any OpenAI-compatible endpoint (DeepSeek API, Qwen on local llama.cpp / vLLM, etc.). All memory is stored locally and injected by retrieval every turn.

### Features
- **Backend-agnostic**: `LLM_BASE_URL + LLM_API_KEY + LLM_MODEL`. Chat, background memory maintenance, vision and embeddings can each use their own endpoint.
- **Long-term memory (retrieval-based)**: each turn injects only the top-K relevant memories + core profile + rolling summary + a window of recent verbatim messages.
- **Self-optimizing memory**: extraction every N turns (only what the user actually said) → dedupe/update against existing entries (old ones are marked superseded, not deleted) → idle-time rolling summary and consolidation (archive stale low-importance items, merge duplicates, rewrite the core profile; automatic backup first).
- **Learning tracker**: records topic status and recurring mistake patterns, referenced in later conversations.
- **Memory survives model swaps** (see below).
- **Image reading**: just send a picture (needs a vision-capable model).
- All data lives in `/data` (JSONL + snapshot). No database service.

### Quick start (Docker)
```bash
cp .env.example .env     # fill TELEGRAM_TOKEN (the bot token from @BotFather), LLM_*, EMBED_*
docker compose up -d --build
docker compose logs -f companion
```
Leave `ALLOWED_USER_IDS` empty at first: send the bot any message, it replies with your numeric ID. Put it in `.env`, then `docker compose up -d`.
No inbound ports are needed (Telegram long polling, outbound only).

### HTTP API: use it as a "local model" for any frontend
Set `API_ADDR=:8088` and `API_KEY=<long random string>` and the service also exposes an OpenAI-compatible API (`TELEGRAM_TOKEN` may then be omitted for API-only mode):

| Endpoint | Notes |
|---|---|
| `POST /v1/chat/completions` | Model `companion`: with memory and persona. **Only the last user message is used**; history and system messages sent by the frontend are ignored (history and memory are managed server-side). Supports `stream`. |
| Same, model `companion-raw` | Plain pass-through to the chat backend: no memory, nothing stored. Point the frontend's auxiliary tasks (title/tag generation, etc.) here to avoid polluting memory. |
| `GET /v1/models` | Lists the two models above |
| `GET /v1/memory?q=` · `DELETE /v1/memory/{id}` | Browse/search and delete memories (a frontend can build a memory panel on this) |
| `GET /healthz` | No auth required |

- In your frontend: Base URL `http://<host>:8088/v1`, Key = `API_KEY`, model `companion`.
- The default conversation is shared with the smallest user ID in the Telegram allowlist, so **the web frontend and Telegram share the same memory and history**. Use the header `X-Companion-Chat: <name>` (or the `user` field) to open a separate conversation.
- Images: base64 `data:` URLs in `image_url` are supported. **Remote URLs are never fetched on your behalf.**
- Streaming replays the finished reply in chunks (this keeps memory writes consistent), so time to first token equals the time to generate the whole reply.
- Binds to loopback only by default (`API_BIND` in compose). To expose it, put it behind a reverse proxy or VPN with TLS.

#### Tool calling (frontend fetches images, etc.)
The `companion` model supports OpenAI `tools`: the frontend declares its own tools (e.g. `capture_screen`, `fetch_image`) in the request. When the model needs one it returns `finish_reason: "tool_calls"`; the frontend runs it and sends the request again with the `assistant(tool_calls)` message and the `tool` result message appended after the same user message, and the server continues that turn.
- Images in tool results: put them in the `tool` message `content` (an array of base64 `data:` `image_url` parts, or a plain `data:image/...` string). The server transcribes them to text with the vision model before handing them to the chat model.
- The tool round trip of a turn is **not written to long-term history**; only the user's own words and the final reply are stored.
- Tool calls per turn are capped by `MAX_TOOL_ROUNDS` (default 4); at the cap the model is forced to answer directly.
- The chat model must support function calling. With DeepSeek thinking enabled, requests with tools must echo `reasoning_content` back (the server forwards it as-is and the frontend must do the same). **Keeping thinking off for chat is recommended.**
- Limitation: if one reply mixes built-in server tools and frontend tools, the whole call is handed to the frontend, and the model's earlier built-in tool results will not appear in the next request.

#### Web search (optional)
With `SEARCH_ENABLED=true` the model gets two built-in tools, `web_search` and `web_fetch`, executed server-side through DeepSeek's Search/Fetch API (about ¥5 per 1000 successful requests; failures are free). Works for both Telegram and the API. Search queries and URLs are sent to that service.

### Usage and cost
Token usage of every call (including DeepSeek cache hit/miss) is accumulated per kind: chat / background maintenance / vision. `/status` shows call count, input, cache hit rate and output; with `PRICE_*` set it also shows an estimated cost (it does not distinguish peak from off-peak hours, for reference only). `/balance` queries your DeepSeek balance.
The recent-message window is trimmed in batches (the rolling summary cuts from 125% down to 60% in one go) instead of dropping one message per turn, which keeps the prompt prefix stable and improves cache hits. `RECENT_TOKENS` is a target; the actual window fluctuates between roughly 60% and 125% of it.

### Building in restricted environments
The project has no third-party modules, so building downloads no dependencies:
```bash
go build -trimpath -o companion ./cmd/companion     # only needs a Go ≥ 1.22 toolchain
make test                                           # go test -race ./...
```
- If Docker can pull the `golang` image: `docker compose build` (use `--build-arg GO_IMAGE=... BASE_IMAGE=...` to point at an internal registry).
- If it cannot: cross-compile elsewhere with `make release`, then `docker build -f Dockerfile.prebuilt .` (the base image only needs to be distroless/static or scratch).
- Without Docker: run the binary directly and point `DATA_DIR` at any writable directory.

### Nested Docker / reaching host services
- Data uses the named volume `companion-data`, independent of host paths, so behavior is the same when nested.
- No `docker.sock`, privileges or ports needed; the container runs non-root, with a read-only root filesystem and `cap_drop: ALL`.
- For a local model on the host use `host.docker.internal`. **Inside DinD it points at the outer container**, so use the LAN IP instead. Nested layers usually cannot see the GPU, so run the model server on the host.
- Optional compose profiles: `local-embed` (CPU embeddings) and `local-llm` (llama.cpp Vulkan). Check image tags and launch flags against your llama.cpp version.

### How memory carries over when you swap models
- Memory is **plain text** (with `by_model` recording who wrote it) and independent of the chat model: change `LLM_*` from DeepSeek to Qwen and memories, profile, summary and learning progress work as before.
- Vectors are tied only to the **embedding model**. If `EMBED_MODEL` changes, old vectors are detected as mismatched: retrieval falls back to keyword search meanwhile, and re-embeds in the background at startup (`AUTO_REEMBED=true`) or on manual `/reembed`.
- Migration/backup: `companion export > mem.json` (no vectors); on the new instance `companion import mem.json` (needs `EMBED_*`, vectors are recomputed on import).
- **Continuous memory ≠ continuous personality**: different models sound different. Use `/pin` to fix replies you like as style examples; they go into the system prompt and help a new model match the tone. `persona.md` also lives in the data directory and can be edited directly.

### Commands
`/memory [keyword]` `/balance` `/forget <id>` `/edit <id> <text>` `/profile [set …]` `/learning` `/pin` `/pins` `/unpin <n>` `/reembed` `/consolidate` `/status` `/help`

### Configuration
See `.env.example`. Common knobs: `RECENT_TOKENS` (recent-message budget), `MEMORY_TOKENS`, `TOP_K`, `MIN_SIM`, `EXTRACT_EVERY`, `IDLE_MINUTES`, `CONSOLIDATE_HOURS`.
DeepSeek V4 has thinking on by default; for chat use `LLM_EXTRA_BODY={"thinking":{"type":"disabled"}}` (already the default in `.env.example`; this field follows the official docs).
`deepseek-flash` supports vision, `deepseek-v4-pro` does not.
To turn off thinking on Qwen3: `LLM_EXTRA_BODY={"chat_template_kwargs":{"enable_thinking":false}}`.
Note: when you load `.env` with a shell `source`, wrap JSON values in single quotes; Docker Compose does not strip quotes, so use the unquoted form there.

### Privacy
- Responds only to allowlisted users; tokens are never logged (URLs are stripped from error messages).
- Message contents are not logged.
- `/forget` permanently deletes from the memory store (compacted immediately), but the **raw chat log** `messages.jsonl` is untouched; clean it manually if needed.
- When using a cloud API, conversations and content to be extracted are sent to that provider.

### Verified / not verified
Verified (in the development sandbox, using fake/mock servers): tool-calling round trip (including image transcription in tool results, no tool noise in history), the built-in search loop and its round cap, usage/cache-hit parsing and `/status`, prompt prefix stability across turns, balance parsing; the HTTP API (auth, last-user-message-only, shared and isolated conversations, SSE format, raw pass-through not stored, remote image URLs rejected); `gofmt`, `go vet`, `go test -race` (store replay/compaction/hard delete/import-export, LLM retry and extra body, embedding batching, retrieval and re-embedding after an embedding-model change, prompt layout, tokens not leaked, bot allowlist and commands); static builds for linux/amd64 and arm64; an end-to-end run of the real binary against a mock Telegram + LLM (receive message → reply → background memory extraction → export → healthcheck → clean exit on SIGTERM); `docker compose config` passes; `docker build` has been built successfully by the user on their own machine.

**Not verified**: real DeepSeek Search/Fetch, `/user/balance` auth, and the actual responses for the `thinking` field and tool_calls; integration with the real Telegram servers; real-world DeepSeek / llama.cpp response details (check model names against the official docs); image tags and flags of llama.cpp in the optional profiles; image-reading quality on a real vision model; long-term (weeks) memory quality, which you need to judge from real use.

### License
MIT
