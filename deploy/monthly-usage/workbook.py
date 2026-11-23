"""Build the monthly XLSX locally from UTF-8 PostgreSQL COPY CSV files."""

from __future__ import annotations

import csv
import os
import re
import shutil
from unicodedata import combining, east_asian_width
from dataclasses import dataclass
from datetime import date
from decimal import Context, Decimal, Inexact
from pathlib import Path
from tempfile import NamedTemporaryFile, SpooledTemporaryFile
from xml.sax.saxutils import escape
from zipfile import ZIP_DEFLATED, ZipFile


COLUMNS = (
    "usage_day", "user_id", "username", "profile_name", "department", "used_groups", "model",
    "requests", "input_tokens", "output_tokens", "cache_read_tokens", "total_tokens", "actual_cost",
)
METRICS = ("请求数", "输入 Token", "输出 Token", "缓存读取 Token", "总 Token", "用户扣费（美元）")
IDENTITY = ("用户 ID", "用户名", "当前真实姓名", "当前所属部门")
DETAIL = ("站点", "用量日期", *IDENTITY, "使用分组", "模型", *METRICS)
def _monthly_headers(unit):
    return (METRICS[0], *(f"{name} ({unit})" for name in METRICS[1:-1]), METRICS[-1])


DEPARTMENT = ("站点", "当前所属部门", "注册且未被封禁人数", "活跃用户数", *_monthly_headers("B"))
USER = ("站点", *IDENTITY, *_monthly_headers("M"))
SHEETS = ("明细", "部门月汇总", "用户月汇总")
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


@dataclass
class Totals:
    requests: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    cache_read_tokens: int = 0
    total_tokens: int = 0
    actual_cost: Decimal = Decimal(0)

    def add(self, other, arithmetic):
        self.requests += other.requests
        self.input_tokens += other.input_tokens
        self.output_tokens += other.output_tokens
        self.cache_read_tokens += other.cache_read_tokens
        self.total_tokens += other.total_tokens
        self.actual_cost = arithmetic.add(self.actual_cost, other.actual_cost)

    def cells(self):
        return (self.requests, self.input_tokens, self.output_tokens,
                self.cache_read_tokens, self.total_tokens, self.actual_cost)

    def monthly(self, exponent, arithmetic):
        # Scale after aggregation. Keep all token digits in the cell and truncate USD toward zero.
        return (self.requests, *(arithmetic.scaleb(Decimal(n), exponent) for n in self.cells()[1:-1]),
                int(self.actual_cost))


def _details(month, site, source, users, overall, arithmetic):
    yield DETAIL
    with Path(source).open(encoding="utf-8-sig", newline="") as stream:
        reader = csv.reader(stream, strict=True)
        try:
            if tuple(next(reader, ())) != COLUMNS:
                raise ValueError("missing or invalid CSV header")
            for row in reader:
                if len(row) != len(COLUMNS):
                    raise ValueError("unexpected column count")
                raw_day, uid, username, profile, department, group, model = row[:7]
                if not raw_day.startswith(month + "-") or date.fromisoformat(raw_day).isoformat() != raw_day:
                    raise ValueError("invalid day or day outside month")
                if not uid:
                    raise ValueError("missing user ID")
                identity = (username, profile, department)
                metrics = Totals(*(_number(raw, key) for raw, key in zip(row[7:], COLUMNS[7:])))
                if uid not in users:
                    users[uid] = (identity, Totals())
                existing, usage = users[uid]
                if identity != existing:
                    raise ValueError("inconsistent user profile in monthly snapshot")
                usage.add(metrics, arithmetic)
                overall.add(metrics, arithmetic)
                yield (site, raw_day, uid, *identity, group, model, *metrics.cells())
        except (ValueError, csv.Error) as exc:
            raise ValueError(f"{site!r}, CSV line {reader.line_num}: {exc}") from exc


def _line_width(text):
    return sum(0 if combining(c) else 2 if east_asian_width(c) in ("F", "W") else 1 for c in text)


# Widths use Excel's character units. Floors leave room for a two-line heading
# and the filter button. Ordinary numbers keep room for their displayed value.
_TEXT_BOUNDS = {
    "站点": (10, 18), "用量日期": (13, 13), "用户 ID": (9, 24), "用户名": (10, 22),
    "当前真实姓名": (11, 22), "当前所属部门": (12, 42), "使用分组": (13, 26), "模型": (18, 36),
}
_NUMBER_FLOORS = {"注册且未被封禁人数": 15, "活跃用户数": 11, "请求数": 10, METRICS[-1]: 14}


def _sheet_widths(headers, samples, numeric):
    widths = []
    for i, label in enumerate(headers):
        if label in _TEXT_BOUNDS:
            lo, hi = _TEXT_BOUNDS[label]
            hist = samples[i]
            # Use the 95th percentile with a little headroom for rarer labels.
            # Buckets are bounded so a million rows cost no extra layout memory.
            target = (sum(hist) * 95 + 99) // 100
            p95 = 0
            while p95 < hi and target > hist[p95]:
                target -= hist[p95]
                p95 += 1
            longest = max((n for n, count in enumerate(hist) if count), default=0)
            widths.append(max(lo, min(hi, longest + 2, p95 + 7)))
        else:
            widths.append(max(_NUMBER_FLOORS.get(label, 14), numeric[i]))
    # Compare like units in equal-width columns. A rare precision-preserving
    # text cell mustn't widen all four token columns to the length of a paragraph.
    token_cols = [i for i, label in enumerate(headers) if "Token" in label]
    common = max(widths[i] for i in token_cols)
    for i in token_cols:
        widths[i] = common
    return widths


def _sheet(archive, index, rows, scratch):
    """Measure displayed cells as we write; <cols> must precede <sheetData> in XLSX."""
    with SpooledTemporaryFile(max_size=1024 * 1024, dir=scratch) as data:
        for row_num, row in enumerate(rows, 1):
            if row_num > MAX_ROWS:
                raise ValueError(f"{SHEETS[index - 1]} exceeds Excel row limit ({MAX_ROWS} including header)")
            if row_num == 1:
                headers = row
                samples = [[0] * (_TEXT_BOUNDS[label][1] + 1) if label in _TEXT_BOUNDS else None
                           for label in headers]
                numeric = [0] * len(row)
            cells = []
            for col, value in enumerate(row):
                ref = f"{chr(ord('A') + col)}{row_num}"
                if not isinstance(value, str):
                    _check_range(value)
                    plain = format(value, "f") if isinstance(value, Decimal) else str(value)
                    if len(plain.lstrip("-").replace(".", "").strip("0")) > 15:
                        value = plain
                if isinstance(value, str):
                    style = ' s="1"'
                    if row_num != 1:
                        span = max((_line_width(line) for line in value.splitlines()), default=0)
                        hist = samples[col]
                        if hist is not None:
                            limit = len(hist) - 1
                            hist[min(span, limit)] += 1
                        else:  # number stored as exact text (beyond Excel's precision)
                            limit = 28
                            numeric[col] = max(numeric[col], min(limit, span + 2))
                        # Slight overflows can shrink; very long strings stay
                        # readable at normal size, with full text in the formula bar.
                        style = ' s="6"' if span <= limit * 1.4 else ""
                    cells.append(f'<c r="{ref}"{style} t="inlineStr"><is><t xml:space="preserve">'
                                 f'{_text(value)}</t></is></c>')
                else:
                    if isinstance(value, int):
                        style, displayed = 2, format(value, ",d")
                    elif index == 1:  # original USD amount
                        style = 3
                        whole, fraction = format(value, ",.10f").split(".")
                        displayed = whole + "." + fraction.rstrip("0").ljust(2, "0")
                    elif value and value.copy_abs() < Decimal("0.001"):
                        style, displayed = 5, format(value, ",.9f").rstrip("0")
                    else:
                        style, displayed = 4, format(value, ",.3f") if value else "0"
                    if len(displayed) > 28:
                        # A power of ten can be an exact Excel number and still
                        # need hundreds of columns' worth of digits in fixed point.
                        style, displayed = 7, format(Decimal(value), ".2E")
                    cells.append(f'<c r="{ref}" s="{style}" t="n"><v>{value}</v></c>')
                    numeric[col] = max(numeric[col], min(255, len(displayed) + 2))
            ht = 32 if row_num == 1 else 20
            data.write(f'<row r="{row_num}" ht="{ht}" customHeight="1">{"".join(cells)}</row>'.encode())
        data.seek(0)
        with archive.open(f"xl/worksheets/sheet{index}.xml", "w", force_zip64=True) as part:
            widths = _sheet_widths(headers, samples, numeric)
            cols = "".join(f'<col min="{i}" max="{i}" width="{w}" customWidth="1"/>'
                           for i, w in enumerate(widths, 1))
            part.write((f'{XML_DECL}<worksheet xmlns="{XMLNS}"><sheetViews><sheetView workbookViewId="0">'
                        '<pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/>'
                        f'</sheetView></sheetViews><sheetFormatPr defaultRowHeight="20"/>'
                        f'<cols>{cols}</cols><sheetData>').encode())
            shutil.copyfileobj(data, part)
            last = chr(ord("A") + len(widths) - 1)
            part.write(f'</sheetData><autoFilter ref="A1:{last}{row_num}"/></worksheet>'.encode())
    return row_num - 1


# Currency uses at least two decimals and can show the ledger's ten decimal places.
_STYLES = f"""<styleSheet xmlns="{XMLNS}">
<numFmts count="3"><numFmt numFmtId="164" formatCode="#,##0.00########"/>
<numFmt numFmtId="165" formatCode="#,##0.000;-#,##0.000;0"/>
<numFmt numFmtId="166" formatCode="0.000######"/></numFmts>
<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font>
<font><b/><color rgb="FFFFFFFF"/><sz val="11"/><name val="Calibri"/></font></fonts>
<fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill>
<fill><patternFill patternType="solid"><fgColor rgb="FF25476A"/><bgColor indexed="64"/></patternFill></fill></fills>
<borders count="1"><border/></borders>
<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
<cellXfs count="8"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment vertical="center"/></xf>
<xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf>
<xf numFmtId="3" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1" applyAlignment="1"><alignment vertical="center"/></xf>
<xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1" applyAlignment="1"><alignment vertical="center"/></xf>
<xf numFmtId="165" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1" applyAlignment="1"><alignment vertical="center"/></xf>
<xf numFmtId="166" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1" applyAlignment="1"><alignment vertical="center"/></xf>
<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment vertical="center" shrinkToFit="1"/></xf>
<xf numFmtId="11" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1" applyAlignment="1"><alignment vertical="center"/></xf></cellXfs>
<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>
</styleSheet>"""


def _package(archive):
    mime = "application/vnd.openxmlformats-officedocument.spreadsheetml"
    archive.writestr("xl/styles.xml", XML_DECL + _STYLES)
    overrides = "".join(f'<Override PartName="/xl/worksheets/sheet{i}.xml" '
                        f'ContentType="{mime}.worksheet+xml"/>' for i in range(1, len(SHEETS) + 1))
    archive.writestr("[Content_Types].xml", XML_DECL +
                     '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
                     '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>'
                     '<Default Extension="xml" ContentType="application/xml"/>'
                     f'<Override PartName="/xl/workbook.xml" ContentType="{mime}.sheet.main+xml"/>'
                     f'<Override PartName="/xl/styles.xml" ContentType="{mime}.styles+xml"/>'
                     f'{overrides}</Types>')
    archive.writestr("_rels/.rels", XML_DECL + f'<Relationships xmlns="{PKG_REL}">'
                     f'<Relationship Id="rId1" Type="{DOC_REL}/officeDocument" Target="xl/workbook.xml"/>'
                     '</Relationships>')
    sheets = []
    relationships = [f'<Relationship Id="styles" Type="{DOC_REL}/styles" Target="styles.xml"/>']
    for i, name in enumerate(SHEETS, 1):
        sheets.append(f'<sheet name="{name}" sheetId="{i}" r:id="rId{i}"/>')
        relationships.append(f'<Relationship Id="rId{i}" Type="{DOC_REL}/worksheet" '
                             f'Target="worksheets/sheet{i}.xml"/>')
    archive.writestr("xl/workbook.xml", XML_DECL + f'<workbook xmlns="{XMLNS}" xmlns:r="{DOC_REL}">'
                     f'<bookViews><workbookView/></bookViews><sheets>{"".join(sheets)}</sheets></workbook>')
    archive.writestr("xl/_rels/workbook.xml.rels", XML_DECL +
                     f'<Relationships xmlns="{PKG_REL}">{"".join(relationships)}</Relationships>')


def build_workbook(output: Path, month: str, sources: dict[str, Path], *, registered: dict[str, int]) -> dict:
    """Write one site's three sheets from a single private COPY snapshot."""
    if not re.fullmatch(r"[0-9]{4}-(?:0[1-9]|1[0-2])", month) or month.startswith("0000"):
        raise ValueError("month must be YYYY-MM")
    if len(sources) != 1:
        raise ValueError("each workbook requires exactly one source site")
    site, source = next(iter(sources.items()))
    if not isinstance(site, str) or not site.strip():
        raise ValueError("site must be a nonempty name")
    if not isinstance(registered, dict) or any(not isinstance(k, str) or type(v) is not int or v < 0
                                               for k, v in registered.items()):
        raise ValueError("invalid registered headcounts")
    output = Path(output)
    if output.is_symlink() or output.resolve() == Path(source).resolve():
        raise ValueError("output must be separate from the source and cannot be a symlink")
    # COPY decimals have at most MAX_TEXT fractional places and at most 308
    # integer digits. Seven extra digits cover summing a full worksheet.
    arithmetic = Context(prec=MAX_TEXT + 308 + 7)
    arithmetic.traps[Inexact] = True
    users = {}
    total = Totals()
    tmp = NamedTemporaryFile(dir=output.parent, prefix=".monthly-usage-", delete=False)
    try:
        with tmp, ZipFile(tmp, "w", compression=ZIP_DEFLATED) as archive:
            _package(archive)
            rows = _sheet(archive, 1, _details(month, site, source, users, total, arithmetic), output.parent)
            # Each account contributes its complete usage to exactly one department.
            # DingTalk stores an organization path; a parent is not a second allocation.
            departments = {dept: Totals() for dept in registered}
            counts = dict.fromkeys(registered, 0)
            for (_, _, department), usage in users.values():
                if department not in departments:
                    departments[department] = Totals()
                    counts[department] = 0
                departments[department].add(usage, arithmetic)
                counts[department] += 1

            def department_rows():
                yield DEPARTMENT
                for key, usage in sorted(departments.items(), key=lambda item: (-item[1].total_tokens, item[0])):
                    yield (site, key or "未填写部门", registered.get(key, 0), counts[key], *usage.monthly(-9, arithmetic))

            def user_rows():
                yield USER
                for uid, (identity, usage) in sorted(users.items(), key=lambda item: (-item[1][1].total_tokens, item[0])):
                    yield (site, uid, *identity, *usage.monthly(-6, arithmetic))

            _sheet(archive, 2, department_rows(), output.parent)
            _sheet(archive, 3, user_rows(), output.parent)
        os.replace(tmp.name, output)
    finally:
        Path(tmp.name).unlink(missing_ok=True)
    return {site: dict(rows=rows, users=len(users), departments=len(departments),
                       requests=total.requests, total_tokens=total.total_tokens,
                       actual_cost=format(total.actual_cost, "f"))}
