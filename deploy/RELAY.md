# 国内轻量中转

`dev-ywlcode-relay` 基于 `dev-ywlcode`，独立入口为 `backend/cmd/relay`，镜像使用 `Dockerfile.relay`。完整网关入口与后台仍保留，方便后续 rebase；中转进程不初始化它们。

适用链路：`boscserver / ecosserver 的完整 Sub2API（协议转换、火山兼容）→ gzip 请求体 → C1 / C2 relay（解压、透传）→ 火山方舟`。

- 固定 Bearer Key，仅启动时读取配置，不查询数据库。
- 支持 `/v1/responses`、`/responses`、`/v1/chat/completions`、`/chat/completions`；转发至配置的上游基地址。
- `/v1/models`、`/models` 直接转发上游，不构建或合并模型目录。
- gzip 在国内解压后向方舟发送明文 JSON；方舟不支持的请求体 gzip 不会继续向上游传递。
- 解压后的请求体逐字节原样转发，不校验或重新编码 JSON，不改推理参数、历史推理字段或消息元数据；响应正文和 SSE 原样回传。
- 协议转换、火山字段兼容与思考历史适配均由 boscserver / ecosserver 的完整 Sub2API 在压缩请求前完成。
- 不连接 PostgreSQL/Redis，不执行计费、额度、并发排队、内容审查、账号调度、重试或后台定时任务。上游错误原样返回。
- 保留流式即时回传、连接复用、请求取消、上传超时及解压后体积上限。

## 配置

| 环境变量 | 默认值 / 用途 |
|---|---|
| `RELAY_LISTEN` | `0.0.0.0:8080`，模型转发入口 |
| `RELAY_STATS_LISTEN` | `127.0.0.1:10800`，独立的本机统计页面入口 |
| `RELAY_KEY_FILE` | 固定中转 Key 文件，必填；也可用 `RELAY_KEY` |
| `RELAY_ADDITIONAL_KEYS_FILE` | 可选的其他固定中转 Key，每行一个；只在启动时读取，最多 63 个，用于保留不同调用方已有的 Key |
| `RELAY_UPSTREAM_KEY_FILE` | 方舟 Key 文件，必填；也可用 `RELAY_UPSTREAM_KEY` |
| `RELAY_UPSTREAM_URL` | `https://ark.cn-beijing.volces.com/api/v3` |
| `RELAY_MAX_BODY_BYTES` | `67108864`，压缩传输体和解压后请求各自最多 64 MiB |
| `RELAY_UPLOAD_TIMEOUT` | `3m`，只限制客户端上传，不限制模型生成时长 |
| `RELAY_RESPONSE_HEADER_TIMEOUT` | `5m`，请求写出后等待上游响应头的超时 |

密钥文件优先于同名环境变量。镜像运行 UID/GID 为 1000，挂载文件需可由该用户读取；不要将密钥写入仓库或镜像。

`RELAY_VOLCENGINE_COMPATIBILITY` 已移除；旧环境中即使保留该变量，也不会启用转换或影响启动。

## 耗时统计

在中转宿主机访问 `http://127.0.0.1:10800/relay`，页面每 5 秒刷新，仅展示以下三个平均耗时，单位毫秒：

| 指标 | 起点 | 终点 |
|---|---|---|
| 平均上传耗时 | T0：中转 HTTP 处理器开始接收请求 | T1：压缩（或普通）请求体接收完毕 |
| 平均本地处理耗时 | T1 | T2：gzip 解压、上游 HTTP 请求构造完成 |
| 平均火山首 token 耗时 | T2 | T3：中转收到火山首个非空模型增量 |

三个阶段互斥，逐请求满足 `(T1−T0) + (T2−T1) + (T3−T2) = T3−T0`。第三段包括获取连接、DNS/TCP/TLS、向火山上传请求及等待模型输出；终点不使用 HTTP 响应头、SSE 心跳、角色声明或空增量。首个非空文本、推理或工具参数增量均视为有效模型输出。T0 之前的 TLS 握手、请求头接收，以及 T3 之后的后续生成和流式回传不属于这三个阶段。

三个平均值始终使用同一窗口：最近 2000 个实际收到首 token 的模型请求，按首 token 到达顺序更新；不必等待整个响应结束。失败且未收到 token、非流式 JSON 响应和模型列表请求不纳入窗口，不以零耗时替代缺失 token。样本不足时使用实际样本数，没有样本时显示“暂无样本”。

固定窗口仅保留每个样本的三个 `int64` 纳秒耗时，共 **48,000 字节**，外加计数器和互斥锁。每个新样本减去最旧三项耗时、加上新三项耗时，更新和读取均为 O(1)。不保存请求 ID、提示词、响应正文、逐请求明细或分位数，也不输出逐请求成功日志。首 token 观察器只在首 token 前解析 SSE，最多暂存 64 KiB 的单个事件；遇到超大事件则停止该请求的计时观察，继续原样转发，不把后续 token 误报为首 token。重启后重新累计统计。

原转发入口的 `GET /relay/stats` 仍需中转 Bearer Key，返回同样的三个平均值、样本数及计数器，没有明细。统计页面独立监听本机端口，不需要在浏览器输入中转 Key。

2 核 2 GB 节点可直接使用上述固定窗口。为使上传和本地处理严格分段，gzip 请求体先完整接收，再解压和构造上游请求。普通请求直接复用接收的正文；gzip 请求直接使用解压后的正文，不构建 JSON 对象或生成转换后的副本。JSON 格式错误由上游判断，状态码与错误正文原样返回；中转仍负责拒绝非法压缩体、超限正文和上传超时。

活跃请求仍可能暂时持有压缩体和解压后的正文。`RELAY_MAX_BODY_BYTES` 是每请求的上限，不是并发总内存上限；48 KB 仅为统计窗口开销。

[JSON 转换基准](RELAY_JSON_BENCHMARK.md) 保留为兼容包的历史测量记录，当前 relay 不再执行这些转换。

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

部署此版本前，需先在 boscserver / ecosserver 的完整 Sub2API 上启用并验收相关账号的火山兼容，验证思考历史及请求字段处理，再切换 C1 / C2 中转版本。

接管旧中转时，将原中转 API Key 和方舟 Key 在服务器本地写入只读文件；若不同调用方使用不同 Key，先核对每个调用方与旧中转有效 Key 的对应关系，并把额外 Key 配入 `RELAY_ADDITIONAL_KEYS_FILE`。运行中不查询数据库，后续 Key 变更需要更新文件并重启。只替换应用容器，保留原数据库、Redis、数据目录及旧镜像用于回退。中转不执行数据库迁移。先在本机备用端口验证每个调用方的 Key、gzip/非 gzip 与完整流式响应，再切换原端口。

健康检查：`/app/relay -healthcheck`；版本检查：`/app/relay -version`。

直接运行或使用 Docker host 网络时，默认即可访问宿主机的 `127.0.0.1:10800/relay`。Docker bridge 网络下，需要将容器内统计入口配置为 `RELAY_STATS_LISTEN=0.0.0.0:10800`，并使用 `-p 127.0.0.1:10800:10800` 将它仅映射到宿主机回环地址。模型转发端口按原部署方式发布。统计页面与转发入口在同一进程内运行，不增加数据库或 Redis 服务。

## 同步主分支

后续 rebase `dev-ywlcode` 时，保留独立入口、relay 包及 Dockerfile。`internal/pkg/volcengine` 继续供完整网关使用；relay 入口不得依赖该兼容包，CI 会检查依赖隔离。运行完整网关兼容回归及中转透传测试，确保兼容逻辑始终在完整网关执行。
