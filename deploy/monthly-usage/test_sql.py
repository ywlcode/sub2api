"""运行 python3 deploy/monthly-usage/test_sql.py。需要本地 Docker 和 postgres:16 镜像。"""

import csv
import io
from decimal import Decimal
from pathlib import Path
import subprocess
import unittest


HERE = Path(__file__).resolve().parent
MIGRATIONS = HERE.parents[1] / "backend" / "migrations"
HEADER = (
    "usage_day,user_id,username,email,used_groups,allowed_groups,subscription_groups,"
    "model,requests,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,"
    "total_tokens,total_cost,actual_cost,profile_name,department"
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
            "179_usage_log_image_input_tokens.sql",
        ]
        setup = [MIGRATIONS / name for name in migrations] + [HERE / "test_sql_fixture.sql"]
        subprocess.run([*cls.psql, "-f", "-"], check=True,
                       input=b"\n".join(path.read_bytes() for path in setup))
        subprocess.run([*cls.psql, "-c", "ALTER DATABASE postgres SET timezone = 'America/Los_Angeles'",
                        "-c", "ALTER DATABASE postgres SET datestyle = 'SQL, DMY'"], check=True)

    def export(self, month):
        result = subprocess.check_output([*self.psql, "-v", f"month={month}", "-f", "-"],
                                         input=(HERE / "usage.sql").read_bytes())
        return list(csv.reader(io.StringIO(result.decode("utf-8"), newline=""), strict=True))

    def test_leap_month(self):
        header, *rows = self.export("2024-02-01")
        self.assertEqual(header, HEADER)
        user = ['张,"三"\n甲', "test@example.invalid"]
        memberships = ["A, B", "plan-A, plan-B"]
        expected = [
            ["2024-02-01", "1", *user, "B, old", *memberships, "client",
             3, 2147483650, 6, 7, 13, 2147483676, "1.5000000003", "0.2500000003"],
            ["2024-02-01", "1", *user, "", *memberships, "other", 1, 0, 0, 0, 0, 0, "1.5", "0"],
            ["2024-02-01", "2", "", "", "", "", "", "client", 1, 0, 0, 0, 0, 0, "0", "0"],
            ["2024-02-01", "4", "无属性", "plain@example.invalid", "", "", "", "client",
             1, 0, 0, 0, 0, 0, "0", "0"],
            ["2024-02-02", "1", *user, "A", *memberships, "client", 1, 6, 7, 8, 9, 30, "0", "0"],
            ["2024-02-29", "1", *user, "", *memberships, "client", 1, 1, 1, 1, 1, 4, "0", "0"],
        ]
        profiles = {"1": ["钉钉姓名", "公司/研发"], "2": ["", "运维"], "4": ["", ""]}
        self.assertEqual(len(rows), len(expected))
        for row, want in zip(rows, expected):
            with self.subTest(key=(want[0], want[1], want[7])):
                self.assertEqual(row[:14], list(map(str, want[:14])))
                self.assertEqual(list(map(Decimal, row[14:16])), list(map(Decimal, want[14:])))
                self.assertEqual(row[16:], profiles[want[1]])

    def test_empty_month(self):
        self.assertEqual(self.export("2024-04-01"), [HEADER])


if __name__ == "__main__":
    unittest.main()
