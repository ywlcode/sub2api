# DeepSeek 火山引擎兼容

在账号管理中创建或编辑 **DeepSeek / API Key** 账号，可开启「火山引擎兼容」。
默认关闭，适用于连接火山方舟或火山中转服务的账号。API 配置保存在
`credentials.deepseek_volcengine_compatibility`，值为布尔值 `true` 或 `false`。
其他平台、其他账号类型不启用该适配。

开关按账号生效，不根据模型别名或域名猜测供应商。原有上游地址、协议选择仍需配置正确。
例如当前 `deepseek-v4-1-flash-261017` 的 Responses 出口为
`https://ark.cn-beijing.volces.com/api/v3/responses`。

开启后处理已确认的请求兼容问题：

- 删除请求历史消息对象上的 `internal_chat_message_metadata_passthrough`。
- 对缺少 `type` 且能明确识别为普通消息的 Responses `input[]` 补上
  `type: "message"`；不把工具调用、工具结果或 reasoning 对象猜成普通消息。
- 删除 Responses 历史 `output_text` 内容块上的 `logprobs`。
- 将 `off/disabled` 思考强度别名转成合法的关闭思考配置，保留合法的 `none`。
  显式 `thinking.type=disabled` 优先，避免同时发送「关闭思考」和高 effort。
- Chat / Messages 历史转成 Responses 时，明文思考使用独立的
  `reasoning.summary` 项，避免将思考包装为 `<thinking>` 后拼入普通回答。
  工具调用及对应结果继续保留。

请求可从 Responses、Chat Completions、Anthropic Messages 三种入口进入，
以实际选定的上游协议决定最终字段位置：

| 实际上游接口 | 思考强度字段 | 关闭思考 |
| --- | --- | --- |
| `/responses` | `reasoning.effort` | `reasoning.effort: "none"` |
| `/chat/completions` | `reasoning_effort` | `reasoning_effort: "none"` |
| `/messages` | `output_config.effort` | `thinking.type: "disabled"`，移除冲突 effort |

因此，Chat 入站的 `"reasoning_effort": "high"` 在 Responses 出口会成为
`"reasoning": {"effort": "high"}`。两种字段同时出现时，沿用转换链的约定，
以非空的 `reasoning.effort` 为准。将 Responses 格式发送到 Chat URL 的兼容入口也保留
`reasoning.summary` 等原生字段。

网关保留用户的合法档位，不统一关闭思考，也不将 `max` 先改为 `xhigh`。
实际档位由火山对应模型解释；例如该 261017 型号的 `minimal` 映射为 `low`，
`medium/xhigh` 映射为 `high`，`max` 保持 `max`。
参考 [火山 DeepSeek-V4.1-Flash 模型说明](https://console.volcengine.com/ark/region%3Acn-beijing/docs/deepseek-v4-1-flash)
和 [三种接口的官方配置对照](https://docs.volcengine.com/docs/ark/agent-model-invocation-practice?lang=en)。

若经过两层 Sub2API，两层对应的 DeepSeek 账号都应开启。
旧逻辑可能在外层删除 `none`，内层无法恢复已删除的关闭意图。
适配同样用于账号测试请求，并在可选的请求 gzip 压缩之前执行。

火山与原生 DeepSeek 的历史思考格式不同。火山 DeepSeek 的明文思考通过
`reasoning.summary[].text` 回传；原生 DeepSeek Responses 读取
`reasoning.content[]` 中的 `reasoning_text`，不支持 `summary` 或加密思考。
切换上游时，已有明文会按目标格式转换，工具调用与结果的关联保持不变。
直连 DeepSeek 官方端点的账号无需开启火山选项即可获得明文思考保留和回传修复；
直连原生 DeepSeek 的账号应保持火山选项关闭。

协议转换同样保留非流式响应中的 `reasoning_text`，使 Chat 客户端收到
`reasoning_content`、Messages 客户端收到 `content[].thinking`，
供下一轮请求回传。有原始 reasoning content 时优先保留原文。
没有明文、只有不可解密的密文，或客户端已经删除思考历史时，字段转换无法恢复原文。
本次字段转换不会生成占位思考、删除工具历史或关闭思考来掩盖该缺口。
参考 [DeepSeek Responses 输入项说明](https://api-docs.deepseek.com/guides/responses_api/)
及 [思考内容回传要求](https://api-docs.deepseek.com/guides/thinking_mode/)。

该选项针对上述协议差异，不更改普通正文或工具参数中的字面标签，
也不改变响应完成判定。模型自身输出 `<thinking>`、`</summary>`、
达到输出上限或上游主动结束，需要结合对应响应事件另行判断。
它不替代模型权限、额度和速率限制配置，也不将任意不完整工具对象补成可用请求。
