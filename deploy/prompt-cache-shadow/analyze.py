#!/usr/bin/env python3
"""Bounded, offline analysis of versioned prompt-cache shadow JSONL logs."""

import argparse
from collections import deque
from contextlib import ExitStack
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
import gzip
import json
import math
import random
import sys


EVENT_NAME = "prompt_cache.shadow"
STATS_NAME = "prompt_cache.shadow_stats"
COMPONENT = "service.prompt_cache_shadow"
MAX_TOKEN_COUNT = (1 << 63) - 1
MAX_LABEL_CHARS = 512
MAX_JSON_DEPTH = 64
MAX_INTEGER_DIGITS = 128
MAX_LINE_BYTES = 16 * 1024 * 1024
WRAPPER_FIELDS = ("log", "logs", "payload", "jsonPayload", "message", "msg")
MAX_WRAPPER_DEPTH = 4
MAX_WRAPPER_OBJECTS = 32
DECISIONS = ("cold", "hit", "miss", "skipped")


class LimitError(Exception):
    """A resource limit prevents a complete report."""


def bounded_int(value):
    # json.loads invokes this hook before converting even irrelevant fields.
    if len(value) - value.startswith("-") > MAX_INTEGER_DIGITS:
        raise ValueError("integer digit limit exceeded")
    return int(value)


def decode_json(value):
    # Do not rely on version-specific recursion limits in the stdlib decoder.
    depth = 0
    in_string = False
    escaped = False
    for char in value:
        if in_string:
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                in_string = False
        elif char == '"':
            in_string = True
        elif char in "{[":
            depth += 1
            if depth > MAX_JSON_DEPTH:
                raise ValueError("JSON nesting exceeds limit")
        elif char in "}]":
            depth -= 1

    def reject_constant(_):
        raise ValueError("non-finite JSON number")

    return json.loads(value, parse_int=bounded_int, parse_constant=reject_constant)


def find_event(value):
    """Unwrap a few common collector envelopes; never search prompt fields."""
    pending = deque([(value, 0)])
    scheduled = 1
    malformed = False
    limited = False
    while pending:
        obj, depth = pending.popleft()
        if not isinstance(obj, dict):
            continue
        if (obj.get("msg") in (EVENT_NAME, STATS_NAME)
                or obj.get("message") in (EVENT_NAME, STATS_NAME)):
            if obj.get("component") == COMPONENT:
                return obj
        for field in WRAPPER_FIELDS:
            inner = obj.get(field)
            if isinstance(inner, str) and inner.lstrip().startswith("{"):
                try:
                    inner = decode_json(inner)
                except (ValueError, RecursionError):
                    malformed = True
                    continue
            if isinstance(inner, dict):
                if depth >= MAX_WRAPPER_DEPTH or scheduled >= MAX_WRAPPER_OBJECTS:
                    limited = True
                    continue
                pending.append((inner, depth + 1))
                scheduled += 1
    if limited:
        raise LimitError("JSON wrapper depth/object limit exceeded")
    if malformed:
        raise ValueError("malformed embedded JSON")
    return None


def account_key(value):
    if type(value) is int and 0 <= value <= MAX_TOKEN_COUNT:
        return str(value)
    if isinstance(value, str) and value.strip() and len(value) <= MAX_LABEL_CHARS:
        return value.strip()
    raise ValueError("invalid account_id")


def token_value(obj, field):
    value = obj.get(field)
    if value is not None and (type(value) is not int or not 0 <= value <= MAX_TOKEN_COUNT):
        raise ValueError("invalid token count")
    return value


@dataclass(frozen=True)
class Event:
    # Retain only fields used for aggregation, never prompts or request/API-key IDs.
    account_id: str
    model: str
    transport: str
    decision: str
    skip_reason: str
    input_tokens: object
    real: object
    simulated: object
    upstream_known: bool
    declared_comparable: bool
    warm_scope: str
    prediction_basis: str
    eviction_counters: tuple

    @classmethod
    def parse(cls, obj):
        if obj.get("state_scope") != "process":
            raise ValueError("unsupported state_scope")
        if not isinstance(obj.get("instance_id"), str) or not obj["instance_id"]:
            raise ValueError("missing instance_id")
        for field in ("model", "transport"):
            if (not isinstance(obj.get(field), str) or not obj[field].strip()
                    or len(obj[field]) > MAX_LABEL_CHARS):
                raise ValueError("missing grouping field")
        for field in ("upstream_cache_known", "comparable"):
            if type(obj.get(field)) is not bool:
                raise ValueError("invalid boolean")
        decision = obj.get("decision")
        if decision not in DECISIONS:
            raise ValueError("invalid decision")
        reason = obj.get("skip_reason")
        if reason is not None and (not isinstance(reason, str) or len(reason) > MAX_LABEL_CHARS):
            raise ValueError("invalid skip_reason")
        input_tokens = token_value(obj, "input_tokens")
        real = token_value(obj, "upstream_cache_read_tokens")
        simulated = token_value(obj, "simulated_cache_read_tokens")
        prediction_basis = "estimated_tokens_v1"
        if obj.get("schema_version") in (2, 3):
            prediction_basis = obj.get("prediction_basis")
            if prediction_basis != {2: "upstream_input_ratio_v2", 3: "local_prefix_anchors_v3"}[obj["schema_version"]]:
                raise ValueError("unsupported normalized prediction basis")
            if simulated is not None and (input_tokens is None or simulated > input_tokens):
                raise ValueError("normalized prediction exceeds or lacks upstream input")
        warm_scope = obj.get("warm_scope", "legacy_request_namespace")
        if (not isinstance(warm_scope, str) or not warm_scope.strip()
                or len(warm_scope) > MAX_LABEL_CHARS):
            raise ValueError("invalid warm_scope")
        evictions = tuple(token_value(obj, name) for name in (
            "account_cache_evictions", "shared_cache_evictions", "warm_state_evictions"))
        if (obj["upstream_cache_known"] and real is not None
                and input_tokens is not None and real > input_tokens):
            raise ValueError("upstream cache exceeds total upstream input")
        return cls(account_key(obj.get("account_id")), obj["model"], obj["transport"],
                   decision, reason or "", input_tokens, real, simulated,
                   obj["upstream_cache_known"], obj["comparable"], warm_scope,
                   prediction_basis, evictions)

    @property
    def real_known(self):
        return self.upstream_known and self.real is not None

    @property
    def incomparable_reason(self):
        if self.skip_reason:
            return "skip_reason"
        if self.decision == "skipped":
            return "decision_skipped"
        if not self.real_known:
            return "upstream_cache_unknown"
        if self.input_tokens is None:
            return "input_tokens_unknown"
        if self.simulated is None:
            return "simulated_cache_unknown"
        if not self.declared_comparable:
            return "declared_incomparable"
        return None


class ReasonCounts:
    def __init__(self, limit):
        self.limit = limit
        self.counts = {}
        self.other_events = 0

    def add(self, reason):
        if reason in self.counts:
            self.counts[reason] += 1
        elif len(self.counts) < self.limit:
            self.counts[reason] = 1
        else:
            self.other_events += 1

    def report(self):
        return {"counts": dict(sorted(self.counts.items())), "other_events": self.other_events}


class ActualUsage:
    def __init__(self):
        self.samples = 0
        self.input_tokens = 0
        self.cache_tokens = 0

    def add(self, event):
        if event.real_known and event.input_tokens is not None:
            self.samples += 1
            self.input_tokens += event.input_tokens
            self.cache_tokens += event.real

    def report(self):
        return {
            "samples": self.samples,
            "input_tokens": self.input_tokens if self.samples else None,
            "real_cache_read_tokens": self.cache_tokens if self.samples else None,
            "real_hit_rate": self.cache_tokens / self.input_tokens if self.input_tokens else None,
        }


class PairedUsage:
    def __init__(self, quantile_samples):
        self.samples = 0
        self.input_tokens = 0
        self.real = 0
        self.simulated = 0
        self.absolute_error = 0
        self.signed_error = 0
        self.underprediction_samples = 0
        self.underprediction_tokens = 0
        self.overprediction_samples = 0
        self.overprediction_tokens = 0
        self.relative_error_sum = 0.0
        self.relative_error_samples = 0
        self.relative_absolute_error = 0
        self.quantile_samples = quantile_samples
        self.errors = []
        self.relative_errors = []
        self.rng = random.Random(0)
        self.relative_rng = random.Random(1)

    def add(self, event):
        self.samples += 1
        self.input_tokens += event.input_tokens
        self.real += event.real
        self.simulated += event.simulated
        error = event.simulated - event.real
        absolute = abs(error)
        self.absolute_error += absolute
        self.signed_error += error
        if error < 0:
            self.underprediction_samples += 1
            self.underprediction_tokens -= error
        elif error > 0:
            self.overprediction_samples += 1
            self.overprediction_tokens += error
        if event.input_tokens:
            relative = absolute / event.input_tokens
            self.relative_error_sum += relative
            self.relative_error_samples += 1
            self.relative_absolute_error += absolute
            self.sample(self.relative_errors, self.relative_rng, relative,
                        self.relative_error_samples)
        self.sample(self.errors, self.rng, absolute, self.samples)

    def sample(self, values, rng, value, population):
        # Algorithm R: exact below the limit, uniform bounded reservoir thereafter.
        if len(values) < self.quantile_samples:
            values.append(value)
        else:
            index = rng.randrange(population)
            if index < self.quantile_samples:
                values[index] = value

    def report(self):
        errors = sorted(self.errors)
        relative_errors = sorted(self.relative_errors)

        def percentile(values, fraction):
            return values[math.ceil(len(values) * fraction) - 1] if values else None

        def quantiles(values, population):
            return {
                "method": "nearest_rank" if population <= self.quantile_samples
                else "reservoir_nearest_rank",
                "approximate": population > self.quantile_samples,
                "retained_samples": len(values),
                "capacity": self.quantile_samples,
            }

        return {
            "samples": self.samples,
            "underprediction_samples": self.underprediction_samples,
            "underprediction_tokens": self.underprediction_tokens,
            "overprediction_samples": self.overprediction_samples,
            "overprediction_tokens": self.overprediction_tokens,
            "input_tokens": self.input_tokens if self.samples else None,
            "real_cache_read_tokens": self.real if self.samples else None,
            "simulated_cache_read_tokens": self.simulated if self.samples else None,
            "real_hit_rate": self.real / self.input_tokens if self.input_tokens else None,
            "simulated_hit_rate": self.simulated / self.input_tokens if self.input_tokens else None,
            "mae_tokens": self.absolute_error / self.samples if self.samples else None,
            "signed_error_tokens": self.signed_error / self.samples if self.samples else None,
            "p50_absolute_error_tokens": percentile(errors, 0.50),
            "p95_absolute_error_tokens": percentile(errors, 0.95),
            "relative_error_samples": self.relative_error_samples,
            "p50_absolute_relative_error": percentile(relative_errors, 0.50),
            "p95_absolute_relative_error": percentile(relative_errors, 0.95),
            "mean_absolute_relative_error": (
                self.relative_error_sum / self.relative_error_samples
                if self.relative_error_samples else None),
            "weighted_absolute_relative_error": (
                self.relative_absolute_error / self.input_tokens if self.input_tokens else None),
            "quantiles": quantiles(errors, self.samples),
            "relative_quantiles": quantiles(relative_errors, self.relative_error_samples),
        }


class Group:
    def __init__(self, quantile_samples, max_reasons):
        self.events = 0
        self.unknown_upstream_samples = 0
        self.unknown_input_samples = 0
        self.incomparable_samples = 0
        self.decision_counts = dict.fromkeys(DECISIONS, 0)
        self.skip_reasons = ReasonCounts(max_reasons)
        self.incomparable_reasons = ReasonCounts(max_reasons)
        self.actual_known = ActualUsage()
        self.comparable = PairedUsage(quantile_samples)
        self.warm_only = PairedUsage(quantile_samples)
        self.max_process_eviction_counters = [None, None, None]

    def add(self, event):
        self.events += 1
        for index, value in enumerate(event.eviction_counters):
            if value is not None:
                previous = self.max_process_eviction_counters[index]
                self.max_process_eviction_counters[index] = value if previous is None else max(previous, value)
        self.decision_counts[event.decision] += 1
        self.unknown_upstream_samples += not event.real_known
        self.unknown_input_samples += event.input_tokens is None
        self.actual_known.add(event)
        if event.skip_reason:
            self.skip_reasons.add(event.skip_reason)
        reason = event.incomparable_reason
        if reason:
            self.incomparable_samples += 1
            self.incomparable_reasons.add(reason)
        else:
            self.comparable.add(event)
            if event.decision in ("hit", "miss"):
                self.warm_only.add(event)

    def report(self):
        return {
            "events": self.events,
            "unknown_upstream_samples": self.unknown_upstream_samples,
            "unknown_input_samples": self.unknown_input_samples,
            "incomparable_samples": self.incomparable_samples,
            "decision_counts": self.decision_counts.copy(),
            "skip_reason_counts": self.skip_reasons.report(),
            "incomparable_reason_counts": self.incomparable_reasons.report(),
            "actual_known": self.actual_known.report(),
            "comparable": self.comparable.report(),
            "warm_only": self.warm_only.report(),
            "max_observed_process_eviction_counters": dict(zip(
                ("account", "shared", "warm_state"), self.max_process_eviction_counters)),
        }


def bounded_lines(stream, max_line_bytes):
    """Yield None for oversized physical lines, draining them in bounded chunks."""
    while True:
        line = stream.readline(max_line_bytes + 1)
        if not line:
            return
        if len(line) > max_line_bytes:
            while line and not line.endswith(b"\n"):
                line = stream.readline(max_line_bytes + 1)
            yield None
        else:
            yield line


class Analyzer:
    def __init__(self, account_ids=(), max_groups=1000, quantile_samples=1024,
                 max_reasons=64, max_line_bytes=1048576):
        if min(max_groups, quantile_samples, max_reasons, max_line_bytes) < 1:
            raise ValueError("limits must be positive")
        if max_line_bytes > MAX_LINE_BYTES:
            raise ValueError(f"max-line-bytes must not exceed {MAX_LINE_BYTES}")
        self.account_ids = {account_key(value) for value in account_ids}
        self.max_groups = max_groups
        self.quantile_samples = quantile_samples
        self.max_reasons = max_reasons
        self.max_line_bytes = max_line_bytes
        self.groups = {}
        self.accounts = {}
        self.logging_instances = {}
        self.observed_instances = set()
        self.counts = dict.fromkeys((
            "lines", "blank_lines", "malformed_json_lines", "oversized_lines",
            "wrapper_limit_lines", "ignored_lines", "shadow_events",
            "unsupported_schema_lines", "invalid_event_lines",
            "account_filtered_events", "analyzed_events", "writer_stats_events",
            "invalid_writer_stats_events"), 0)

    def consume(self, stream):
        for line in bounded_lines(stream, self.max_line_bytes):
            self.counts["lines"] += 1
            if line is None:
                self.counts["oversized_lines"] += 1
                continue
            if not line.strip():
                self.counts["blank_lines"] += 1
                continue
            try:
                obj = decode_json(line.decode("utf-8"))
                obj = find_event(obj)
            except LimitError:
                self.counts["wrapper_limit_lines"] += 1
                continue
            except (ValueError, RecursionError):
                self.counts["malformed_json_lines"] += 1
                continue
            if obj is None:
                self.counts["ignored_lines"] += 1
                continue
            if obj.get("msg") == STATS_NAME or obj.get("message") == STATS_NAME:
                try:
                    self.add_logging_snapshot(obj)
                except ValueError:
                    self.counts["invalid_writer_stats_events"] += 1
                continue
            self.counts["shadow_events"] += 1
            if type(obj.get("schema_version")) is not int or obj["schema_version"] not in (1, 2, 3):
                self.counts["unsupported_schema_lines"] += 1
                continue
            try:
                event = Event.parse(obj)
            except ValueError:
                self.counts["invalid_event_lines"] += 1
                continue
            instance = obj["instance_id"]
            if instance not in self.observed_instances and len(self.observed_instances) >= self.max_groups:
                raise LimitError("max-groups exceeded by observed instances")
            if len(instance) > MAX_LABEL_CHARS:
                self.counts["invalid_event_lines"] += 1
                continue
            self.observed_instances.add(instance)
            if self.account_ids and event.account_id not in self.account_ids:
                self.counts["account_filtered_events"] += 1
                continue
            key = (event.account_id, event.model, event.transport, event.warm_scope,
                   event.prediction_basis)
            if key not in self.groups:
                if len(self.groups) >= self.max_groups:
                    raise LimitError("max-groups exceeded; narrow --account-id or raise --max-groups")
                self.groups[key] = Group(self.quantile_samples, self.max_reasons)
            self.groups[key].add(event)
            if event.account_id not in self.accounts:
                self.accounts[event.account_id] = ActualUsage()
            self.accounts[event.account_id].add(event)
            self.counts["analyzed_events"] += 1

    def add_logging_snapshot(self, obj):
        instance = obj.get("instance_id")
        sequence = token_value(obj, "sequence")
        counters = obj.get("logging")
        if (type(obj.get("schema_version")) is not int or obj["schema_version"] != 1
                or obj.get("state_scope") != "process" or not isinstance(instance, str)
                or not instance.strip() or len(instance) > MAX_LABEL_CHARS
                or sequence is None or sequence < 1 or not isinstance(counters, dict)):
            raise ValueError("invalid writer snapshot")
        parsed = {}
        for field in ("submitted_events", "written_events", "queue_dropped_events",
                      "oversize_dropped_events", "write_failed_events", "maintenance_errors",
                      "capacity_deleted_files", "age_deleted_files", "retained_bytes"):
            value = token_value(counters, field)
            if value is None:
                raise ValueError("missing writer counter")
            parsed[field] = value
        shortened = counters.get("retention_shortened")
        oldest = counters.get("oldest_segment_time")
        if (type(shortened) is not bool or not isinstance(oldest, str)
                or len(oldest) > MAX_LABEL_CHARS):
            raise ValueError("invalid retention fields")
        if instance not in self.logging_instances and len(self.logging_instances) >= self.max_groups:
            raise LimitError("max-groups exceeded by writer instances")
        self.counts["writer_stats_events"] += 1
        previous = self.logging_instances.get(instance)
        if previous is None or sequence > previous["sequence"]:
            self.logging_instances[instance] = {
                "instance_id": instance, "sequence": sequence, **parsed,
                "retention_shortened": shortened, "oldest_segment_time": oldest,
            }

    def logging_report(self):
        instances = [self.logging_instances[k] for k in sorted(self.logging_instances)]
        missing = (sum(row["queue_dropped_events"] + row["oversize_dropped_events"]
                       + row["write_failed_events"] for row in instances)
                   if instances else None)
        return {
            "known": bool(instances),
            "instances_without_snapshot": sorted(self.observed_instances - self.logging_instances.keys()),
            "scope": "process_lifetime_at_latest_snapshot_not_selected_account_or_window",
            "lost_events": missing,
            "retention_shortened": (any(row["retention_shortened"] for row in instances)
                                    if instances else None),
            "instances": instances,
        }

    def report(self, min_real_hit_rate=None):
        threshold = min_real_hit_rate.as_integer_ratio() if min_real_hit_rate is not None else None
        selected = set()
        accounts = []
        for account_id, usage in sorted(self.accounts.items()):
            include = threshold is None or (
                usage.input_tokens > 0
                and usage.cache_tokens * threshold[1] >= usage.input_tokens * threshold[0])
            if include:
                selected.add(account_id)
            accounts.append({"account_id": account_id, **usage.report(), "selected": include})
        groups = [
            {"account_id": key[0], "model": key[1], "transport": key[2], "warm_scope": key[3],
             "prediction_basis": key[4], **group.report()}
            for key, group in sorted(self.groups.items()) if key[0] in selected
        ]
        return {
            "report_schema_version": 1,
            "event_schema_version": 3,
            "supported_event_schema_versions": [1, 2, 3],
            "state_scope": "process",
            "filters": {"account_ids": sorted(self.account_ids),
                        "min_real_hit_rate": str(min_real_hit_rate)
                        if min_real_hit_rate is not None else None},
            "limits": {"max_line_bytes": self.max_line_bytes, "max_groups": self.max_groups,
                       "quantile_samples_per_cohort": self.quantile_samples,
                       "max_reasons_per_counter": self.max_reasons,
                       "max_label_chars": MAX_LABEL_CHARS, "max_json_depth": MAX_JSON_DEPTH,
                       "max_integer_digits": MAX_INTEGER_DIGITS},
            "counts": self.counts.copy(),
            "selected_events": sum(group["events"] for group in groups),
            "high_hit_filtered_events": sum(
                group.events for key, group in self.groups.items() if key[0] not in selected),
            "account_actual_usage": accounts,
            "logging": self.logging_report(),
            "groups": groups,
        }


def positive_int(value):
    try:
        parsed = bounded_int(value)
    except ValueError:
        raise argparse.ArgumentTypeError("must be a positive integer") from None
    if parsed < 1:
        raise argparse.ArgumentTypeError("must be a positive integer")
    return parsed


def line_bytes(value):
    parsed = positive_int(value)
    if parsed > MAX_LINE_BYTES:
        raise argparse.ArgumentTypeError(f"must not exceed {MAX_LINE_BYTES}")
    return parsed


def account_argument(value):
    try:
        return account_key(value)
    except ValueError:
        raise argparse.ArgumentTypeError("account ID must be nonblank and at most 512 characters") from None


def hit_rate(value):
    try:
        if len(value) > 128:
            raise ValueError
        parsed = Decimal(value)
        if (not parsed.is_finite() or not 0 <= parsed <= 1
                or abs(parsed.as_tuple().exponent) > 1000):
            raise ValueError
    except (InvalidOperation, ValueError):
        raise argparse.ArgumentTypeError("must be a decimal in [0, 1] (up to 128 characters)") from None
    return parsed


def iter_text(report):
    """Yield bounded report chunks so the CLI need not copy the whole text."""
    def number(value):
        if value is None:
            return "unknown"
        return str(value) if type(value) is int else f"{value:.6g}"

    def cohort(label, stats):
        return (
            f"  {label}: samples={stats['samples']} input={number(stats['input_tokens'])} "
            f"real_cache={number(stats['real_cache_read_tokens'])} "
            f"sim_cache={number(stats['simulated_cache_read_tokens'])}\n"
            f"    real_hit_rate={number(stats['real_hit_rate'])} "
            f"sim_hit_rate={number(stats['simulated_hit_rate'])} "
            f"MAE={number(stats['mae_tokens'])} signed_error(sim-real)={number(stats['signed_error_tokens'])}\n"
            f"    underprediction_samples={stats['underprediction_samples']} "
            f"underprediction_tokens={number(stats['underprediction_tokens'])} "
            f"overprediction_samples={stats['overprediction_samples']} "
            f"overprediction_tokens={number(stats['overprediction_tokens'])}\n"
            f"    abs_error_p50={number(stats['p50_absolute_error_tokens'])} "
            f"abs_error_p95={number(stats['p95_absolute_error_tokens'])} "
            f"abs_relative_error_p50={number(stats['p50_absolute_relative_error'])} "
            f"abs_relative_error_p95={number(stats['p95_absolute_relative_error'])} "
            f"mean_abs_relative_error={number(stats['mean_absolute_relative_error'])} "
            f"weighted_abs_relative_error={number(stats['weighted_absolute_relative_error'])} "
            f"quantiles_approximate={stats['quantiles']['approximate']} "
            f"relative_quantiles_approximate={stats['relative_quantiles']['approximate']}"
        )

    yield "Offline prompt-cache shadow analysis (schema=1/2/3, process state; no calibration)"
    yield "counts=" + json.dumps(report["counts"], sort_keys=True)
    logging = report["logging"]
    yield (f"logging: known={logging['known']} lost_events={number(logging['lost_events'])} "
           f"retention_shortened={logging['retention_shortened']} scope={logging['scope']}")
    for instance in logging["instances"]:
        yield "  writer=" + json.dumps(instance, sort_keys=True)
    for instance in logging["instances_without_snapshot"]:
        yield "  writer_snapshot_missing=" + json.dumps(instance)
    yield (f"selected_events={report['selected_events']} "
           f"high_hit_filtered_events={report['high_hit_filtered_events']}")
    for account in report["account_actual_usage"]:
        yield (f"account={json.dumps(account['account_id'])} "
               f"actual_samples={account['samples']} "
               f"actual/input={number(account['real_hit_rate'])} selected={account['selected']}")
    for group in report["groups"]:
        yield (f"account={json.dumps(group['account_id'])} model={json.dumps(group['model'])} "
               f"transport={json.dumps(group['transport'])} warm_scope={json.dumps(group['warm_scope'])} "
               f"prediction_basis={json.dumps(group['prediction_basis'])} events={group['events']} "
               f"incomparable={group['incomparable_samples']} "
               f"upstream_unknown={group['unknown_upstream_samples']}")
        known = group["actual_known"]
        yield (f"  actual_known: samples={known['samples']} "
               f"input={number(known['input_tokens'])} "
               f"real_cache={number(known['real_cache_read_tokens'])} "
               f"real_hit_rate={number(known['real_hit_rate'])}")
        yield cohort("comparable", group["comparable"])
        yield cohort("warm_only (hit/miss)", group["warm_only"])
        yield "  max_observed_process_eviction_counters=" + json.dumps(group["max_observed_process_eviction_counters"], sort_keys=True)
        for field in ("decision_counts", "skip_reason_counts", "incomparable_reason_counts"):
            yield f"  {field}=" + json.dumps(group[field], sort_keys=True)


def render_text(report):
    """Convenience renderer for callers that explicitly need a single string."""
    return "\n".join(iter_text(report))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("files", nargs="*", default=[], help="JSONL files, or '-' for stdin (default)")
    parser.add_argument("--account-id", action="append", type=account_argument,
                        default=[], help="account ID; repeatable")
    parser.add_argument("--min-real-hit-rate", type=hit_rate, help="account-level actual/input threshold [0,1]")
    parser.add_argument("--json", action="store_true", help="emit one JSON report")
    parser.add_argument("--max-line-bytes", type=line_bytes, default=1048576)
    parser.add_argument("--max-groups", type=positive_int, default=1000)
    parser.add_argument("--quantile-samples", type=positive_int, default=1024)
    parser.add_argument("--max-reasons", type=positive_int, default=64)
    args = parser.parse_args(argv)
    files = args.files or ["-"]
    if files.count("-") > 1:
        parser.error("stdin '-' may only appear once")
    try:
        analyzer = Analyzer(args.account_id, args.max_groups, args.quantile_samples,
                            args.max_reasons, args.max_line_bytes)
        for path in files:
            with ExitStack() as stack:
                opener = gzip.open if path.endswith(".gz") else open
                stream = sys.stdin.buffer if path == "-" else stack.enter_context(opener(path, "rb"))
                analyzer.consume(stream)
        report = analyzer.report(args.min_real_hit_rate)
        if args.json:
            json.dump(report, sys.stdout, ensure_ascii=True, allow_nan=False)
            sys.stdout.write("\n")
        else:
            for chunk in iter_text(report):
                sys.stdout.write(chunk)
                sys.stdout.write("\n")
    except (OSError, EOFError, LimitError, ValueError) as exc:
        print(f"analyze.py: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
