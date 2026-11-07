# 国内轻量中转

`dev-ywlcode-relay` 基于 `dev-ywlcode`，独立入口为 `backend/cmd/relay`，镜像使用 `Dockerfile.relay`。完整网关入口与后台仍保留，方便后续 rebase；中转进程不初始化它们。

适用链路：`boscserver / ecosserver → gzip 请求体 → 国内 relay → 火山方舟`。

- 固定 Bearer Key，仅启动时读取配置，不查询数据库。
- 支持 `/v1/responses`、`/responses`、`/v1/chat/completions`、`/chat/completions`；转发至配置的上游基地址。
- `/v1/models`、`/models` 直接转发上游，不构建或合并模型目录。
- gzip 在国内解压后向方舟发送明文 JSON；方舟不支持的请求体 gzip 不会继续向上游传递。
- 与完整网关共用火山兼容规则：推理参数别名、历史推理字段和已知消息元数据转换；不重写响应文本或 SSE。
- 不做跨协议转换。当前美国网关已把请求转换成 Responses，继续使用原来的中转地址和 Key 即可。
- 不连接 PostgreSQL/Redis，不执行计费、额度、并发排队、内容审查、账号调度、重试或后台定时任务。上游错误原样返回。
- 保留流式即时回传、连接复用、请求取消、上传超时及解压后体积上限。

## 配置

| 环境变量 | 默认值 / 用途 |
|---|---|
| `RELAY_LISTEN` | `0.0.0.0:8080` |
| `RELAY_KEY_FILE` | 固定中转 Key 文件，必填；也可用 `RELAY_KEY` |
| `RELAY_UPSTREAM_KEY_FILE` | 方舟 Key 文件，必填；也可用 `RELAY_UPSTREAM_KEY` |
| `RELAY_UPSTREAM_URL` | `https://ark.cn-beijing.volces.com/api/v3` |
| `RELAY_VOLCENGINE_COMPATIBILITY` | `true` |
| `RELAY_MAX_BODY_BYTES` | `67108864`，压缩传输体和解压后请求各自最多 64 MiB |
| `RELAY_UPLOAD_TIMEOUT` | `3m`，只限制客户端上传，不限制模型生成时长 |
| `RELAY_RESPONSE_HEADER_TIMEOUT` | `5m`，请求写出后等待上游响应头的超时 |

密钥文件优先于同名环境变量。镜像运行 UID/GID 为 1000，挂载文件需可由该用户读取；不要将密钥写入仓库或镜像。

## 耗时统计

每个完成的转发请求输出一条 JSON 日志 `relay_request`，不保存提示词、响应正文或 Key。日志由容器日志驱动保存，可配置大小轮转。

`GET /relay/stats` 使用同一个中转 Bearer Key 鉴权，返回启动后的请求/错误/并发计数、最近 4096 次请求的上传、处理返回、总耗时均值及 P50/P95，以及最近 20 条请求明细。重启会清空内存统计，旧日志由 Docker 保留策略决定。

时间单位为毫秒：

- `user_upload_ms`：处理器收到请求后至请求体读取和 gzip 解压完成；不包括后续兼容转换。
- `compatibility_ms`：火山请求体兼容转换。
- `processing_ms`：上传完成至下游响应写完，包含兼容转换、上游连接/上传/处理及回传。
- `upstream_connection_ms`：获取可用连接的时间，复用连接时接近零；新连接时包含 DNS/TCP/TLS。
- `upstream_request_write_ms`：上游请求头写出至请求正文写完，不含模型处理时间。
- `upstream_first_byte_ms`：开始转发至收到上游首个响应字节，不等于模型首个文字 token。
- `total_ms`：整个处理器耗时；客户端断开和流中断会在 `error` 标记。

统计基于服务端观察，不表示客户端已收到所有数据。流内容不进行逐事件解析，避免再次引入大响应处理开销。

## 构建与部署

```sh
docker build -f Dockerfile.relay \
  --build-arg VERSION=2.19.8-relay \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg DATE="$(date -u +%FT%TZ)" \
  --label org.opencontainers.image.revision="$(git rev-parse HEAD)" \
  -t sub2api-relay:your-commit .
```

该镜像只用于用户指定的国内中转，不能用于替换 boscserver、ecosserver 的完整网关服务。发布前运行中转测试、原火山兼容回归及适用 CI，保留准确提交和镜像校验和。镜像可同步归档到两台美国服务器，但它们继续运行完整网关。

接管旧中转时，将原中转 API Key 和方舟 Key 在服务器本地写入只读文件；只替换应用容器，保留原数据库、Redis、数据目录及旧镜像用于回退。中转不执行数据库迁移。先在本机备用端口验证 gzip/非 gzip 与完整流式响应，再切换原端口。

健康检查：`/app/relay -healthcheck`；版本检查：`/app/relay -version`。

## 同步主分支

后续 rebase `dev-ywlcode` 时，保留独立入口、relay 包及 Dockerfile；火山兼容逻辑集中于 `internal/pkg/volcengine`，完整网关的原函数只保留账号开关和调用包装。运行两侧回归，避免出现两份不同的兼容实现。
