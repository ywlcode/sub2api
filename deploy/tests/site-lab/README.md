# 本地主从站故障实验

此目录默认操作名为 `sub2api-site-lab` 的 Docker Compose 实验项目，使用两套独立 PostgreSQL/Redis、模拟模型上游、故障中继。Docker 网络设为 `internal`，模型请求不访问真实上游。测试密钥随机生成，生成的配置、数据库卷和证据不要提交。

需要 Docker Compose、Python 3，以及可用的 Sub2API 运行时基础镜像。示例使用本地已有的 `sub2api-ywlcode:4e0339570`。工具镜像 `sub2api-oauth-lab-tun:0.1` 需要 Python 3、`ip`、`tc`；也可用 Alpine 安装 `python3 iproute2 iproute2-tc` 后构建同名本地镜像。准备脚本中的 PostgreSQL/Redis 镜像也需预先存在或可以拉取。

```sh
lab_dir="$HOME/.cache/sub2api-audit/site-lab-local"
python3 deploy/tests/site-lab/prepare.py "$lab_dir"
# 先在 frontend 中运行 pnpm build（产物直接写入 backend/internal/web/dist），再编译后端。
cd backend
CGO_ENABLED=0 go build -tags embed -o "$lab_dir/build/sub2api" ./cmd/server
cd ..
docker build -t sub2api-site-lab:workspace "$lab_dir/build"
docker compose -f "$lab_dir/compose.json" up -d
python3 deploy/tests/site-lab/lab.py "$lab_dir" seed
for phase in protocols policy delivery network lifecycle; do
  python3 -u deploy/tests/site-lab/test_lab.py "$lab_dir" "$phase" || break
done
python3 deploy/tests/site-lab/collect.py "$lab_dir"
# 单独创建一个全新的 mock 实验项目，验证账号开关；不要在真实账号实验库上运行。
# python3 deploy/tests/site-lab/test_account_switch.py "$switch_lab_dir"
```

首次初始化及 policy 测试应在全新的实验数据库执行。各测试阶段依次运行，不能并发修改同一实验环境。数据库 SQL 修改只用于构造测试用户/权限/订阅场景；生产应使用正常管理接口。记录位于生成目录的 `evidence/`，包括原始同步记录、模拟上游记录及每项断言结果。同步和模型请求记录不包含原始认证密钥。

账号开关实验覆盖管理接口保存、无需重启的开关同步、三协议同时直连主站/从站、关闭后转发主站、账号新增模型自动生效、并发 5 和 GPT 透传。

故障实验覆盖三协议、思考和工具历史、gzip 与原 Key、计费归属、权限撤销、余额和订阅限额、丢失 ACK、超过一页的账务积压、从站强制终止、延迟回应、随机 60% 丢包、100% 断流、已放行请求、主站重启、认证/身份/schema 错误和独立模式。

`tc netem` 只在 `link` 容器的 `eth0` 注入，不改变主机网卡。从站到国内模拟上游的流量不经过 link；主从站同步与 GPT 转发经过 link。60% 丢包是该网卡每个出站包的概率，经过中继的请求和响应都会受影响，不能直接等同于某条真实公网的端到端丢包率。测试不能替代公网线路实测或生产规模压测。

部分 Docker 版本不会为 `internal` 网络发布端口。`lab.py` 会显示主机可访问的容器内网地址；可用 `docker compose exec` 在容器内访问。该实验特意使用 HTTP，生产公网配置保持 HTTPS。

每阶段结束会清除丢包并恢复同步中继。若测试进程被强制终止，手动恢复：

```sh
docker compose -f "$lab_dir/compose.json" exec -T link tc qdisc del dev eth0 root
docker compose -f "$lab_dir/compose.json" restart link
docker compose -f "$lab_dir/compose.json" start primary secondary
```

停止而保留测试数据库：`docker compose -f "$lab_dir/compose.json" stop`。`down` 保留命名卷；只有确认不再需要实验数据时才使用 `down -v`。同一主机一次只运行一套同名实验项目；`prepare.py --project NAME --image IMAGE` 可准备独立命名的项目，目录和数据卷也必须独立。

配置补齐和传输大小实验（全新 mock 项目 seed 后运行；不用于真实账号实验库）：

```sh
python3 -u deploy/tests/site-lab/test_config_sync.py "$lab_dir"
```

脚本记录 gzip 前后上下行 HTTP 正文字节，验证并发配置、设置新增删除、账号别名、运行时请求体限制、独立暂停结果和安全规则排除；随后增加 1,000 个合成用户与 Key，测量全量和单用户增量。会修改测试主站 YAML 并重启测试容器，保留实验结果供复核。脚本支持从 `evidence/config-sync.json` 的已完成步骤继续；重新测量整个流程应使用新项目和新数据目录。

复审补充实验（上述五阶段通过后，在同一合成实验项目运行）：

```sh
python3 -u deploy/tests/site-lab/test_busy_sync.py "$lab_dir"
python3 -u deploy/tests/site-lab/test_reconciliation.py "$lab_dir"
```

前者让主站四个写入者持续修改同一用户余额，同时由从站发起 400 次流式请求，跨多个租约观察就绪状态、上传和最终余额。后者通过真实管理员 HTTP 接口检查拒收详情、手工补扣后结清、幂等重试、冲突操作以及免收，并核对从站不再叠加已处理账单。这两个脚本会修改合成实验库，不用于真实用户或生产。
