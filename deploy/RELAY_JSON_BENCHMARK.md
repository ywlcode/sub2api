# 火山 JSON 兼容转换基准

本文保留兼容包的历史测量结果。当前 C1 / C2 relay 已移除兼容转换，仅解压并原样转发；以下数据不代表当前中转的处理耗时。

2026-11-09，在 `dev-ywlcode-relay` 的本地修改上测量。两条路径消费完全相同的合成请求，不包含生产请求数据。

- `decoded`：优化前的完整解码、转换和重新编码路径，也保留为特殊 JSON 形状的兼容回退。
- `wire`：JSON 和 UTF-8 校验后，仅索引已知字段；原始正文、工具参数及未知子树不展开成 Go 对象。没有修改时直接复用输入字节，有修改时一次分配输出正文。
- 环境：AMD Ryzen 7 H 260 / Linux amd64，Go 1.27.3，Alpine 3.21 容器限制为 **2 CPU / 2 GiB**，`GOMAXPROCS=2`。
- 每项基准运行三次，每次 700 ms；表内分别取各指标的中位数。MB 按 1,000,000 字节计算。
- 仅测 JSON 兼容转换，不包含 gzip 解压、HTTP 网络、火山推理或统计页面。容器资源限制相同不代表生产服务器的处理器性能相同。

| 合成请求 | 请求大小 | 原耗时 | 新耗时 | 加速比 | 原 B/op | 新 B/op | 分配量减少 |
|---|---:|---:|---:|---:|---:|---:|---:|
| 小请求，参数别名及消息类型转换 | 109 B | 4.136 µs | 1.578 µs | 2.62× | 2,504 B | 1,064 B | 57.5% |
| 长正文，已符合上游格式 | 3.244 MB | 9.478 ms | 2.425 ms | 3.91× | 15.171 MB | 约 0 MB | 接近 100% |
| 长正文，补消息类型及删除已知元数据/logprobs | 3.244 MB | 9.582 ms | 2.852 ms | 3.36× | 15.154 MB | 3.253 MB | 78.5% |
| 长推理历史，content → summary | 3.244 MB | 9.372 ms | 2.852 ms | 3.29× | 15.247 MB | 3.253 MB | 78.7% |
| 1000 条混合消息、推理和工具历史 | 2.533 MB | 9.437 ms | 2.027 ms | 4.66× | 19.384 MB | 2.854 MB | 85.3% |

`B/op` 是每次请求累计分配的堆内存，不是进程 RSS、峰值或所有活跃请求的总内存。已兼容长正文的新路径为 0 allocs/op；约 1 B/op 的测量摊销不代表复制了正文。混合历史的对象分配次数从 18,340 降到 2,021 次。

原输入缓冲不被修改。输出 JSON 的键顺序、空白和字符串转义可能与原来的统一重新编码不同；兼容字段的语义、大整数、用户文本和工具参数保持一致。被检查的对象包含重复键、转义键或超过 64 个字段时，整个请求回退到原解码路径；非法 UTF-8 仍使用原解码器的替换行为。

复现基准：

```sh
cd backend
go test ./internal/pkg/volcengine -run '^$' \
  -bench BenchmarkNormalizeRequest -benchmem -benchtime=700ms -count=3 -cpu=2
```

本次受资源限制的运行方式：

```sh
cd backend
CGO_ENABLED=0 go test -c -o /tmp/sub2api-relay-normalize.test ./internal/pkg/volcengine
docker run --rm --network none --cpus 2 --memory 2g \
  --mount type=bind,src=/tmp/sub2api-relay-normalize.test,dst=/relay.test,readonly \
  alpine:3.21 /relay.test -test.run '^$' \
  -test.bench BenchmarkNormalizeRequest -test.benchmem \
  -test.benchtime=700ms -test.count=3 -test.cpu=2
```

验证通过：

- 中转、转换包和独立入口的默认 race 测试及 unit 标签测试。
- 完整网关 `TestVolcengine*`、`TestDeepSeekReasoningReplay*`，默认和 unit 标签两种构建。
- 与原路径的差分检查、字段顺序排列、2000 组请求形状、大整数、工具参数、转义键、重复键、异常 JSON、输入不变和输出幂等性。
- 原始 JSON 及请求形状两轮模糊测试，各 60 秒，共约 88 万次执行。
- 转换包在 2 CPU / 2 GiB 容器内的完整默认测试。
- `go vet`、静态中转构建及依赖隔离检查；仍无数据库、Redis 或完整网关服务依赖。

原始基准日志：`/tmp/sub2api-relay-normalize-2c2g-final.log`。本次中转二进制 `/tmp/sub2api-relay-normalize` 的 SHA-256 为 `51eb620ea09b499d3d8d488131fe3ac900620c9226f7196b9d01e9a9170cf258`。未推送或部署。
