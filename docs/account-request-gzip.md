# 账号级请求体 gzip 压缩

在管理后台新建或编辑 API Key 账号时，可以开启「请求体 gzip 压缩」。默认关闭，配置存储在 `accounts.extra.request_gzip_enabled`，不需要数据库迁移。

仅对支持解压 gzip 请求体的上游开启，例如已验证兼容的中转服务。该设置压缩的是 sub2api 发给所选上游的请求体，不改变客户端到 sub2api 的上传，也不改变响应流。

## 发送行为

- 对 POST / PUT / PATCH 的 JSON HTTP 请求使用 gzip level 1；请求体至少为 1 KiB，且压缩后更小才使用 gzip。
- 发送压缩数据时设置 `Content-Encoding: gzip`，使用压缩后的 `Content-Length`，保留 `GetBody` 以支持原有重试。
- 小请求、压缩无收益的请求、已有内容编码的请求、WebSocket、multipart 文件上传、未知长度或不可重放的请求体保持原样。
- OAuth、Bedrock 和 Service Account 账号不启用此选项。
- 压缩期间响应请求取消；压缩错误不会开始网络发送。正常的请求超时和流式响应处理仍由原有转发逻辑负责。
- 不额外增加「压缩失败后改发未压缩请求」的重试。上游拒绝 gzip 时，按照原有账号重试和错误处理规则执行；需要关闭开关来恢复未压缩发送。

API 创建、更新、部分更新和批量更新账号时，可通过 `extra` 设置：

```json
{
  "extra": {
    "request_gzip_enabled": true
  }
}
```

设置为 `false` 即关闭；未配置或为 `null` 也视为关闭。请遵循相应管理 API 的其他字段及更新语义。

## 实现边界

压缩在账号选定之后、HTTP 传输之前执行，使用独立的 `upstream_request_gzip.go` 装饰器，经公共 `gatewayHTTPUpstream` 入口接入；账号测试请求也使用同一压缩逻辑。调度缓存保留开关，避免选出账号后丢失配置。

原始请求体不被改写，重试、切换账号、请求哈希和计费仍使用原有数据。压缩收益取决于实际内容，不能用重复文本的压缩率推算所有 20 MB 请求的带宽收益。

OpenAI 自动时区会在确定实际代理出口后再次改写请求；该路径读取保留的原始 JSON，完成改写后重新压缩，保证请求头和请求体中的时区一致。
