# DeepSeek 火山引擎兼容

在账号管理中创建或编辑 **DeepSeek / API Key** 账号，可开启「火山引擎兼容」。
配置为 `credentials.deepseek_volcengine_compatibility: true`，默认关闭。
只有 DeepSeek 平台、API Key 类型且开关为布尔值 `true` 时启用下述处理。
其他平台、其他账号类型以及关闭开关的账号保持原有行为，不根据域名或模型别名启用。

## 协议和地址

开启后，用户只可使用 `/v1/responses` 和 `/v1/messages`，上游固定使用 Responses。
`/v1/chat/completions` 返回 HTTP 400，包括将 Responses 格式请求体发到 Chat URL 的情况。
`/v1/responses` 也不接受 Chat 的 `messages` 请求体。
账号原先的 `api_protocol` 配置和旧的 Chat 探测结果不会让请求回退到 Chat 或 Messages 上游；
账号连接测试同样固定使用 Responses。

上游 base URL 优先取 `api_base_urls.responses`，其次沿用 `base_url`。
应配置支持 Responses 的地址，例如 `https://ark.cn-beijing.volces.com/api/v3`，
对应出口为 `/api/v3/responses`。旧的 Anthropic 专用地址需要改为 Responses 地址。
自定义 relay 地址同样受账号开关控制。

`/responses/compact`、历史中的 `compaction` / `compaction_trigger` 和仅含 ID 的
`item_reference` 不受支持，返回明确错误；不会借助 Chat 回退伪装成压缩成功。

## 完整历史和思考

本路径使用无状态完整历史，不新增 Redis 思考缓存，也不读取旧的全局思考缓存恢复历史。
不接受 `store: true`、`previous_response_id` 或 `conversation`，避免悄悄删除状态引用导致丢失上下文。
Responses 和 Messages 均在协议转换前校验这些字段。Messages 入口保留完整历史，
不启用通用的历史裁剪或自动 `previous_response_id` 续传。

客户端必须保留并在下一轮回传完整思考：

- Responses 回传完整的 reasoning 项。优先采用 `content[].reasoning_text` 原文；
  没有密文时，可采用该火山 DeepSeek 方言的 `summary[].summary_text` 明文。
- Messages 回传 `content[].thinking`，思考与普通 text 块分开，保留原文空白。
- 有完整明文时移除异源签名/密文，再按火山的 `reasoning.summary` 形状发送。
- 仅有 reasoning ID、密文、带密文的真实摘要、签名空壳或 redacted thinking，
  无法恢复完整原文，返回 HTTP 400，要求重新发送完整历史。
- 普通 assistant 历史本身没有思考字段不会因此报错，也不生成占位思考。
- 工具调用和结果须完整配对，不伪造缺失结果；思考和正文不会拼成 `<thinking>` 标签。

开启思考时不支持 `reasoning.summary: "none"`；客户端可在界面隐藏思考，
但仍须在协议历史中保存和回传。显式关闭思考时可保留这个设置。

火山 DeepSeek 的明文思考通过 `reasoning.summary` 回传；官方 DeepSeek Responses
通过 `reasoning.content` 的 `reasoning_text` 回传。直连官方 DeepSeek 的账号应关闭火山选项，
继续使用其既有适配。
参考 [火山 Agent 调用规范](https://docs.volcengine.com/docs/ark/agent-model-invocation-practice?lang=zh)、
[DeepSeek Responses](https://api-docs.deepseek.com/guides/responses_api/) 和
[DeepSeek 思考模式](https://api-docs.deepseek.com/guides/thinking_mode/)。

## 参数、工具和响应

`off/disabled` 思考强度别名转成 `none`；显式关闭思考优先，原生嵌套
`reasoning.effort` 优先于旧的 `reasoning_effort`。Messages 的 `output_config.effort`
转换成 Responses 的 `reasoning.effort`，保留用户明确指定的合法档位和输出长度，
不套用通用转换器的最小输出长度。

删除历史消息上的 `internal_chat_message_metadata_passthrough` 和历史 `output_text`
上的 `logprobs`；明确的普通消息缺少 type 时补 `type: "message"`。
Responses Lite 的 `input[].additional_tools` 提升为顶层 tools，复用现有的 custom、
namespace 和 tool_search 适配与回程还原。工具参数、扩展字段以及正文中的字面标签不做全局清洗。

SSE 按空行分帧，支持 LF、CRLF、CR、多行 data、BOM、心跳和 UTF-8 分块，
丢弃缺少终止空行的半个事件。Messages 流式转换将每个思考项暂存到该项结束，
优先输出完整 raw 原文；没有 raw 时才采用 summary，避免先到的短摘要覆盖原文。
这会推迟思考文本的显示，正文继续流式返回。暂存只属于当前请求，发现 raw 后释放
summary，输出该项后释放原文，不新增 Redis 或跨请求缓存。
思考只在 item.done 或最终响应快照中出现时也会补回，不重复追加 delta/done 快照；
保留 `incomplete` 和输出上限状态，上游失败、取消或意外 EOF 不伪造成功完成，
终结后不再输出额外事件。

## 客户端文本压缩

Codex 的第三方模型压缩通过普通 `/responses` 请求生成交接摘要。当前仅匹配已实测的
完整 `CONTEXT CHECKPOINT COMPACTION` 指令：最后一条为 user 文本消息，且没有工具
定义。Messages 入口携带相同指令时，经过转换后采用相同处理；普通摘要请求不会因此
被改写。此处理仍只作用于开启火山兼容的 DeepSeek / API Key 账号。

该请求首次保留客户端的思考设置。Sub2API 等待并校验完整结果，必须正常完成且包含
非空正文 `output_text`，才输出正文摘要的 JSON 或完整 Responses SSE；不将 reasoning
冒充正文。正文只出现在 item.done / 最终快照时也能恢复，正文与快照矛盾、拒绝或工具
输出不视为有效摘要。压缩本身产生的思考不进入摘要响应或后续历史。

只有思考、空正文、输出上限截断、断流或暂时连接故障时，使用同一账号、完整原历史
关闭思考重做一次。两次调用合计最多五分钟，不额外切换账号或进入调度重试；认证、
参数、内容策略拒绝及取消不因此重复调用。显式的输出上限保持原值，未指定时使用
8192 tokens，重试也不擅自突破上限。两次已知的用量合并记录，未知用量不估算。

流式等待沿用 SSE 注释心跳；两次仍失败时返回 Responses `response.failed` 或
Anthropic `error`，不输出虚假的 completed / end_turn。客户端取消会终止等待并关闭
上游响应，不再提交第二次调用。单个 SSE 事件最多 4 MiB，正文最多 1 MiB，暂存只属于
当前请求，结束即释放，不新增 Redis 或跨请求缓存。

这是对已观察到的 Codex 文本压缩请求的保护，不代表支持 `/responses/compact`、原生
compaction item 或尚未核验的客户端压缩指令，也不能保证上游持续故障时仍能继续任务。

参考 [火山 DeepSeek 模型说明](https://docs.volcengine.com/docs/ark/deepseek-v4-1-flash?lang=zh) 和
[Anthropic 流式协议](https://platform.claude.com/docs/en/build-with-claude/streaming)。

## 部署职责

boscserver / ecosserver 上的完整 Sub2API 承担协议转换和火山适配，账号探针也经过相同的
最终适配，适配发生在可选的请求 gzip 压缩之前。C1 / C2 轻量 relay 仅解压、透传和计时，
不承担火山字段或响应内容改写。两侧代码分别发布。
