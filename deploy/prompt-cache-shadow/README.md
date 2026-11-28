# Prompt-prefix cache 影子观测与分析

仅观测 OpenAI 平台，不修改用户计费、额度、调度或响应。boscserver 是当前观测节点，ecosserver、xs199 不启用。离线工具只读本地 JSONL/gzip，使用 Python 3.9+ 标准库，不保存提示词或请求标识。

```bash
python3 -B deploy/prompt-cache-shadow/analyze.py --account-id 130 --json shadow.jsonl.gz
python3 -B -m unittest discover -s deploy/prompt-cache-shadow -p 'test_*.py' -v
```

不传路径时读 stdin；`-` 最多一次。账号 ID 属于各站本地数据库，mmc 对照应使用明确的账号清单，不能把按本次命中率挑出的账号当作固定参考。

## v3 本地逻辑缓存

本地逻辑前缀按用户／平台／实际模型／实际协议隔离；测试或缺少 UserID 时回退到 Key／分组。不同用户不共享。上游账号、`prompt_cache_key`、`session_id`、`conversation_id` 的变化保留为账号内诊断，不切断用户已有的逻辑缓存。主预测取账号与逻辑前缀候选中的较大值。

请求分成稳定的规范化记录，各记录分别分词；每 128 个估算 token 建断点，同时保存记录边界。这样追加消息不改变旧消息末尾的分词边界。已识别的消息、推理、工具调用项去掉传输 `id/status`，保留角色、调用 ID、工具名、参数、输出及顺序；文本项去掉 annotations/logprobs。工具定义和推理／格式控制仍参与匹配。JSON Schema 中名为 signature/encrypted_content 的属性不再误判为不透明输入。

密文／compaction／thinking 签名等稳定块用本节点 HMAC 替代后参与指纹，不解密、不保留密文。原样重复的块可以复用；密文改变不会复用其 token 锚点。成功请求的**总输入 token**在完整记录边界建立历史计量锚点。纯明文请求的主预测为下列候选的较大值，上限为本次上游总输入：

- `floor(本次总输入 × 最长逻辑或账号前缀估算数 / 本次估算总输入)`；
- 匹配记录边界上历史已观测的输入 token 总量。

含不透明块的部分匹配只采用历史实测记录锚点；HMAC 占位符不是密文的 token 长度，不能用它把少量已匹配明文放大到整笔输入。完整输入完全相同仍可判断为全量本地复用。首次出现或改变的不透明块没有锚点时预测为 0，但成功后仍登记指纹及总输入锚点，供后续请求复用。

这不是读取本次真实缓存数进行拟合，也没有固定最低命中率。首次观察、未知服务器端上下文、密文改变、容量淘汰、近似分词及模板差异仍可能低估；不能保证每笔不低于上游。

能拿到完整 Responses output 的成功 JSON、SSE 终态及 WS 终态，将输出转换为同样的记录指纹，使用历史总输入＋输出 token 建立下一轮锚点。只保存固定指纹，不积累 SSE 原文。不完整响应、不具备完整 output 的兼容流不会伪造输出索引，按事件的 `output_prefix_indexed` 判断覆盖。所有锚点只在请求成功并有合法计量时提交；失败、断流、取消不预热。

媒体、未展开的服务端引用和存储 prompt 不纳入本次扩展，分别记录 `media_input`、`server_side_reference`、`server_side_context`、`stored_prompt`；坏 JSON、超深输入、正文或指纹预算超限同样保留明确原因，未知上游计数不补零。

## 配置与边界

管理员在 `/admin/upstream-channels` 的渠道编辑中设置“模拟缓存观测”。开关按账号当前明确绑定的上游渠道生效，与模型定价渠道不同；未绑定或关闭渠道的账号不计算、不记录模拟观测。迁移时已有渠道保留开启，新建渠道默认关闭。旧客户端更新名称或备注且未提供开关字段时，保留原值；编辑仍使用渠道版本和确认机制，避免覆盖他人的修改。

渠道开关是节点总开关和 `account_ids` 筛选之后的额外条件，仅观测 OpenAI，不改请求、上游用量、用户扣费或额度。节点总开关关闭时，页面开启渠道也不会启动观测；当前仅 boscserver 启用总开关。关闭后已开始的请求可以完成观测，后续 HTTP 请求和现有 WebSocket 的新一轮请求重新检查。启用不清空未过期的历史指纹。

管理操作在本进程立即使策略缓存失效，并通过现有 Redis 通知其他实例；自动绑定变化或通知缺失最多由 5 秒缓存到期兜底。读取失败只跳过观测，负缓存避免故障时逐请求重查；单次刷新数据库查询最多等待 250 毫秒。策略不随主从配置同步，从站仍禁用观测。日志新增 `upstream_channel_id`，表达该次观测开始时的账号所属渠道。

```yaml
gateway:
  prompt_cache_shadow:
    enabled: false
    account_ids: []
    compute_concurrency: 8
    ttl_seconds: 3600
    min_prefix_tokens: 1024
    prefix_tokens_step: 128
    max_entries: 1000000
    max_body_bytes: 8388608
    log_dir: ""                # DATA_DIR/logs/prompt-cache-shadow
    log_retention_days: 7
    log_max_total_mb: 5120
    log_queue_size: 1024
```

配置是节点专用启动配置，不随主从同步；从站即使误开也不初始化。参数可用 `GATEWAY_PROMPT_CACHE_SHADOW_*` 环境变量覆盖，修改需要重启。保留期默认 1 小时，正文最多 8 MiB。并发仍为 8（可配置 1～64），准入在正文副本和 WS 序列化之前，繁忙直接记 `observer_busy`。每笔在途指纹（含密文身份）最多 16384 个，进程在途最多 65536 个，预算超限记录 `fingerprint_limit` 或 `observer_budget`。

索引默认从 10 万扩到 100 万条，配置上限仍为 100 万。其中 25 万为账号诊断、65 万为用户逻辑前缀、10 万为实测锚点，独立 LRU；锚点只保存达到配置最小前缀阈值的成功请求完整输入和完整输出边界，不再被密集的普通断点挤掉。独立用户／模型预热与密文身份历史分别最多 10000 条。容量上限仍可能使条目早于 TTL 淘汰，不承诺所有流量下留满一小时。

密文历史仅记录同一用户／平台／模型／协议下成功见过的 HMAC 身份，并标记是否来自已完成的响应输出，不能把身份相同当成 token 长度相同。日志 `opaque_known_blocks`、`opaque_output_known_blocks`、`opaque_unseen_blocks` 区分当前保留历史中的关联情况；unseen 可能是首次出现、密文变化或历史被回收，不等于已证明首次出现。`measured_anchor_status=hit/not_found/expired` 与锚点有效淘汰／过期回收累计计数一起使用，不能仅凭一次 not_found 断言原因。没有锚点的密文仍保持原来的保守预测，不按本次上游真实缓存计数拟合。

词表复用仓库现有分词器，各记录按 UTF-8 安全的 8192 字节块分词，仍是近似模板。扩大正文上限不会无界扩容，但会增加大请求的临时解析和分词内存；不能继续用旧版 2 MiB 的内存实验代替新版 8 MiB 的峰值。

## 重启恢复

启用节点在日志目录下创建私有 `state/`，每分钟及优雅退出时检查状态；有新的成功提交才原子写入 `cache.bin`。只含 HMAC 指纹、历史计量、时间及本地 HMAC 密钥；目录 0700、文件 0600，不属于日志，不应上传到普通日志采集器。总条目和文件大小有上限，校验完整文件后才恢复；允许一分钟以内的墙钟回拨，略超前的条目时间收紧到恢复时间，不延长 TTL；忽略过期条目，并按当前 TTL 和容量收紧。每次进程有新 instance_id，`state_persistent/state_restored` 表达是否持久化、是否恢复，不能只凭 instance_id 改变判断全部冷启动。

新版私有快照格式为 V4，包含独立锚点与密文身份历史；可以读取旧 V3 并提取仍保留的实测逻辑锚点，旧文件没有的密文身份不补造。日志事件仍用 schema 3 / `local_prefix_anchors_v3`，计算公式未变。回退到仅支持 V3 快照的旧二进制时会告警并冷启动观测器，不影响正式计费；需要保留旧观测状态时，在停旧进程后独立备份其 V3 文件再升级。

损坏、权限不安全或不可读的状态文件会告警并冷启动，不影响业务；保存失败保留旧快照并告警。异常退出可能丢失最后一次快照之后约一分钟的观测。旧版没有状态文件，因此第一次升级不能恢复旧进程的内存。持久化依赖现有数据卷，不写 PostgreSQL、Redis 或用户账单。

## 日志与离线统计

接受 `msg/message=prompt_cache.shadow`、`component=service.prompt_cache_shadow` 的合法事件。schema 1 保留 `estimated_tokens_v1`；schema 2 要求 `upstream_input_ratio_v2`；schema 3 要求 `local_prefix_anchors_v3`。分析器按 `(account_id, model, transport, warm_scope, prediction_basis)` 分组，三版不混算、不改写旧预测。v3 `warm_scope=user_model`，仅代表该用户用过模型，不表示每个会话都已有完整历史。

`input_tokens` 始终是包含缓存读取的总输入。`actual_known` 包含所有真实用量已知的事件；`comparable` 只包含主预测和实际均已知、成功且未跳过的事件；`warm_only` 进一步排除 cold。命中率是缓存 token 总和／同组总输入，不是逐笔比例平均。主预测、原始估算前缀、账号匹配、共享逻辑匹配、历史锚点、密文块数、输出是否索引、过期与累计淘汰均分开记录。

每组同时报告低估／高估的请求数和 token 总差额，避免正负抵消掩盖问题；相对误差为 `abs(sim-real)/input`。绝对和相对误差各自有界采样计算 P50/P95，nearest-rank，报告精确／近似标记。零输入不进入相对误差。没有样本的统计为 null/unknown，不伪装成零；实际缓存未知不会当作零命中。

累计 `account_live_evictions` 区分有效条目淘汰，`account_expired_reclaims` 表达过期回收；历史总 evictions 仍保留兼容。进程累计计数不能逐笔相加，也不是某个账号的精确淘汰数。

`--min-real-hit-rate` 按账号全部 actual_known 用量加权筛选，不能代替固定 mmc 家族清单。可比子集的结果不代表全部流量。修改规则前保存基线，使用后续自然请求验证；旧日志缺完整前缀，不能离线重放新版 TTL、逻辑缓存或记录归一化。

## 日志保留与读取保护

观测日志使用独立有界异步队列，不进入普通日志、Docker stdout 或 Ops 数据库。目录 0700、文件 0600，单事件 16 KiB，默认队列 1024；每分钟及退出输出统计，分别记录队列丢弃、超长丢弃、写失败与清理错误。按 UTC 日或未压缩 64 MiB 分段，关闭段 gzip 压缩，最近 7 天、总量 5 GiB，容量优先，可能提前删除旧段。只操作本观测器命名的普通文件，独占锁阻止多进程共用目录。

分析器兼容最多 4 层／32 对象包装，广度优先并剪除超深分支；JSON 最大深度 64，整数先限文本 128 位再转换。单行默认 1 MiB、最大 16 MiB；坏行计数后继续。默认最多 1000 组，每统计集合对绝对和相对误差各保留 1024 样本，原因最多 64 类、标签最多 512 字符。JSON 在处理成功后流式输出；超出分组或文件错误退出 1、不输出部分报告，参数错误退出 2。不去重重复文件，不自动校验时间窗、采样完整性或配置一致性。
