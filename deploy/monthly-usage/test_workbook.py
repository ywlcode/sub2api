"""Offline workbook tests. Run with python3 -B -m unittest discover -p test_workbook.py."""

import csv
from decimal import Decimal, localcontext
import json
import os
from pathlib import Path
import stat
from tempfile import TemporaryDirectory
import unittest
from unittest.mock import patch
from xml.etree import ElementTree as ET
from zipfile import ZipFile

import workbook

try:
    import openpyxl
except ImportError:
    openpyxl = None


HEADER = ("usage_day,user_id,username,profile_name,department,used_groups,model,"
          "requests,input_tokens,output_tokens,cache_read_tokens,total_tokens,actual_cost").split(",")
METRICS = ["请求数", "输入 Token", "输出 Token", "缓存读取 Token", "总 Token", "用户扣费（美元）"]
SHEETS = ["明细", "部门月汇总", "用户月汇总"]
DETAIL = ["站点", "用量日期", "用户 ID", "用户名", "当前真实姓名", "当前所属部门", "使用分组", "模型", *METRICS]
DEPARTMENTS = ["站点", "当前所属部门", "活跃用户数", *METRICS]
USERS = ["站点", "用户 ID", "用户名", "当前真实姓名", "当前所属部门", *METRICS]
MONTH = "2024-02"
SITE = "测试站"
USER_ID = "0001234567890123456789"
DEPARTMENT = "总部/研发, 总部/质量"
NS = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"


def record(**updates):
    fields = dict(usage_day="2024-02-01", user_id=USER_ID, username="同名用户",
                  profile_name="王五", department=DEPARTMENT, used_groups='一组, "二组"',
                  model="model", requests="2", input_tokens="3", output_tokens="5",
                  cache_read_tokens="7", total_tokens="26", actual_cost="0.0123456789")
    fields.update(updates)
    return [fields[key] for key in HEADER]


class WorkbookTest(unittest.TestCase):
    def setUp(self):
        tmp = TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.src = self.root / "data.csv"
        self.xlsx = self.root / "report.xlsx"

    def write(self, *rows, header=True):
        with self.src.open("w", encoding="utf-8-sig", newline="") as handle:
            writer = csv.writer(handle)
            if header:
                writer.writerow(HEADER)
            writer.writerows(rows)

    def build(self):
        return workbook.build_workbook(self.xlsx, MONTH, {SITE: self.src})

    def cells(self, sheet):
        with ZipFile(self.xlsx) as archive:
            document = ET.fromstring(archive.read(f"xl/worksheets/sheet{sheet}.xml"))
        return [[(cell.get("t", "n"), cell.findtext(f"{NS}is/{NS}t", cell.findtext(f"{NS}v")))
                 for cell in row] for row in document.iter(f"{NS}row")]

    def values(self, sheet):
        return [[value for _, value in row] for row in self.cells(sheet)]

    def assert_summary(self, summary, **expected):
        # report.py consumes a JSON-serializable mapping. Extra summary fields are allowed.
        totals = json.loads(json.dumps(summary))
        self.assertEqual(set(totals), {SITE})
        for key, value in expected.items():
            with self.subTest(summary_key=key):
                actual = totals[SITE][key]
                if key == "actual_cost":
                    self.assertIsInstance(actual, str)
                    self.assertEqual(Decimal(actual), Decimal(value))
                else:
                    self.assertEqual(actual, value)

    def test_monthly_rollups_count_ids_and_keep_department_paths_together(self):
        # The 26 total includes 11 cache creation tokens absent from the three visible components.
        first = record()
        second = record(usage_day="2024-02-29", model="second", requests="1", input_tokens="2",
                        output_tokens="4", cache_read_tokens="6", total_tokens="13", actual_cost="0.02")
        same_name = record(user_id="98")
        unassigned = record(user_id="99", username="无部门用户", department="", profile_name="")
        self.write(first, same_name, unassigned, second)
        self.assert_summary(self.build(), rows=4, users=3, departments=2, requests=7,
                            total_tokens=91, actual_cost="0.0570370367")

        detail = self.values(1)[1:]
        self.assertEqual(len(detail), 4)
        self.assertCountEqual([row for row in detail if row[2] != "99"],
                              [[SITE, *data] for data in (first, second, same_name)])
        unassigned_detail, = [row for row in detail if row[2] == "99"]
        self.assertEqual(unassigned_detail[:5], [SITE, *unassigned[:4]])
        self.assertEqual(unassigned_detail[6:], unassigned[5:])

        self.assertCountEqual(self.values(2)[1:], [
            [SITE, DEPARTMENT, "2", "5", "8", "14", "20", "65", "0.0446913578"],
            [SITE, "未填写部门", "1", *unassigned[-6:]],
        ])
        users = self.values(3)[1:]
        self.assertEqual(len(users), 3)
        self.assertCountEqual([row for row in users if row[1] != "99"], [
            [SITE, USER_ID, "同名用户", "王五", DEPARTMENT, "3", "5", "9", "13", "39", "0.0323456789"],
            [SITE, "98", "同名用户", "王五", DEPARTMENT, *same_name[-6:]],
        ])
        unassigned_user, = [row for row in users if row[1] == "99"]
        self.assertEqual(unassigned_user[:4], [SITE, *unassigned[1:4]])
        self.assertEqual(unassigned_user[5:], unassigned[-6:])

    def test_exact_cost_and_large_token_count_on_every_sheet(self):
        tokens = 9007199254740993
        cost = "123456789012345.123456789012345678"
        penny_fraction = "0.000000000000000001"
        combined = "123456789012345.123456789012345679"
        self.write(record(total_tokens=str(tokens), actual_cost=cost),
                   record(model="second", actual_cost=penny_fraction))
        with localcontext() as context:
            context.prec = 5
            summary = self.build()
        self.assert_summary(summary, rows=2, users=1, departments=1, requests=4,
                            total_tokens=tokens + 26, actual_cost=combined)
        # A numeric Excel cell is read as a double. XML can contain digits the cell loses.
        # Compare the value a reader can recover, including exact text cells for large values.
        def read_number(cell):
            kind, raw = cell
            return Decimal(str(float(raw))) if kind == "n" else Decimal(raw)

        expected = [
            [(tokens, Decimal(cost)), (26, Decimal(penny_fraction))],
            [(tokens + 26, Decimal(combined))],
            [(tokens + 26, Decimal(combined))],
        ]
        for sheet, metrics in enumerate(expected, 1):
            with self.subTest(sheet=SHEETS[sheet - 1]):
                self.assertCountEqual([(read_number(row[-2]), read_number(row[-1]))
                                       for row in self.cells(sheet)[1:]], metrics)

    def test_empty_month_has_only_the_requested_sheets_and_headers(self):
        self.write()
        self.assert_summary(self.build(), rows=0, users=0, departments=0, requests=0,
                            total_tokens=0, actual_cost="0")
        with ZipFile(self.xlsx) as archive:
            book = ET.fromstring(archive.read("xl/workbook.xml"))
        self.assertEqual([sheet.get("name") for sheet in book.iter(f"{NS}sheet")], SHEETS)
        for index, header in enumerate((DETAIL, DEPARTMENTS, USERS), 1):
            with self.subTest(sheet=SHEETS[index - 1]):
                self.assertEqual(self.values(index), [header])

    def test_text_is_literal_and_report_is_private(self):
        name = " 王<&>\n五 "
        group = "https://example.test/path"
        department = "@部门"
        data = record(username="=1+1", profile_name=name, department=department,
                      used_groups=group, model="+model")
        self.write(data)
        self.build()
        for sheet, labels in enumerate(([SITE, *data[:7]], [SITE, department],
                                        [SITE, USER_ID, "=1+1", name, department]), 1):
            with self.subTest(sheet=SHEETS[sheet - 1]):
                row, = self.cells(sheet)[1:]
                self.assertEqual(row[:len(labels)], [("inlineStr", label) for label in labels])
                numbers = row[len(labels):]
                self.assertEqual([kind for kind, _ in numbers], ["n"] * len(numbers))
        with ZipFile(self.xlsx) as archive:
            self.assertIsNone(archive.testzip())
            for path in archive.namelist():
                if path.endswith((".xml", ".rels")):
                    doc = ET.fromstring(archive.read(path))
                    self.assertIsNone(doc.find(f".//{NS}f"))
                    self.assertIsNone(doc.find(f".//{NS}hyperlink"))
                    for element in doc.iter():
                        self.assertNotEqual(element.get("TargetMode"), "External")
        if os.name == "posix":
            self.assertEqual(stat.S_IMODE(self.xlsx.stat().st_mode), 0o600)

    def test_invalid_csv_does_not_replace_report_or_leave_temp(self):
        self.write(record())
        self.build()
        original = self.xlsx.read_bytes()
        cases = {
            "empty file": [], "missing header": [record()], "reordered header": [HEADER[::-1]],
            "extra column": [HEADER + ["email"]], "short row": [HEADER, record(), record()[:-1]],
            "wrong month": [HEADER, record(usage_day="2024-03-01")],
            "invalid date": [HEADER, record(usage_day="2024-02-30")],
            "missing user ID": [HEADER, record(user_id="")],
            "inconsistent snapshot": [HEADER, record(), record(department="其他部门")],
            "fractional count": [HEADER, record(requests="1.5")],
            "nonfinite cost": [HEADER, record(actual_cost="NaN")],
            "out of range": [HEADER, record(actual_cost="1e-10000")],
            "XML control character": [HEADER, record(username="bad\x01")],
            "overlong department": [HEADER, record(department="名" * 32768)],
        }
        for name, case in cases.items():
            with self.subTest(name=name):
                self.write(*case, header=False)
                with self.assertRaises(ValueError):
                    self.build()
                self.assertEqual(self.xlsx.read_bytes(), original)
                self.assertEqual(set(self.root.iterdir()), {self.src, self.xlsx})

    def test_excel_row_limit_counts_header(self):
        with patch.object(workbook, "MAX_ROWS", 3):
            self.write(record(), record(model="second"))
            self.assertEqual(self.build()[SITE]["rows"], 2)
            original = self.xlsx.read_bytes()
            self.write(record(), record(model="second"), record(model="third"))
            with self.assertRaises(ValueError):
                self.build()
            self.assertEqual(self.xlsx.read_bytes(), original)
            self.assertEqual(set(self.root.iterdir()), {self.src, self.xlsx})

    def test_source_and_site_isolation(self):
        self.write()
        original = self.src.read_bytes()
        with self.assertRaises(ValueError):
            workbook.build_workbook(self.xlsx, MONTH, {SITE: self.src, "other": self.src})
        with self.assertRaises(ValueError):
            workbook.build_workbook(self.src, MONTH, {SITE: self.src})
        self.xlsx.symlink_to(self.src)
        with self.assertRaises(ValueError):
            self.build()
        self.assertTrue(self.xlsx.is_symlink())
        self.assertEqual(self.src.read_bytes(), original)

    @unittest.skipUnless(openpyxl, "optional openpyxl reader")
    def test_openpyxl_round_trip(self):
        self.write(record(username="=1+1", used_groups="https://example.test/path"))
        self.build()
        book = openpyxl.load_workbook(self.xlsx)
        try:
            self.assertEqual(book.sheetnames, SHEETS)
            detail = book["明细"]
            self.assertEqual(detail["C2"].value, USER_ID)
            self.assertEqual(detail["D2"].value, "=1+1")
            self.assertEqual(detail["D2"].data_type, "s")
            self.assertIsNone(detail["G2"].hyperlink)
            self.assertEqual(detail["N2"].value, 0.0123456789)
        finally:
            book.close()


if __name__ == "__main__":
    unittest.main()
