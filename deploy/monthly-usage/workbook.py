"""Build the monthly XLSX locally from UTF-8 PostgreSQL COPY CSV files."""

from __future__ import annotations

import calendar
import csv
import os
import re
from dataclasses import dataclass, field
from datetime import date
from decimal import Context, Decimal, Inexact
from pathlib import Path
from tempfile import NamedTemporaryFile
from xml.sax.saxutils import escape
from zipfile import ZIP_DEFLATED, ZipFile


COLUMNS = (
    "usage_day", "user_id", "username", "email", "used_groups", "allowed_groups",
    "subscription_groups", "model", "requests", "input_tokens", "output_tokens",
    "cache_creation_tokens", "cache_read_tokens", "total_tokens", "total_cost", "actual_cost",
    "profile_name", "department",
)
DETAIL = (
    "站点", "用量日期", "用户 ID", "用户名", "邮箱", "使用分组", "允许分组", "订阅分组",
    "模型", "请求数", "输入 Token", "输出 Token", "缓存创建 Token", "缓存读取 Token",
    "总 Token", "总费用", "实际费用", "当前真实姓名", "当前所属部门", "精确原值",
)
SUMMARY = ("站点", "明细行数", "请求数", "总 Token", "总费用", "实际费用", "用户数", "精确原值")
DAILY = ("站点", "日期", "日志请求数", "仪表盘请求数", "请求数差额", "日志总 Token",
         "仪表盘总 Token", "总 Token 差额", "日志实际费用", "仪表盘实际费用", "实际费用差额", "对账状态", "精确原值")
SHEETS = ("说明", "按站点汇总", "明细", "每日对账")
MAX_ROWS = 1_048_576  # includes header
MAX_TEXT = 32_767
XMLNS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
DOC_REL = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
PKG_REL = "http://schemas.openxmlformats.org/package/2006/relationships"
XML_DECL = '<?xml version="1.0" encoding="UTF-8"?>'
INVALID_XML = re.compile(r"[\x00-\x08\x0b\x0c\x0e-\x1f\ud800-\udfff\ufffe\uffff]")
EXCEL_ESCAPE = re.compile(r"_(?=x[0-9a-fA-F]{4}_)")
MIN_NUMBER = Decimal("2.2250738585072014e-308")
MAX_NUMBER = Decimal("9.99999999999999e307")


def _text(value):
    if INVALID_XML.search(value) or len(value.encode("utf-16-le")) // 2 > MAX_TEXT:
        raise ValueError("invalid XML character or text exceeds Excel's cell length limit")
    value = EXCEL_ESCAPE.sub("_x005F_", value)
    return escape(value).replace("\r", "&#13;")


def _check_range(number):
    magnitude = abs(number) if isinstance(number, int) else number.copy_abs()
    if magnitude and not MIN_NUMBER <= magnitude <= MAX_NUMBER:
        raise ValueError("number outside Excel's supported range")


def _number(raw, field):
    if field.endswith("_cost"):
        if not isinstance(raw, str) or len(raw) > MAX_TEXT or not re.fullmatch(
            r"[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]{1,5})?", raw
        ):
            raise ValueError(f"invalid decimal in {field}")
        value = Decimal(raw)
        if abs(value.as_tuple().exponent) > MAX_TEXT:
            raise ValueError(f"decimal scale exceeds Excel cell length in {field}")
    else:
        if not re.fullmatch(r"[0-9]{1,308}", raw):
            raise ValueError(f"invalid count in {field}")
        value = int(raw)
    return value


def _with_exact_values(prefix, names, values, suffix=()):
    exact = []
    for name, value in zip(names, values):
        if isinstance(value, str):  # missing dashboard measurement
            continue
        _check_range(value)  # summary sums can exceed the range of individual rows
        plain = format(value, "f") if isinstance(value, Decimal) else str(value)
        digits = plain.lstrip("-").replace(".", "").strip("0")
        if len(digits) > 15:
            exact.append(f"{name}={plain}")
    return (*prefix, *values, *suffix, "\n".join(exact))


@dataclass
class _DailyUsage:
    requests: int = 0
    total_tokens: int = 0
    actual_cost: Decimal = Decimal(0)


@dataclass
class _SiteUsage:
    rows: int = 0
    users: set = field(default_factory=set)
    total_cost: Decimal = Decimal(0)
    days: dict = field(default_factory=dict)


def _day(raw, month):
    if not isinstance(raw, str) or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", raw):
        raise ValueError("day must be YYYY-MM-DD")
    day = date.fromisoformat(raw)
    if raw[:7] != month:
        raise ValueError("day outside report month")
    return day


def _details(month, site, source, usage, arithmetic):
    yield DETAIL
    with Path(source).open(encoding="utf-8-sig", newline="") as stream:
        reader = csv.reader(stream, strict=True)
        try:
            if tuple(next(reader, ())) != COLUMNS:
                raise ValueError("missing or invalid CSV HEADER (including column order)")
            for row in reader:
                if len(row) != len(COLUMNS):
                    raise ValueError(f"expected {len(COLUMNS)} columns, got {len(row)}")
                day = _day(row[0], month)
                names = COLUMNS[8:16]
                numbers = tuple(_number(raw, name) for raw, name in zip(row[8:16], names))
                requests = numbers[0]
                total_tokens, total_cost, actual_cost = numbers[-3:]
                log = usage.days.setdefault(day, _DailyUsage())
                log.requests += requests
                log.total_tokens += total_tokens
                log.actual_cost = arithmetic.add(log.actual_cost, actual_cost)
                usage.rows += 1
                if row[1]:
                    usage.users.add(row[1])
                usage.total_cost = arithmetic.add(usage.total_cost, total_cost)
                yield _with_exact_values((site, *row[:8]), names, numbers, row[16:])
        except (ValueError, csv.Error) as exc:
            raise ValueError(f"{site!r}, CSV line {reader.line_num}: {exc}") from exc


@dataclass
class _Comparison:
    comparable_days: int = 0
    different_days: int = 0
    net: _DailyUsage = field(default_factory=_DailyUsage)


def _daily_rows(month, site, logs, history, comparison, arithmetic):
    yield DAILY
    dashboard = {}
    for record in history:
        day = _day(record["day"], month)
        if day in dashboard:
            raise ValueError(f"duplicate dashboard day: {day}")
        dashboard[day] = _DailyUsage(
            _number(str(record["requests"]), "requests"),
            _number(str(record["total_tokens"]), "total_tokens"),
            _number(record["actual_cost"], "actual_cost"),
        )
    first = date.fromisoformat(month + "-01")
    for n in range(1, calendar.monthrange(first.year, first.month)[1] + 1):
        day = first.replace(day=n)
        log = logs.get(day, _DailyUsage())
        other = dashboard.get(day)
        if day not in logs and other is None:
            state = "未见记录"
            values = ("未见记录", "未知", "未知") * 3
        elif other is None:
            state = "仪表盘未知"
            values = (log.requests, "未知", "未知", log.total_tokens, "未知", "未知",
                      log.actual_cost, "未知", "未知")
        else:
            req = other.requests - log.requests
            tokens = other.total_tokens - log.total_tokens
            cost = arithmetic.subtract(other.actual_cost, log.actual_cost)
            different = bool(req or tokens or cost)
            state = "有差额" if different else "相符"
            if day not in logs:
                state += "（仅仪表盘有记录）"
            values = (log.requests, other.requests, req, log.total_tokens, other.total_tokens, tokens,
                      log.actual_cost, other.actual_cost, cost)
            comparison.comparable_days += 1
            comparison.different_days += different
            comparison.net.requests += req
            comparison.net.total_tokens += tokens
            comparison.net.actual_cost = arithmetic.add(comparison.net.actual_cost, cost)
        yield _with_exact_values((site, day.isoformat()), DAILY[2:-2], values, (state,))


def _sheet(archive, index, widths, rows, *, table=True):
    """Stream worksheet XML straight into the zip; only one row is buffered."""
    with archive.open(f"xl/worksheets/sheet{index}.xml", "w", force_zip64=True) as part:
        def write(xml):
            part.write(xml.encode("utf-8"))

        write(f'{XML_DECL}<worksheet xmlns="{XMLNS}">')
        if table:
            write('<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" '
                  'topLeftCell="A2" activePane="bottomLeft" state="frozen"/>'
                  '</sheetView></sheetViews>')
        write("<cols>")
        for col, width in enumerate(widths, 1):
            write(f'<col min="{col}" max="{col}" width="{width}" customWidth="1"/>')
        write("</cols><sheetData>")
        for row_num, row in enumerate(rows, 1):
            if row_num > MAX_ROWS:
                raise ValueError(f"{SHEETS[index - 1]} exceeds Excel row limit ({MAX_ROWS} including header)")
            cells = []
            for col, value in enumerate(row):
                ref = f"{chr(ord('A') + col)}{row_num}"  # all report sheets fit within A:Z
                if isinstance(value, str):
                    cells.append(f'<c r="{ref}" t="inlineStr"><is><t xml:space="preserve">'
                                 f'{_text(value)}</t></is></c>')
                else:
                    cells.append(f'<c r="{ref}" t="n"><v>{value}</v></c>')
            write(f'<row r="{row_num}">{"".join(cells)}</row>')
        write("</sheetData>")
        if table:
            last = chr(ord("A") + len(widths) - 1)
            write(f'<autoFilter ref="A1:{last}{row_num}"/>')
        write("</worksheet>")


def _package(archive):
    mime = "application/vnd.openxmlformats-officedocument.spreadsheetml"
    overrides = "".join(f'<Override PartName="/xl/worksheets/sheet{i}.xml" '
                        f'ContentType="{mime}.worksheet+xml"/>' for i in range(1, len(SHEETS) + 1))
    archive.writestr("[Content_Types].xml", XML_DECL +
                     '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
                     '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>'
                     '<Default Extension="xml" ContentType="application/xml"/>'
                     f'<Override PartName="/xl/workbook.xml" ContentType="{mime}.sheet.main+xml"/>'
                     f'{overrides}</Types>')
    archive.writestr("_rels/.rels", XML_DECL + f'<Relationships xmlns="{PKG_REL}">'
                     f'<Relationship Id="rId1" Type="{DOC_REL}/officeDocument" Target="xl/workbook.xml"/>'
                     '</Relationships>')
    sheets = []
    relationships = []
    for i, name in enumerate(SHEETS, 1):
        sheets.append(f'<sheet name="{name}" sheetId="{i}" r:id="rId{i}"/>')
        relationships.append(f'<Relationship Id="rId{i}" Type="{DOC_REL}/worksheet" '
                             f'Target="worksheets/sheet{i}.xml"/>')
    archive.writestr("xl/workbook.xml", XML_DECL + f'<workbook xmlns="{XMLNS}" xmlns:r="{DOC_REL}">'
                     f'<bookViews><workbookView/></bookViews><sheets>{"".join(sheets)}</sheets></workbook>')
    archive.writestr("xl/_rels/workbook.xml.rels", XML_DECL +
                     f'<Relationships xmlns="{PKG_REL}">{"".join(relationships)}</Relationships>')


def build_workbook(output: Path, month: str, sources: dict[str, Path], reconciliation=None) -> dict:
    """Publish one site's XLSX and return {site: totals} with exact decimal strings.

    Requires UTF-8 COPY CSV with its exact header. Optional reconciliation
    maps each site to daily dashboard records (day, requests, total_tokens,
    actual_cost). Missing days remain unknown. Invalid input preserves prior output.
    """
    if not re.fullmatch(r"[0-9]{4}-(?:0[1-9]|1[0-2])", month) or month.startswith("0000"):
        raise ValueError("month must be YYYY-MM")
    if len(sources) != 1:
        raise ValueError("each workbook requires exactly one source site")
    site, source = next(iter(sources.items()))
    if not isinstance(site, str) or not site.strip():
        raise ValueError("site must be a nonempty name")
    output = Path(output)
    if output.is_symlink() or output.resolve() == Path(source).resolve():
        raise ValueError("output must be separate from the source and cannot be a symlink")
    # COPY decimals have at most MAX_TEXT fractional places and at most 308
    # integer digits. Seven extra digits cover summing a full worksheet.
    arithmetic = Context(prec=MAX_TEXT + 308 + 7)
    arithmetic.traps[Inexact] = True
    usage = _SiteUsage()
    tmp = NamedTemporaryFile(dir=output.parent, prefix=".monthly-usage-", delete=False)
    try:
        with tmp, ZipFile(tmp, "w", compression=ZIP_DEFLATED) as archive:
            _package(archive)
            _sheet(archive, 3, (20, 15, 22, 24, 36, 24, 24, 24, 30, 15, 20, 20, 20, 20,
                                20, 22, 22, 22, 24, 55), _details(month, site, source, usage, arithmetic))
            actual = Decimal(0)
            for log in usage.days.values():
                actual = arithmetic.add(actual, log.actual_cost)
            total = dict(rows=usage.rows, requests=sum(log.requests for log in usage.days.values()),
                         total_tokens=sum(log.total_tokens for log in usage.days.values()),
                         total_cost=usage.total_cost, actual_cost=actual, users=len(usage.users))
            summary_row = _with_exact_values((site,), total.keys(), total.values())
            _sheet(archive, 2, (20, 14, 18, 22, 22, 22, 14, 55), (SUMMARY, summary_row))
            history = reconciliation.get(site, ()) if reconciliation is not None else ()
            comparison = _Comparison()
            _sheet(archive, 4, (20, 15, 16, 18, 16, 22, 22, 22, 22, 22, 22, 30, 55),
                   _daily_rows(month, site, usage.days, history, comparison, arithmetic))
            net = comparison.net
            net_values = (net.requests, net.total_tokens, net.actual_cost)
            if not comparison.comparable_days:
                net_values = ("未知",) * 3
            notes = [
                ("月度用量报告", month),
                ("站点", site),
                ("本月日志", f"{usage.rows} 条明细" if usage.rows else "本月无用量日志记录"),
                ("可对账天数", comparison.comparable_days),
                ("其中有差额的天数", comparison.different_days if comparison.comparable_days else "未知"),
                ("净差项目", "仪表盘 − 日志（仅可对账日）", "精确原值"),
                *(_with_exact_values((label,), (label,), (value,)) for label, value in
                  zip(("净差请求数", "净差 Token", "净差实际费用（美元）"), net_values)),
                ("分组", "使用分组是该用户当天在该模型上实际使用的组。"),
                ("允许分组", "导出时显式授权给该用户的组，未列公开组。"),
                ("订阅分组", "导出时仍有效的订阅组。"),
                ("明细", "每行对应一天、一个用户和一个模型。真实姓名和部门取导出时的资料，缺失时留空。"),
                ("汇总", "按日志汇总。请求数累计各行请求，用户数按非空用户 ID 去重。"),
                ("金额", "总费用为原始费用，实际费用为用户计费金额，单位均为美元。计费额包含订阅用量，不等同于现金收支。"),
                ("每日对账", "按北京时间逐日对比独立仪表盘历史汇总与用量日志。差额 = 仪表盘 − 日志。"),
                ("历史差额", "差额无法分配给具体用户或模型，不计入用户明细和日志汇总。"),
                ("未知", "缺少当日仪表盘记录时，仪表盘数值和差额均为未知。已有记录中的零保留为零。"),
                ("未见记录", "日志和仪表盘均无记录时标为“未见记录”。仅有仪表盘记录时，当日日志合计按零参与对账。"),
                ("数值精度", "超过 15 位有效数字时，请以该行“精确原值”列的十进制文本核账。"),
            ]
            _sheet(archive, 1, (26, 100, 55), notes, table=False)
        for key in ("total_cost", "actual_cost"):
            total[key] = format(total[key], "f")
        os.replace(tmp.name, output)
    finally:
        Path(tmp.name).unlink(missing_ok=True)
    return {site: total}
