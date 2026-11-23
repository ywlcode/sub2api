#!/usr/bin/env python3
"""Scheduled monthly mail from one authoritative Sub2API ledger. No pip dependencies."""
import argparse
from datetime import date, datetime, timedelta
from email import policy
from email.headerregistry import Address
from email.message import EmailMessage
from email.utils import formatdate, make_msgid
import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import smtplib
import ssl
import subprocess
import sys
import tempfile
from zoneinfo import ZoneInfo

from workbook import build_workbook

HERE = Path(__file__).resolve().parent
TZ = ZoneInfo("Asia/Shanghai")
SITE = {"boscserver": "主站", "ecosserver": "独立站"}
SMTP_CONFIG = """
SELECT json_object_agg(key, value) FROM settings WHERE key IN
 ('smtp_host','smtp_port','smtp_from','smtp_username','smtp_password');
"""


def period(s):
    if not re.fullmatch(r'[0-9]{4}-(0[1-9]|1[0-2])', s):
        raise ValueError("invalid YYYY-MM")
    return date.fromisoformat(s + "-01")


def months(first, now):
    d = period(first)
    while d < now.astimezone(TZ).date().replace(day=1):
        yield d.isoformat()[:7]
        d = (d + timedelta(days=32)).replace(day=1)


def db(sql, month=None, output=subprocess.PIPE):
    args = ["docker", "exec", "-i", "-e", "PGCLIENTENCODING=UTF8", "-e",
            "PGOPTIONS=-c default_transaction_read_only=on -c statement_timeout=300000 "
            "-c lock_timeout=3000 -c max_parallel_workers_per_gather=0",
            "sub2api-postgres", "sh", "-c",
            'exec psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" "$@"',
            "psql"]
    if month is not None:
        args.extend(["-v", "month=" + period(month).isoformat()])
    return subprocess.run(args, input=sql.encode(), stdout=output, timeout=360,
                          check=True).stdout


def sync_dir(directory):
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_state(path, value):
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False, mode="w") as f:
        tmp = Path(f.name)
        try:
            json.dump(value, f, ensure_ascii=False)
            f.flush()
            os.fsync(f.fileno())
        except BaseException:
            tmp.unlink(missing_ok=True)
            raise
    os.replace(tmp, path)
    sync_dir(path.parent)


def archive(state, site, month, revision=None):
    suffix = f"-r{revision}" if revision is not None else ""
    return state / f"Sub2API-{site}-{month}{suffix}.xlsx"


def render(state, site, month, revision=None):
    filename = archive(state, site, month, revision)
    with tempfile.TemporaryDirectory(prefix=".build-", dir=state) as tmp:
        tmp = Path(tmp)
        src = tmp / "usage.csv"
        # The first result is one JSON line (IDs are not exported); the rest is COPY CSV.
        sql = "\n".join(("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;",
                         (HERE / "headcounts.sql").read_text(), (HERE / "usage.sql").read_text(), "COMMIT;"))
        with tempfile.TemporaryFile(dir=tmp) as snapshot, src.open("wb") as f:
            db(sql, month, snapshot)
            snapshot.seek(0)
            registered = json.loads(snapshot.readline())
            shutil.copyfileobj(snapshot, f)
        xlsx = tmp / "out.xlsx"
        summary = build_workbook(xlsx, month, {site: src}, registered=registered)[site]
        with xlsx.open("rb") as f:
            os.fsync(f.fileno())
        os.replace(xlsx, filename)
        sync_dir(state)
    print(f"rendered {site} {month}: {json.dumps(summary, ensure_ascii=False)}", flush=True)
    return filename


def mailbox(raw):
    if not isinstance(raw, str) or '\n' in raw or '\r' in raw:
        raise ValueError("invalid single mailbox")
    addr = Address(addr_spec=raw)
    if not addr.domain or not addr.username or raw != addr.addr_spec:
        raise ValueError("invalid single mailbox")
    return raw


class Uncertain(RuntimeError):
    pass


def deliver(state, site, month, recipient, smtp_factory=None, *, revision=None, note=None):
    path = archive(state, site, month, revision)
    marker = path.with_suffix(".delivery.json")
    if marker.exists():
        record = json.loads(marker.read_text())
        if record["status"] == "accepted":
            print(f"already accepted: {site} {month}", flush=True)
            return
        raise Uncertain(f"check SMTP records before retry: {site} {month}")
    if not path.exists():
        render(state, site, month, revision)
    cfg = json.loads(db(SMTP_CONFIG))
    sender = mailbox(cfg["smtp_from"].strip())
    recipient = mailbox(recipient)
    msg = EmailMessage(policy=policy.SMTP)
    version = f"（修订版 {revision}）" if revision is not None else ""
    msg["Subject"] = f"Sub2API {SITE[site]}（{site}）使用月报 · {month}{version}"
    msg["From"], msg["To"] = sender, recipient
    msg["Date"], msg["Message-ID"] = formatdate(localtime=False), make_msgid(domain=sender.rsplit('@', 1)[1])
    msg.set_content(f"{SITE[site]} {site} · {month}（北京时间）{version}。附件含明细、部门月汇总、用户月汇总。 "
                    "三张表均来自可归属用户的用量日志。总 Token 含未单列的缓存创建量；费用为用户扣费的美元等值（含订阅用量）。"
                    "部门 Token 用 B（十亿）、用户 Token 用 M（百万）；两张汇总表按部门或用户合计后舍去美元小数。"
                    "注册人数含截至月底已注册的零请求用户；部门、启用、删除及整账号封禁均按本次导出时状态判断。"
                    + ("\n" + note if note else ""))
    msg.add_attachment(path.read_bytes(), maintype="application",
                       subtype="vnd.openxmlformats-officedocument.spreadsheetml.sheet", filename=path.name)
    wire = msg.as_bytes()
    record = {"message_id": str(msg["Message-ID"]), "status": "sending"}
    host, port = cfg["smtp_host"].strip(), int(cfg["smtp_port"])
    context = ssl.create_default_context()
    factory = smtp_factory or (smtplib.SMTP_SSL if port == 465 else smtplib.SMTP)
    kwargs = {"timeout": 90}
    if port == 465:
        kwargs["context"] = context
    server = factory(host, port, **kwargs)
    try:
        server.ehlo()
        if port != 465:
            server.starttls(context=context)
            server.ehlo()
        server.login(cfg["smtp_username"], cfg["smtp_password"])
        size = server.esmtp_features.get("size")
        if size and size.isdigit() and int(size) > 0 and len(wire) > int(size):
            raise ValueError("attachment exceeds advertised SMTP limit")
        for op, arg in ((server.mail, sender), (server.rcpt, recipient)):
            code, _ = op(arg)
            if not 200 <= code < 300:
                raise RuntimeError(f"SMTP envelope rejected ({code})")
        write_state(marker, record)
        try:
            code, _ = server.data(wire)
        except smtplib.SMTPDataError as exc:
            # smtplib.data() rejects DATA before streaming the body.
            code = exc.smtp_code
        except (OSError, smtplib.SMTPException) as exc:
            raise Uncertain(f"SMTP result unknown: {site} {month}") from exc
        if not 200 <= code < 300:
            marker.unlink()
            sync_dir(state)
            raise RuntimeError(f"SMTP DATA rejected ({code})")
        write_state(marker, dict(record, status="accepted", accepted_at=datetime.now(TZ).isoformat()))
        print(f"SMTP accepted: {site} {month} ({code})", flush=True)
    finally:
        server.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("render", "send", "scheduled"))
    parser.add_argument("month", nargs="?")
    parser.add_argument("--config", type=Path, default=Path("/etc/sub2api-monthly-usage.json"))
    parser.add_argument("--state", type=Path, default=Path("/var/lib/sub2api-monthly-usage"))
    parser.add_argument("--revision", type=int, help="separately tracked manual correction (2 or higher)")
    parser.add_argument("--note", help="additional accounting context for the correction email")
    a = parser.parse_args()
    if a.revision is not None and (a.action == "scheduled" or a.revision < 2):
        parser.error("use revision 2 or higher with render or send")
    if a.note and (a.action != "send" or a.revision is None):
        parser.error("a note is only valid when sending a manual revision")
    os.umask(0o077)
    cfg = json.loads(a.config.read_text())
    site = cfg["site"]
    if site not in SITE:
        parser.error("expected boscserver or ecosserver")
    recipient = mailbox(cfg["recipient"])
    now = datetime.now(TZ)
    if a.action == "scheduled":
        if a.month:
            parser.error("scheduled takes no month")
        due = months(cfg["first_month"], now)
    else:
        if not a.month or period(a.month) >= now.date().replace(day=1):
            parser.error("specify a closed month in Asia/Shanghai")
        due = [a.month]
    a.state.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (a.state / ".lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        uncertain = failed = False
        for month in due:
            if a.action == "render":
                if archive(a.state, site, month, a.revision).with_suffix(".delivery.json").exists():
                    parser.error("already submitted; render to a separate --state for corrections")
                render(a.state, site, month, a.revision)
            else:
                try:
                    deliver(a.state, site, month, recipient, revision=a.revision, note=a.note)
                except Uncertain as exc:
                    print(str(exc), file=sys.stderr)
                    uncertain = True
                except Exception as exc:
                    # Neither credentials nor server-supplied recipient details belong in journals.
                    print(f"{site} {month}: {type(exc).__name__}; submission failed", file=sys.stderr)
                    failed = True
        return 1 if failed else 78 if uncertain else 0


if __name__ == "__main__":
    sys.exit(main())
