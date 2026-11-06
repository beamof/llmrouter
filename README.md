# 🚦 llmrouter

一个用 Go 写的大模型代理路由：统一入口 + 网页后台管理。请求体与响应一律透传，不改动任何内容。

## 特性

- **统一入口**：`/v1/chat/completions`、`/v1/responses`、`/v1/models`
- **多 Provider 管理**：同一模型名可挂多个 Provider，按优先级自动故障转移（网络错误 / 401 / 403 / 408 / 429 / 5xx 时切换下一个）
- **字节级透传**：请求体只读取 `model` 字段用于路由，其余不解析、不改写、不注入；流式（SSE）响应边收边发，非流式响应原样回传
- **网页后台**（中文，单文件，无前端依赖）：Provider / 模型增删改、优先级排序、用量统计（按 token 计）、每日 token 上限、按时段静音（mute）、上游余额查询
- 状态落盘为单个 JSON 文件（默认 `data.json`），零外部依赖，单二进制

## 快速开始

```bash
go build -o llmrouter .
./llmrouter -addr 127.0.0.1:8080 -admin-token 你的后台令牌
```

打开 `http://127.0.0.1:8080` 配置 Provider 与模型，然后把客户端的 Base URL 指过来：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}]}'
```

## API Key 管理

后台「Keys」页可生成 / 删除客户端访问密钥（`sk-r-…`）。客户端携带 `Authorization: Bearer sk-r-…` 访问 `/v1/*`：

- **一个 Key 都没有时，代理入口不鉴权**（本机使用最方便）；生成了第一个 Key 后，未带 Key 的请求将被拒绝（401）
- 环境变量 `LLMROUTER_API_TOKEN`（若有）始终有效，用量里记为「环境变量」
- 用量按 Key 记账：用量页有按 Key 的汇总卡片，明细表含 Key 列；删除 Key 立即生效，历史用量保留

## 参数

| 参数 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-addr` | `LLMROUTER_ADDR` | `127.0.0.1:8080` | 监听地址 |
| `-data` | `LLMROUTER_DATA` | `data.json` | 状态文件路径 |
| `-admin-token` | `LLMROUTER_ADMIN_TOKEN` | 空 | 后台令牌；空则后台不鉴权（仅建议本机使用） |
| `-api-token` | `LLMROUTER_API_TOKEN` | 空 | 额外的固定代理令牌（始终有效，与 Keys 页互不影响） |

## 路由规则

- 请求体里的 `model` 字段用于路由（唯一的读取项），其余内容一概不碰
- Provider 有两个基址：`Chat 基址`（chat completions）与 `Responses 基址`（responses 协议，留空则跟随 Chat 基址）。两种协议基址不同的厂商（如智谱 Coding Plan 没有 `/responses`，可单独填 `https://open.bigmodel.cn/api/v1`）一个 Provider 即可搞定
- 候选 = 该名字的所有模型条目，剔除「当前时间处于静音时段」和「今日 token 用量已达上限」的，按优先级从大到小依次尝试
- 上游返回**任何 4xx/5xx**（含 400/404/429 等）或网络错误时自动换下一个候选；全部失败时返回 502，并附上最后一个上游的状态码与错误详情
- **会话粘性**：客户端请求带 `X-Session-ID: <任意会话标识>` 头时，该会话首次请求按上述规则选模型并绑定；后续请求直连绑定的模型（不再套用优先级/静音/每日上限规则），仅当绑定上游失败才按规则重选并重新绑定。绑定 1 小时无活动过期，仅存内存（重启即清）：

  ```bash
  curl http://127.0.0.1:8080/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -H 'X-Session-ID: conv-123' \
    -d '{"model":"gpt-4o","messages":[...]}'
  ```
- 静音时段按服务器本地时间，每天重复，支持跨零点（如 22:00~06:00）
- 非流式响应会顺带读取 `usage` 字段记账 token（兼容 `prompt_tokens/completion_tokens` 与 `input_tokens/output_tokens`）；流式响应不解析流（保证透传），因此只计请求数、不计入每日 token 上限
- Provider 的 API Key 为空时，客户端的 Authorization 头会原样转发（自带 Key 模式）

## 模型测试按钮

每行模型的「测试」做的是**连通性探测**：GET 上游 `/models`，不发对话、不消耗 token。结果三态：

- ✅ 端口通（若上游返回模型列表，还会核对模型名是否在列表中）
- ⚠️ 端口通但有异常（鉴权失败 / 列表里没有该模型名）
- ❌ 连接失败（地址不对或服务不可达）

## 余额查询

Provider 可配置「余额查询 URL」（服务端 GET 该地址并带上该 Provider 的 Key）。任何返回 JSON 的接口都可以，例如 DeepSeek：

```
https://api.deepseek.com/user/balance
```

后台会展示原始 JSON，并自动把长得像余额的字段（balance / credit / quota / amount / remaining…）挑出来高亮显示。

## 测试

```bash
go test ./...
```
