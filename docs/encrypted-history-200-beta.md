# 200 流式加密历史有损恢复（Beta）

入口：系统设置 → 网关转发行为 → **200 流式加密历史有损恢复（Beta）**。
默认关闭，管理 API 字段为 `openai_200_omit_encrypted_history_enabled`。独立于 400、409 开关；旧客户端省略字段时保留已保存值，设置读取失败按关闭处理。使用现有 settings，无需数据库迁移。作为可选策略字段同步，旧从站可以忽略。

## 触发与恢复

仅适用于实际账号平台为 `openai` 的原生 HTTP Responses 流；混合分组按实际选中账号的平台判断，不按模型名或请求格式判断。其他平台不读取本 Beta 开关、不执行本 Beta 的历史省略或重试。

上游 HTTP 状态为 200，在尚未交付输出时发送 `response.failed` 或 `error`，明确拒绝回放的加密历史。HTTP 200 只表示传输成功，不能作为流成功的依据。

识别范围：现有 `invalid_encrypted_content`、有明确解密诊断的 `thinking_signature_invalid`，以及结构化错误 message 中完整的 `Encrypted function output content could not be decrypted or decoded.` 句子。后者参考 [CC Switch v4.0.32 密文处理](https://github.com/farion1231/cc-switch/blob/v4.0.32/src-tauri/src/proxy/opaque_state_rectifier.rs)；仅泛称 encrypted、脱敏文案、普通 server_error、正文/metadata 中出现相同文字，不构成恢复依据。拒绝有歧义或过大的错误载荷。

1. 沿用现有首输出暂存和精确 reasoning 密文恢复。精确恢复不能处理或已用完一次机会时，才检查 200 Beta。
2. 清理当前可读任务之前符合条件的旧密文；保留支持的 reasoning summary/content、可读消息、工具调用参数、结果、call_id、顺序及 JSON 数字精度。旧密文被明确的省略提示替代，不能还原丢失内容。
3. 关闭失败尝试的响应体、丢弃其暂存事件；同一客户端请求内，对同账号、模型、凭据及路线追加一次恢复请求。成功流使用新尝试的 response ID 和响应头。
4. 400 与 200 Beta **共用一次有损恢复预算**，且有损恢复不能晚于第 3 次原生 HTTP 发送。最多是原请求、一次精确恢复、一次有损恢复。HTTP 与 SSE 相互切换不能刷新预算。

参考 CC Switch 的“输出前截获、清理后同账号重试”流程，不将 SSE 错误伪装成 HTTP 400，也不复制其对压缩条目、任意 400/422 或其他平台的更宽处理。

## 边界

- 已向客户端交付任何本次响应（包括已提交的早期插件事件/心跳）、正文、推理或工具调用时，不透明重放。实际边界比“只发送 HTTP 200 头理论上仍可继续”更保守。
- 保留现有权限准入、输入完整性、账号归属、交付屏障及共享存储检查。已保留加密输出、未确认交付、归属冲突、存储错误、请求取消时拒绝恢复。
- 当前加密任务、当前轮密文工具结果、没有可读当前任务、未知内容结构、`previous_response_id`/`conversation`、item_reference、compaction、压缩端点及 WebSocket 不适用。其他平台、桥接和原样透传不进入本入口。
- 不因普通 200、网络中断或超时删除历史；恢复尝试再失败时按真实终态结束，不借用额外有损恢复次数或借此切换账号。
- 正常流收到有效输出就继续现有 flush 流程，不增加固定等待，也不查询 200 开关。仅在识别到拒绝时读取设置和清理请求历史；复用现有有界暂存、首输出超时及资源清理。
- 不修改客户端本地历史，不清空 owner 缓存，不把被省略的输入重新登记为成功消费。客户端后续继续带旧密文时，可能再次触发一次恢复，并增加延迟和上游消耗。

## 诊断

沿用请求内上游事件，恢复尝试记录 `kind=retry`、`stage=native_sse`、`reason=encrypted_history_200_omitted`，`upstream_status_code` 保持真实的 **200**。可选 `encrypted_history_200` 记录 `applied`、`after_409`、固定枚举 `decision`，含义与 400 诊断一致；`applied` 表示准备了替换，不保证后续准入及发送成功。诊断不包含密文、正文、凭据，也不混入客户端响应。

重试成功仍是成功请求，中间拒绝不被当作最终失败；重试失败不能因为 HTTP 200 被记为成功。测试需要检查 SSE 终态，而不是只断言 HTTP 状态。

## 本地验证

新增回归首先在旧实现复现“多条密文 + HTTP 200 response.failed 只调用一次并失败”，再验证修复。

服务测试覆盖 API Key/OAuth、owner 绑定开关、409 后续接、多个密文、加密工具结果、SSE 命名/多行/裸错误、精确→有损、HTTP/SSE 共享预算、已经输出/取消/交付/路线/输入保护、其他平台隔离和正常首内容立即 flush。管理接口验证独立开关、默认关闭、缺失字段保留；前端验证加载保存、与 400/409 独立、文案与类型。

真实 handler 测试使用 localhost HTTP 假供应商、独立 Redis Unix socket 和独立网关实例，覆盖先学习历史归属、200 拒绝、同账号恢复、旧记录不变，以及重试再遇 SSE 拒绝、HTTP 400/503、断连和已经输出。不会调用生产上游，不代表所有真实供应商兼容性已验证。

```sh
go test -race -tags=unit ./internal/service ./internal/handler ./internal/handler/admin \
  -run '^Test(OpenAI200|OpenAI400|OpenAI409|ForwardEncryptedContentSSERetry|EncryptedSSERetry|UpdateSettingsOpenAI200|UpdateSettingsOpenAI400|UpdateSettingsOpenAI409)' \
  -count=1 -timeout=5m
```

真实 handler 用例需要 `redis-server` 在 PATH 中，必须核对没有被跳过。不需要创建 Docker 网络。
