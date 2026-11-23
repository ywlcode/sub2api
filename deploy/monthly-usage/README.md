# 使用月报

在 boscserver（主站）和 ecosserver（独立站）**分别**安装。每站在北京时间每月 1 日 06:15 读取自己的 PostgreSQL，将上一本地自然月已入账的请求按天、用户 ID、请求模型汇总，发送一封邮件和一个 Excel。不需要改业务容器或数据库结构；不要在 xs199 安装：从站已经补交的用量在主站账本中。

每份工作簿只有三张表：

| Sheet | 粒度与口径 |
|---|---|
| 明细 | 北京时间日期 × 用户 ID × 请求模型；含用户名、当前真实姓名、当前所属部门和日志的实际使用分组。 |
| 部门月汇总 | 每个部门一行，含活跃用户数。按导出时的完整部门路径归属，每人只计一次；空部门汇入“未填写部门”。 |
| 用户月汇总 | 每个活跃用户 ID 一行，跨天与模型相加；同名账户分开。 |

三张表共用同一份月度快照，显示请求数、输入 Token、输出 Token、缓存读取 Token、总 Token 和用户扣费（美元）。总 Token 沿用账本口径，包含未单列的缓存创建量。超过 Excel 15 位有效数字的个别值直接在原单元格以文本保真，不另加列。费用只取 `actual_cost`，是含订阅用量的用户实际计费等值，不代表现金支付流水。用户资料取导出时值；不导出账户邮箱。请求模型缺失时回退到日志记录的模型。请求数不含未记入用量账本的失败尝试；已丢失的明细或尚未入账的请求不分摊到部门和用户。月报包含用户身份资料，须限制归档和邮件的访问。

## 安装、首次生成与补发

要求 Linux、systemd、Docker 和 Python ≥ 3.9（含亚洲时区数据），本机已有 Sub2API 数据库容器 `sub2api-postgres`，Sub2API 后台已配置 SMTP。邮件读取本站当前 SMTP 设置，465 使用隐式 TLS，其他端口强制 STARTTLS；仅向单独配置的运维收件人发信。任务需要在本机读 Docker socket，仅以管理员身份安装受信任的源码。

1. 创建 `/etc/sub2api-monthly-usage.json`，root:root、0600，内容形如 `{"site":"boscserver","first_month":"2026-10","recipient":"owner@example.org"}`。独立站填 `ecosserver`。起始月必须是计划补发的第一个月。收件地址仅存在本机私有配置，不提交到 Git。
2. 在本站运行 `sh install.sh`，再用 `systemctl list-timers sub2api-monthly-usage.timer` 确认下次触发。任务使用北京时间，不依赖宿主机时区。
3. 立即导出已结束的月份（不发信）：`python3 -B /opt/sub2api-monthly-usage/report.py render 2026-10`。归档位于 `/var/lib/sub2api-monthly-usage/Sub2API-<site>-YYYY-MM.xlsx`，目录 0700，文件 0600；临时 CSV 在正常退出后删除。手动发送：`python3 -B /opt/sub2api-monthly-usage/report.py send 2026-10`。
4. 立即依计划补发所有欠报：`systemctl start sub2api-monthly-usage.service`。停机错过执行由持久定时器补触发，程序从起始月份检查每个已结束月份，已受理的不重复发送。用 `journalctl -u sub2api-monthly-usage.service` 查看结果。

本机发送记录为同名 `.delivery.json`：`accepted` 代表发件 SMTP 服务器已受理；不证明邮件已进入收件箱。文件锁串行化手工与定时任务。认证、连接或明确 SMTP 拒绝等可重试错误由 systemd 一小时后重试。若在传送内容阶段断线或进程在受理记录落盘前退出，状态保留为 `sending` 并阻止同月自动重发。先按 Message-ID 核实邮件侧记录：确认没收到再由管理员移走该月标记并运行 `send`；已送达则保留记录。修订同月报告时，用 `report.py render YYYY-MM --revision 2`（按当前账本重算），核对后运行 `report.py send YYYY-MM --revision 2`。修订版附件和受理标记使用独立的 `-r2` 后缀，正常定时任务不会重复补发它。仅调整格式时，可沿用封存明细制作修订附件，避免账本保留策略影响历史数字。可用 `--note` 在修订邮件正文说明已知统计缺口。归档含用户身份资料，请限制访问，按本地保留策略清理附件并保留小型发送标记。

卸载定时任务：`systemctl disable --now sub2api-monthly-usage.timer`，停止同名 service 后移除专用的两个 unit 文件，执行 `systemctl daemon-reload`。归档和投递状态需要单独保留或按需清理。

## 验证

在本目录运行 `python3 -B -m unittest discover -v`。SQL 集成测试需要已缓存的 `postgres:16` 镜像，在独立无网络容器内验证真正的 PostgreSQL 查询。其余用例不需访问生产或发送真实邮件。
