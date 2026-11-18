# 数据库全量与精简备份

系统设置 → 数据备份 → 定时备份中的 **精简备份** 默认关闭。开启后，仅后续定时备份及由这些备份形成的月度归档使用精简范围。手动备份保持全量，已有备份不变。备份记录显示“精简备份”，文件名包含 `_compact`，恢复确认框说明不包含哪些历史。

备份仍包含完整数据库结构、约束、索引定义和序列；仅省略指定表的数据或已完成事件的行，不删除线上数据库记录。恢复后的这些历史表为空，后续请求重新产生数据。数据目录、Redis、环境变量、主从配对 token 和固定的 TOTP 加密密钥仍须沿用现有部署备份方式；数据库备份不代替这些文件的备份。

## 精简范围

| 数据 | 精简备份行为 |
| --- | --- |
| 错误、重试和系统日志 | 省略 `ops_error_logs`、`ops_retry_attempts`、`ops_system_logs`、`ops_system_log_cleanup_audits` |
| 运维指标 | 省略 `ops_system_metrics`、`ops_metrics_hourly`、`ops_metrics_daily`、`ops_network_minutes`、`ops_ingress_reject_aggregates` |
| 请求诊断、耗时、抓包目录记录 | 省略 `request_connection_traces`、`request_timing_details`、`request_capture_tasks`、`request_capture_records` |
| 请求阶段耗时桶 | 省略 `usage_request_phase_hourly_global`、`usage_request_phase_hourly_dirty` |
| 用户流量事件 | 省略 `user_traffic_events`；保留较小的 `user_traffic_hourly` 汇总 |
| 计费事件 | `usage_billing_events` 只省略 `complete`、`legacy`；保留 `pending`、`applied`、`blocked` 及未来未知状态 |
| 历史报价 | 省略 `usage_billing_quotes`。它保存核账精度证据，不是余额或恢复队列；用户使用记录中的正式费用仍保留 |
| 计费去重 | **完整保留** `usage_billing_dedup` 和 `usage_billing_dedup_archive`，避免恢复后重放旧请求重复扣费 |
| 主从状态 | **完整保留** `site_*`，尤其站点身份、outbox、回执、拒收待处理事件和同步游标 |
| 业务数据 | 保留用户、Key、账号/凭据、余额、订阅、权限/路由、设置、使用明细、用量汇总、充值订单、任务/资金预留、安全审计及处罚状态 |

使用明确的表清单，不用 `ops_*` 或 `usage_*` 通配排除：这两个命名空间也有配置和运行状态。已列出的表及其继承/分区子表一起排除；数据库新增其他表默认备份。将来新增外键，使保留数据引用被排除数据时，精简导出会报错，要求重新审查范围，不能生成不可恢复的备份。

## 一致性与资源

精简导出持有与全量备份相同的迁移互斥锁，在只读 REPEATABLE READ 事务中导出快照。`pg_dump` 的业务数据和单独导出的未完成计费事件共用该快照；并发发生的余额扣减、去重记录和事件状态不会来自不同时间点。不暂停正常网关计费，不修改生产表。

SQL 经有背压的管道直接 gzip 到临时文件，不把整库或事件队列读入 Go 内存。读端取消、进程失败都会中止导出并释放快照/锁；失败备份不作为成功文件上传。恢复使用 `psql --single-transaction --set ON_ERROR_STOP=1 --no-psqlrc`，任何 SQL 错误都失败回滚。

精简备份不承诺固定小于 3 GB。保留的用量、长期去重和主从积压仍会随业务增长；文件大小应根据实际请求量与压缩率估算。2026-11-18 boscserver 抽样推算首次满 60 天约 2.5–2.9 GB，前提是请求量和模型结构维持当时水平，且没有大量未完成账单；不是生产实测的 60 天文件。

## 验证

后端专项：

```sh
go test -tags=unit ./internal/service ./internal/repository -run 'Test(Backup|PgDumper|CompactBackup)' -count=1
```

前端：`vitest run src/views/admin/__tests__/BackupView.spec.ts src/i18n/__tests__/localeKeyCompleteness.spec.ts`，以及 `vue-tsc --noEmit`。

真实恢复用例 `TestCompactBackupPostgresRestore` 通过全部迁移建库，执行实际 `pg_dump`/`psql`，覆盖并发扣费快照、恢复后补扣与重复投递、冷热去重、主从身份/outbox/回执、分区/继承诊断表排除、特殊字符和非 ISO 数据库日期设置、完整备份兼容、恢复 SQL 失败回滚、外键变化拒绝以及取消导出的锁释放。

运行时提供 `SUB2API_BACKUP_TEST_SOCKET` 和 `SUB2API_BACKUP_TEST_CONTAINER`。容器名以 `sub2api-compact-backup-` 开头，socket 目录以 `sub2api-compact-` 开头。只使用专用 PostgreSQL 容器，配置 `--network=none`、临时数据目录和绑定挂载的 Unix socket，禁止使用生产数据库。PostgreSQL 的 `unix_socket_directories` 需同时保留 `/var/run/postgresql`（供镜像初始化使用）和绑定目录；不要创建默认 Docker 网桥或占用公司内网网段。该测试创建并销毁自身随机命名的数据库；完成后删除本轮容器和临时目录。
