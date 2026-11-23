"""Offline workbook tests. Run with python3 -B -m unittest discover -p test_workbook.py."""

import csv
from decimal import localcontext
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


HEADER = ("usage_day,user_id,username,email,used_groups,allowed_groups,subscription_groups,"
          "model,requests,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,"
          "total_tokens,total_cost,actual_cost,profile_name,department").split(",")
MONTH = "2024-02"
SITE = "测试站"
NS = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"


def record(day="2024-02-01", **updates):
    fields = dict(zip(HEADER, (day, "0001234567890123456789", '=1+1', "user@example.test",
                               '一组, "二组"', "允许组", "", "model", "1", "2", "3", "1",
                               "4", "10", "0.1000000001", "0.05", " 王<&>\n五 ", "@部门")))
    fields.update(updates)
    return [fields[key] for key in HEADER]


def daily(day, requests, tokens, cost):
    return dict(day=day, requests=requests, total_tokens=tokens, actual_cost=cost)


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

    def build(self, events=None):
        return workbook.build_workbook(self.xlsx, MONTH, {SITE: self.src}, events)

    def cells(self, sheet):
        with ZipFile(self.xlsx) as archive:
            document = ET.fromstring(archive.read(f"xl/worksheets/sheet{sheet}.xml"))
        return [[(cell.get("t"), cell.findtext(f"{NS}is/{NS}t", cell.findtext(f"{NS}v")))
                 for cell in row] for row in document.iter(f"{NS}row")]

    def values(self, sheet):
        return [[value for _, value in row] for row in self.cells(sheet)]

    def test_detail_types_text_safety_and_exact_totals(self):
        long = "9007199254740993"
        cost = "123456789012345.123456789012345678"
        self.write(record(total_tokens=long, actual_cost=cost),
                   record(model="+second", actual_cost="0.000000000000000001"),
                   record(user_id="98", profile_name="", department="", actual_cost="0"))
        with localcontext() as context:
            context.prec = 5
            summary = self.build()
        self.assertEqual(json.loads(json.dumps(summary))[SITE], {
            "rows": 3, "users": 2, "requests": 3, "total_tokens": int(long) + 20,
            "total_cost": "0.3000000003", "actual_cost": "123456789012345.123456789012345679",
        })
        self.assertEqual(self.values(2)[1][:7], [SITE, "3", "3", str(int(long) + 20),
                                                 "0.3000000003", summary[SITE]["actual_cost"], "2"])
        row = self.cells(3)[1]
        self.assertEqual([v for _, v in row[:9]], [SITE, *record()[:8]])
        self.assertEqual([kind for kind, _ in row[:9]], ["inlineStr"] * 9)
        self.assertEqual([kind for kind, _ in row[9:17]], ["n"] * 8)
        self.assertEqual(row[17:19], [("inlineStr", " 王<&>\n五 "), ("inlineStr", "@部门")])
        self.assertIn(f"total_tokens={long}", row[-1][1])
        self.assertIn(f"actual_cost={cost}", row[-1][1])
        with ZipFile(self.xlsx) as archive:
            self.assertIsNone(archive.testzip())
            for path in archive.namelist():
                doc = ET.fromstring(archive.read(path))
                self.assertIsNone(doc.find(f".//{NS}f"))
                self.assertIsNone(doc.find(f".//{NS}hyperlink"))
                for element in doc.iter():
                    self.assertNotEqual(element.get("TargetMode"), "External")
        if os.name == "posix":
            self.assertEqual(stat.S_IMODE(self.xlsx.stat().st_mode), 0o600)

    def test_daily_and_cover_use_only_comparable_days(self):
        self.write(record(), record(model="second"), record("2024-02-02"), record("2024-02-04"))
        self.build({SITE: [daily("2024-02-03", 3, 4, "0.11"), daily("2024-02-01", 0, 0, "0"),
                           daily("2024-02-04", 1, 10, "0.05")]})
        cover = {row[0]: row[1] for row in self.values(1)}
        self.assertEqual([cover[k] for k in ("可对账天数", "其中有差额的天数", "净差请求数",
                                              "净差 Token", "净差实际费用（美元）")],
                         ["3", "2", "1", "-16", "0.01"])
        days = self.values(4)
        self.assertEqual(len(days), 30)  # leap February, plus header
        self.assertEqual(days[-1][1], "2024-02-29")
        self.assertEqual(days[1][2:11], ["2", "0", "-2", "20", "0", "-20", "0.10", "0", "-0.10"])
        self.assertEqual(days[2][2:5], ["1", "未知", "未知"])
        self.assertEqual(days[3][2:5], ["0", "3", "3"])
        self.assertEqual(days[4][11], "相符")
        self.assertEqual(days[-1][2:5], ["未见记录", "未知", "未知"])
        self.assertEqual(days[-1][11], "未见记录")

    def test_header_only_and_no_dashboard(self):
        self.write()
        totals = self.build()[SITE]
        self.assertEqual(totals, dict(rows=0, users=0, requests=0, total_tokens=0,
                                      total_cost="0", actual_cost="0"))
        self.assertEqual(len(self.values(3)), 1)
        self.assertTrue(all(row[11] == "未见记录" for row in self.values(4)[1:]))
        cover = {row[0]: row[1] for row in self.values(1)}
        self.assertEqual(cover["净差请求数"], "未知")

    def test_invalid_csv_does_not_replace_report_or_leave_temp(self):
        self.write(record())
        self.build()
        original = self.xlsx.read_bytes()

        for case in ([], [record()], [HEADER[::-1]], [HEADER, record()[:-1]],
                     [HEADER, record("2024-03-01")], [HEADER, record("2024-02-30")],
                     [HEADER, record(requests="1.5")], [HEADER, record(actual_cost="NaN")],
                     [HEADER, record(actual_cost="1e-10000")], [HEADER, record(username="bad\x01")],
                     [HEADER, record(department="名" * 32768)]):
            with self.subTest(rows=len(case)):
                self.write(*case, header=False)
                with self.assertRaises(ValueError):
                    self.build()
                self.assertEqual(self.xlsx.read_bytes(), original)
                self.assertEqual(set(self.root.iterdir()), {self.src, self.xlsx})

    def test_excel_row_limit_counts_header(self):
        with patch.object(workbook, "MAX_ROWS", 30):
            self.write(*[record()] * 29)
            self.assertEqual(self.build()[SITE]["rows"], 29)
            original = self.xlsx.read_bytes()
            self.write(*[record()] * 30)
            with self.assertRaisesRegex(ValueError, "明细.*row limit"):
                self.build()
            self.assertEqual(original, self.xlsx.read_bytes())

    def test_dashboard_rejects_duplicates_and_invalid_records(self):
        self.write()
        entry = daily("2024-02-29", 0, 0, "1e-10")
        self.build({SITE: [entry]})
        self.assertEqual(self.cells(4)[-1][9], ("n", "1E-10"))
        for records in ([entry, entry], [dict(entry, day="2024-03-01")],
                        [dict(entry, actual_cost="Infinity")], [dict(entry, requests="")]):
            with self.assertRaises(ValueError):
                self.build({SITE: records})

    def test_source_and_site_isolation(self):
        self.write()
        with self.assertRaises(ValueError):
            workbook.build_workbook(self.xlsx, MONTH, {SITE: self.src, "other": self.src})
        with self.assertRaises(ValueError):
            workbook.build_workbook(self.src, MONTH, {SITE: self.src})
        self.xlsx.symlink_to(self.src)
        with self.assertRaises(ValueError):
            self.build()
        self.assertTrue(self.xlsx.is_symlink())

    @unittest.skipUnless(openpyxl, "optional openpyxl reader")
    def test_openpyxl_round_trip(self):
        self.write(record(email="https://example.test/path"))
        self.build()
        book = openpyxl.load_workbook(self.xlsx)
        try:
            self.assertEqual(book.sheetnames, ["说明", "按站点汇总", "明细", "每日对账"])
            detail = book["明细"]
            self.assertEqual(detail["D2"].value, "=1+1")
            self.assertEqual(detail["D2"].data_type, "s")
            self.assertIsNone(detail["E2"].hyperlink)
            self.assertEqual(detail["P2"].data_type, "n")
            self.assertEqual(detail["P2"].value, 0.1000000001)
            self.assertEqual(detail["R2"].value, " 王<&>\n五 ")
        finally:
            book.close()


if __name__ == "__main__":
    unittest.main()
