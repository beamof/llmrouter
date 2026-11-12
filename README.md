# 🚦 llmrouter

用 Go 写的大模型代理路由：统一入口 + 网页后台管理。标准上游的请求体与响应一律字节级透传；
WorkBuddy、Zed、Command Code 等私有协议上游由内置适配器按其线协议转发。

## 特性

- **统一入口**：`/v1/chat/completions`、`/v1/responses`、`/v1/models`
- **多 Provider + 多路由组**：同一模型名可挂多个 Provider，按优先级自动故障转移；路由组对外是独立的虚拟模型名，模型配置全局共享，组间只有组内优先级不同
- **透传优先**：请求体只读取 `model` 字段用于路由，其余不改写、不注入；SSE 边收边发，非流式原样回传
- **路由治理**：每日 token 上限、并行上限、按时段静音、429 自动静音、会话粘性、Responses 协议转换
- **内置上游适配**：WorkBuddy / CodeBuddy（国内 / 国际）、Zed、Command Code
- **网页后台**（中文、单文件、无前端依赖）：Provider / 模型 / 路由组管理、用量与会话统计、余额查询、异常日志、版本检测与一键升级、深浅色跟随系统
- 状态落盘为单个 JSON 文件，零外部依赖，单二进制

## 快速开始

```bash
go build -o llmrouter .
./llmrouter -addr 127.0.0.1:8080 -admin-token 你的后台令牌
```

打开 `http://127.0.0.1:8080` 配置 Provider 与模型，再把客户端 Base URL 指过来：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}]}'
```

## 参数

| 参数 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-addr` | `LLMROUTER_ADDR` | `127.0.0.1:8080` | 监听地址 |
| `-data` | `LLMROUTER_DATA` | `data.json` | 状态文件；用量与会话统计写入同目录的 `data.json.usage` |
| `-admin-token` | `LLMROUTER_ADMIN_TOKEN` | 空 | 后台令牌；空则后台不鉴权（仅建议本机使用） |
| `-api-token` | `LLMROUTER_API_TOKEN` | 空 | 固定的代理令牌，始终有效 |
| `-trust-proxy` | `LLMROUTER_TRUST_PROXY` | 关 | 反向代理后开启，按 `X-Forwarded-For` 判定来源 IP |
| `-version` | — | — | 打印版本号后退出 |

## API Key 管理

后台「Keys」页生成 / 删除客户端密钥（`sk-r-…`），客户端带 `Authorization: Bearer sk-r-…` 访问 `/v1/*`：

- 一个 Key 都没有时不鉴权；生成第一个 Key 后，未带 Key 的请求返回 401
- 用量按 Key 记账，删除 Key 立即生效，历史用量保留

## 路由规则

- **路由组**：请求组名（默认组为 `auto`）时只在本组成员内按组内优先级选路；直接指定模型名时按该名字的全部配置路由。`/v1/models` 始终列出全部组名
- **协议匹配**：`/v1/responses` 只选能处理 Responses 的模型——标准 Provider 两种协议都透传；仅支持 chat 的上游默认跳过并自动改用其他可用模型（不跨协议硬发）。设置页开启「Responses 协议转换」后，这类上游才以 responses → chat 翻译方式参与
- **故障转移**：上游返回任意 4xx/5xx 或网络错误时换下一个候选，全部失败返回 502 并附最后一个上游的错误
- **可用性过滤**：静音时段、每日 token 上限、并行上限（`max_concurrent`，满了不排队而是换候选，全满返回 404）都会剔除候选；静音按服务器本地时间每天重复，可跨零点
- **429 自动静音**：上游 429 时该模型条目静音 5 小时（上游给出恢复时刻则静音到该时刻），期间含会话绑定一并跳过，可在后台手动解除
- **会话粘性**：带 `X-Session-ID` 的请求首次选路后绑定该模型，后续直连（不再套用优先级 / 静音 / 上限），绑定失效才重选；未带该头时按客户端 IP 算 UUID v3 作为会话 ID（同一 IP 固定不变）。默认 1 小时无活动过期（设置页可调，最多 30 天），仅存内存
- **Provider 基址**：Chat 与 Responses 可分别配置（留空跟随 Chat）；每个 Provider 可单独配 `http://`、`https://`、`socks5(h)://` 网络代理，留空直连
- **上下文声明**：模型可填上下文窗口（tokens），`/v1/models` 对每条条目同时输出 `context_length`（OpenRouter 口径）与 `max_model_len`（vLLM 口径）；路由组默认取成员最小值，也可在「组设置」里手填覆盖（0 = 自动），组和成员都未声明则不输出该字段（客户端回退到自身默认值）。只用于向客户端声明，不参与路由，也不改变请求体透传
- **用量记账**：自动统计输入 / 输出 token 与缓存命中（用量页展示缓存量与命中率）
- **会话统计**：按「天 × 会话 × Key × Provider × 模型」保留 35 天，记录请求数、token、输出速度、首字节耗时与思考等级（客户端声明值 + 实际出站值）
- Provider 的 API Key 为空时，客户端的 Authorization 头原样转发（自带 Key 模式）

## 上游适配

三类私有协议上游与标准 Provider 混挂，共用全部路由规则。基址固定为官方端点、不可自定义
（凭证随请求发出，避免发往其他主机）；需要代理时在 Provider 的「网络代理」栏配置。

| | WorkBuddy / CodeBuddy | Zed | Command Code |
|---|---|---|---|
| 凭证入口 | 扫描本机客户端 / 粘贴凭证 JSON | 浏览器原生登录 / 粘贴回调 URL | 读取本机 CLI 登录 / 粘贴 API Key |
| responses 承接 | 需开全局「Responses 协议转换」 | 直接承接 | 直接承接 |
| 额外操作 | 每日签到 / 余额快照 | 套餐用量刷新 | 套餐与模型目录同步 |

- **WorkBuddy**：复用官方客户端登录态（glm、deepseek、minimax、kimi、hunyuan 等），凭证自动续期；非流式请求由网关聚合返回
- **Zed**：复用 Zed 账号额度（`gpt-*`、`claude-*`），登录一次长期有效、凭证自动续期；`system_id` 依次取弹窗、`LLMROUTER_ZED_SYSTEM_ID`、本机安装数据库
- **Command Code**：复用订阅额度（Go / Pro / GOAT / Max / Ultra）；`max_tokens` 上限 200000；可选零数据保留

## 后台与小工具

- **模型测试**：GET 上游 `/models` 做连通性探测，不发对话、不消耗 token
- **余额查询**：Provider 可填余额接口 URL，后台展示原始 JSON 并高亮余额类字段
- **异常日志**：记录会话建立（含选择理由与候选顺序）与各类失败，保留最近 500 条
- **管理令牌防爆破**：同一来源连续输错 5 次锁定 15 分钟，计数仅存内存
- **自动升级**：后台「设置 → 常规」检测 GitHub 最新 Release，下载当前平台资产、校验 SHA256 后替换二进制并按原参数重启；要求程序目录可写。macOS 不在发布矩阵中，只能手动编译
