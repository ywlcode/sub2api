# 用户请求耗时展示（dev-ywlcode）

`/usage` 和 `/dashboard` 将原平均耗时展示替换为上传、处理返回两项；用户请求记录按上传、处理返回、首字、平均 TPS 排列。`/admin/usage` 的请求记录以「耗时：上传 / 处理返回」展示，并隐藏延迟分组中的首个推理事件、首个可见文本和首个响应字节。Ops 页面仍保留这些诊断指标。

## 计时口径

- 用户上传：应用开始读取请求体 (`body_read_start`) 到完整接收 (`body_received`)，包含第一次读取等待。不能用 `body_first_byte` 作为起点，否则单次读取的大请求会被误算为零。
- 处理返回：请求体完整接收到请求处理结束 (`total_ms`)，包含此后的准入等待、转发、重试、生成、本地写出。它不是客户端收到最后一个字节的时间。
- 首字继续优先使用已有的 `queue_timing.server_first_token_ms`，旧记录保留原首字口径。TPS 继续使用原 `duration_ms`，不因展示拆分而更改性能、计费或调度口径。
- 仅应用实际观测；反向代理已缓冲的上传时间、原生 WebSocket 帧到达前的上传时间不可还原。WebSocket 不使用 HTTP 握手耗时冒充逐轮耗时。

沿用 `request_timing_details` 的 **30 天保留期**，不新增迁移或修改历史迁移。两项均值只包含筛选范围内有一份 trace、请求体读取完成且时间边界有效的请求。缺失、过期、上传中断、一个计费记录关联多个真实请求的情况显示 `—`，不充当零样本。实测零值仍有效。均值接口同时返回 `timing_sample_count`；页面提示采样范围与保留期。

采集扩展至 HTTP Responses、Chat Completions、Completions、Messages、图片、音频和 Gemini generateContent 接口。沿用现有异步持久化和授权后的分页批量补充；平均值查询仅用于用户/API Key 范围，限时 2 秒，按数值边界聚合而不聚合完整 JSON。观测查询失败不会阻止使用量页面加载。

## 后续 rebase 接入点

新增计算、平均值查询、服务接入辅助、前端格式化、用户行和均值组件均放在独立文件。上游公共文件只保留以下接入：

1. `requesttiming.ContentObservation` 新增 `request_phases`，在派生内容观测之前派生请求级耗时。没有 HTTP attempt 的有效请求也应获得两项耗时。
2. `UsageStats` / `UserDashboardStats` 嵌入 `RequestPhaseAverages`；`UsageService` 的统计方法补充可选的观测查询。
3. `usageStatsWhereConditions` 是总量与耗时均值共用的筛选条件。上游新增筛选项时应在此同步，保留用户隔离和结束时间不包含的规则。
4. `RequestTiming` 的 HTTP 接口匹配，不能对 WS 长连接整体启用逐请求计时。
5. `UsageTable` 接入用户耗时组件和管理端双值；两个统计卡片接入 `RequestTimingAverages`。中英文文案独立于上游的旧耗时文案。

上游未来提供等价的请求阶段字段时，可将独立组件的数据来源切换到上游字段，再移除重复采集。不要以旧 `duration_ms` 减去 `queue_wait_ms` 计算上传耗时，这两个值没有所需的上传边界。

## 关键验证

后端：`go test -tags=unit ./internal/pkg/requesttiming ./internal/server/middleware -run 'TestRequestPhases|TestRequestTiming'`。

数据库：`go test -tags=integration ./internal/repository -run TestUsageRequestPhaseAveragesRespectFiltersAndMissingSamples`（独立测试容器）。

前端：`RequestTimingDisplay.spec.ts`、`UsageTable.spec.ts`、`UsageStatsCards.spec.ts`、`UserDashboardStats.spec.ts`、`LatencyObservabilityPanel.spec.ts`，以及中英文键完整性与前端构建。
