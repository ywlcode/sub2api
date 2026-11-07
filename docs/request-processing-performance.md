# 请求正文处理性能优化

本轮基于 `dev-ywlcode` 的 `73b6ba333`，目标是降低重复 JSON 解码、整份正文复制和本地模型目录构建的成本。没有修改计费、账号调度、错误重试或 gzip 策略。

## 实现与边界

### Codex bootstrap

`openai_bootstrap_normalize.go` 提供合并入口：先检查是否存在 `function_call_output`，存在时共用一次重复键检查和一次 `UseNumber` 解码，按原顺序执行 automation、delegation，最后最多序列化一次。普通消息历史直接返回原始字节。

两个规则仍分别验证整个请求，然后才修改对应条目。保留原有独立入口和用例，差异测试比较合并入口与原来的顺序调用，包括混合工具历史、真实 call_id、previous_response_id、重复键、转义键、大整数和非法 JSON。

### DeepSeek reasoning_content

`deepseek_reasoning_batch.go` 保留账号/实际出站模型的适用范围和单空格占位规则。多个缺口先修改各自消息，再组装一次完整正文；单个缺口直接执行一次原有 sjson 编辑，避免为短历史增加独立校验遍历。真实推理、工具调用和未修改的正文片段保持原样。

重复或不可靠的消息定位、非法的多消息正文走原来的逐条编辑路径。原实现保留为异常输入回退和差异测试基准。所有编辑都产生新切片，不改写共享输入。

### 请求内解析结果

`requestmodel/request_cache.go` 在同一个 HTTP 请求中，按不可变正文切片的身份和长度复用 JSON 合法性、模型候选以及 stream 字段分析。JSON 顶层 model、session.model 和 stream 在同一轮字段遍历中收集。

分组选择、模型白名单、Key 模型策略、长上下文检查、合成路由、仅允许流式请求检查使用这些结果；权限决定本身仍按原流程执行。重复键、大小写变体、Live 的 session 优先级、multipart 路径均保持原规则。模型映射或协议转换产生新正文后，旧解析结果失效。

借用的 JSON 只读视图仅在同步处理期间使用；保留的模型名会复制成独立小字符串。`RequestBodyLimit` 在请求结束时释放解析缓存中的正文引用，防止脱离请求的异步任务通过 context 延长大正文的存活时间。没有增加大缓冲池，也没有改变原有分块上传、超时或取消处理。

### 防蒸馏

先检查平台、分组、成功用量和缓存中的启用配置，再遍历正文。配置读取错误仍跳过观测，不阻断业务；最终 `Observe` 保留原来的开关校验。HTTP 和 WS 共用该入口。

### 本地 Codex 模型目录缓存

只缓存 `GatewayService.BuildCodexModelsManifestForGroup` 的昂贵目录构建结果；仓库读取和用户可见模型过滤仍照常执行。

缓存键包含有效平台、最终可见模型列表、分组配置、账号模型配置/元数据/分组限制/影子账号身份、合成路由及运行时映射版本。配置输入变化就不能命中旧结果；凭据仅参与摘要计算，不进入缓存值或日志。账号使用时间不参与摘要。

同一缓存键同时构建时使用 singleflight 合并。每实例最多 128 项、正文合计 32 MiB、单项最大 8 MiB，TTL 30 秒；过大结果直接构建返回。缓存值和调用方响应切片隔离，显式失效带版本保护，避免旧构建完成后重新填入已失效的缓存。上游原生模型目录的既有缓存保持原逻辑。

## 验证

在仓库的 `backend` 目录，使用 Go 1.27.3：

```sh
go test github.com/v03413/bepusdt/app/conf github.com/v03413/bepusdt/app/task github.com/v03413/bepusdt/app/task/notify
go test -tags=unit ./...
go test -race -tags=unit ./internal/pkg/requestmodel ./internal/service -run 'Test(RequestCandidates|RequestCache|RequestFields|ModelCandidates|GroupManifest|CachedGroupManifest|BatchDeepSeek|OpenAIUsageProtectionDisabledErrorsAndConfigChanges)' -count=1

go test -tags=unit ./internal/handler -run '^$' -fuzz '^FuzzNormalizeCodexBootstraps$' -fuzztime=20s -parallel=2
go test -tags=unit ./internal/service -run '^$' -fuzz '^FuzzBatchDeepSeekReasoning$' -fuzztime=30s -parallel=2
go test ./internal/pkg/requestmodel -run '^$' -fuzz '^FuzzJSONModelCandidatesCombined$' -fuzztime=20s -parallel=2
```

容器运行全量测试时必须同时挂载仓库的 `tools/fixtures`：部分既有测试会读取 backend 目录以外的固定契约样本。

## 基准方法

所有数据均为本地合成正文，不调用真实上游或使用用户请求内容。单线程基准包含约 1、5、20 MiB 正文，以及 1、50 条需要补字段的历史消息；同一环境重复两轮。并发基准使用 1 MiB 正文、50 条历史消息，固定 1、8、25 个 worker，每组执行 100 次并重复两轮。

```sh
go test -tags=unit ./internal/handler ./internal/service ./internal/pkg/requestmodel -run '^$' -bench 'Benchmark(CodexBootstraps|DeepSeekReasoningBatch|RequestModelCandidates|CodexGroupManifestCache|OpenAIUsageProtectionDisabled)$' -benchtime=200ms -count=2
go test -tags=unit ./internal/service -run '^$' -bench '^BenchmarkDeepSeekReasoningConcurrent$' -benchtime=100x -count=2
```

`B/op` 是累计分配量，不是同时驻留内存；并发 `ns/op` 是吞吐摊销耗时，单次延迟另报 p95。模型目录基准包含配置摘要和返回切片复制，不包含仓库 I/O。局部函数收益不能直接换算为生产整机 CPU 降幅，部署后应在相近流量下重新采样。

## 本次结果（2026-11-07）

功能提交：`1bdac826c`、`a955fb237`、`029078283`、`3abdc5aa8`。环境为 AMD Ryzen 7 H 260 / Linux amd64 / Go 1.27.3；下表取两轮均值。正文大小沿用基准名称，约为对应 MiB。

| 局部操作 | 修改前 | 修改后 | 修改前累计分配 | 修改后累计分配 |
|---|---:|---:|---:|---:|
| 20 MiB 普通 Responses 的两项 bootstrap 检查 | 112.96 ms | 12.04 ms | 336.02 MiB | 0 |
| 20 MiB DeepSeek，补 50 条历史消息 | 368.90 ms | 28.48 ms | 1,020.43 MiB | 20.05 MiB |
| 20 MiB 请求，4 次模型候选检查 | 61.15 ms | 6.30 ms | 160.06 MiB | 约 5.4 KiB |
| 50 个模型的本地 Codex 目录构建/缓存命中 | 61.32 ms | 0.144 ms | 13.47 MiB | 1.12 MiB |

单条历史的 1 MiB DeepSeek 样本耗时基本持平（约 1 ms），累计分配减半（约 2 MiB→1 MiB）。20 MiB 单条历史的两轮波动范围为修改前 14.9–17.8 ms、修改后 17.3–17.4 ms；主要收益是避免消息数组复制，并没有给这类短历史承诺 CPU 加速。防蒸馏关闭时，1–20 MiB 样本均约 0.35 微秒，不再随正文大小增长。

固定并发基准，1 MiB 正文、50 条缺字段历史，每组 100 次操作：

| worker 数 | 修改前单次 p95 | 修改后单次 p95 |
|---|---:|---:|
| 1 | 23.59 ms | 2.17 ms |
| 8 | 48.30 ms | 3.86 ms |
| 25 | 133.15 ms | 11.77 ms |

验证记录：

- 全量后端单元测试已执行。容器最初缺少仓库外的 `tools/fixtures`，补挂载后仓库测试通过；服务层仅剩域名解析和缺少 Python 的两项环境失败，在本机复跑均通过。
- 修改相关的处理器、路由、中间件回归、DeepSeek 定向回归、模型目录配置失效与内存上限测试通过。
- 请求缓存与模型目录缓存的 race 检查通过；模型字段、bootstrap、DeepSeek 的新旧实现差异模糊测试通过。
- 最后一轮 DeepSeek 模糊测试额外覆盖多消息正文，共执行 52,461 次。发现并修复了 Array/ForEach 在畸形 JSON 上的差异；触发样本保存为 `testdata/fuzz/FuzzBatchDeepSeekReasoning/d608005ee3b61090`。
- 原始本地记录保存在 `output/request-processing-optimization/`，其中 `benchmarks-final.txt` 和 `concurrent-benchmarks.txt` 对应本表。没有执行真实上游请求或生产压测。

## 后续 rebase

新增实现和测试放在独立文件；原处理器、middleware、服务主要保留小的调用接入点。rebase 时保留两项 bootstrap 的独立规则、DeepSeek 原算法回退，以及模型权限检查顺序。若上游新增影响模型目录的账号配置字段，需要同时检查 `groupManifestKey` 是否覆盖它。
