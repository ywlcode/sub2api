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
    "usage_day,user_id,username,profile_name,department,used_groups,model,requests,"
    "input_tokens,output_tokens,cache_read_tokens,total_tokens,actual_cost"
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
            ["2024-02-02", "1", *user, "A", "client", 1, 6, 7, 9, 30, "0"],
            ["2024-02-29", "1", *user, "", "client", 1, 1, 1, 1, 4, "0"],
        ]
        self.assertEqual(len(rows), len(expected))
        for row, want in zip(rows, expected):
            with self.subTest(key=(want[0], want[1], want[6])):
                self.assertEqual(row[:-1], list(map(str, want[:-1])))
                self.assertEqual(Decimal(row[-1]), Decimal(want[-1]))

    def test_email_names_are_blank(self):
        header, *rows = self.export("2024-05-01")
        self.assertEqual(header, HEADER)
        self.assertEqual([row[1:4] for row in rows], [["5", "", "小林"], ["6", "@小周", ""]])

    def test_empty_month(self):
        self.assertEqual(self.export("2024-04-01"), [HEADER])


if __name__ == "__main__":
    unittest.main()
