# 🚦 llmrouter

一个用 Go 写的大模型代理路由：统一入口 + 网页后台管理。标准上游请求体与响应一律透传，不改动任何内容；WorkBuddy 与 Zed 类型上游由内置适配器按其私有协议转发。

## 特性

- **统一入口**：`/v1/chat/completions`、`/v1/responses`、`/v1/models`
- **多 Provider 管理**：同一模型名可挂多个 Provider，按优先级自动故障转移（网络错误 / 401 / 403 / 408 / 429 / 5xx 时切换下一个）
- **多路由组**：可新建多个路由组，每组对外是一个独立的虚拟模型名。模型配置（Provider、模型名、每日上限、并行上限、静音时段等）全局共享，同一个模型可加入多个组，组间只有**组内优先级**不同；默认组保留原有的 `auto` 语义
- **字节级透传**：请求体只读取 `model` 字段用于路由，其余不解析、不改写、不注入；流式（SSE）响应边收边发，非流式响应原样回传
- **Responses 协议转换（默认关闭）**：`/v1/responses` 只路由到原生支持 Responses 的模型；仅支持 chat 的上游（WorkBuddy）默认被跳过并自动改用其他模型。设置页开启转换后，这类上游才以「responses → chat」翻译方式承接请求
- **WorkBuddy/CodeBuddy 适配**：国内版（copilot.tencent.com，历史 workbuddy.cn 域名仍可服务）/ 国际版（codebuddy.ai，历史 workbuddy.ai 同站）两种内置 Provider 类型，复用官方客户端登录态调用其模型额度（详见下文）
- **Zed 适配**：复用 Zed 编辑器账号的模型额度（`gpt-*`、`claude-*`），原生登录 + JWT 自动重铸 + 双向协议转换，chat 与 responses 两种接入协议直接承接（详见下文）
- **第三方供应商接入框架**：WorkBuddy 与 Zed 共用同一套适配器骨架（Kind + 凭证 + `xxAttempt` 分流），新增私有协议上游按固定清单接入（详见下文）
- **网页后台**（中文，单文件，无前端依赖）：路由组 / Provider / 模型增删改、优先级排序、用量统计（按 token 计）、每日 token 上限、并行上限、按时段静音（mute）、上游余额查询、会话与异常日志
- **深浅色主题**：跟随系统 `prefers-color-scheme` 自动切换，无需手动设置
- **在线升级**：后台比对 GitHub Release 最新版本，一键下载当前平台安装包、校验 SHA256、替换二进制并自动重启（见下文「版本检测与自动升级」）
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
| `-trust-proxy` | `LLMROUTER_TRUST_PROXY` | 关 | 部署在反向代理之后时开启：按 `X-Forwarded-For` 判定来源 IP。默认只取 TCP 对端，避免客户端伪造头绕过失败计数 |
| `-version` | — | — | 打印当前版本号后退出（发布版为 tag，如 `v0.18.0`；源码构建为 `dev`） |

## 版本检测与自动升级

发布流水线用 `-X main.appVersion=vX.Y.Z` 把 tag 注入二进制；后台「设置 → 常规 → 版本与更新」据此比对 GitHub 最新 Release：

- **检测更新**：读取 [GitHub Release](https://github.com/beamof/llmrouter/releases) 的最新版本与发布说明。未认证的 GitHub API 有 60 次/小时限制，检测结果缓存 5 分钟，点按钮可强制刷新
- **立即升级**：下载与当前平台匹配的发布资产（`llmrouter-{windows,linux}-{x86_64,arm64}`），用 Release 附带的 `SHA256SUMS.txt` 校验 SHA256，通过后替换当前可执行文件并按**原命令行参数**重启
- 替换由复制到临时目录的「升级助手」完成：等旧进程退出 → 旧文件改名为 `<exe>.old`（下次启动自动清理）→ 放入新文件 → 拉起新进程。Windows 下运行中的 exe 无法直接覆盖或改名，因此必须由独立副本执行
- 要求程序所在目录可写；源码构建（`dev`）也可安装发布版。升级失败时会回滚旧二进制，结果写入异常日志（kind = `update`）
- macOS 不在发布矩阵中，该平台只能手动编译运行，不支持自动升级

> 升级会重启服务，进行中的请求会中断，建议避开高负载时段。检测为手动触发，不做后台轮询。

## 管理令牌防爆破

服务端启用 `-admin-token` 后，后台接口按**来源 IP** 统计令牌失败次数：

- 连续输错 **5 次**即锁定该来源 **15 分钟**
- 锁定期间**不再校验令牌**，直接返回 `尝试次数过多，请 N 秒后再试`（即使提交正确令牌也一样）
- 锁定到期自动解除并清空计数；期间只要有一次校验通过，失败计数立即清零
- 前几次失败会提示剩余次数（如「再错 2 次将锁定」）
- 计数仅存内存，重启即清；未配置管理令牌时（后台开放）完全不计数

## 路由规则

- 请求体里的 `model` 字段用于路由（唯一的读取项），其余内容一概不碰
- **路由组**：每个路由组对外是一个虚拟模型名（默认组默认叫 `auto`），请求该名字时只在**本组成员**里按组内优先级选路。模型配置全局共享，同一个模型可加入多个路由组，各组只维护自己的优先级；修改每日上限、并行上限、静音时段等共享参数会同时影响所有引用它的组。后台「设置 → 模型路由」页可新建、重命名、删除路由组，也可把已存在的模型加入当前组；删除组只移除成员关系，仅不再属于任何组的模型配置才会被删除，且至少保留一个组。直接指定某个模型名时仍按该名字的全部模型配置路由（不受组限制）。`/v1/models` 始终列出全部路由组名
- Provider 有两个基址：`Chat 基址`（chat completions）与 `Responses 基址`（responses 协议，留空则跟随 Chat 基址）。两种协议基址不同的厂商（如智谱 Coding Plan 没有 `/responses`，可单独填 `https://open.bigmodel.cn/api/v1`）一个 Provider 即可搞定
- **每个 Provider 可单独配置网络代理**（`proxy` 字段）：支持 `http://`、`https://`、`socks5://`、`socks5h://`（socks5 由标准库实现，域名交由代理解析；裸写 `127.0.0.1:7890` 视为 http 代理，支持 `user:pass@host:port` 认证）。该 Provider 的所有出站请求——对话转发、模型列表、余额查询、WorkBuddy 续期/签到/目录——都走同一代理；留空 = 直连（仍读 `HTTP(S)_PROXY` 环境变量）。代理地址非法时请求直接失败，**不会绕过代理直连**
- 候选 = 命中路由组时该组内的模型条目，或直接指定模型名时该名字的所有条目；再剔除「当前时间处于静音时段」「今日 token 用量已达上限」「并行请求数已达上限」的，按优先级从大到小依次尝试
- **协议匹配**：`/v1/responses` 只选择能处理 Responses 的模型——标准 Provider 两种协议都透传；WorkBuddy 上游仅支持 chat，默认跳过。若请求的模型名只挂在这类仅 chat 的模型上，会自动改用其他支持 Responses 的可用模型（不跨协议硬发）。路由组请求只在组内兜底，不会串到其他组。在设置页开启 **Responses 协议转换** 后，WorkBuddy 才以「responses → chat」翻译方式参与 responses 路由
- 上游返回**任何 4xx/5xx**（含 400/404/429 等）或网络错误时自动换下一个候选；全部失败时返回 502，并附上最后一个上游的状态码与错误详情。其中 **429 额外触发该模型静音 5 小时**（见下）
- **会话粘性**：客户端请求带 `X-Session-ID: <任意会话标识>` 头时，该会话首次请求按上述规则选模型并绑定；后续请求直连绑定的模型（不再套用优先级/静音/每日上限规则），仅当绑定上游失败才按规则重选并重新绑定。绑定按「会话 × 接入协议 × 请求模型名」隔离（同一会话混用 chat 与 responses 时各自绑定），默认 **1 小时**无活动过期（设置页「会话有效期」可调，保存后立即按最近活跃时间重算现有绑定；填 0 恢复默认，上限 30 天），仅存内存（重启即清）。**用量页的「活跃会话」面板实时列出当前所有绑定**（会话 ID、Key、请求→绑定模型、Provider、请求次数、输出速度、首字节耗时、最近活跃与剩余有效期，每 30 秒自动刷新），可点每行的「结束」主动解绑，也可经 API 操作：

   ```bash
   curl http://127.0.0.1:8080/api/sessions            # 列出活跃会话
   curl -X DELETE 'http://127.0.0.1:8080/api/sessions?id=conv-123'  # 结束该会话的所有绑定
   ```

   ```bash
   curl http://127.0.0.1:8080/v1/chat/completions \
     -H 'Content-Type: application/json' \
     -H 'X-Session-ID: conv-123' \
     -d '{"model":"gpt-4o","messages":[...]}'
   ```
- **429 自动静音**：上游返回 `429 Too Many Requests` 时，该模型条目自动静音 **5 小时**，期间所有请求（含会话绑定）都跳过它、改走后续候选；同一模型再次 429 不会缩短已有静音，只会延长。到期自动恢复；后台「模型路由」状态列显示「限流静音」与到期时间，可点「解除」手动恢复。静音状态写入 `data.json`，重启不丢
- 静音时段按服务器本地时间，每天重复，支持跨零点（如 22:00~06:00）
- **并行上限**：每个模型条目可设「并行上限」（`max_concurrent`，0 = 不限）。某条路由的在途请求数达到上限后，新请求**不再排队**，而是自动落到该模型名的下一个候选；全部候选都满时返回 404。计数按模型条目独立，同一模型名挂在多个 Provider 上互不影响；仅存内存，重启归零。后台模型列表的「并行」列实时显示 `在途 / 上限`
- **模型列表暴露**：`/v1/models` 始终返回全部路由组名（至少含默认组 `auto`）。模型路由页每条路由的「暴露」开关可单独控制其模型名是否出现在列表中（同名路由任一开启即出现）；关闭只影响列表展示，直接指定 `model` 名或走所在路由组仍可正常调用
- 非流式响应会顺带读取 `usage` 字段记账 token（兼容 `prompt_tokens/completion_tokens` 与 `input_tokens/output_tokens`）；流式响应透传时保留 8KB 滚动尾部副本（不改任何字节），从末尾 `usage` 事件提取 token，同样计入统计与每日上限
- 缓存命中单独统计：优先读取 `prompt_tokens_details.cached_tokens` / `input_tokens_details.cached_tokens`，缺失时回落 `prompt_cache_hit_tokens`（DeepSeek）、`cache_read_input_tokens`、顶层 `cached_tokens`（含在输入 token 内）；流式按 SSE 事件解析 usage，不会把正文里的 `"usage"` 文本误当用量，用量页展示缓存量与命中率
- **会话用量**：带 `X-Session-ID` 的请求同时按「天 × 会话 × Key × Provider × 模型」落盘聚合（请求数、输入/缓存/输出 token、生成耗时、首字节耗时），保留 35 天；会话过期、主动结束或进程重启后仍可在用量页「用量明细 → 按会话」查看，活跃会话带绿点。输出速度 = 累计输出 tokens / 累计生成耗时，流式从首字节算（排除排队与预填充），非流式用端到端耗时；首字节耗时（TTFT）仅流式统计
- Provider 的 API Key 为空时，客户端的 Authorization 头会原样转发（自带 Key 模式）

## WorkBuddy 上游

Provider「类型」选 **WorkBuddy 国内版 / 国际版** 后，可复用腾讯 WorkBuddy / CodeBuddy 客户端的登录态调用其模型额度（glm、deepseek、minimax、kimi、hunyuan 等），与标准 Provider 混挂、共用全部路由规则（优先级 / 故障转移 / 会话粘性 / 每日上限 / 并行上限 / 用量记账）。

**凭证导入**（后台 Provider 弹窗，二选一）：

- **扫描本机凭证**：自动读取官方客户端的登录态快照（Windows：`%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\*.info`，macOS/Linux 对应 `CodeBuddyExtension` 数据目录；另兜底扫描 `~/.workbuddy`、`~/.codebuddy` 等 CLI 目录）。客户端 v5.6.2+ 的快照对 token 加密（AES-256-GCM 信封），需在「凭证密钥」栏提供 `atRestSecretKey` 才能解开
- **粘贴凭证 JSON**：粘贴 `{account:{uid…}, auth:{accessToken…, refreshToken…, expiresAt…}}` 形态的 JSON（可从其他工具导出）

导入后 llmrouter 持有 refreshToken **自动续期**（临期 5 分钟窗口懒刷新 + 凭证失效时强制续期各一次重试，refresh token 一次性轮换、单飞并发保护、JWT 身份核对），与客户端互不影响；同一账号重复导入会原地更新。凭证明文保存在 `data.json`（与 API Key 同级，文件权限 0600），请妥善保管该文件。复用官方客户端登录态是否被上游服务条款允许，请自行确认。

**行为差异**（与标准 Provider 相比）：

- 请求按上游要求做**出站归一化**：`developer` 角色折成 `system`、`tool_choice` 折成字符串、`image_url` 字符串包对象、`max_completion_tokens` 翻译、注入首条 system（官方 CLI 默认提示词）、注入前缀缓存键（`prompt_cache_key`）、强制 `stream_options.include_usage`（用量统计依赖最终帧）；推理档位（`reasoning_effort`）按模型目录（`GET /v3/config`，自动拉取缓存 6 小时，失败退回内置清单）解析注入，deepseek 系未声明思考时按官方 CLI 行为兜底 `high`
- **请求头对齐官方 CLI 指纹**（`@tencent-ai/codebuddy-code` 契约）：`X-CodeBuddy-Request`、`X-IDE-*`、`X-Conversation-*` 会话族与追踪族；`Origin`/`Referer`/`X-Domain` 跟随实际请求端点；chat 请求绝不携带 `X-Refresh-Token`
- 上游**只支持流式**：非流式客户端的请求由本网关强制流式后聚合成完整 `chat.completion` 返回（响应头改回 `application/json`，不透传上游 `Content-Length`）；流式透传时过滤上游插入的「整片只有换行」的节拍分片
- 上游 429 限流文案若带恢复时刻（如 `2026-09-11 19:43:46 UTC+8 重置`），自动静音到该时刻（解析不出退 5 小时）；`code 11128` 为上游对请求形态/参数的拒绝，换下一候选不静音
- `/v1/responses` 请求在设置页开启 **Responses 协议转换** 后，由网关翻译成 chat 协议出站，回程再渲染回 responses 事件流/对象；默认关闭时该模型不参与 responses 路由，请求会自动改走其他原生支持 Responses 的模型
- **签到与余额**（计费接口对齐官方）：国内版个人账号每天 **09:00 / 21:00（服务器本地时间）自动签到**并刷新余额快照，Provider 列表与弹窗也有手动「签到 / 查余额」按钮。上游把所有非成功签到都编成 HTTP 400 + code 10001，仅凭 `msg` 文案区分「今天已签到」（幂等成功）与真失败。余额：个人账号走资源包接口按周期口径聚合（缺总额不画进度），企业账号（`enterprise_id` 非空）走企业积分接口（`credit` 已用 / `limitNum` 总额，`-1` = 无限额度，保留小数），快照存进凭证随状态文件落盘；国际版不支持签到（不发起请求），企业账号不签个人到（「签到」变为刷新企业积分）
- 模型目录按官方 CLI 契约筛选（`/v3/config` 的 `data.agents` 中 `name=cli` 的清单为准，剔除 `disabled`、与非空 `availableModels` 取交集；不维护固定排除名单）；模型「测试」改用 `/v3/config` 目录探测；添加模型时的上游模型下拉来自目录
- 该类型的基址固定为由地区端点派生的官方地址，不能自定义（避免把凭证发往其他主机）

> WorkBuddy 适配移植自 [Agent2Providers](https://github.com/ProvidersPlugin/Agent2Providers)（AGPL-3.0）；本仓库中该部分为衍生作品，适用 AGPL-3.0。请求头族、模型目录算法与默认系统提示词另参照 [ccLoad](https://github.com/caidaoli/ccload) 的 `internal/codebuddyauth` 实现及其 [UPSTREAM.md](https://github.com/caidaoli/ccload/blob/main/internal/codebuddyauth/UPSTREAM.md) 协议说明（其对官方 CLI 契约的整理）。

## Zed 上游

Provider「类型」选 **Zed 账号** 后，可复用 Zed 编辑器账号自带的模型额度（`gpt-*`、`claude-*`），与标准 Provider 混挂、共用全部路由规则。

**登录**（后台 Provider 弹窗）：点击「登录 Zed」会在浏览器打开 zed.dev 的原生应用授权页，完成后自动创建/更新 Provider 并同步模型条目（优先级 0、默认暴露，可自行调整；重复登录原地更新，用户调整过的条目不动）。服务器上没有浏览器时，把授权后浏览器地址栏的完整回调 URL 粘贴进弹窗兜底完成。

- 登录不是 OAuth code/PKCE：每次登录生成临时 RSA-2048 密钥，公钥随授权 URL 交给 zed.dev，返回的原生凭据经 RSA-OAEP/SHA-256 加密回传到本机回环端口，私钥用完即弃不落盘
- **system_id**（可选）：Zed 试用额度绑定真实安装。取值链：弹窗表单 → 环境变量 `LLMROUTER_ZED_SYSTEM_ID` → 本机 Zed 安装数据库（`db/0-global/db.sqlite`，内置只读解析器，零依赖）。全部缺失时省略该头，由上游决定账号是否有试用权限；不要手填别机器的值
- 原生凭据长期有效，用它换取的**推理 JWT 短期有效**：临期 2 分钟懒重铸 + 请求被拒（401 / 非 plan 的 403）强制重铸重试一次，单飞并发保护；上游 403 且含 "plan" 表示套餐不含该模型，只把该模型静音 1 小时（渠道内其他模型继续可用）
- 请求按 Zed 线协议封装：`thread_id/prompt_id/intent/provider/model/provider_request` 信封 + 原生客户端请求头（`User-Agent: Zed/…`、`x-zed-version` 等）。`gpt-*` 走 Responses 线协议（`/v1/responses` 客户端请求保真透传归一化，chat 客户端先转换），`claude-*` 走 Anthropic Messages 线协议（chat 中间形态转换，`max_tokens` 默认 8192、thinking 预算与输出上限的约束对齐上游校验）
- 上游**恒为流式**（`{status,event}` 自有 SSE 帧）：非流式客户端由网关聚合；usage 从 `response.completed` / Anthropic `message_delta` 提取记账
- 两种接入协议（`/v1/chat/completions`、`/v1/responses`）都直接承接，不需要开 Responses 协议转换
- 数据面基址固定为官方 `https://cloud.zed.dev/completions`，不能自定义（避免把凭证发往其他主机）；需代理时在「网络代理」栏配置
- 套餐用量（`plan` / 已用请求数）随登录与「重铸 / 刷新账号」按钮更新，展示在 Provider 列表

> Zed 线协议为逆向实现，参照 [ccLoad](https://github.com/caidaoli/ccload)（MIT）的 `internal/zedauth` 与 `zed_wire.go`。上游协议变化时可能需要同步更新（如 `Zed/1.23.2` 版本号常量）。使用他人账号或违反 Zed 服务条款的风险请自行评估。

## 第三方供应商接入框架

WorkBuddy 与 Zed 两个适配器遵循同一套骨架：**Provider 挂 `Kind` 与凭证 → 代理候选循环分流到 `xxAttempt` → 适配器内完成鉴权、出站转换与回程翻译**。新增一类私有协议上游（官方客户端登录态、自有信封/线协议）时按下面的清单接入；标准 OpenAI 兼容上游不需要任何代码，直接加 Provider 即可。

### 总体约定

- **单包平铺、零外部依赖**：所有代码在 `package main`，新适配器做成 `xxx.go` 前缀的平铺文件（参照 `workbuddy*.go` / `zed*.go`），不建子包
- **内部中间形态是 chat/completions**：两种客户端协议（chat / responses）与任意上游线协议之间的转换都经过 chat 中间形态，转换器按「chat ↔ 上游线协议」成对编写，避免 N×M 组合爆炸。已有可复用资产：`wbResponsesToChat`（responses 请求 → chat）、`wbRespStream`（chat 分片 → responses 事件流）、`wbAgg`（chat 分片 → 聚合对象）、`zedconv.go` 的四个转换器（chat ↔ Responses 线协议、chat ↔ Anthropic 线协议）
- **会话粘性、优先级、故障转移、每日/并行上限、用量记账对适配器免费**：这些都作用在模型条目层，适配器只需按契约（见下）处理单个候选

### 接入清单（七步）

| # | 改动 | 位置 | 参照 |
|---|---|---|---|
| 1 | `Provider` 增加 `Kind` 值与凭证字段（如 `Zed *ZedCred`），过期时刻用 `*time.Time`（`omitempty` 对零值 time 不生效） | `store.go` | `WorkBuddyCred` / `ZedCred` |
| 2 | 凭证读写方法：`UpdateXxxAuth`（**换指针拷贝**回写，绝不原地改字段——`Candidate` 里的 Provider 是浅拷贝，共享指针会产生数据竞争）与 `ProviderByXxx`（按账号唯一键去重，重复导入/登录原地更新） | `store.go` | `UpdateWBAuth` / `ProviderByZedUser` |
| 3 | Kind 校验：`handleProviderUpsert` 的 switch 加分支；**基址固定为官方端点、不接受自定义**（凭证随请求发出，允许改基址等于允许把凭证发往任意主机）；校验凭证字段存在（编辑时勿丢） | `admin.go` | workbuddy / zed 分支 |
| 4 | `Server` 增加该类型的单飞锁表（`xxMu` + `xxLocks map[string]*sync.Mutex`，键 = provider ID）与登录会话表（浏览器授权流程才需要），`main.go` 注册 `/api/...` 路由与 admin handler；`handleProviderDelete` 里调用 `xxForgetProvider` 清锁 | `main.go` / `admin.go` / 适配器文件 | `zedMu` / `zedLogins` / `zedForgetProvider` |
| 5 | 凭证获取入口：登录（浏览器授权/回环回调/粘贴兜底）、扫描本机客户端、或粘贴导入；成功后 `UpsertProvider` + 同步模型条目（**只增不删**，不覆盖用户调整过的优先级/上限） | 适配器文件 | `zedlogin.go` / `workbuddyscan.go` |
| 6 | 代理候选循环加分流分支（紧跟 `wbOf` / `zedOf` 判定处）：命中即调用 `s.xxAttempt(...)`，签名对齐 `wbAttempt`；`candidateSupportsProtocol` 声明该类型能否承接 `/v1/responses` | `proxy.go` | zed 分支 |
| 7 | 适配器本体 `xxAttempt`：按下面的契约实现 | 适配器文件 | `workbuddy.go` / `zedwire.go` |

### `xxAttempt` 契约

```
func (s *Server) xxAttempt(w http.ResponseWriter, r *http.Request,
	c Candidate, sendBody []byte, keyID string,
	responsesMode bool, tm *requestTiming, sm *usageSample) error
```

1. **名额与日志**：入口 `defer s.store.Release(c.Model.ID)`（名额在 `TryAcquire` 已占）；失败路径用 `s.logFail(LogEntry{...})` 记日志后返回 `error`，候选循环会换下一候选并汇总失败原因——不要自己写响应给客户端（返回错误即可）
2. **凭证生命周期**：
   - 懒刷新：距过期不足窗口（WorkBuddy 5 分钟 / Zed 2 分钟）时刷新一次，**失败不阻断**，沿用旧 token 继续发请求
   - 强制刷新：请求被上游判定凭据失效（如 401）时强制刷新并**原候选重试一次**，用 `refreshed` 标志防循环；刷新也失败则报「请重新登录/导入」
   - 单飞：per-provider 互斥锁包住刷新，**等锁的请求拿到锁后先复查 store 里的凭证**（可能已被别人刷新），refresh token 一次性轮换的场景必须这样避免浪费旧值
   - 回写走第 2 步的换指针方法
3. **出站构造**：请求归一化 / 线协议转换 / 请求头对齐官方客户端指纹，全部不透传客户端原始头（私有协议上游按官方客户端契约发送）
4. **错误分类**（决定路由行为）：
   - 凭据级 → 强制刷新重试一次（见上）
   - 模型级（如 Zed 套餐不含该模型）→ `s.store.MuteModel(modelID, 时长, 原因)` 只静音该条目，渠道内其他模型继续可用
   - 上游限流 → `MuteOn429`（静音到恢复时刻或默认时长）
   - 其他 → 记日志返回 error，换下一候选
5. **回程语义**（对齐 `wbAttempt`）：
   - 上游恒为流式而客户端要流式：首字节经 `firstByteReader` 打点（TTFT），翻译后直写客户端（`flushWriter`）；**响应头惰性写**——首个事件前仍可换候选，已写字节后中断只能 `warn` 记日志并返回 `nil`（不能换候选）
   - 客户端要非流式：先聚合（`wbAgg` 或等价物）再写响应头与 body，**中途断流不提交 200**、返回 error 交给候选循环换下一家
   - usage 从上游终帧/事件提取，填进 `*sm = tm.sample(usage, 是否流式)`，记账与会话统计自动生效
6. **协议能力**：`candidateSupportsProtocol` 里声明——上游原生支持 responses 透传则恒真；只有 chat 且想承接 `/v1/responses` 则挂全局「Responses 协议转换」开关；像 Zed 那样适配层内双协议转换的恒真

### 测试模式

- 伪造上游：`httptest.NewServer` 模拟该供应商全部端点（鉴权、模型目录、对话 SSE），参照 `zedStartFakeCloud`；把可注入的端点基址做成 `Server` 字段（如 `zedBase`，默认官方地址）
- e2e 走完整链路：`newTestServer` / `postJSON`（`main_test.go`），覆盖登录/导入 → 建 Provider → 两种客户端协议 × 流式/非流式 → usage 落盘 → 401 强制刷新重试 → 模型级静音
- 转换器用表驱动单测（参照 `zedconv_test.go`）；需要真实文件格式的（如 SQLite fixture）放 `testdata/`

### 已接入类型速览

| | WorkBuddy / CodeBuddy | Zed |
|---|---|---|
| Kind | `workbuddy` / `workbuddy-intl` | `zed` |
| 凭证入口 | 本机扫描 / 粘贴 JSON | 浏览器原生登录（RSA）/ 粘贴回调 |
| 凭证结构 | refresh token 一次性轮换 | 原生凭据长期有效，JWT 重铸 |
| 上游线协议 | chat completions（仅流式） | gpt→Responses、claude→Anthropic（自有信封） |
| responses 承接 | 需开全局转换开关 | 适配层内直接承接 |
| 定额动作 | 签到 / 余额快照 | 套餐用量快照 |

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

## 异常日志

后台「日志」页记录会话建立与各类失败（成功请求的 token 用量走用量统计，不重复记录）：

**会话建立**（每个会话只记一次；会话中途换了模型才再记一条，复用绑定不重复记录）包含选中的模型、会话 ID、接入协议（Chat / Responses）、原始请求的 model 名、候选顺序，以及**选择理由**：

| 理由 | 场景 |
|---|---|
| 新会话，按优先级选中最高可用候选 | 首次请求 |
| 新会话，路由组「auto」：组内优先级最高 | 请求 `model` 填的是路由组名 |
| 会话内更换模型：原绑定上游本次不可用，按优先级切换到最高可用候选 | 绑定失效后重新选路 |

会话日志只在带 `X-Session-ID` 头时产生。若同一次请求内所有上游都失败，会话绑定会被清空，下次请求视为新会话，因此不会记为「会话内更换模型」。

若同优先级模型因并行已满而让位，理由里会额外说明。展开每条会话日志可看到完整候选顺序（带优先级）与被剔除条目及原因（静音 / 每日上限 / 并行已满 / Provider 不存在）。

**失败记录**包含：

| 类型 | 触发条件 |
|---|---|
| 上游失败 | 单个上游连接失败、超时，或返回非 429 的 4xx/5xx（此时会自动故障转移到下一个候选） |
| 全部失败 | 所有候选都失败，返回 502 |
| 无可用上游 | 模型未配置、处于静音时段、已达每日上限或并行已满 |
| 请求体 | 请求体不是合法 JSON，或缺少 `model` 字段 |
| API Key | 客户端 API Key 无效 |
| 后台令牌 | 管理令牌错误，或锁定期间的访问尝试 |
| 限流静音 | 上游返回 429，该模型被自动静音 5 小时 |

每条记录包含时间、类型、说明、模型 / Provider / Key、HTTP 状态码，以及可展开的上游错误原文。
最多保留最近 500 条，超出时丢弃最旧的；可点「清空日志」一次清空。侧边栏「日志」右侧的红色角标显示最近 1 小时的条数。

## 测试

```bash
go test ./...
```
