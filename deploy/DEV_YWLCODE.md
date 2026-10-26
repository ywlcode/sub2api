# dev-ywlcode 分支维护与 Docker 发布

本指南用于 `ywlcode/sub2api` 的自用分支 `dev-ywlcode`：在开发机同步上游、测试和构建镜像，再更新 `boscserver:/root/sub2api-deploy` 中的应用容器。PostgreSQL、Redis 继续使用现有 Docker 容器及数据目录。

## 仓库、分支与发布边界

| 名称 | 用途 |
| --- | --- |
| `upstream` | `https://github.com/ranxi2001/sub2api.git`，获取上游更新 |
| `upstream/production` | rebase 的目标，以 fetch 后的远端跟踪引用为准 |
| `origin` | 自己的 `ywlcode/sub2api` fork；当前通过 HTTPS 获取、SSH 推送 |
| `dev-ywlcode` | 在上游基础上保留自用功能和维护文档 |

不要把本地 `production` 或 `origin/production` 当成已经同步的最新上游。自用修改按功能保持独立提交，减少无关格式调整；上游已实现等价功能时，核对行为和数据兼容后再决定是否移除本地补丁。

rebase 会改变被重放提交的 SHA，已经构建或部署的镜像不会随分支移动而更新。已部署提交应保留独立、不可移动的标签，例如 `deploy-ywlcode-<提交短 SHA>`，并保留对应镜像。不要用 `v*` 标签代替自用部署标记，仓库的 Release 工作流会匹配这类标签。

## 将 dev-ywlcode rebase 到上游 production

以下命令从仓库根目录、在同一个 Bash 终端中分块执行。每块成功后再继续；不要在生产部署目录执行 rebase。

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

### 2. 保留恢复点并开始 rebase

```bash
ywlcode_old_base=$(git merge-base HEAD upstream/production)
ywlcode_backup="backup/dev-ywlcode-$(date -u +%Y%m%dT%H%M%SZ)"
git branch "$ywlcode_backup"
git log --oneline upstream/production..HEAD
git rebase upstream/production
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
git range-diff "$ywlcode_old_base..$ywlcode_backup" "upstream/production..dev-ywlcode"
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

## 阶段 A：导入镜像并准备服务器配置

以下步骤会加载镜像、修改配置，但不会切换运行中的应用。

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
scp "$ywlcode_archive" "${ywlcode_archive}.sha256" boscserver:/root/sub2api-deploy/
ssh boscserver "cd /root/sub2api-deploy && sha256sum -c sub2api-ywlcode-${ywlcode_sha}.tar.gz.sha256 && docker load -i sub2api-ywlcode-${ywlcode_sha}.tar.gz && docker run --rm --network none sub2api-ywlcode:${ywlcode_sha} -version"
```

文件校验、镜像加载、服务器端程序版本必须都成功。镜像名是服务器本地已加载的标签，不需要先上传公共镜像仓库；`SUB2API_IMAGE` 也不能填写 GitHub 分支地址。

### 修改 .env

只修改服务器 `/root/sub2api-deploy/.env` 中的 `SUB2API_IMAGE`，值必须与本次加载的镜像标签一致。例如已经构建过的功能提交：

```dotenv
SUB2API_IMAGE=sub2api-ywlcode:3f751ce65
```

此例中的 SHA 不是固定发布版本。下次构建了新提交，就填对应的新标签；不要把新产物继续标成旧 SHA。同一源码 SHA 也可能因为基础镜像或构建时间不同而生成不同产物，已验证的旧镜像和归档应保留；需要重做构建时使用额外构建标识区分，并记录新的归档校验和。

保持现有 Compose 服务名、host 网络、`127.0.0.1:8080` 监听、数据挂载和 Nginx 配置。不要覆盖 `.env` 或 `data/config.yaml`，不要重新生成现有 JWT、TOTP 加密密钥及数据库/Redis 凭据。它们不是新镜像构建的一部分，也不能提交到 Git。

仅修改 `.env` 不会改变现有容器。`docker compose restart sub2api` 也不能替换镜像或应用新的容器环境变量，需要下一阶段的重新创建。

## 阶段 B：真正切换应用

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
