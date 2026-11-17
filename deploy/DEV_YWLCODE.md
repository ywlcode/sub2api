# dev-ywlcode 分支维护与 Docker 发布

本指南用于 `ywlcode/sub2api` 的自用分支 `dev-ywlcode`：在开发机同步上游、测试和构建镜像，向 `ecosserver`、`boscserver` 的 `/root/sub2api-deploy` 上传同一镜像，再先后更新两机应用容器。PostgreSQL、Redis 继续使用现有 Docker 容器及数据目录。

**强制顺序：准备并校验目标镜像 → ecosserver 切换并完成真实请求验收 → boscserver 主站切换并验收 → 配对从站切换并验收。** 双机镜像必须先上传完成；涉及主从升级时，从站候选镜像也应提前准备好。Codex 的模型服务依赖 boscserver，禁止双机同时切换，也禁止在 ecosserver 验收完成前切换 boscserver。主从对内始终是主站先切换，旧从站需在混跑窗口继续兼容工作。详见 [部署顺序要求](README.md#强制部署顺序双机上传先-ecosserver后-boscserver)。

## 仓库、分支与发布边界

| 名称 | 用途 |
| --- | --- |
| `upstream` | `https://github.com/ranxi2001/sub2api.git`，获取上游更新 |
| `upstream/production` | rebase 的目标，以 fetch 后的远端跟踪引用为准 |
| `origin` | 自己的 `ywlcode/sub2api` fork；当前通过 HTTPS 获取、SSH 推送 |
| `dev-ywlcode` | 在上游基础上保留自用功能和维护文档 |

不要把本地 `production` 或 `origin/production` 当成已经同步的最新上游。自用修改按功能保持独立提交，减少无关格式调整；上游已实现等价功能时，核对行为和数据兼容后再决定是否移除本地补丁。

rebase 会改变被重放提交的 SHA，已经构建或部署的镜像不会随分支移动而更新。已部署提交应保留独立、不可移动的标签，例如 `deploy-ywlcode-<提交短 SHA>`，并保留对应镜像。不要用 `v*` 标签代替自用部署标记，仓库的 Release 工作流会匹配这类标签。

## 本机 Docker 测试网络约束

**本机测试不得占用 `node*`、`open*` 或其他公司内网地址所在的网段。** Docker 网桥创建的直连路由可能优先于公司网关，即使容器已经停止也会继续影响 SSH。`internal: true` 只限制容器网络对外访问，不能避免网段冲突。

| 已知用途 | 必须避让的网段 |
| --- | --- |
| `node*`（如 node033、node036） | `172.19.0.0/16`，已配置主机位于 `172.19.20.*` |
| `open*`、EDA 机器 | `172.28.0.0/16`，已配置主机位于 `172.28.9.*`、`172.28.10.*` |
| 开发机公司网卡 | `172.38.8.0/22`，包括开发机 `172.38.8.109` |

本机**新建测试网络统一避开整个 `172.16.0.0/12`**，同时避开表中的公司接入网段及实际 LAN、Wi-Fi、VPN 和 SSH 目标网段。上表是已知禁用范围，不是完整公司网络清单；不能认为某个 `10.*` 或 `192.168.*` 网段天然安全。

1. 启动前读取 `~/.ssh/config` 及其 Include，核对 `ip -4 route show table all`、`ip -6 route show table all`、`ip rule show` 和全部 `docker network inspect` 的 IPAM。公司远端网段可能只有默认路由，不能仅凭路由表中没有专用条目就认定可用。
2. 为本轮项目显式配置经核对的小网段（按实际容器数量选择 `/27`、`/24` 等），记录项目、网段和现有路由。Compose 必须设置 `networks.*.ipam.config[].subnet`；Testcontainers 和临时测试容器也须使用已核对的网络。不能依赖 Docker 自动选段；无法控制网络创建的测试应放到独立环境执行。
3. 启动前后分别执行 `ip route get 172.19.20.33`、`ip route get 172.19.20.36`、`ip route get 172.28.9.106`，确认仍走预期公司网卡或 VPN，而不是 `docker0`、`br-*`，并测试对应 TCP 22 端口。若原本就不通，记录基线，不能把测试后的相同失败宣称为连通性通过。出现新冲突立即清理本轮网络，不能用改 SSH 地址或添加更具体的临时路由掩盖。
4. 实验结束执行对应项目的 `docker compose down`，或按本轮标签移除容器和专属网络；仅 `stop` 不算完成清理。需要保留的数据库命名卷和证据继续保留，未经确认不使用 `down -v`。最后再次检查 Docker 网络、路由和 SSH 端口。

不得为了清理测试而执行全局 `docker system prune`／`network prune`、重启 Docker 或重建正式 `sub2api-bosc-secondary` 网络。已有正式从站不属于测试清理范围；调整 Docker daemon 默认地址池也不属于普通测试步骤。

## Cyber 平台隔离与固定提示

所有平台的上游 `cyber_policy` 都保留内部审计、脱敏证据和运维错误记录。只有有效平台为 OpenAI 的命中会创建本地会话封禁；检查已有封禁时也按当前有效平台判断，历史无平台标记仅对 OpenAI 生效。DeepSeek 等平台的命中不创建本地会话封禁，用户切换到这些平台不受 OpenAI 会话标记阻拦。

本地封禁及识别出的 Cyber 错误统一使用固定提示 `cyber-security, 请更换无风控模型或开启新会话`。HTTP、SSE 和 WebSocket 都不向用户透传供应商的 message、code 或原始正文，也不调用错误透传规则；内部分类和审计保留脱敏后的原始证据。多行 SSE 先收齐完整事件再识别，不能逐行漏过错误。

普通非 Cyber 上游 HTTP 403 继续按平台、启用状态和优先级匹配既有错误透传规则。会话已被封禁的拒绝不重试、不冷却上游账号，也不算一次新的 Cyber 命中。

修改或同步相关代码后，可运行专项回归：

```bash
cd backend
go test -tags=unit ./internal/service ./internal/handler -run 'TestForbidden|TestCyberPolicy|TestUpstreamSessionBlocked|Test.*CyberPolicy|Test.*Passthrough.*Platform' -count=1
```

## 将 dev-ywlcode rebase 到上游 production

以下命令从仓库根目录、在同一个 Bash 终端中分块执行。每块成功后再继续；不要在生产部署目录执行 rebase。

### 主从站的字段兼容要求

以下要求适用于每次上游升级。实现已改为固定字段契约，配置和事件分别校验；新增字段是否可以安全混跑，仍须逐项审查并通过真实新旧二进制测试，不能仅凭协议版本相同判断。历次字段分类和测试口径见 [2.21.5 升级记录](REBASE_2.21.5.md) 与 [2.21.6 升级记录](REBASE_2.21.6.md)。架构职责见 [主从站说明](SITES.md)，仓库工作约定见 [AGENTS.md](../AGENTS.md)。

同步协议维护明确、固定的字段清单，独立于物理表的所有列及列顺序。禁止因为 `SELECT *`、整行 `to_jsonb` 或动态列枚举，让未审查的新字段自动成为同步协议。配置行、账号 `extra`/`credentials`、settings 和网关配置中的新增语义都需要审查，不能只比较 SQL 列名。

每次 rebase 从共同基线到目标 SHA 逐项检查新增、删除、改名、类型、默认值及语义变化，留下可复核的字段清单：

| 字段类别 | 同步与升级要求 |
| --- | --- |
| 本地路由、鉴权、权限、并发、余额、订阅、计费所需字段 | 必须包含在同步契约中，并接入从站实际读取逻辑及缓存失效；仅落库不等于功能支持 |
| 可选配置字段 | 旧从站允许忽略不认识的字段；新从站对缺失字段使用经过审查的兼容默认值 |
| 用量、错误、耗时等可选明细字段 | 新主站接收旧从站上报时允许缺失，使用明确默认值或保留未知；不能把缺失耗时记成实际测量的 0 毫秒 |
| 代理、连接池、数据库、Redis、存储和资源预算等节点配置 | 留在本节点，避免把主站网络或内存配置复制到从站 |
| 后台展示、登录支付等无需从站执行的字段 | 明确标记不参与同步；添加它们不应导致从站失去租约 |

字段清单至少记录来源、用途、分类、是否必需、缺失默认值、旧从站行为、消费者及缓存、回归用例。新增字段必须由自动审查/测试提示并明确分类；不能以“数据库已经有列”或“JSON 会忽略未知字段”代替审查。必需字段缺失、类型不兼容和不支持的破坏性语义应明确报错。身份、权限、余额和扣费依据不得因兼容处理变成零费用或无限额度。

固定升级顺序为 **新主站 + 旧从站 → 新主站 + 新从站**：

1. 主站先切换后，旧从站须能继续安装它认识的配置、忽略新增可选字段并正常续租。现有鉴权、路由和计费行为保持正确。
2. 从站稍后切换时，必须自动补齐并应用新增同步字段。若旧从站已推进增量游标，不能因此永远漏装这些字段；用经过验证的全量重装或投影版本机制解决，并核对首次同步大小、分页和耗时。
3. 新主站接收旧从站请求明细时，对缺失可选字段采用明确默认值或不写入该列以使用数据库默认值；区分缺失与显式 `null`，更新旧记录时不能意外覆盖已有新字段。
4. 配置兼容与账单事件兼容分别检查。配置有差异不应阻塞仍可正确解析的历史账单；保留原始事件、摘要、命名空间、回执和扣费去重，不因补默认值改变同一事件的身份或重算历史费用。
5. 如果新增路由/权限/限额无法由旧从站等价执行，升级窗口内保留经验证的旧语义，新增能力在从站具备支持后启用；无法兼容时明确拒绝不支持的处理并报告原因，不能让旧从站静默绕过新规则。

兼容测试至少覆盖：新主站增加可选列而旧从站仍工作；增加 JSON/策略字段后旧从站忽略；旧从站省略新增明细字段而新主站正确导入；从站升级后补齐字段并影响真实路由；必需字段缺失被拒绝；列顺序不同不影响兼容；旧库升级与全新从站安装；上报丢 ACK、重复/乱序事件、两端重启后不重扣不丢账；主站不可达后 60 秒停服并在恢复后续传。从站停机不应影响主站模型请求。仅允许新增可选字段时保持兼容版本；破坏性变化需要独立协议/语义版本及明确的过渡策略。

合并时还需核对鉴权快照版本撞号、新字段是否进入缓存、Wire 生成代码、新后台任务的 `RunsBackgroundJobs()` 门控、网关中间件顺序、动态配置读取和请求体内存预算。保留火山三协议隔离、thinking/reasoning、summary/compaction、call_id、工具历史及首内容立即 flush 的回归。已部署迁移的完整文件名和内容不得改变。

### 升级前后的性能对照

**每次 rebase 都要对比升级前定制版与合并后的候选版，不能只与上游原版比较，也不能只跑单元测试或 CI。** 在开始重放前保存旧 SHA、不可变镜像、有效配置、工具链和实验数据快照；没有基线时，从保留的旧 SHA 在同条件实验环境补测。

- 两版使用相同硬件、容器 CPU/内存限制、数据库规模、配置、账号路由、上下文、输出预算、思考强度、协议及网络条件。记录缓存冷启动与预热后的结果，重复采样确认差异；不能通过减少功能、放宽额度或改变并发口径来制造性能提升。
- 使用隔离模拟上游测量本地处理开销，再用真实上游做端到端验证，区分服务开销与公网、上游抖动。真实测试保留相同 TPM 限制和启动节流，记录实际在途峰值。
- 覆盖主站直连、从站本地 DeepSeek、从站 gzip 转发 GPT、并发模型请求下的配置续租与账单上报。按 400 个合成用户、从低并发逐步到 200 并发并探测 200 以上的负载；包含约 100K 输入上下文和固定输出预算。具体模型、推理强度、输出 token 数在测试前冻结并写入结果。
- 每条请求记录上传、排队、首个可见推理/正文内容和完整响应耗时，汇总平均、P50/P95/P99、吞吐、错误率及取消/流中断。首字口径须一致，不能把空 SSE 条目或 HTTP 200 当成成功输出。
- 采集 CPU、RSS/峰值内存、GC、数据库 QPS/等待、鉴权/配置缓存命中、连接数，以及同步全量/增量压缩前后字节、续租延迟和 outbox 积压/追平速度。特别检查旧优化是否被冲突处理覆盖、新正文预算是否引入 503、后台任务是否误在从站启动。
- 在独立链路注入延迟、丢包、断流，验证模型转发不拖住续租、账单重传不重复扣费、单方宕机行为和恢复时间。不得在生产链路或主机网卡注入故障。
- 使用 Codex、Claude Code、OpenCode、dsh 验证实际工具调用与压缩；分别覆盖 GPT 转发和 DeepSeek 本地处理，包含 OpenAI 与非 OpenAI 的不同压缩路径。执行前核对当前客户端能力，不宣称不存在的协议组合通过。

结果必须列出旧/新 SHA、环境、负载、样本数、指标绝对值与差值、原始证据位置和未覆盖项。测试前确定验收口径及允许波动；明显退化需定位、修复并复测。没有集成压测数据时只能报告尚未验证，不能承诺无性能下降。实验使用独立 Docker 项目及数据，不影响现有站点；参考 [主从故障实验](tests/site-lab/README.md)，它本身不代表已覆盖本节全部场景。

### 1. 检查工作区并获取远端状态

```bash
git switch dev-ywlcode
git status --short --branch
git remote -v
test -z "$(git status --porcelain)"
```

最后一条失败时，先单独提交或妥善保存工作区内容，不要依靠自动 stash 混入未确认修改。

```bash
git fetch upstream production
git fetch origin dev-ywlcode
ywlcode_remote_before=$(git rev-parse origin/dev-ywlcode)
git merge-base --is-ancestor "$ywlcode_remote_before" HEAD
```

最后一条必须成功，表示远端自用分支的提交已经包含在本地。若失败，先核对本地和远端的差异；不能继续强推覆盖远端。`ywlcode_remote_before` 必须保留为本次检查过的值。

默认从已获取的 upstream/production 固定目标。如果用户指定 `v2.21.4` 等标签，先获取并核对标签，再把下面的引用改为该标签；记录它与最新 production 的差异，不能自行替换成另一个版本。

```bash
ywlcode_target_ref=upstream/production
ywlcode_target_sha=$(git rev-parse "${ywlcode_target_ref}^{commit}")
ywlcode_before_sha=$(git rev-parse HEAD)
git show -s --format='%H %cs %s' "$ywlcode_target_sha" "$ywlcode_before_sha"
```

此时完成上述字段审查并保存升级前性能基线，再开始重放。

### 2. 保留恢复点并开始 rebase

```bash
ywlcode_old_base=$(git merge-base HEAD "$ywlcode_target_sha")
ywlcode_backup="backup/dev-ywlcode-$(date -u +%Y%m%dT%H%M%SZ)"
git branch "$ywlcode_backup"
git log --oneline "$ywlcode_target_sha..HEAD"
git rebase "$ywlcode_target_sha"
```

发生冲突时，逐个检查、编辑和暂存已解决的文件，再执行：

```bash
git rebase --continue
```

需要放弃本次 rebase 时执行：

```bash
git rebase --abort
```

不要用 `--skip` 消除没有理解的冲突，它会跳过整个提交。也不要整文件盲选 `ours` / `theirs`；rebase 中这些名称的含义容易与普通合并混淆，且可能丢失上游修复或自用规则。

当前内容审查定制的主要冲突点及复核目标：

| 位置 | 需要保留和重新验证的行为 |
| --- | --- |
| `backend/internal/service/content_moderation*.go` | `ban_scope` 兼容旧配置；OpenAI 专属封禁不把整个账号设为 disabled |
| `backend/internal/repository/content_moderation_user.go` | 事务计数、封禁归属、解封版本围栏；不解除其他来源的账号禁用 |
| `backend/internal/handler/security_audit_helper.go`、`backend/internal/server/routes/gateway.go` | 实际平台解析后的拦截、路由别名、无提示词审计接口及 WebSocket 每轮检查 |
| 管理页、API 类型和中英文文案 | 范围选项、状态读回和解封入口一致 |
| 数据库迁移及 SQL 测试夹具 | 文件名、校验和、字段定义及新旧数据库升级路径兼容 |

“请求惩罚策略”是另一套独立机制，不属于本次内容审查定制的修改范围；不要为解决冲突顺带改变其处罚语义。

### 3. 检查差异并运行回归

```bash
git range-diff "$ywlcode_old_base..$ywlcode_backup" "$ywlcode_target_sha..dev-ywlcode"
git diff --check
```

工具链和依赖以当前的 [go.mod](../backend/go.mod)、[前端 package.json](../frontend/package.json)、[Dockerfile](../Dockerfile) 和 [贡献指南](../CONTRIBUTING.md) 为准，先确认 `go version` 和 `pnpm --version` 可用且版本匹配。下面是内容审查定制的回归集合；若上游更新还涉及其他模块，应补充对应检查。

```bash
(
  cd backend
  go test -tags=unit ./internal/service ./internal/repository ./internal/handler ./internal/server/routes ./migrations \
    -run 'Test.*(Moderation|SecurityPlatform|GatewayRoutes|GatewayPOSTRoute|ResponsesWebSocketHas)' -count=1
)
```

数据库集成测试必须在可用的 Docker 测试环境执行，不能使用生产数据库。确认测试没有因为 Docker 不可用而跳过：

```bash
(
  cd backend
  go test -tags=integration ./internal/repository \
    -run 'TestModeration(Platform|EmailMigration|MailManagement)' -count=1 -v
)
pnpm --dir frontend install --frozen-lockfile
pnpm --dir frontend exec vitest run \
  src/views/admin/__tests__/RiskControlView.spec.ts \
  src/components/admin/moderation/__tests__/ModerationUsers.spec.ts \
  src/i18n/__tests__/localeKeyCompleteness.spec.ts
pnpm --dir frontend run build
```

测试名称和路径可能随上游变化，更新命令时确认实际运行了目标用例，不能把 `no tests to run` 或跳过结果记为通过。

以上内容审查用例只是其中一部分。还必须完成本节规定的主从字段兼容矩阵、故障/账单回归、真实客户端验证及升级前后性能对照，并记录结果。

### 4. 推送重写后的分支

确认差异和测试结果后，在保留上述变量的同一终端执行：

```bash
git push --force-with-lease="refs/heads/dev-ywlcode:${ywlcode_remote_before}" origin dev-ywlcode
```

这里使用明确的旧远端 SHA，避免后台 fetch 改变默认 lease 的判断依据。若推送被拒绝，说明远端已变化，应重新核对；不要改用无保护的 `--force`。普通新增提交、未执行 rebase 时，正常 `git push origin dev-ywlcode` 即可。

## 数据库迁移不能随 rebase 重编号

本项目在 `schema_migrations` 中按**完整文件名和内容校验和**跟踪迁移，并按文件名排序执行。上游也出现数字前缀 `307`，不等于必然冲突；需要检查完整文件名、依赖顺序和字段语义。

`307_content_moderation_platform_bans.sql` 添加：

- `content_moderation_user_state.openai_blocked`，默认 false。
- `content_moderation_logs.ban_scope`，默认 `all`。

迁移一旦在任一需继续维护的环境执行，就保留其文件名和内容。不要为了与上游编号连续而改名、删除或改写，也不要修改数据库中的 checksum 绕过启动校验。之后的结构调整新增迁移；遇到上游同名文件或同字段不同语义时，先设计兼容方案，不能简单选择一边。

## 本地构建分支镜像

构建前先提交所有应包含的源码，确认位于 `dev-ywlcode` 且工作区干净。镜像标签、程序版本和 revision 都绑定实际构建提交，避免浮动的 `latest` 或仅有分支名的标签。

### 按 Wi-Fi 状态选择代理或直连 SSH

**连接 Wi-Fi 时可使用本机 `127.0.0.1:10800` 代理加速上传；Wi-Fi 断开时不用 10800，直接通过 SSH／SCP 连接服务器并继续部署，无需等待 Wi-Fi 恢复。** 10800 不可用只影响这条代理路径，不阻止直连 SSH。

当前 Xray 的 10800 入站配置为 SOCKS5，SSH 可使用 `nc -X 5`。本地端口仍在监听或国内目标可访问，均不能证明海外代理链路可用；选用代理后应探测实际部署目标，失败则回退直连。

在同一个 Bash 终端中设置以下传输参数，后续 SSH／SCP 共用该数组。直连显式关闭 `ProxyCommand`、`ProxyJump` 及已有复用连接，避免继承 SSH 配置中的代理：

```bash
ywlcode_direct_ssh=(-o BatchMode=yes -o ConnectTimeout=10 -o ProxyCommand=none -o ProxyJump=none -o ControlPath=none)
ywlcode_ssh_options=("${ywlcode_direct_ssh[@]}")
if LC_ALL=C nmcli -t -f TYPE,STATE device status 2>/dev/null |
  awk -F: '$1 == "wifi" && $2 == "connected" { connected=1 } END { exit !connected }'; then
  ywlcode_ssh_options=(-o BatchMode=yes -o ConnectTimeout=10 -o 'ProxyCommand=nc -X 5 -x 127.0.0.1:10800 %h %p' -o ProxyJump=none -o ControlPath=none)
  for ywlcode_host in ecosserver boscserver; do
    if ! ssh "${ywlcode_ssh_options[@]}" "$ywlcode_host" 'printf "deploy-proxy-ok\n"'; then
      ywlcode_ssh_options=("${ywlcode_direct_ssh[@]}")
      break
    fi
  done
fi
for ywlcode_host in ecosserver boscserver; do
  ssh "${ywlcode_ssh_options[@]}" "$ywlcode_host" 'printf "deploy-ssh-ok\n"' || exit 1
done
```

Wi-Fi 未连接时直接探测 SSH，不探测或重试 10800。只有实际选用的 SSH 路径也不可达时，才处理服务器连接问题。若构建或下载工具另行配置了 10800 代理，Wi-Fi 断开时也应为该操作取消代理；SSH 的传输参数不会修改这些工具的配置。

```bash
git switch dev-ywlcode
git status --short --branch
test -z "$(git status --porcelain)"
ywlcode_commit=$(git rev-parse HEAD)
ywlcode_sha=$(git rev-parse --short=9 HEAD)
ywlcode_version=$(tr -d '\r\n' < backend/cmd/server/VERSION)
ywlcode_image="sub2api-ywlcode:${ywlcode_sha}"
ywlcode_archive="/tmp/sub2api-ywlcode-${ywlcode_sha}.tar.gz"
```

以下构建命令适用于当前 Linux 开发机：本机代理使用 loopback 地址，因此让构建步骤使用 host 网络并传入现有代理变量。否则容器中的 `127.0.0.1` 指向构建容器自身，曾导致 Alpine 依赖下载报 `Connection refused`。没有本机代理、或在其他环境构建时，应按实际网络去掉或调整这些网络参数，不必修改仓库 Dockerfile。

```bash
docker buildx build \
  --platform linux/amd64 \
  --network host --allow network.host \
  --build-arg HTTP_PROXY --build-arg HTTPS_PROXY --build-arg ALL_PROXY \
  --load --progress plain \
  --build-arg VERSION="${ywlcode_version}-ywlcode.${ywlcode_sha}" \
  --build-arg COMMIT="$ywlcode_commit" \
  --label org.opencontainers.image.source=https://github.com/ywlcode/sub2api \
  --label org.opencontainers.image.revision="$ywlcode_commit" \
  -t "$ywlcode_image" .

docker run --rm --network none "$ywlcode_image" -version
docker image inspect "$ywlcode_image" \
  --format '{{.Os}}/{{.Architecture}} revision={{index .Config.Labels "org.opencontainers.image.revision"}}'
```

确认输出是 `linux/amd64`，程序 commit 和 revision 都与本次构建 SHA 一致。`-version` 只验证程序和镜像启动入口，不代表已经在生产数据库上完成升级或业务验收。

### 发布镜像禁止携带本机代理

**构建、下载和 SSH 上传的代理仅用于对应操作，发布镜像的运行环境不得预设 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY` 及其小写形式。** Wi-Fi 连接时仍可按上一节使用 `10800`；这里检查的是镜像中的默认环境变量。

2026-11-15 部署曾复用带有 `127.0.0.1:10800` 代理变量的本机旧镜像，导致服务器上的 `wget` 健康探测误走不存在的代理。程序 `/readyz` 已正常响应，Docker 健康状态仍不通过，最终触发自动回退。修正产物 `737ea559f-r2` 已清空这些变量。

优先使用仓库 Dockerfile 构建。复用旧运行层会继承它的 `ENV`；`docker commit` 也可能保留容器环境，包括 Docker 客户端 `~/.docker/config.json` 自动注入的代理。仅在宿主机执行 `unset`、只改变 SSH 传输路径或补充 `NO_PROXY`，都不能代替检查最终镜像。

镜像变量已清空也不代表 `docker run` 启动后的环境一定为空：本机 Docker 客户端仍可能自动注入代理。因此下面的镜像检查和服务器实际探测都要执行，不要将本机临时测试容器直接 `commit` 为发布产物。

导出归档前，以及两台服务器导入镜像后，在各自主机上将 `ywlcode_image` 设为候选镜像标签，执行以下检查。失败时先修正镜像，不进入应用切换；命令只输出变量名，不打印可能含凭据的代理地址：

```bash
docker image inspect "$ywlcode_image" | python3 -c '
import json
import sys

image = json.load(sys.stdin)[0]
proxy_names = {"http_proxy", "https_proxy", "all_proxy"}
bad = []
for entry in image["Config"].get("Env") or []:
    name, _, value = entry.partition("=")
    if name.lower() in proxy_names and value:
        bad.append(name)
if bad:
    sys.exit("禁止发布：镜像带有非空代理默认值：" + ", ".join(bad))
print("镜像代理检查通过")
'
```

确需复用已有运行层时，在**最后一个 `FROM` 之后**显式覆盖继承值；清理步骤必须进入最终镜像，不能只写在构建阶段：

```dockerfile
ENV HTTP_PROXY="" HTTPS_PROXY="" ALL_PROXY="" \
    http_proxy="" https_proxy="" all_proxy=""
```

重建使用新的不可变标签和归档名称，重新校验程序 commit、revision、二进制与归档 SHA256，再重复上述代理检查。不要覆盖旧发布标签或改写回退镜像。确有需要的服务器运行代理由该服务器配置显式提供，并核对 Compose、`.env` 和 Docker 客户端配置没有重新注入开发机地址。

双机切换前，分别在服务器上用候选镜像检查实际运行环境，并使用镜像内的 `wget` 探测现有服务。此命令适用于现有 Linux、宿主机 IPv4 8080 端口部署；临时容器使用 host 网络访问仍在运行的旧服务，不启动第二个应用，也不挂载数据库或数据目录。使用 `127.0.0.1` 避免宿主机 `localhost` 解析为 IPv6 后，误测未监听的 IPv6 端口。不要在探测命令中临时覆盖代理变量，以免掩盖镜像或 Docker 客户端注入的问题：

```bash
docker run --rm --pull never --network host --entrypoint sh "$ywlcode_image" \
  -c '
for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; do
    if [ -n "$(printenv "$name")" ]; then
        echo "禁止发布：运行时注入了 $name" >&2
        exit 1
    fi
done
wget -q -T 5 -O /dev/null http://127.0.0.1:8080/health
'
```

如果候选镜像的探测工具或宿主机发布端口改变，应相应调整命令。这里仅验证镜像中的探测工具和网络环境；切换后仍须确认新容器的原始 `Healthcheck.Test` 通过并达到 healthy、`/readyz` 正常，再完成真实用户响应验收。

## 阶段 A：双机导入镜像

以下步骤只上传、校验和加载镜像，不修改运行配置，不切换应用。任何部署都必须完成两台服务器的镜像准备。

### 导出、传输和校验

```bash
set -o pipefail
docker image save --platform linux/amd64 "$ywlcode_image" | gzip -1 > "$ywlcode_archive"
```

上一条必须成功后再继续。校验文件使用相对文件名，便于在服务器的部署目录直接验证：

```bash
(
  cd /tmp
  sha256sum "sub2api-ywlcode-${ywlcode_sha}.tar.gz" \
    > "sub2api-ywlcode-${ywlcode_sha}.tar.gz.sha256"
)
scp "${ywlcode_ssh_options[@]}" "$ywlcode_archive" "${ywlcode_archive}.sha256" ecosserver:/root/sub2api-deploy/ &
ywlcode_ecos_upload=$!
scp "${ywlcode_ssh_options[@]}" "$ywlcode_archive" "${ywlcode_archive}.sha256" boscserver:/root/sub2api-deploy/ &
ywlcode_bosc_upload=$!
ywlcode_upload_status=0
wait "$ywlcode_ecos_upload" || ywlcode_upload_status=1
wait "$ywlcode_bosc_upload" || ywlcode_upload_status=1
test "$ywlcode_upload_status" -eq 0
```

两次上传都成功后，在两机分别执行校验及导入；任一步失败都不得进入应用切换：

```bash
(
  for ywlcode_host in ecosserver boscserver; do
    ssh "${ywlcode_ssh_options[@]}" "$ywlcode_host" "cd /root/sub2api-deploy && sha256sum -c sub2api-ywlcode-${ywlcode_sha}.tar.gz.sha256 && docker load -i sub2api-ywlcode-${ywlcode_sha}.tar.gz && docker run --rm --network none sub2api-ywlcode:${ywlcode_sha} -version && docker image inspect sub2api-ywlcode:${ywlcode_sha} --format '{{.Id}} revision={{index .Config.Labels \"org.opencontainers.image.revision\"}}'" || exit 1
  done
)
```

两机文件校验、镜像加载和程序版本必须全部成功，镜像 ID、commit、revision 必须一致并与本地产物相符。镜像名是服务器本地已加载的标签，不需要先上传公共镜像仓库；`SUB2API_IMAGE` 也不能填写 GitHub 分支地址。

两机分别通过上文的[镜像代理检查与健康探测](#发布镜像禁止携带本机代理)后，才进入阶段 B。

## 阶段 B：先切换 ecosserver

先登录 `ecosserver`；本阶段不得在 `boscserver` 修改 `.env` 或重建应用。

### 修改当前服务器的 .env

只修改服务器 `/root/sub2api-deploy/.env` 中的 `SUB2API_IMAGE`，值必须与本次加载的镜像标签一致。例如已经构建过的功能提交：

```dotenv
SUB2API_IMAGE=sub2api-ywlcode:3f751ce65
```

此例中的 SHA 不是固定发布版本。下次构建了新提交，就填对应的新标签；不要把新产物继续标成旧 SHA。同一源码 SHA 也可能因为基础镜像或构建时间不同而生成不同产物，已验证的旧镜像和归档应保留；需要重做构建时使用额外构建标识区分，并记录新的归档校验和。

保持现有 Compose 服务名、host 网络、`127.0.0.1:8080` 监听、数据挂载和 Nginx 配置。不要覆盖 `.env` 或 `data/config.yaml`，不要重新生成现有 JWT、TOTP 加密密钥及数据库/Redis 凭据。它们不是新镜像构建的一部分，也不能提交到 Git。

仅修改 `.env` 不会改变现有容器。`docker compose restart sub2api` 也不能替换镜像或应用新的容器环境变量，需要下一阶段的重新创建。

### 切换当前服务器的应用

这一阶段与准备镜像、修改 `.env` 是独立操作。执行前确认将进行应用切换，可能短暂中断请求，现有流式和 WebSocket 连接也可能断开。本流程不自动备份 PostgreSQL 或 Redis；本次操作已明确选择不备份它们。

在服务器部署目录先记录依赖容器身份，并只读模拟更新范围：

```bash
cd /root/sub2api-deploy
docker inspect --format '{{.Name}} {{.Id}} {{.State.StartedAt}}' sub2api-postgres sub2api-redis
docker compose --dry-run up -d --no-deps --pull never sub2api
```

模拟输出应只涉及应用 `sub2api`。确认后再实际执行：

```bash
docker compose up -d --no-deps --pull never sub2api
```

- `sub2api` 指定唯一更新目标，`--no-deps` 跳过 PostgreSQL、Redis 等依赖服务，`--pull never` 要求使用本地已经加载的镜像。
- 不使用整套 `docker compose up -d`、整套 pull、`down -v` 或重新初始化脚本。它们超出应用更新范围；现有依赖容器使用的镜像 digest 与 `.env` 中的浮动标签也可能不同。
- **不重建数据库容器不代表数据库没有变化。** 新应用启动会连接 PostgreSQL 并自动执行尚未应用的迁移，正常运行时也会读写 Redis。当前定制增加的是 `307` 两个字段；未来 rebase 后需要重新审查全部新增迁移，不能假定永远只有 `307`。

### 切换后验证

```bash
docker compose ps
curl --fail --silent --show-error http://127.0.0.1:8080/health
docker exec sub2api /app/sub2api -version
docker inspect --format '{{.Name}} {{.Id}} {{.State.StartedAt}}' sub2api-postgres sub2api-redis
```

等待应用达到 healthy，确认健康接口成功、实际 commit 与目标镜像一致、数据库迁移成功，并与更新前记录比较 PostgreSQL/Redis 的容器 ID 和启动时间。`compose up -d` 返回成功不等于这些检查已经通过。需要日志时查看 `docker compose logs --tail 100 sub2api`，分享前先脱敏。

### ecosserver 的真实用户请求验收

服务检查通过后，还必须观察至少一个**新版启动后进入的真实用户模型请求**，确认上游处理和下游响应均正常结束。可以观察已有生产流量；主动发起请求时使用已获授权的测试用户／Key，不创建新凭据或修改账号策略来绕过验收。

发布记录至少包含：目标 commit、容器启动时间、请求 ID、请求开始及完成时间、模型、最终状态，以及能证明响应正常完成的日志／诊断证据。流式请求应有正常终止证据，且没有流中断、客户端取消或下游写入错误。记录证据时不保存 API Key、提示词或用户内容。

只有 `/health` 成功、模型列表可访问、首字已输出或用量已入库，都不算完整响应验收。没有用户请求、请求尚未结束或证据不足时继续等待，**不得进入阶段 C**；发现故障时先修复或回退 ecosserver，保持 boscserver 继续服务。

## 阶段 C：ecosserver 验收后再切换 boscserver

核对并保存阶段 B 的服务和用户请求验收证据后，才登录 `boscserver`，按阶段 B 的步骤修改其 `SUB2API_IMAGE`、dry-run 并只重建应用容器，再验证版本、健康、迁移及真实用户请求。

切换命令、健康检查及失败回退应通过独立 SSH／服务器端脚本执行，避免在 boscserver 模型 API 短暂不可用时依赖 Codex 再生成恢复命令。始终保留已验收的 ecosserver 和各机旧镜像；两台应用不得同时切换。

内容审查的“仅 OpenAI”需要在「内容审查 → 设置 → 命中通知 → 封禁范围」手动选择；升级不会自动改变旧配置或解除历史整号禁用。详细行为见 [内容审查用户管理](../docs/content-moderation-user-allowlists.md#内容审查封禁范围)。

## 回退与后续更新

保留上一个已验证镜像及其对应配置记录；回退时加载旧产物、将 `SUB2API_IMAGE` 改为它的实际标签，再只重新创建应用服务。先确认旧程序与已经升级的数据库兼容。保留新增字段不代表旧程序仍执行新策略：旧版不识别 OpenAI 专属封禁状态，回退需要同时复核审查配置和处罚行为，不自动删除迁移字段或修改 checksum。

首次切换前要核实实际运行版本。此次检查发现旧镜像标签写着 `2.17.1`，而容器内程序实际已替换为 `2.17.3`，并有二进制备份。因此仅保留那个 `2.17.1` 标签，不能保证回到当时实际运行的程序；需要准备并验证相应的回退产物。

之后统一通过本分支构建镜像更新。项目后台的一键更新/版本回退当前仍指向 `ranxi2001/sub2api` 的 Release，会替换容器内二进制，不能用它维护 `dev-ywlcode`，否则会丢失定制并再次产生镜像标签与实际程序不一致的问题。

## 本次操作记录（历史快照）

记录时间为 2026-10-26（服务器时区 Asia/Shanghai），仅反映本次已完成的准备操作，不代表未来实时状态：

| 项目 | 记录 |
| --- | --- |
| 镜像源提交 | `3f751ce6516ecc09adcf34f8da8255f7e6ab1017` |
| 已构建并加载的镜像 | `sub2api-ywlcode:3f751ce65` |
| 镜像程序版本 | `2.17.3-ywlcode.3f751ce65` |
| 服务器镜像归档 | `/root/sub2api-deploy/sub2api-ywlcode-3f751ce65.tar.gz` |
| 归档 SHA256 | `aa13e374d20bd35b6a2ef42a9b0ab83b9bce6507b3230665751559a14573acd5` |
| `.env` 中的应用镜像 | 已设置为 `sub2api-ywlcode:3f751ce65` |
| 应用切换 | 尚未实际执行阶段 B，只执行过 dry-run |
| 依赖服务 | PostgreSQL、Redis 容器保持原有 ID、启动时间，未备份或重建 |

本次文档提交会让 Git 分支继续前移，但上述镜像仍准确对应功能提交 `3f751ce65`；仅写文档不会重建镜像、更新服务器 `.env` 或启动新版本。

## 参考

- [Git rebase：冲突处理、继续与中止](https://git-scm.com/docs/git-rebase)
- [Git push：明确期望值的 force-with-lease](https://git-scm.com/docs/git-push)
- [Docker Compose dry-run](https://docs.docker.com/reference/cli/docker/compose/)
- [Docker Compose up 的服务选择、依赖与拉取策略](https://docs.docker.com/reference/cli/docker/compose/up/)
- [Docker Compose restart 不应用新的容器配置](https://docs.docker.com/reference/cli/docker/compose/restart/)
