# Prompt-prefix cache 影子日志离线分析

`analyze.py` 只读取本地 JSONL、gzip 压缩的 JSONL／标准输入，使用 Python 3.9+ 标准库；不连接服务、不发模型请求、不保存提示词、请求 ID 或 API Key ID。回归测试使用合成日志；线上准确度须按同一版本、同一统计集合的自然请求单独验证。

```bash
python3 deploy/prompt-cache-shadow/analyze.py shadow-1.jsonl shadow-2.jsonl
python3 deploy/prompt-cache-shadow/analyze.py --account-id 42 --account-id 7 --min-real-hit-rate 0.8 --json shadow.jsonl
python3 deploy/prompt-cache-shadow/analyze.py --json - < shadow.jsonl
python3 deploy/prompt-cache-shadow/analyze.py --min-real-hit-rate 0.95 --json /path/to/prompt-cache-shadow/shadow-*.jsonl*
python3 -B -m unittest discover -s deploy/prompt-cache-shadow -p 'test_*.py' -v
```

不传文件时读 stdin；`-` 最多出现一次，也可与文件组合。`.gz` 按后缀流式解压；不要同时传入重复内容或正在生成的 `.gz.tmp`，建议先取得关闭段的快照。`--json` 输出一个 JSON 报告，否则输出可读文本。账号 ID 在报告中统一为字符串。

## 日志与统计口径

只接受 `msg` 或 `message` 为 `prompt_cache.shadow`，且 `component=service.prompt_cache_shadow` 的事件；`schema_version` 必须为整数 `1` 或 `2`，`state_scope=process`，`instance_id`、`account_id`、`model`、`transport` 必须有效。支持常见 `log`、`logs`、`payload`、`jsonPayload`、`message`、`msg` 包装，其内容可为对象或编码后的 JSON 对象，最多展开 4 层、检查 32 个对象。不拆数组／多行载荷，不拼接分散在包装内外的字段，不从任意文本中猜测 JSON。

按 `(account_id, model, transport, warm_scope, prediction_basis)` 聚合，多个实例的已记录结果直接汇总，**不重放或合并进程缓存状态**。新版 `warm_scope=account_model`，旧日志缺省归为 `legacy_request_namespace`，两种冷启动口径分开统计。schema 1 的预测按 `estimated_tokens_v1` 分组；schema 2 要求 `prediction_basis=upstream_input_ratio_v2`。两种 Token 单位不合并，因此同一目录跨升级读取也不会混算。`platform`、`api_key_id`、`request_id` 和估算器参数不作为分组维度。

| 输出 | 口径 |
| --- | --- |
| `actual_known` | 有真实 `input_tokens`，且 `upstream_cache_known=true`、真实缓存 token 非空的样本，包括模拟跳过样本 |
| `comparable` | 同时满足上游已知、真实输入与模拟缓存非空、`comparable=true`、非 `skipped`、无 `skip_reason` |
| `warm_only` | 可比样本中 `decision=hit/miss`；排除 `cold/skipped`，不根据真实上游命中推断预热 |
| 命中率 | 各自缓存 token 总和／同一统计集合的 `input_tokens` 总和，绝不是逐请求比率的平均 |
| MAE／有符号误差 | `mean(abs(sim-real))`／`mean(sim-real)`，单位 token；正值表示高估 |
| 相对误差 | 对 `input>0` 样本：`mean_absolute_relative_error=mean(abs(sim-real)/input)`；`weighted_absolute_relative_error=sum(abs(sim-real))/sum(input)` |
| 绝对误差 p50／p95 | `p50_absolute_error_tokens`／`p95_absolute_error_tokens`，nearest-rank；精确／近似标记见 `quantiles.approximate` |
| 相对误差 p50／p95 | `p50_absolute_relative_error`／`p95_absolute_relative_error`，对逐请求 `abs(sim-real)/input` 独立有界采样；标记见 `relative_quantiles.approximate` |

`input_tokens` 是**包含真实已缓存 token 的上游总输入**，不能使用未缓存部分或 `estimated_input_tokens` 替代。schema 2 的主预测为 `floor(input_tokens × matched_prefix_tokens / estimated_input_tokens)`，上限为总输入；使用整数运算，不读取真实缓存数、不设最低命中比例。匹配在提交前冻结，完成时仅把前缀比例换算到上游 Token 单位。JSON 序列化与近似分词的原始 Token 数单独保留，不能直接拿它与上游缓存 Token 比较。

分析器直接统计已记录的主预测，不按诊断字段重算。schema 1 继续保留旧值及其可能超过 1 的模拟命中率，不把历史数据静默转换为新版结果。`committed=false` 不自动取消本次有效比较。

可选扩展字段兼容缺省／`null`，不改变上述统计：

- schema 2 的 `simulated_cache_read_tokens` 使用上游输入 Token 单位；`raw_simulated_cache_read_tokens` 保留未换算的 `matched_prefix_tokens`，用于诊断分词偏差。`scaled_simulated_cache_read_tokens` 保留为新版主预测的兼容别名。schema 1 中主预测仍是原始前缀数，`scaled_simulated_cache_read_tokens` 仅为当时的折算候选。两版均不使用真实缓存数量校准。
- `shared_prefix_tokens` 仅诊断同租户／模型、不区分账号与 `prompt_cache_key` 的前缀重合，不加入该账号模拟命中。
- `prompt_layout`、`expired`、`captures` 为布局、过期和捕获诊断。通用 `instructions_before_input` 只是上游 wire 序列化的近似，不承诺特定模型的内部布局或精确分词。
- `body_view=upstream_wire`、`output_prefix_indexed=false` 表达观察视图及不索引输出前缀的协议约定；分析器不据此重建正文。这些扩展值不保存到报告，也不影响账号筛选与读取诊断计数。
- `account_cache_evictions`、`shared_cache_evictions`、`warm_state_evictions` 是查询时的进程累计 LRU 淘汰计数。每组报告的 `max_observed_process_eviction_counters` 取已观测最大值，不逐请求相加，也不是该账号或该时段的淘汰数；旧日志缺失时为 unknown。

`--min-real-hit-rate` 接受 `[0,1]` 内的小数，按账号跨模型／传输的 **actual_known 缓存总量／输入总量** 筛选，达到阈值即保留该账号全部分组。上游未知样本不进入此分母；没有正输入分母的账号在开启筛选时不入选，即使阈值为 0。`account_actual_usage` 列出账号筛选依据；`counts` 是筛选前的读取诊断，`selected_events` 是最终保留事件数。

未知缓存值、缺少输入和不可比样本不会补成 0。没有可用样本的统计为 JSON `null`／文本 `unknown`；显式 0 缓存是已知未命中。0 输入样本仍进入绝对 token 误差，排除出全部相对误差统计；总输入为 0 时命中率及相对误差为空。文本与 JSON 均展示分组 `actual_known`，包括全部模拟跳过的分组。原始跳过原因见 `skip_reason_counts`，分析器排除原因见 `incomparable_reason_counts`。

## 边界与限制

- 逐行有界读取，默认单行最多 `1048576` 字节（含换行），`--max-line-bytes` 可调整但最多 16 MiB；超长行分块丢弃。坏 JSON／UTF-8、无效事件、不支持的 schema、包装超限分别计数并继续。包装广度优先检查，剪除过深分支，不阻断其他浅层有效事件。JSON 本身最多嵌套 64 层；任何字段的 JSON 整数最多 128 位，先限长再转换，超限按坏 JSON 计数。
- 默认最多 `1000` 个分组，每组／每统计集合对绝对和相对误差各保存最多 `1024` 个独立样本，超过容量用固定种子的 reservoir 近似分位数。原因计数最多 `64` 种，分组标签及原因最多 `512` 字符。可调整 `--max-groups`、`--quantile-samples`、`--max-reasons`；额外原因累加到 `other_events`。除分位数外的总量与均值使用全部有效样本。内存取决于这些上限，不随日志行数线性增长；JSON 在全部输入处理成功后流式编码输出。
- 超出分组上限或文件读取失败时退出码为 1，且不输出部分报告，避免把不完整账号统计当作筛选依据；参数错误退出码为 2。可计数的坏行不会使退出码非零，应同时检查报告中的计数。
- 不去重重叠文件／重复请求，不校验日志完整性、时间窗口或配置一致性；固定种子的采样结果可重复，但输入顺序变化可能改变近似分位数。跨版本、分词器、TTL 或实例混合日志宜预先分文件分析。
- 模拟不是供应商缓存的精确复现；进程冷启动、分词估算、供应商路由／TTL／缓存粒度、缺失用量及采样偏差都会影响比较。可比子集的结果不能代表所有请求。

## 观测器配置

以下是启动配置默认值。默认关闭；修改挂载的 `config.yaml` 后需重新加载应用进程，未增加数据库迁移或后台页面：

```yaml
gateway:
  prompt_cache_shadow:
    enabled: false
    account_ids: []
    compute_concurrency: 8
    ttl_seconds: 600
    min_prefix_tokens: 1024
    prefix_tokens_step: 128
    max_entries: 100000
    max_body_bytes: 2097152
    log_dir: ""                 # 空值使用 DATA_DIR/logs/prompt-cache-shadow
    log_retention_days: 7
    log_max_total_mb: 5120       # MiB；约 5 GiB，容量优先于留存天数
    log_queue_size: 1024
```

`enabled=true` 后，仅观测实际账号 `platform=openai` 的请求；从站角色即使误开开关也不创建观测器。`account_ids: []` 表示这些 OpenAI 账号全部纳入；建议先填写少数高命中对照账号及需要研究的低命中账号。boscserver 是本次目标，ecosserver、xs199 不启用。由从站转发到主站并由主站调度的 OpenAI 请求仍在主站观测。账号 ID 属于各站自己的数据库，不能跨站复用列表。`GATEWAY_PROMPT_CACHE_SHADOW_ENABLED=true` 可覆盖启用开关，其他参数亦沿用网关配置的环境变量映射。

配置是**本节点专用诊断**，不随主从请求策略同步。各进程分别预热、过期和淘汰，重启后重新冷启动；缓存不会写 PostgreSQL、Redis、从站账单或用户响应。内存中只保留进程随机密钥生成的 HMAC 前缀、Token 数及时间，日志不含提示词、工具正文、密文或密钥。

首版覆盖 `OpenAIGatewayService` 的 Responses、Chat Completions、Messages 兼容入口，HTTP JSON／SSE，以及原生 WebSocket ctx-pool／passthrough 的逐轮请求；观察的协议以**实际转给上游的请求**为准，仅允许 OpenAI 账号。未接入原生 Anthropic 账号、Gemini、图片、音视频等入口。

## 独立日志与保留边界

每次请求完成后尝试输出一条 `prompt_cache.shadow` JSON 事件；生产输出使用独立的有界异步队列和文件 worker，不进入普通应用日志、Docker stdout 或 Ops 系统日志数据库。普通日志级别和采样不影响该文件。目录默认 `/app/data/logs/prompt-cache-shadow/`（设置 `DATA_DIR` 时跟随它），目录 0700、文件 0600；沿用现有 `/app/data` 持久卷，无数据库迁移或后台页面。启用时在服务初始化阶段准备目录，初始化失败只关闭观测器并记录警告；业务转发、用量和计费继续执行。

文件按 UTC 日切换或未压缩大小 64 MiB 分段，关闭段用 gzip BestSpeed 流式压缩，压缩时同时核算源文件及临时文件的空间预算。默认保留最近 7 天的段，合计最多 5120 MiB；达到容量限制先删除最旧段，并在统计事件记录 `retention_shortened=true`、容量删除计数。保留按段最后修改时间计算，因此日切换边界可能保留少量超过 7 天的记录；不能保证任意流量下留满 7 天。只清理本观测器命名的普通文件，排除其他日志和符号链接。独占文件锁阻止多进程共用同一日志目录；多副本需要各自目录。

队列默认 1024 条，单条待写字段／编码事件最多 16 KiB，满队列、过大事件和磁盘写失败分别累计，业务生产者不等待磁盘。压缩、清理、日志写入全部在同一后台 worker 执行；优雅退出时停止接收并排空队列、输出最终统计及关闭文件，强制终止无法保证未落盘队列及最后一分钟计数。

worker 每分钟及优雅关闭时写入 `prompt_cache.shadow_stats`，包含进程 `instance_id`、递增 `sequence` 及 `logging` 计数。离线分析器按每进程最新序号去重，报告累计写入、丢弃、写失败及保留不足情况；这些是**截至快照的进程累计值，不是选中账号或文件时间窗口的精确丢失量**，不能将其直接作为某个账号的缺失率。没有快照时丢失数为 unknown；`instances_without_snapshot` 列出有请求事件但无统计快照的实例。重启改变实例 ID，定期汇总前应收齐对应进程的段。队列满可能偏向丢弃高峰请求，不能忽略该样本偏差。计算阶段因 `observer_busy`／`observer_budget` 跳过的请求也不会为模拟器登记前缀，可能低估后续命中；收敛前需先核对这些跳过计数。

## 本地内存测量

2026-11-27 Linux amd64／Go 1.27.4 的独立测量（不带 race，使用合成文本）中，拆分后的共 10 万条前缀索引 GC 后约 19.8 MiB，另有 1 万条预热状态约 2.0 MiB；65536 个在途指纹约 2.5 MiB；装满 1024 条、每条 60 个标量字段的日志队列约 4.0 MiB；首次初始化共享 o200k 分词器约 6.7 MiB。上述上限是整个进程共用，不按账号或用户重复分配；实际队列事件的字符串长度仍会影响占用，待写字段单条限 16 KiB。

默认前缀参数下，含 HTTP 正文副本读取的两笔并发、每笔约 1 MiB 文本的计算，采样到的额外活跃 Go 堆约 27.5 MiB；每笔约 2 MiB 时约 37.4 MiB。两笔 2 MiB 请求的累计分配约 391 MiB，这是 GC 周期内先后分配的总量，并非同时常驻量，仍会增加 GC 压力。测量没有覆盖所有 JSON 结构，也不是进程 RSS／cgroup 的硬内存上限；Go 堆保留、采样遗漏及其他网关任务均会影响实际峰值。分词器若已由其他功能初始化，其词表不重复加载。

默认并发调整为 8 后，同机补测八笔并发、每笔约 1 MiB 文本的额外活跃 Go 堆峰值约 61.8 MiB；每笔约 2 MiB 时约 102.2 MiB，八笔均完成计算，没有 `observer_busy`／`observer_budget` 跳过。该并发实验早于索引拆分；结合现版索引、预热状态、典型日志队列和分词词表的约 35 MiB 预算，粗略合计约 137 MiB 活跃 Go 堆，不是同时装满后的联合实测峰值。八笔 2 MiB 的累计分配约 1.51 GiB，仍需关注 GC；不能继续把两笔并发的测量当作默认 8 并发的内存预算。

复现实验（backend 目录）：

```bash
go test ./internal/pkg/promptcache -run '^$' -bench '^BenchmarkShadowRetainedMemory$' -benchtime=1x -count=1
SUB2API_SHADOW_MEMORY_PROFILE=1 go test -tags=unit ./internal/service -run '^TestPromptCacheShadowMemoryProfile$' -count=1 -v
```

## 模拟算法

模拟在发往上游之前查找最长连续 Token 前缀；工具、指令、历史内容及顺序发生变化会改变匹配，`stream` 和普通 metadata 不参与匹配。默认至少 1024 Token、之后每 128 Token 一个断点，仅为候选策略，可独立修改；使用仓库已有 `o200k_base`／`cl100k_base` 近似分词，不能声称复刻供应商内部模板。匹配在发送时冻结，请求成功且有有效总输入用量后才登记缓存；流中断、失败终态和客户端断开不登记。未知真实缓存字段不会被当成零，仍可以为以后请求预热。已结束但上游仍实际保留缓存的失败请求，在本候选中可能被低估。

账号前缀按租户／平台／账号／模型／实际出站协议、正文 `prompt_cache_key` 和出站 `session_id`／`conversation_id` 的 HMAC 分区；原始会话头不保留、不落日志。HTTP 以出站端点识别协议，WS 明确为 Responses，Messages 没有或传空 `system` 均不改变协议。无可识别端点时保留正文推断。默认 `max_entries=100000` 共用总额，账号索引分配 75000、共享诊断索引分配 25000，独立 LRU，彼此不能挤占容量；共享索引不区分账号和上述会话标识，但仍隔离租户／平台／模型／协议。

预热状态单独按 `(platform, account_id, model)` 记录成功完成，不含租户、会话键或前缀，也不与前缀共用 LRU 或 TTL；默认最多 10000 条（随较小 `max_entries` 缩减，最少 16 条）。同一账号／模型更换会话、前缀过期或被淘汰后计为 `miss`，仍进入 `warm_only`。进程重启、首次使用或独立预热状态被淘汰后才重新记 `cold`；需结合预热淘汰计数判断剩余选择偏差。

密文／压缩项、`previous_response_id` 等服务端上下文引用、媒体输入和超过 `max_body_bytes` 的正文输出不可比原因，不尝试解密或从上游补取。仅有最终 usage 的部分结果亦不当作正常成功样本。每进程默认最多同时进行 8 笔观测计算，`compute_concurrency` 可配置为 1～64（0 使用默认 8），亦可通过 `GATEWAY_PROMPT_CACHE_SHADOW_COMPUTE_CONCURRENCY` 设置；准入门限覆盖 HTTP 正文副本读取和 WS 序列化，繁忙时在复制／序列化之前立即记 `observer_busy` 并跳过计算；每请求最多保留 16384 个断点，进程在途最多保留 65536 个断点，超限分别记 `fingerprint_limit`／`observer_budget`，失败、重试及连接关闭均释放预算。跳过时仍记录本次真实上游用量。分词按 8192 字节的 UTF-8 安全边界分块，`tokenization_mode=chunked_8192_bytes`；块边界也属于明确的估算误差来源。

## 对比与收敛

先收集同一进程、同一参数的一段自然流量，再以 `--min-real-hit-rate 0.95` 筛选高命中账号。优先看 `warm_only` 的相对误差 p50／p95及输入加权误差，同时核对 `actual_known`、不可比数量，避免只挑成功样本。可按 `--account-id` 导出具体账号；近似采样标记随分位数一起输出。

逐笔诊断时使用日志的以下信息：

- `estimated_input_tokens` 与真实 `input_tokens` 的差异提示分词／模板偏差；schema 2 已将主预测换算为上游输入单位，原始前缀数留在 `raw_simulated_cache_read_tokens`。**不读取真实缓存数进行拟合**。
- `shared_prefix_tokens` 高而账号本地命中低，说明同租户／模型内容在更宽的分区中出现过，可能是其他账号，也可能是同账号的正文缓存键或出站会话头变化；它是账号／分区差异的线索，不证明供应商支持跨账号共享。`routing_fingerprint` 只用于核对同一进程内会话路由是否改变。
- `decision=cold`、`warm_scope`、`expired`、`instance_id`、`cache_age_ms` 和各分区淘汰计数用于区分预热、重启、TTL 与容量问题。预热状态被淘汰仍可能造成冷启动分类偏差，不能只看 `warm_only`。
- `output_prefix_indexed=false`：首版只登记成功请求的输入前缀，不登记新生成的助手输出；下一轮重新发送该输出时，这一段可能仍算未命中，属于已知低估来源。
- `captures>1` 表示一个逻辑请求发生了多次物理提交；日志比较最终提交正文，不能把中间失败尝试当成独立成功样本。

先用高命中账号检验前缀模拟本身；低命中上游与模拟值的差异还可能包含供应商路由、缓存落点或保留策略，不能直接据此调低预测，也不能只看聚合均值接近就推断逐请求准确。继续检查 warm_only 的 P95、不可比覆盖率和异常账号／模型。

调整规则前保存基线，使用新的后续样本验证，不能在同一批真实命中数上调参后宣称已经收敛。离线换算旧日志只能验证计量公式，不等于新版在线效果。回归使用合成请求／本地假上游；线上误差需要新版上线并收集自然流量后才能测量。本目录不执行配置修改、付费调用或生产发布。
