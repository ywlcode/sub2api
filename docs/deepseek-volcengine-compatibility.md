# DeepSeek 火山引擎兼容

仅 `platform=deepseek`、`type=apikey` 且
`credentials.deepseek_volcengine_compatibility=true` 的账号启用下述处理。
其他平台、其他账号类型及关闭开关的账号保持原有行为；不根据模型别名自动启用。

## 本轮修复的范围（2026-11-18）

**不是全部 DeepSeek，也不是全部 Responses。** 下表均以这三个账号条件为前提；
即使模型名称包含 DeepSeek，或地址指向 Ark，只要未开启开关，也不启用新行为。

| 协议/路径 | 修改 | 保持的边界 |
| --- | --- | --- |
| 火山 DeepSeek Responses | 接受并保留原生 `summary + encrypted_content`、不透明密文历史；仅在缺少可用 summary 时转换已有 `content[].reasoning_text` | 普通 DeepSeek 官方明文处理、其他 Responses 供应商不变 |
| 火山 DeepSeek Responses | 工具结果中的原生图片保持在 `function_call_output.output` 中 | 其他供应商的媒体提升逻辑不变；已有图片字段别名规则仍适用 |
| 火山 DeepSeek Messages | 保留原生搜索调用/结果；仍删除本地模拟搜索块 | 不按模型名全局关闭搜索过滤 |
| 火山 DeepSeek Messages 流式 | 结合 stop_reason 判断终态、以实际内容计首字、覆盖累计用量快照 | 不改普通 DeepSeek/Kimi/GLM 等原生 Messages 行为 |
| 火山 DeepSeek 三协议 | 保留供应商工具用量诊断；SSE 分帧遵守配置上限 | 不自动开通搜索，不新增收费，不改其他平台计量 |

不改账号调度、跨供应商会话归属、OpenAI 200/400/409 Beta、主从鉴权及计费协议。
不增加思考内容缓存，不建立跨请求历史存储，不依赖主站在线恢复思考内容。

## 三协议原生转发

开启后使用 adaptive 路由，用户的三种协议分别转发到火山的同名原生端点。
旧账号即使保存了 `api_protocol=responses`，运行时也采用该策略；不修改账号调度。

| 用户入口 | 直连火山的基地址 | 实际上游路径 |
|---|---|---|
| `/v1/chat/completions` | `https://ark.cn-beijing.volces.com/api/v3` | `/api/v3/chat/completions` |
| `/v1/responses` | `https://ark.cn-beijing.volces.com/api/v3` | `/api/v3/responses` |
| `/v1/messages` | `https://ark.cn-beijing.volces.com/api/compatible` | `/api/compatible/v1/messages` |

分协议地址优先取 `api_base_urls.chat_completions/responses/anthropic`。
旧账号缺少 Chat/Responses 地址时沿用 Responses 基地址或 `base_url`。
直连北京 Ark 域名时可推导 Messages 地址；自定义 relay 必须显式配置其 Messages
基地址，缺失时明确报错，不能退回 DeepSeek 官方默认地址。
账号编辑界面显示三个地址，保存时同样不会用 DeepSeek 默认地址填补空项。

例如通过本机 ark-route-proxy 时，三个基地址均可设为 `http://127.0.0.1:7000`。
该地址须符合现有上游 URL 校验策略，代理和 C2 必须已经支持 Messages 路由。
连接测试按 adaptive 规则测试三种协议，并经过相同的最终兼容处理。

原有 Messages → Responses → Messages 专用桥、思考重建状态机和 Chat 禁用逻辑已移除。
Chat 保留 `developer` 角色、`reasoning_content` 和原生工具调用；Messages 保留
`thinking`、`signature`、`tool_use/tool_result`，不再转换成 Responses 历史。
原生协议已有的通用网关处理（模型映射、计费、请求头策略等）继续生效。

## 保留的字段差异适配

- Responses：官方 DeepSeek 明文思考使用 `content[].reasoning_text`；火山当前
  `deepseek-v4-1-flash-261017` 的完整明文思考放在 `summary` 数组中，条目类型为
  `summary_text`、文本字段为 `text`。`summary` 这个名字不代表一定是缩略摘要。
  火山部分 Seed 型号采用另一种“摘要＋完整思考密文”方式，不能按通用字段名称推断模型能力。
- 火山原生 reasoning 同时带 `summary` 与 `encrypted_content` 是合法形状。保留整个
  原生条目，包括 `id`、`status`、`content:null`、不透明密文及扩展字段。不能因为存在密文
  就判定明文不完整，也不能见到 summary 就删除密文。只有 ID、没有任何内容仍不能回放。
- 缺少可用 summary、但有明确 `content[].reasoning_text` 明文时才翻译为 summary；
  不改写已有原生 summary，不删除附带密文。纯密文保持不透明并交由火山校验，Sub2API
  不解密、不保证外部供应商密文能跨厂商使用，也不伪造恢复后的思考。
- Responses 的工具图片保留在原调用的结果里，不再另造 user 消息。`call_id` 及历史顺序
  保持；普通 DeepSeek 官方路径继续采用自己的媒体和明文适配。
- Responses：原生嵌套 `reasoning.effort` 优先于旧的扁平别名；显式关闭思考优先。
  `off/disabled` 别名归一为 `none`。Chat 保留扁平 `reasoning_effort`。
- Messages：指定 `output_config.effort` 时使用 `thinking.type=adaptive`；显式
  disabled/none 优先。不生成预算，不把 high/max 相互映射，不删除原生签名。
- Messages：保留火山原生 `server_tool_use`、`web_search_tool_result` 和结果内的
  `encrypted_content`。只有工具 ID 带本地模拟前缀 `srvtoolu_ws_` 的搜索块继续移除；
  移除后空消息保留现有占位处理。搜索权限由火山账号的服务开通情况决定。
- 删除历史消息的 `internal_chat_message_metadata_passthrough` 和 Responses
  历史 `output_text.logprobs`；普通 Responses 消息缺少 type 时补 `message`。
- Responses Lite 的 `additional_tools` 提升为顶层声明。原生 custom/apply_patch
  和 namespace 保持原样；不再全部降为 function。
- `tool_search` 实测虽然返回 HTTP 200，模型没有原生执行搜索，仍保留 function
  代理、发现工具声明提升和回程还原。这项适配保留同一请求里的原生 custom、namespace
  及其调用历史，不修改配对用的 `call_id`。

这些处理在完整 Sub2API 内完成，发生在请求 gzip 压缩之前。已符合要求的普通历史
走快速检查，不为每次请求新建跨请求思考缓存。

## 完整历史、SSE 与错误

Responses 继续采用无状态完整历史，不接受 `store:true`、`previous_response_id`、
`conversation`、ID-only 引用或 compaction item。这是本项目的路由/历史策略，
不代表火山平台没有服务端存储能力。思考条目须携带原生可回放内容（明文或不透明密文），
工具调用和结果完整配对；仍不开放 `reasoning.summary:none` 配合开启思考的用法，
火山公开说明中的 summary 取值为 auto/concise/detailed。
客户端可隐藏显示，但仍需保存历史。Chat/Messages 采用对应原生协议的历史要求。

SSE 分帧保护保留：支持 LF/CRLF/CR、多行 data、BOM、心跳和 UTF-8 分块，
不把 EOF 时未结束的半帧当作完整事件。Responses 保留终止状态校验；原生 Messages
保留上游 error，缺少终止事件时输出明确 error，不能伪造 message_stop 成功。
Messages 必须先收到完整 `message_delta` 中已识别的 `delta.stop_reason`，再收到完整
`message_stop`。`end_turn/stop_sequence` 表示本轮结束；`tool_use` 交回客户端执行工具；
`max_tokens` 表示完整收到但被长度截断，原样交给客户端判断，不能当作充分回答或自动重放。
只有 message_stop、缺少或未知 stop_reason 时，返回明确 SSE error，不向客户端发送成功
终止事件。已输出内容的断流不透明重试；已经完整结束的响应不因之后的连接关闭而误报失败。

Messages 首字时间取首个非空正文、思考或工具内容，不把 message_start、ping、空思考条目
或 signature 元数据计作首字。首个内容仍按事件边界立即 flush。用量按累计快照覆盖同名
字段（包括显式零），缺失字段保留前值，不把多次 usage 相加。异常仍携带已观察用量和错误，
不能仅凭 HTTP 200 判定流式成功。

普通 SSE 适配层和账号连接测试均遵守 `gateway.max_line_size`；包含多行 data 的整个事件
也受限，不能由内层 500 MiB 默认上限绕过较小配置。只按需增长单事件缓冲，不预分配上限。
文本压缩仍使用更小的事件上限，取配置上限与 4 MiB 的较小值。
不再缓存完整 Messages 思考块等待转换，原生事件按帧传回。

## 原生工具用量与收费边界

Chat/Responses 读取 `usage.tool_usage` 中的 `web_search`、`knowledge_search`、`mcp`、
`doubao_app`；Messages 单独读取 `usage.server_tool_use.web_search_requests`，
不把“搜索请求次数”与“成功/可计费搜索次数”混为一谈。累计快照覆盖，缺失为未知，不能补零。
客户端原始 usage 不剥离。内部转发结果保留这些独立计数，并以
`volcengine.tool_usage.observed` 结构化日志记录账号、协议、上游请求 ID 和计数；不记录历史或密文。

本轮仅补诊断证据，**不接入自动搜索收费，也不新增使用记录表字段或主从账单字段**。
诊断日志按节点现有日志保留策略保存，不等同于主站使用记录数据库中的持久账单明细。
原有 token 计费不变：不能填入 `WebSearchCalls`（会替代 token 计费），也不能套用 Grok 的
`SearchCount`（未配价格时仍有默认搜索价格）。完整原生工具收费/报表需要单独明确计费口径后实现。

已知边界：火山可能自然产生没有思考的工具回合，该历史切到 DeepSeek 官方后，官方
可能报缺少 `reasoning_text`。原生转发不会凭空恢复不存在的历史；本次不无条件插入
`ET_`、空格或伪造 reasoning，也未改跨账号调度。这是独立的跨供应商续接问题。

## 客户端文本压缩

保留已实测的 Codex 文本压缩保护：普通 `/responses` 请求，最后一条 user 文本为
完整 `CONTEXT CHECKPOINT COMPACTION` 指令且没有工具。普通摘要请求和原生 Messages
请求不命中该特殊处理；不支持 OpenAI 专用 `/responses/compact`。

首次保留客户端推理设置，等待完整结果；必须正常完成并包含非空 `output_text`
才返回正文摘要。reasoning 不冒充摘要，不写入压缩后的历史。
只有思考、空正文、输出上限截断、断流或暂时连接故障时，以同一账号、完整原历史、
关闭思考重试一次。认证、参数、内容策略拒绝和取消不进入该恢复重试。

两次合计最多五分钟，显式输出上限保持原值，未指定时使用 8192 tokens。
已知用量合并记录，未知用量不估算；等待时沿用 SSE 注释心跳，取消会终止等待。
仍失败时返回明确 `response.failed`，不伪造 completed。单个事件最多 4 MiB，
正文最多 1 MiB，暂存随请求结束释放，不新增 Redis 或跨请求缓存。
这项保护不能保证上游持续故障时任务仍可完成。

火山的 `context_management.edits` 属于指定模型的上下文编辑能力，不等于 OpenAI
`/responses/compact`，不能据此给所有火山 DeepSeek 模型开启原生压缩。

## 仍需按模型确认的能力

- 火山提供原生 `messages/count_tokens`；当前多协议账号仍走已有本地估算路径，
  不声称它等价于官方精确计数，也不在普通请求前增加远程计数调用。
- `X-Ark-Max-Wait-Timeout-Ms` 可经账号请求头覆写配置。启用更长队列等待前，需一起核对
  客户端、Sub2API、Go 中转/C2 和上游各跳超时；本轮不延长全局等待，也不无条件重试已执行请求。
- 工具权限、缓存、状态存储和上下文编辑以当前型号及账号权限为准。保留未知原生扩展字段
  不等于已经验证其调度、收费或客户端支持。
- DeepSeek 官方 Messages 新文档也列出了原生搜索块，但普通官方账号的能力判断与收费
  不在本次火山开关范围内，不能从这次修复推导为“全部 DeepSeek 已启用原生搜索”。

## 部署配套

完整 Sub2API 负责字段差异兼容；ark-route-proxy 只做线路选择和 HTTP 转发；
C2 relay 只做解压、路径/鉴权适配和透传，不解析改写模型 JSON。
新增 Messages 路由使用 `x-api-key` 入站鉴权；显式错误的 Bearer 不会被备用头绕过。
C2 将 Messages 路径转到 `/api/compatible/v1/messages`，使用上游 x-api-key；
Chat/Responses 仍走 `/api/v3` 和 Bearer。

发布顺序：先更新 C2 和 ark-route-proxy 的路由能力，再配置三个基地址并更新完整
Sub2API；逐台验证三协议、开关推理、工具续接、压缩和错误响应。旧 Messages 专用桥
已删除，不能先切新网关再等待中转支持该路径。

## 官方资料与验证范围

官方资料复核：2026-11-17 UTC / 北京时间 11 月 18 日；模型：`deepseek-v4-1-flash-261017`。
现有验证覆盖三协议的推理开关、流式/非流式工具续接、custom、namespace、tool_search
代理及普通 Responses 文本压缩；本轮真实上游证据另确认原生 summary＋密文历史往返、
Codex/OpenCode 第二轮历史和工具结果图片可被火山接受。不能将直接上游测试当作整站部署验收。
修复候选代码通过真实网关函数直连火山，已验证新生成思考→原生历史流式续接→Codex 文本
压缩三次调用均完整完成，并校验出站 reasoning 整块未改；另验证 Messages 开启思考的
流式请求收到 end_turn 和 message_stop、按实际内容记录首字。未切换任何生产节点。
Messages 原生搜索实测因测试凭据未开通服务返回 403 AccessDenied，未擅自开通；
其历史保留及用量快照有网关回归测试，**不宣称完成真实搜索往返**。

回归覆盖原生/官方/开启/关闭兼容开关、普通续接/工具续接/文本压缩、流式/非流式、
缺失/未知/正常 stop_reason、半帧/EOF/上游错误、首字计时、搜索快照不重复计数、
其他平台隔离和原 token 计费不变。运行专项：

```sh
cd backend
go test -tags=unit ./internal/service -run 'Test.*(Volcengine|DeepSeekReasoning|DeepSeek.*Normalization|FilterWebSearch)' -count=1
```

官方通用 schema、历史示例与型号能力表不一致时，优先核对正式接口字段、当前型号的
Agent 调用规范和真实请求证据；记录限制，不能靠字段名称、HTTP 200 或旧示例猜测。

- [火山 DeepSeek 模型说明](https://docs.volcengine.com/docs/ark/deepseek-v4-1-flash?lang=zh)
- [火山 Agent 调用规范](https://docs.volcengine.com/docs/ark/agent-model-invocation-practice?lang=zh)
- [火山 API 结构升级公告](https://docs.volcengine.com/docs/ark/api-schema-upgrade-notice?lang=zh)
- [火山 Responses](https://docs.volcengine.com/docs/ark/create-model-responses-api?lang=zh)
- [火山 Chat](https://docs.volcengine.com/docs/ark/chat-api?lang=zh)
- [火山 Messages](https://docs.volcengine.com/docs/ark/messages-api?lang=zh)
- [火山流式输出、终态和累计用量](https://docs.volcengine.com/docs/ark/streaming-output?lang=zh)
- [火山 Messages token 计数](https://docs.volcengine.com/docs/ark/messages-count-tokens-api?lang=zh)
- [火山上下文缓存](https://docs.volcengine.com/docs/ark/context-cache?lang=zh)
- [火山 Responses 上下文编辑](https://docs.volcengine.com/docs/ark/responses-api-context-editing?lang=zh)
- [火山突发流量和等待设置](https://docs.volcengine.com/docs/ark/traffic-burst-handling-best-practices?lang=zh)
- [DeepSeek Responses](https://api-docs.deepseek.com/guides/responses_api/)
- [DeepSeek Anthropic](https://api-docs.deepseek.com/guides/anthropic_api/)
- [DeepSeek 思考模式](https://api-docs.deepseek.com/zh-cn/guides/thinking_mode/)
