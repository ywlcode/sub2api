# DeepSeek 火山引擎兼容

仅 `platform=deepseek`、`type=apikey` 且
`credentials.deepseek_volcengine_compatibility=true` 的账号启用下述处理。
其他平台、其他账号类型及关闭开关的账号保持原有行为；不根据模型别名自动启用。

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

- Responses：官方 DeepSeek 明文思考使用 `content[].reasoning_text`，火山使用
  `summary[].summary_text`。只翻译已有明文；不把密文或缺失思考伪装成可恢复历史。
- Responses：原生嵌套 `reasoning.effort` 优先于旧的扁平别名；显式关闭思考优先。
  `off/disabled` 别名归一为 `none`。Chat 保留扁平 `reasoning_effort`。
- Messages：指定 `output_config.effort` 时使用 `thinking.type=adaptive`；显式
  disabled/none 优先。不生成预算，不把 high/max 相互映射，不删除原生签名。
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
`conversation`、ID-only 引用或 compaction item。思考历史必须含可恢复明文，工具调用
和结果完整配对；开启思考时不能要求 `reasoning.summary:none` 后丢弃必要历史。
客户端可隐藏显示，但仍需保存历史。Chat/Messages 采用对应原生协议的历史要求。

SSE 分帧保护保留：支持 LF/CRLF/CR、多行 data、BOM、心跳和 UTF-8 分块，
不把 EOF 时未结束的半帧当作完整事件。Responses 保留终止状态校验；原生 Messages
保留上游 error，缺少终止事件时输出明确 error，不能伪造 message_stop 成功。
不再缓存完整 Messages 思考块等待转换，原生事件按帧传回。

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

文档核对与真实上游验证日期：2026-11-11；模型：`deepseek-v4-1-flash-261017`。
验证覆盖三协议的推理开关、流式/非流式工具续接、custom、namespace、tool_search
代理及普通 Responses 文本压缩。不同模型的功能以对应模型说明为准。

- [火山 DeepSeek 模型说明](https://docs.volcengine.com/docs/ark/deepseek-v4-1-flash?lang=zh)
- [火山 Agent 调用规范](https://docs.volcengine.com/docs/ark/agent-model-invocation-practice?lang=zh)
- [火山 Messages](https://docs.volcengine.com/docs/ark/messages-api?lang=zh)
- [DeepSeek Responses](https://api-docs.deepseek.com/guides/responses_api/)
- [DeepSeek Anthropic](https://api-docs.deepseek.com/guides/anthropic_api/)
