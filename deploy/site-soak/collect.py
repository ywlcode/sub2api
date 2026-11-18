#!/usr/bin/env python3
"""Download status/full reports over existing SSH configuration; no API keys."""
import argparse
import concurrent.futures
import datetime as dt
from decimal import Decimal
import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess


def compare_billing(primary, secondary, node, cutoff=None):
    primary_total, secondary_total = len(primary),len(secondary)
    if cutoff is not None:
        primary=[r for r in primary if dt.datetime.fromisoformat(r['created_at']).timestamp()<=cutoff]
        secondary=[r for r in secondary if dt.datetime.fromisoformat(r['created_at']).timestamp()<=cutoff]
    index = {row["request_id"]:row for row in primary}
    missing, mismatches = [], []
    for row in secondary:
        target = "site:" + hashlib.sha256((node + "\0" + row["request_id"]).encode()).hexdigest()[:56]
        remote = index.get(target)
        if remote is None:
            missing.append({"local_request_id":row["request_id"], "primary_request_id":target})
            continue
        fields = [name for name in ("model","input_tokens","output_tokens") if remote[name] != row[name]]
        if Decimal(str(row["actual_cost"])) != Decimal(str(remote["actual_cost"])):
            fields.append("actual_cost")
        if fields:
            mismatches.append({"local_request_id":row["request_id"], "fields":fields})
    return {"secondary_rows":len(secondary), "primary_rows":len(primary), "missing_imports":missing,
            "excluded_recent_primary_rows":primary_total-len(primary), "excluded_recent_secondary_rows":secondary_total-len(secondary),
            "compared_through":dt.datetime.fromtimestamp(cutoff,dt.timezone.utc).isoformat() if cutoff is not None else None,
            "mismatches":mismatches, "matched":len(secondary)-len(missing)-len(mismatches),
            "consistent":not missing and not mismatches and len(primary)==len(secondary)}


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest",required=True)
    parser.add_argument("--full",action="store_true")
    args = parser.parse_args()
    manifest_path = Path(args.manifest).resolve()
    manifest = json.loads(manifest_path.read_text())
    directory = manifest_path.parent
    def fetch(node):
        host, role, path = node["host"], node["role"], node["directory"]
        if args.full:
            command = ["python3",path+"/soak.py","report","--directory",path]
        else:
            command = ["cat",path+"/status.json"]
        try:
            result = subprocess.run(["ssh","-o","BatchMode=yes","-o","ConnectTimeout=12","-o","ProxyCommand=none",
                "-o","ProxyJump=none","-o","ControlPath=none","-o","ServerAliveInterval=10","-o","ServerAliveCountMax=3",
                host,shlex.join(command)],text=True,capture_output=True,timeout=180 if args.full else 30)
            if result.returncode:
                raise RuntimeError(result.stderr[-1000:])
            value = json.loads(result.stdout)
            status = value["status"] if args.full else value
            assert status["run_id"] == manifest["run_id"]
            (directory/(role+("-report.json" if args.full else "-status.json"))).write_text(json.dumps(value,ensure_ascii=False,indent=2)+"\n")
            return role,value
        except Exception as error:
            return role,{"collection_error":str(error)}
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        results = dict(pool.map(fetch,manifest["nodes"]))
    summary = {"run_id":manifest["run_id"], "collected_at":dt.datetime.now(dt.timezone.utc).isoformat(), "nodes":{}}
    for role,value in results.items():
        status = value.get("status",value)
        if "collection_error" in value:
            summary["nodes"][role] = value
            continue
        age = (dt.datetime.now(dt.timezone.utc)-dt.datetime.fromisoformat(status["updated_at"])).total_seconds()
        summary["nodes"][role] = {k:status.get(k) for k in ("phase","start_at","end_at","updated_at","requests_completed",
            "requests_successful","requests_failed","health","max_observed_sync_gap_s","sync_age_s","active_alerts",
            "billing_rows","billing_duplicate_ids","request_records_without_completion","monitor_observation_gaps","collector_errors")}
        summary["nodes"][role]["status_stale"] = status["phase"] not in ("finished","stopped") and age>30
        if args.full:
            summary["nodes"][role]["failure_categories"] = value.get("failure_categories",{})
    if args.full and all("billing" in v for v in results.values()) and len(results)==2:
        sample_times=[dt.datetime.fromisoformat(v['billing_sample_at']).timestamp() for v in results.values() if v.get('billing_sample_at')]
        if len(sample_times)==2:
            # Do not call rows newer than the other host's periodic census
            # missing imports. Keep a small boundary margin for in-flight SQL.
            cutoff=min(sample_times)-10
            summary["billing"] = compare_billing(results["primary"]["billing"],results["secondary"]["billing"],manifest["secondary_node_id"],cutoff)
        else:
            summary["billing"]={"consistent":None,"reason":"billing census not yet available on both hosts"}
    secondary = summary["nodes"].get("secondary",{})
    summary["observed_gap_at_least_17_minutes"] = ((secondary.get("max_observed_sync_gap_s") or 0)>=1020) if "phase" in secondary else None
    summary["note"] = "Running-window results are provisional; collection failures and observation gaps must not be counted as successful coverage. Billing can lag until the final settlement grace ends."
    (directory/"summary.json").write_text(json.dumps(summary,ensure_ascii=False,indent=2)+"\n")
    print(json.dumps(summary,ensure_ascii=False,indent=2))


if __name__ == "__main__":
    main()
