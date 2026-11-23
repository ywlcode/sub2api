#!/bin/sh
# Run locally on the selected ledger host as root after provisioning its private config.
set -eu
cd "$(dirname "$0")"
python3 - <<'PY'
import json, os, stat
from pathlib import Path
p = Path('/etc/sub2api-monthly-usage.json')
s = p.lstat()
if not stat.S_ISREG(s.st_mode) or s.st_uid != 0 or s.st_mode & 0o077:
    raise SystemExit("expected root-only JSON config")
cfg = json.loads(p.read_text())
if cfg.get('site') not in ('boscserver','ecosserver') or not cfg.get('recipient') or not cfg.get('first_month'):
    raise SystemExit("configure authoritative site, single recipient and first_month")
PY
install -d -m 0755 /opt/sub2api-monthly-usage
install -m 0644 usage.sql headcounts.sql workbook.py report.py /opt/sub2api-monthly-usage/
install -m 0644 sub2api-monthly-usage.service sub2api-monthly-usage.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now sub2api-monthly-usage.timer
