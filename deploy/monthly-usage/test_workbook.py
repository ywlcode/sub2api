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
          "requests,input_tokens,output_tokens,cache_read_tokens,total_tokens,actual_cost,"
          "openai_usage,deepseek_usage").split(",")
TOKENS = ["输入 Token", "输出 Token", "缓存读取 Token", "总 Token"]
COST = "用户扣费（美元）"
METRICS = ["请求数", *TOKENS, COST]
SHEETS = ["明细", "部门OpenAI平台月汇总", "部门Deepseek平台月汇总", "用户月汇总"]
DETAIL = ["站点", "用量日期", "用户 ID", "用户名", "当前真实姓名", "当前所属部门", "使用分组", "模型", *METRICS]
DEPARTMENTS = ["站点", "当前所属部门", "注册且未被封禁人数", "活跃用户数", "请求数",
               *(f"{label} (B)" for label in TOKENS), COST]
USERS = ["站点", "用户 ID", "用户名", "当前真实姓名", "当前所属部门", "请求数",
         *(f"{label} (M)" for label in TOKENS), COST]
MONTH = "2024-02"
SITE = "测试站"
USER_ID = "0001234567890123456789"
DEPARTMENT = "总部/研发, 总部/质量"
NS = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"


def record(platform="openai_usage", **updates):
    fields = dict(usage_day="2024-02-01", user_id=USER_ID, username="同名用户",
                  profile_name="王五", department=DEPARTMENT, used_groups='一组, "二组"',
                  model="model", requests="2", input_tokens="3", output_tokens="5",
                  cache_read_tokens="7", total_tokens="26", actual_cost="0.0123456789")
    fields.update(openai_usage="", deepseek_usage="")
    fields.update(updates)
    if platform and platform not in updates:
        fields[platform] = json.dumps([fields[key] for key in HEADER[7:13]])
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

    def build(self, registered=None):
        return workbook.build_workbook(self.xlsx, MONTH, {SITE: self.src},
                                       registered={} if registered is None else registered)

    def worksheet(self, sheet):
        with ZipFile(self.xlsx) as archive:
            return ET.fromstring(archive.read(f"xl/worksheets/sheet{sheet}.xml"))

    def cells(self, sheet):
        document = self.worksheet(sheet)
        return [[(cell.get("t", "n"), cell.findtext(f"{NS}is/{NS}t", cell.findtext(f"{NS}v")))
                 for cell in row] for row in document.iter(f"{NS}row")]

    def values(self, sheet):
        return [[value for _, value in row] for row in self.cells(sheet)]

    def metric_rows(self, sheet):
        # Compare the six metrics at the precision a spreadsheet reader can recover.
        # An exact text cell is valid for values beyond Excel's numeric precision.
        rows = []
        for row in self.cells(sheet)[1:]:
            labels = [value for _, value in row[:-6]]
            metrics = [Decimal(str(float(value))) if kind == "n" else Decimal(value)
                       for kind, value in row[-6:]]
            rows.append([*labels, *metrics])
        return rows

    def widths(self, sheet):
        document = self.worksheet(sheet)
        headers = self.values(sheet)[0]
        defaults = document.find(f"{NS}sheetFormatPr")
        default = float(defaults.get("defaultColWidth", 8.43)) if defaults is not None else 8.43
        widths = [default] * len(headers)
        for column in document.iter(f"{NS}col"):
            for pos in range(int(column.get("min")) - 1, min(int(column.get("max")), len(headers))):
                widths[pos] = float(column.get("width", default))
        return dict(zip(headers, widths))

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
        first = record(actual_cost="0.9876543219")
        second = record(usage_day="2024-02-29", model="second", requests="1", input_tokens="2",
                        output_tokens="4", cache_read_tokens="6", total_tokens="13", actual_cost="0.92")
        same_name = record(user_id="98", actual_cost="0.9876543219")
        unassigned = record(user_id="99", username="无部门用户", department="", profile_name="",
                            actual_cost="-1.9")
        self.write(first, same_name, unassigned, second)
        self.assert_summary(self.build(), rows=4, users=3, departments=2, requests=7,
                            total_tokens=91, actual_cost="0.9953086438")

        detail = self.values(1)[1:]
        self.assertEqual(len(detail), 4)
        self.assertCountEqual([row for row in detail if row[2] != "99"],
                              [[SITE, *data[:13]] for data in (first, second, same_name)])
        unassigned_detail, = [row for row in detail if row[2] == "99"]
        self.assertEqual(unassigned_detail[:5], [SITE, *unassigned[:4]])
        self.assertEqual(unassigned_detail[6:], unassigned[5:13])

        # The department spends 2.8953086438, while its two users separately truncate to 1 and 0.
        with self.subTest(sheet=SHEETS[1]):
            self.assertCountEqual(self.metric_rows(2), [
                [SITE, DEPARTMENT, "0", "2", 5, Decimal("8e-9"), Decimal("14e-9"),
                 Decimal("20e-9"), Decimal("65e-9"), 2],
                [SITE, "未填写部门", "0", "1", 2, Decimal("3e-9"), Decimal("5e-9"),
                 Decimal("7e-9"), Decimal("26e-9"), -1],
            ])
        users = self.metric_rows(4)
        self.assertEqual(len(users), 3)
        self.assertCountEqual([row for row in users if row[1] != "99"], [
            [SITE, USER_ID, "同名用户", "王五", DEPARTMENT, 3, Decimal("5e-6"), Decimal("9e-6"),
             Decimal("13e-6"), Decimal("39e-6"), 1],
            [SITE, "98", "同名用户", "王五", DEPARTMENT, 2, Decimal("3e-6"), Decimal("5e-6"),
             Decimal("7e-6"), Decimal("26e-6"), 0],
        ])
        unassigned_user, = [row for row in users if row[1] == "99"]
        self.assertEqual(unassigned_user[:4], [SITE, *unassigned[1:4]])
        self.assertEqual(unassigned_user[5:], [2, Decimal("3e-6"), Decimal("5e-6"),
                                               Decimal("7e-6"), Decimal("26e-6"), -1])

    def test_mixed_platforms_count_each_user_once_and_keep_other_usage(self):
        self.write(record(requests="4", actual_cost="1.75",
                          openai_usage='["1","0","0","0","1","0.75"]',
                          deepseek_usage='["2","1","1","3","10","0"]'),
                   record(model="second", requests="1", actual_cost="0.75"),
                   record(user_id="2", platform="deepseek_usage", actual_cost="0"),
                   record(user_id="3", platform=None, department="其他平台"))
        summary = self.build(registered={DEPARTMENT: 7})
        self.assert_summary(summary, users=3, departments=2, requests=9, total_tokens=104,
                            actual_cost="2.5123456789")
        self.assertEqual(self.metric_rows(2), [
            [SITE, DEPARTMENT, "7", "1", 2, Decimal("3e-9"), Decimal("5e-9"),
             Decimal("7e-9"), Decimal("27e-9"), 1],
        ])
        self.assertEqual(self.metric_rows(3), [
            [SITE, DEPARTMENT, "7", "2", 4, Decimal("4e-9"), Decimal("6e-9"),
             Decimal("10e-9"), Decimal("36e-9"), 0],
        ])
        self.assertEqual(len(self.metric_rows(1)), 4)
        self.assertEqual([row[1] for row in self.metric_rows(4)], [USER_ID, "2", "3"])
        self.assertEqual(self.metric_rows(4)[0][-6:], [
            5, Decimal("6e-6"), Decimal("10e-6"), Decimal("14e-6"), Decimal("52e-6"), 2,
        ])

    def test_registered_counts_and_usage_cover_the_union_of_departments(self):
        self.write(record(), record(model="second"), record(user_id="2", department=""),
                   record(user_id="3", department="仅有用量"))
        self.build(registered={DEPARTMENT: 7, "": 4, "仅有注册": 5})
        rows = self.metric_rows(2)
        self.assertCountEqual([row[:5] for row in rows], [
            [SITE, DEPARTMENT, "7", "1", 4], [SITE, "未填写部门", "4", "1", 2],
            [SITE, "仅有用量", "0", "1", 2], [SITE, "仅有注册", "5", "0", 0],
        ])
        inactive, = [row for row in rows if row[1] == "仅有注册"]
        self.assertEqual(inactive[-6:], [0] * 6)
        self.assertEqual(len(self.metric_rows(1)), 4)
        self.assertEqual(len(self.metric_rows(4)), 3)
        self.assertEqual(self.worksheet(2).find(f"{NS}autoFilter").get("ref"), "A1:J5")

    def test_no_usage_still_lists_registered_departments(self):
        self.write()
        self.build(registered={"": 2, "研发": 3})
        self.assertCountEqual(self.metric_rows(2), [
            [SITE, "未填写部门", "2", "0", *([0] * 6)],
            [SITE, "研发", "3", "0", *([0] * 6)],
        ])
        self.assertEqual(self.metric_rows(3), self.metric_rows(2))
        self.assertEqual(self.values(1), [DETAIL])
        self.assertEqual(self.values(4), [USERS])

    def test_registered_is_a_required_keyword_argument(self):
        self.write()
        with self.assertRaises(TypeError):
            workbook.build_workbook(self.xlsx, MONTH, {SITE: self.src})
        with self.assertRaises(TypeError):
            workbook.build_workbook(self.xlsx, MONTH, {SITE: self.src}, {})

    def test_exact_cost_and_large_token_counts_on_every_sheet(self):
        inputs, outputs, cached = 2**53 + 1, 10**20 + 17, 10**30 + 31
        total = inputs + outputs + cached + 11
        cost = "12345678901234568.999999999999999999"
        penny_fraction = "0.000000000000000001"
        combined = 12345678901234569
        self.write(record(platform="deepseek_usage", input_tokens=str(inputs), output_tokens=str(outputs),
                          cache_read_tokens=str(cached), total_tokens=str(total), actual_cost=cost),
                   record(platform="deepseek_usage", model="second", actual_cost=penny_fraction))
        with localcontext() as context:
            context.prec = 5
            summary = self.build()
        self.assert_summary(summary, rows=2, users=1, departments=1, requests=4,
                            total_tokens=total + 26, actual_cost=combined)
        self.assertCountEqual([row[-6:] for row in self.metric_rows(1)], [
            [2, inputs, outputs, cached, total, Decimal(cost)],
            [2, 3, 5, 7, 26, Decimal(penny_fraction)],
        ])
        totals = (inputs + 3, outputs + 5, cached + 7, total + 26)
        for sheet, places in ((3, 9), (4, 6)):
            with self.subTest(sheet=SHEETS[sheet - 1]):
                # Construct expected decimals exactly, even beyond the default context precision.
                scaled = [Decimal(f"{count}e-{places}") for count in totals]
                row, = self.metric_rows(sheet)
                self.assertEqual(row[-6:], [4, *scaled, combined])

    def test_empty_month_has_only_the_requested_sheets_and_headers(self):
        self.write()
        self.assert_summary(self.build(), rows=0, users=0, departments=0, requests=0,
                            total_tokens=0, actual_cost="0")
        with ZipFile(self.xlsx) as archive:
            book = ET.fromstring(archive.read("xl/workbook.xml"))
        self.assertEqual([sheet.get("name") for sheet in book.iter(f"{NS}sheet")], SHEETS)
        for index, header in enumerate((DETAIL, DEPARTMENTS, DEPARTMENTS, USERS), 1):
            with self.subTest(sheet=SHEETS[index - 1]):
                self.assertEqual(self.values(index), [header])

    def test_fixed_row_heights_and_wrapping_preserve_original_values(self):
        department = "研发/Research/" * 80 + "\n 第二行 "
        long_row = record(department=department, model="推理模型/" * 50)
        self.write(record(user_id="2", department="研发"), long_row)
        self.build(registered={"研发": 1, department: 1})
        self.assertIn([SITE, *long_row[:13]], self.values(1)[1:])

        with ZipFile(self.xlsx) as archive:
            styles = ET.fromstring(archive.read("xl/styles.xml")).find(f"{NS}cellXfs")
        for index, sheet in enumerate(SHEETS, 1):
            with self.subTest(sheet=sheet):
                document = self.worksheet(index)
                header, *data = self.values(index)
                self.assertIn(department, [row[header.index("当前所属部门")] for row in data])
                widths = self.widths(index)
                defaults = document.find(f"{NS}sheetFormatPr")
                default_height = defaults.get("defaultRowHeight", "15") if defaults is not None else "15"
                rows = document.findall(f"{NS}sheetData/{NS}row")
                self.assertEqual(len(rows), 3)
                for row_num, row in enumerate(rows, 1):
                    self.assertEqual(float(row.get("ht", default_height)), 32 if row_num == 1 else 20,
                                     f"{sheet} row {row_num}")
                    if "ht" in row.attrib:
                        self.assertIn(row.get("customHeight"), ("1", "true"), f"{sheet} row {row_num}")
                    for label, cell in zip(header, row):
                        style = styles[int(cell.get("s", "0"))]
                        alignment = style.find(f"{NS}alignment")
                        wraps = alignment is not None and alignment.get("wrapText") in ("1", "true")
                        if row_num > 1:
                            self.assertFalse(wraps, cell.get("r"))
                        elif widths[label] < sum(1 if ch.isascii() else 2 for ch in label):
                            self.assertTrue(wraps, label)

    def test_department_width_follows_typical_display_width_on_each_sheet(self):
        # Distinct departments and users keep the same distribution after either rollup.
        def measure(prefix, first_prefix=None):
            if first_prefix is None:
                first_prefix = prefix
            departments = [f"{first_prefix if i == 0 else prefix}{i:02}" for i in range(100)]
            self.write(*(record(user_id=str(i), department=dept) for i, dept in enumerate(departments)))
            self.build(registered=dict.fromkeys(departments, 1))
            return [self.widths(i)["当前所属部门"] for i in (1, 2, 3, 4)]

        short = measure("A")
        one_outlier = measure("A", "部" * 200)
        typical_ascii = measure("A" * 19)
        typical_chinese = measure("部" * 19)
        for i, sheet in enumerate(SHEETS):
            with self.subTest(sheet=sheet):
                self.assertLess(short[i], typical_ascii[i])
                self.assertLess(typical_ascii[i], typical_chinese[i])
                self.assertLess(one_outlier[i], typical_ascii[i])

    def test_token_widths_match_and_identity_and_headcount_columns_stay_compact(self):
        # Long IDs are text (including leading zeroes). All token units are exercised.
        uid = "0" * 60 + "7"
        department = "研发中心/基础设施/数据平台/用量计量"
        self.write(record(user_id=uid, department=department, input_tokens="1", output_tokens="2",
                          cache_read_tokens="12345678901234", total_tokens="12345678901237"))
        self.build(registered={department: 765})
        for index, suffix in enumerate(("", " (B)", " (B)", " (M)"), 1):
            with self.subTest(sheet=SHEETS[index - 1]):
                widths = self.widths(index)
                token_widths = [widths[label + suffix] for label in TOKENS]
                self.assertEqual(token_widths, [token_widths[0]] * len(TOKENS))
                if index in (2, 3):
                    # The long registration header can wrap over two lines.
                    for label in ("注册且未被封禁人数", "活跃用户数"):
                        self.assertLess(widths[label], len("注册且未被封禁人数") * 2, label)
                else:
                    self.assertLess(widths["用户 ID"], widths["当前所属部门"])
                    header, row = self.values(index)
                    self.assertEqual(row[header.index("用户 ID")], uid)

    def test_one_extreme_token_count_does_not_stretch_four_columns(self):
        ordinary = [record(user_id=str(i), department=f"部门{i}") for i in range(100)]
        # The first count needs exact text. The second fits in Excel's numeric precision
        # but its ordinary decimal display is just as wide.
        for count in ("9" * 240, "1" + "0" * 239):
            outlier = record(user_id="extreme", department="极端值", input_tokens="0",
                             output_tokens="0", cache_read_tokens=count, total_tokens=count)
            self.write(*ordinary, outlier)
            self.build()
            for index, suffix, scale in ((1, "", 0), (2, " (B)", 9), (4, " (M)", 6)):
                with self.subTest(leading_digit=count[0], sheet=SHEETS[index - 1]):
                    widths = self.widths(index)
                    for label in TOKENS:
                        self.assertLess(widths[label + suffix], 40, label)
                    header, *rows = self.values(index)
                    col = header.index("总 Token" + suffix)
                    self.assertIn(Decimal(f"{count}e-{scale}"), [Decimal(row[col]) for row in rows])

    def test_numeric_width_uses_typical_values_on_its_own_sheet(self):
        def measure(requests, cost):
            self.write(*(record(platform=platform, model=f"{platform}{sign}", requests=requests,
                                actual_cost=sign + cost)
                         for platform in ("openai_usage", "deepseek_usage") for sign in ("", "-")))
            self.build()
            return [self.widths(i) for i in (1, 2, 3, 4)]

        small = measure("1", "1")
        large = measure("123456789012345", "123456789012345678901234567890.1")
        # Requests grow on all sheets. The two costs cancel on all summaries.
        for index, sheet in enumerate(SHEETS):
            with self.subTest(sheet=sheet):
                self.assertGreater(large[index]["请求数"], small[index]["请求数"])
                if index == 0:
                    self.assertGreater(large[index][COST], small[index][COST])
                else:
                    self.assertEqual(large[index][COST], small[index][COST])

    def test_text_is_literal_and_report_is_private(self):
        name = " 王<&>\n五 "
        group = "https://example.test/path"
        department = "@部门"
        data = record(username="=1+1", profile_name=name, department=department,
                      used_groups=group, model="+model")
        self.write(data)
        self.build(registered={department: 1})
        for sheet, labels in enumerate(([SITE, *data[:7]], [SITE, department], [SITE, department],
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
            "old header": [HEADER[:13]],
            "invalid platform JSON": [HEADER, record(openai_usage="{")],
            "wrong platform type": [HEADER, record(deepseek_usage="null")],
            "missing platform metric": [HEADER, record(openai_usage='["1"]')],
            "JSON number": [HEADER, record(deepseek_usage='[1,"2","3","4","5","0"]')],
            "fractional platform count": [HEADER, record(openai_usage='["1","1.1","0","0","0","0"]')],
            "present zero requests": [HEADER, record(openai_usage='["0","0","0","0","0","0"]')],
            "nonfinite platform cost": [HEADER, record(deepseek_usage='["1","2","3","4","5","NaN"]')],
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
            workbook.build_workbook(self.xlsx, MONTH, {SITE: self.src, "other": self.src}, registered={})
        with self.assertRaises(ValueError):
            workbook.build_workbook(self.src, MONTH, {SITE: self.src}, registered={})
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
