"""运行 python3 deploy/monthly-usage/test_sql.py。需要本地 Docker 和 postgres:16 镜像。"""

import csv
import io
import json
from decimal import Decimal
from pathlib import Path
import subprocess
import unittest


HERE = Path(__file__).resolve().parent
MIGRATIONS = HERE.parents[1] / "backend" / "migrations"
HEADER = (
    "usage_day,user_id,username,profile_name,department,used_groups,model,requests,"
    "input_tokens,output_tokens,cache_read_tokens,total_tokens,actual_cost,platform_usage"
).split(",")


class MonthlyUsageSQL(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # 仅使用本机 Docker socket、已有镜像和匿名数据卷；不读取宿主机的 PG* 环境。
        docker = ["docker", "--host", "unix:///var/run/docker.sock"]
        container = subprocess.check_output([
            *docker, "run", "--pull=never", "--rm", "--detach", "--network=none",
            "--user", "postgres", "--entrypoint", "sleep", "postgres:16", "infinity",
        ], text=True).strip()
        cls.addClassCleanup(subprocess.run, [*docker, "rm", "-f", container],
                            check=True, stdout=subprocess.DEVNULL)
        cls.execute = [*docker, "exec", "-i", container]
        subprocess.run([
            *cls.execute, "sh", "-ec",
            """initdb -D "$PGDATA" --locale=C -E UTF8 -A trust >/dev/null
               pg_ctl -D "$PGDATA" -l /tmp/postgres.log -o "-c listen_addresses=''" -w start >/dev/null""",
        ], check=True)
        cls.psql = [*cls.execute, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1"]
        migrations = [
            "001_init.sql", "003_subscription.sql", "005_schema_parity.sql",
            "007_add_user_allowed_groups.sql", "012_add_user_subscription_soft_delete.sql",
            "014_drop_legacy_allowed_groups.sql", "018_user_attributes.sql", "075_add_usage_log_upstream_model.sql",
            "077_add_usage_log_requested_model.sql", "089_usage_log_image_output_tokens.sql",
            "179_usage_log_image_input_tokens.sql", "181_prompt_audit.sql", "293_security_penalties.sql",
            "343_usage_log_platform_snapshot.sql",
        ]
        subprocess.run([*cls.psql, "-f", "-"], check=True,
                       input=b"\n".join((MIGRATIONS / name).read_bytes() for name in migrations))
        subprocess.run([*cls.psql, "-c", "CREATE DATABASE empty_report TEMPLATE postgres"], check=True)
        subprocess.run([*cls.psql, "-f", "-"], check=True, input=(HERE / "test_sql_fixture.sql").read_bytes())
        subprocess.run([*cls.psql, "-c", "ALTER DATABASE postgres SET timezone = 'America/Los_Angeles'",
                        "-c", "ALTER DATABASE postgres SET datestyle = 'SQL, DMY'"], check=True)

    def export(self, month):
        result = subprocess.check_output([*self.psql, "-U", "report_reader", "-d", "postgres",
                                          "-v", f"month={month}", "-f", "-"],
                                         input=(HERE / "usage.sql").read_bytes())
        return list(csv.reader(io.StringIO(result.decode("utf-8"), newline=""), strict=True))

    def test_leap_month(self):
        header, *rows = self.export("2024-02-01")
        self.assertEqual(header, HEADER)
        user = ['张,"三"\n甲', "钉钉姓名", "公司/研发"]
        expected = [
            ["2024-02-01", "1", *user, "B, old", "client",
             3, 2147483650, 6, 13, 2147483676, "0.2500000003"],
            ["2024-02-01", "1", *user, "", "other", 1, 0, 0, 0, 0, "0"],
            ["2024-02-01", "2", "", "", "运维", "", "client", 1, 0, 0, 0, 0, "0"],
            ["2024-02-01", "4", "无属性", "", "", "", "client", 1, 0, 0, 0, 0, "0"],
            ["2024-02-01", "7", "", "", "已删", "", "client", 1, 1, 2, 4, 10, "6"],
            ["2024-02-02", "1", *user, "A", "client", 1, 6, 7, 9, 30, "0"],
            ["2024-02-29", "1", *user, "", "client", 1, 1, 1, 1, 4, "0"],
        ]
        self.assertEqual(len(rows), len(expected))
        for row, want in zip(rows, expected):
            with self.subTest(key=(want[0], want[1], want[6])):
                self.assertEqual(row[:12], list(map(str, want[:-1])))
                self.assertEqual(Decimal(row[12]), Decimal(want[-1]))
                self.assertEqual(row[13:], [""])

    def test_historical_platforms_share_one_detail_row(self):
        header, mixed, free = self.export("2024-06-01")
        self.assertEqual(header, HEADER)
        self.assertEqual(mixed[:7], ["2024-06-01", "1", '张,"三"\n甲', "钉钉姓名", "公司/研发", "A, B, old, plan-B", "deepseek"])
        self.assertEqual(mixed[7:13], ["8", "2147483668", "28", "52", "2147483788", "9000000004.1200000012"])
        self.assertEqual(json.loads(mixed[13]), {
            "openai": ["3", "2147483658", "22", "44", "2147483757", "9000000001.0000000003"],
            "deepseek": ["2", "6", "6", "8", "27", "1.1200000001"],
            "anthropic": ["1", "1", "0", "0", "1", "1.0000000000"],
        })
        self.assertEqual(free[0], "2024-06-02")
        self.assertEqual(json.loads(free[13]), {"deepseek": ["1", "0", "0", "0", "0", "0.0000000000"]})

    def test_custom_platform_first_seen_at_next_month_boundary(self):
        header, only = self.export("2024-07-01")
        self.assertEqual(header, HEADER)
        self.assertEqual(only[0], "2024-07-01")
        self.assertEqual(only[7:13], ["1", "4", "3", "1", "10", "0.0000000000"])
        self.assertEqual(json.loads(only[13]), {"lab-x/42": only[7:13]})

    def test_email_names_are_blank(self):
        header, *rows = self.export("2024-05-01")
        self.assertEqual(header, HEADER)
        self.assertEqual([row[1:4] for row in rows], [["5", "", "小林"], ["6", "@小周", ""]])

    def test_default_department_uses_the_same_display_value_in_both_exports(self):
        _, *rows = self.export("2024-05-01")
        self.assertEqual({row[1]: row[4] for row in rows}, {"5": "公司/研发", "6": "old-option"})
        counts = self.headcounts("2024-05-01")
        self.assertEqual(counts["公司/研发"], 2)  # 选项标签与同名文本计入同一个部门。
        self.assertEqual(counts["old-option"], 1)
        self.assertNotIn("不应回退", counts)

    def test_empty_month(self):
        self.assertEqual(self.export("2024-04-01"), [HEADER])

    def headcounts(self, month, database="postgres", setup=None):
        if setup:
            # 在单独的可回滚事务中改变策略，再以报表读者执行同一查询。
            args = ["-c", "BEGIN", "-c", setup, "-c", "SET LOCAL ROLE report_reader"]
        else:
            args = ["-U", "postgres" if database == "empty_report" else "report_reader",
                    "-c", "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"]
        raw = subprocess.check_output([*self.psql, "-At", "-v", f"month={month}", "-d", database,
                                       *args, "-f", "-", "-c", "ROLLBACK"],
                                      input=(HERE / "headcounts.sql").read_bytes())
        self.assertTrue(raw.endswith(b"\n"))
        self.assertEqual(raw.count(b"\n"), 1)
        counts = json.loads(raw)
        self.assertIsInstance(counts, dict)
        self.assertTrue(all(type(n) is int and n > 0 for n in counts.values()))
        return counts

    def test_department_headcounts(self):
        expected = {"公司/研发": 1, "运维": 3, '仅注册"部门"\n二组': 1, "": 1, "处罚到期": 1, "会话限制": 1}
        self.assertEqual(self.headcounts("2024-02-01"), expected)
        self.assertEqual(self.headcounts("2024-03-01"), {**expected, "三月新增": 1})

    def test_empty_database_headcounts(self):
        self.assertEqual(self.headcounts("2024-02-01", database="empty_report"), {})

    def test_inactive_user_block_policy(self):
        for update in [
            "UPDATE settings SET value='false' WHERE key='risk_control_enabled'",
            """UPDATE settings SET value='{"enabled":false,"observe_only":false,"policy":"user_block"}'
               WHERE key='security_penalty_config'""",
            """UPDATE settings SET value='{"enabled":true,"observe_only":true,"policy":"user_block"}'
               WHERE key='security_penalty_config'""",
            """UPDATE settings SET value='{"enabled":true,"observe_only":false,"policy":"session_block"}'
               WHERE key='security_penalty_config'""",
        ]:
            with self.subTest(update=update):
                counts = self.headcounts("2024-02-01", setup=update)
                self.assertEqual(counts["运维"], 3)
                self.assertNotIn("纯管理员部门", counts)
                self.assertEqual(counts["网关封禁"], 1)
                self.assertEqual(counts["会话限制"], 1)

    def test_both_exports_share_read_only_transaction(self):
        month = "2024-02-01"
        script = b"\n".join([b"BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;",
                             (HERE / "usage.sql").read_bytes(), (HERE / "headcounts.sql").read_bytes(),
                             b"COMMIT;"])
        raw = subprocess.check_output([*self.psql, "-At", "-U", "report_reader", "-d", "postgres",
                                       "-v", f"month={month}", "-f", "-"], input=script)
        csv_body, json_line, end = raw.rsplit(b"\n", 2)
        self.assertEqual(end, b"")
        rows = list(csv.reader(io.StringIO(csv_body.decode(), newline=""), strict=True))
        self.assertEqual(rows, self.export(month))
        self.assertEqual(json.loads(json_line), self.headcounts(month))


if __name__ == "__main__":
    unittest.main()
