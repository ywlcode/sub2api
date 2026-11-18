# 主从站 36 小时持续观测

`soak.py` 使用 Python 3.11+ 标准库，独立于 Sub2API 进程运行。无需构建镜像或更换服务；不重启应用，不修改主从配置，不制造生产断网。模型探针会产生正常用量，使用用户明确授权的现有 Key。

从站只调用 `deepseek-flash-local`，本机 `http://127.0.0.1:8080/v1/responses`，high 思考，输出预算 1536 tokens（包含推理）。每 15 秒启动一次，始终最多一个在途请求；慢请求完成后才启动下一次，不补发堆积任务。每 40 次请求附约 32 KiB 合成上下文。正常最多约 8640 次/36 小时，不是高并发压测。仅完整 `response.completed`、completed 状态、目标标记和 EOF 都满足才算成功；SSE 错误、失败/不完整终态、无终态、总超时分别留证。

探针只要求固定标记和 1–16 的数字，避免复杂生成任务消耗大量思考预算。启动阶段旧探针要求 40 条短建议，曾出现输出预算截断；这些失败记录保留，完整报告归入 `output_budget_exhausted`，不能当成主从网络故障。

两机共享绝对 UTC 起止时间，程序重启不会重置测试期限。36 小时后停止发起模型请求，再观察 120 秒让在途请求及账单完成，写出最终报告；从站随后删除本轮凭据文件。未能在收尾时间完成的账单不能标记为已结算，应保留后续人工对账。

## 观测内容

- 从站实际 `site synchronization completed` 日志：成功安装时间、间隔、耗时、全量/增量、传输大小。
- 同步失败阶段、连接两端、HTTP 协议、连接复用、切换池编号；账单传输及回执相关日志。正常同步成功日志没有连接池编号，不能凭健康探测声称逐个池都正常。
- 30、60、300、1020、1200 秒没有观察到成功安装时记录告警，恢复时关闭告警。**20 分钟租约内模型仍可能成功，不能用模型成功证明同步成功。** 租约剩余值由日志估算，`/readyz` 才是当前放行状态；进程重启会立即清除内存租约。
- 每 5 秒采集 `/health`、`/readyz`；每 30 秒采集容器状态、主从身份、outbox、最老事件、拒收、数据库连接/锁等待、宿主机负载和可用内存/磁盘。
- 从站每 30 秒在应用容器内使用一次新连接访问主站 `/health`，使用与应用相同的 DNS 和网络环境。它只判断新连接是否可达，不等于已授权的同步成功，不会伪造 `/internal/sites/sync` 请求。
- 主站读取 Nginx JSON access log，仅保留 `/internal/sites/sync`、`/internal/sites/events` 的状态、协议、请求时间和 request_id。跟踪 inode 和位置，普通 rename 日志轮转补读旧文件；截断、旧文件已经消失等缺口明确记载。
- 每 5 分钟只读核对本轮会话的用量明细。主站和从站各保留明细，结束后通过 `collect.py` 下载并比较 `site:` 导入 ID、tokens 和费用，不将单纯传输 200 当成已入账。

数据保存在各机专用目录的 `evidence.sqlite3`、`status.json` 和最终 `report.json`。SQLite WAL 持久化，不保存 Key、上游凭据、请求正文或模型推理内容。Key 单独保存到从站 `api-key`，权限 0600；主站观测程序不需要 Key。同步日志与 access log 只保存允许字段，工具错误先脱敏。

## 启动与停止

从站和主站分别准备 `config.json`（不含凭据），其中 `role` 为 `secondary` 或 `primary`：

```json
{
  "run_id": "site-soak-36h-YYYYMMDDTHHMMSSZ",
  "role": "secondary",
  "model": "deepseek-flash-local",
  "start_at": "2026-11-18T15:00:00+00:00",
  "end_at": "2026-11-20T03:00:00+00:00",
  "settlement_grace_s": 120,
  "api_key_id": 710,
  "secondary_url": "http://127.0.0.1:8080",
  "primary_url": "https://api.kxyai.top",
  "nginx_access_log": "/var/log/nginx/api.kxyai.top.access.log"
}
```

以独立 systemd 系统服务运行：

```ini
[Unit]
Description=Sub2API finite site connectivity observation
After=network-online.target docker.service
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=xs
WorkingDirectory=/home/xs/sub2api-site-soak/RUN_ID
ExecStart=/usr/bin/python3 -u /home/xs/sub2api-site-soak/RUN_ID/soak.py run --directory /home/xs/sub2api-site-soak/RUN_ID
Restart=on-failure
RestartSec=10
TimeoutStopSec=120
KillMode=control-group
UMask=0077
MemoryMax=512M
CPUQuota=20%
Nice=10
IOSchedulingClass=idle
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

主站更换为 root 用户和专用目录。先确认该用户已有 Docker 操作权限，且 Nginx 日志可读。只启停本次监控服务，不能误操作 `sub2api`。

查看 `status.json` 或 `systemctl status <本轮服务名>`。在运行目录创建 `STOP`，或 `systemctl stop <本轮服务名>` 可停止；不会停止 Sub2API。提前停止会报告 `stopped`，不能冒充 36 小时完成。恢复时删除 `STOP` 后重启监控服务，原截止时间不延长。`api-key` 在提前停止时保留供恢复使用；确定取消后可手动删除。

监控机器、Docker 日志或采集程序本身失联时，缺口会单独记录。双机证据应结合判断：主站无到达记录可能是网络或发送端问题；主站已完整响应但从站未安装可能是返回链路、连接或安装故障。一次 36 小时未复现不能证明未来绝不发生。

本机运行 `python3 collect.py --manifest <本轮 manifest.json>` 下载状态摘要；加 `--full` 下载完整报告并自动比较两端账单。SSH 不可达会明确显示采集失败，不复用旧报告冒充当前状态。程序不向聊天、邮箱或其他外部服务发送通知；异常留在状态、事件库和 systemd journal 中，供当前聊天后续检查。

## 验证

```sh
cd deploy/site-soak
python3 -m unittest -v test_soak.py
```

覆盖流终态、HTTP 200 流内错误、缺失 EOF、硬超时、日志解码、去重、1025 秒同步缺口、阈值恢复、重启保持原期限、Nginx 轮转及凭据字段过滤。只创建本机临时 HTTP 监听，不创建 Docker 网络，不占用公司内网路由。
