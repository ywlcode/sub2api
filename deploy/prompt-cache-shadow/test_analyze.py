"""Synthetic fixtures only; no services, network, or third-party dependencies."""

from decimal import Decimal
import gzip
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

import analyze


HERE = Path(__file__).resolve().parent


def event(**changes):
    record = {
        "msg": "prompt_cache.shadow",
        "component": "service.prompt_cache_shadow",
        "schema_version": 1,
        "instance_id": "synthetic-process-1",
        "state_scope": "process",
        "account_id": 42,
        "platform": "openai",
        "model": "synthetic-model",
        "api_key_id": 12,
        "request_id": "synthetic-request",
        "transport": "http",
        "input_tokens": 1000,
        "upstream_cache_known": True,
        "upstream_cache_read_tokens": 800,
        "simulated_cache_read_tokens": 600,
        "comparable": True,
        "skip_reason": "",
        "decision": "hit",
        "estimated_input_tokens": 1050,
        "matched_prefix_tokens": 600,
        "tokenizer": "synthetic-estimator",
        "prefix_tokens_step": 128,
        "min_prefix_tokens": 1024,
        "ttl_seconds": 600,
        "cache_age_ms": 100,
        "committed": True,
        "duration_us": 12,
    }
    record.update(changes)
    return record


def jsonl(records):
    return ("\n".join(json.dumps(record) for record in records) + "\n").encode("utf-8")


def aggregate(records, threshold=None, **limits):
    analyzer = analyze.Analyzer(**limits)
    analyzer.consume(io.BytesIO(jsonl(records)))
    return analyzer.report(threshold)


def writer_snapshot(sequence=1, instance="synthetic-process-1", **changes):
    counters = dict.fromkeys(("submitted_events", "written_events", "queue_dropped_events",
                             "oversize_dropped_events", "write_failed_events", "maintenance_errors",
                             "capacity_deleted_files", "age_deleted_files", "retained_bytes"), 0)
    counters.update(retention_shortened=False, oldest_segment_time="")
    counters.update(changes)
    return {"msg": "prompt_cache.shadow_stats", "component": "service.prompt_cache_shadow",
            "schema_version": 1, "state_scope": "process", "instance_id": instance,
            "sequence": sequence, "logging": counters}


class WriterLogTests(unittest.TestCase):
    def test_no_snapshot_means_unknown_losses(self):
        stats = aggregate([event()])["logging"]
        self.assertFalse(stats["known"])
        self.assertIsNone(stats["lost_events"])
        self.assertIsNone(stats["retention_shortened"])
        self.assertEqual(stats["instances_without_snapshot"], ["synthetic-process-1"])

    def test_latest_cumulative_snapshot_not_sum_and_not_account_filtered(self):
        older = writer_snapshot(1, submitted_events=10, written_events=8, queue_dropped_events=2)
        newer = writer_snapshot(2, submitted_events=20, written_events=15,
                                queue_dropped_events=3, oversize_dropped_events=1,
                                write_failed_events=1, retention_shortened=True,
                                capacity_deleted_files=2)
        report = aggregate([event(), newer, older, newer], account_ids=["different-account"])
        self.assertEqual(report["selected_events"], 0)
        stats = report["logging"]
        self.assertTrue(stats["known"])
        self.assertEqual(stats["lost_events"], 5)
        self.assertTrue(stats["retention_shortened"])
        self.assertEqual(stats["instances_without_snapshot"], [])
        self.assertEqual(stats["instances"][0]["written_events"], 15)
        text = analyze.render_text(report)
        self.assertIn("lost_events=5", text)
        self.assertIn("not_selected_account_or_window", text)

    def test_stats_instances_and_bad_stats_are_bounded(self):
        report = aggregate([writer_snapshot(), writer_snapshot(2, queue_dropped_events=-1),
                            writer_snapshot(3, write_failed_events=True),
                            event(instance_id="no-snapshot")])
        self.assertEqual(report["counts"]["invalid_writer_stats_events"], 2)
        self.assertEqual(report["logging"]["instances_without_snapshot"], ["no-snapshot"])
        self.assertIn('writer_snapshot_missing="no-snapshot"', analyze.render_text(report))
        with self.assertRaises(analyze.LimitError):
            aggregate([writer_snapshot(), writer_snapshot(instance="second")], max_groups=1)

    def test_cli_reads_gzip_and_plain_without_changing_metrics(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "observations.jsonl.gz"
            with gzip.open(path, "wb") as stream:
                stream.write(jsonl([event(), writer_snapshot(submitted_events=1, written_events=1)]))
            proc = subprocess.run([sys.executable, str(HERE / "analyze.py"), "--json", str(path)],
                                  capture_output=True, check=False)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            report = json.loads(proc.stdout)
            self.assertEqual(report["selected_events"], 1)
            self.assertEqual(report["logging"]["lost_events"], 0)
            path.write_bytes(b"not a gzip stream")
            proc = subprocess.run([sys.executable, str(HERE / "analyze.py"), "--json", str(path)],
                                  capture_output=True, check=False)
            self.assertEqual(proc.returncode, 1)
            self.assertEqual(proc.stdout, b"")


class PredictionUnitsTests(unittest.TestCase):
    def test_local_anchors_keep_their_own_group_and_directional_errors(self):
        records = [event(schema_version=3, prediction_basis="local_prefix_anchors_v3",
                         warm_scope="user_model", simulated_cache_read_tokens=value,
                         upstream_cache_read_tokens=500) for value in (300, 800)]
        records.append(event(schema_version=2, prediction_basis="upstream_input_ratio_v2"))
        report = aggregate(records)
        self.assertEqual(report["counts"]["analyzed_events"], 3)
        groups = {g["prediction_basis"]: g for g in report["groups"]}
        local = groups["local_prefix_anchors_v3"]["comparable"]
        self.assertEqual(local["underprediction_samples"], 1)
        self.assertEqual(local["underprediction_tokens"], 200)
        self.assertEqual(local["overprediction_samples"], 1)
        self.assertEqual(local["overprediction_tokens"], 300)

    def test_legacy_and_normalized_predictions_are_not_merged(self):
        legacy = event(simulated_cache_read_tokens=1200)
        normalized = event(schema_version=2, prediction_basis="upstream_input_ratio_v2",
                           raw_simulated_cache_read_tokens=1200,
                           simulated_cache_read_tokens=800)
        report = aggregate([legacy, normalized])
        self.assertEqual(report["counts"]["analyzed_events"], 2)
        self.assertEqual(len(report["groups"]), 2)
        groups = {g["prediction_basis"]: g for g in report["groups"]}
        self.assertEqual(groups["estimated_tokens_v1"]["comparable"]["simulated_hit_rate"], 1.2)
        self.assertEqual(groups["upstream_input_ratio_v2"]["comparable"]["simulated_hit_rate"], 0.8)
        self.assertIn('prediction_basis="upstream_input_ratio_v2"', analyze.render_text(report))

    def test_normalized_prediction_must_use_known_units_and_valid_bound(self):
        base = event(schema_version=2, prediction_basis="upstream_input_ratio_v2")
        records = [base | changes for changes in (
            {"prediction_basis": None}, {"prediction_basis": "estimated_tokens_v1"},
            {"simulated_cache_read_tokens": 1001},
        )]
        report = aggregate(records + [base])
        self.assertEqual(report["counts"]["invalid_event_lines"], 3)
        self.assertEqual(report["counts"]["analyzed_events"], 1)

    def test_normalized_unknown_usage_is_still_unknown(self):
        report = aggregate([event(schema_version=2, prediction_basis="upstream_input_ratio_v2",
                                  input_tokens=None, upstream_cache_read_tokens=None,
                                  upstream_cache_known=False, simulated_cache_read_tokens=None,
                                  comparable=False, decision="skipped",
                                  skip_reason="upstream_usage_unavailable")])
        self.assertEqual(report["counts"]["analyzed_events"], 1)
        self.assertIsNone(report["groups"][0]["comparable"]["simulated_hit_rate"])


class MetricsTests(unittest.TestCase):
    def test_new_and_legacy_warmth_are_not_merged(self):
        report = aggregate([event(), event(warm_scope="account_model", decision="miss",
                                         simulated_cache_read_tokens=0,
                                         account_cache_evictions=12, shared_cache_evictions=25,
                                         warm_state_evictions=0)])
        self.assertEqual(len(report["groups"]), 2)
        current = next(group for group in report["groups"] if group["warm_scope"] == "account_model")
        self.assertEqual(current["warm_only"]["samples"], 1)
        self.assertEqual(current["warm_only"]["simulated_cache_read_tokens"], 0)
        self.assertEqual(current["max_observed_process_eviction_counters"],
                         {"account": 12, "shared": 25, "warm_state": 0})
        self.assertIn('warm_scope="account_model"', analyze.render_text(report))

    def test_eviction_counters_unknown_or_maximum_never_summed(self):
        report = aggregate([event(account_cache_evictions=7), event(account_cache_evictions=10),
                            event(account_cache_evictions=-1)])
        self.assertEqual(report["counts"]["invalid_event_lines"], 1)
        stats = report["groups"][0]["max_observed_process_eviction_counters"]
        self.assertEqual(stats, {"account": 10, "shared": None, "warm_state": None})

    def test_token_weighting_instead_of_request_average(self):
        stats = aggregate([
            event(input_tokens=100, upstream_cache_read_tokens=100, simulated_cache_read_tokens=0),
            event(input_tokens=900, upstream_cache_read_tokens=0, simulated_cache_read_tokens=900),
        ])["groups"][0]["comparable"]
        self.assertEqual(stats["input_tokens"], 1000)
        self.assertEqual(stats["real_cache_read_tokens"], 100)
        self.assertEqual(stats["simulated_cache_read_tokens"], 900)
        self.assertEqual(stats["real_hit_rate"], 0.1)
        self.assertEqual(stats["simulated_hit_rate"], 0.9)
        self.assertNotEqual(stats["real_hit_rate"], 0.5)

    def test_error_sign_quantiles_and_relative_input_error(self):
        stats = aggregate([
            event(input_tokens=100, upstream_cache_read_tokens=80, simulated_cache_read_tokens=50),
            event(input_tokens=900, upstream_cache_read_tokens=100, simulated_cache_read_tokens=110),
        ])["groups"][0]["comparable"]
        self.assertEqual(stats["mae_tokens"], 20)
        self.assertEqual(stats["signed_error_tokens"], -10)
        self.assertEqual(stats["p50_absolute_error_tokens"], 10)
        self.assertEqual(stats["p95_absolute_error_tokens"], 30)
        self.assertAlmostEqual(stats["p50_absolute_relative_error"], 10 / 900)
        self.assertEqual(stats["p95_absolute_relative_error"], 0.3)
        self.assertAlmostEqual(stats["mean_absolute_relative_error"], (0.3 + 10 / 900) / 2)
        self.assertEqual(stats["weighted_absolute_relative_error"], 0.04)
        self.assertFalse(stats["quantiles"]["approximate"])
        self.assertFalse(stats["relative_quantiles"]["approximate"])
        over = aggregate([event(simulated_cache_read_tokens=900)])["groups"][0]["comparable"]
        self.assertEqual(over["signed_error_tokens"], 100)

    def test_skips_and_warm_only_do_not_use_upstream_hit_as_warmth(self):
        group = aggregate([
            event(decision="cold", simulated_cache_read_tokens=0),
            event(decision="miss", upstream_cache_read_tokens=0, simulated_cache_read_tokens=0),
            event(decision="hit"),
            event(decision="skipped", comparable=False, skip_reason="unsupported_body"),
        ])["groups"][0]
        self.assertEqual(group["events"], 4)
        self.assertEqual(group["comparable"]["samples"], 3)
        self.assertEqual(group["warm_only"]["samples"], 2)
        self.assertEqual(group["warm_only"]["real_cache_read_tokens"], 800)
        self.assertEqual(group["warm_only"]["mae_tokens"], 100)
        self.assertEqual(group["warm_only"]["p50_absolute_relative_error"], 0)
        self.assertEqual(group["warm_only"]["p95_absolute_relative_error"], 0.2)
        self.assertEqual(group["decision_counts"], {"cold": 1, "hit": 1, "miss": 1, "skipped": 1})
        self.assertEqual(group["skip_reason_counts"]["counts"], {"unsupported_body": 1})
        self.assertEqual(group["incomparable_samples"], 1)

    def test_unknown_is_null_and_excluded_even_if_declared_comparable(self):
        records = [
            event(upstream_cache_known=False, upstream_cache_read_tokens=700),
            event(upstream_cache_read_tokens=None),
            event(simulated_cache_read_tokens=None),
            event(input_tokens=None),
            event(comparable=False),
        ]
        group = aggregate(records)["groups"][0]
        self.assertEqual(group["unknown_upstream_samples"], 2)
        self.assertEqual(group["unknown_input_samples"], 1)
        self.assertEqual(group["incomparable_samples"], 5)
        self.assertEqual(group["comparable"]["samples"], 0)
        for field in ("input_tokens", "real_cache_read_tokens", "simulated_cache_read_tokens",
                      "real_hit_rate", "simulated_hit_rate", "mae_tokens", "signed_error_tokens",
                      "p50_absolute_error_tokens", "mean_absolute_relative_error",
                      "p50_absolute_relative_error", "p95_absolute_relative_error"):
            self.assertIsNone(group["comparable"][field], field)
        self.assertEqual(group["actual_known"]["samples"], 2)
        only_unknown = aggregate(records[:2])["groups"][0]
        self.assertIsNone(only_unknown["actual_known"]["real_cache_read_tokens"])
        reasons = group["incomparable_reason_counts"]["counts"]
        self.assertEqual(reasons["upstream_cache_unknown"], 2)
        self.assertEqual(reasons["simulated_cache_unknown"], 1)
        self.assertEqual(reasons["input_tokens_unknown"], 1)
        self.assertEqual(reasons["declared_incomparable"], 1)

    def test_missing_token_fields_are_unknown_but_explicit_zero_is_known(self):
        missing = event()
        del missing["upstream_cache_read_tokens"]
        group = aggregate([
            missing,
            event(input_tokens=0, upstream_cache_read_tokens=0, simulated_cache_read_tokens=0),
        ])["groups"][0]
        stats = group["comparable"]
        self.assertEqual(stats["samples"], 1)
        self.assertEqual(stats["real_cache_read_tokens"], 0)
        self.assertEqual(stats["mae_tokens"], 0)
        self.assertIsNone(stats["real_hit_rate"])
        self.assertIsNone(stats["mean_absolute_relative_error"])
        self.assertIsNone(stats["p50_absolute_relative_error"])
        self.assertIsNone(stats["p95_absolute_relative_error"])
        self.assertEqual(stats["relative_error_samples"], 0)

    def test_zero_input_excluded_from_all_relative_statistics_only(self):
        group = aggregate([
            event(input_tokens=0, upstream_cache_read_tokens=0, simulated_cache_read_tokens=50),
            event(input_tokens=100, upstream_cache_read_tokens=80, simulated_cache_read_tokens=90),
        ])["groups"][0]
        for field in ("comparable", "warm_only"):
            stats = group[field]
            self.assertEqual(stats["samples"], 2)
            self.assertEqual(stats["mae_tokens"], 30)
            self.assertEqual(stats["p95_absolute_error_tokens"], 50)
            self.assertEqual(stats["relative_error_samples"], 1)
            self.assertEqual(stats["p50_absolute_relative_error"], 0.1)
            self.assertEqual(stats["p95_absolute_relative_error"], 0.1)
            self.assertEqual(stats["mean_absolute_relative_error"], 0.1)
            self.assertEqual(stats["weighted_absolute_relative_error"], 0.1)

    def test_simulated_estimate_may_exceed_true_input_without_clamping(self):
        stats = aggregate([event(input_tokens=100, upstream_cache_read_tokens=0,
                                 simulated_cache_read_tokens=150)])["groups"][0]["comparable"]
        self.assertEqual(stats["simulated_hit_rate"], 1.5)
        self.assertEqual(stats["signed_error_tokens"], 150)

    def test_estimator_metadata_does_not_replace_true_upstream_input(self):
        stats = aggregate([event(estimated_input_tokens=99999, matched_prefix_tokens=99999,
                                 committed=False)])["groups"][0]["comparable"]
        self.assertEqual(stats["input_tokens"], 1000)
        self.assertEqual(stats["simulated_cache_read_tokens"], 600)

    def test_optional_extended_protocol_fields_do_not_calibrate_prediction(self):
        base = event(matched_prefix_tokens=600, estimated_input_tokens=2000)
        extended = {
            **base, "scaled_simulated_cache_read_tokens": 300,
            "shared_prefix_tokens": 900, "prompt_layout": "instructions_before_input",
            "expired": False, "captures": 2, "body_view": "upstream_wire",
            "output_prefix_indexed": False,
        }
        original = aggregate([base])["groups"]
        self.assertEqual(aggregate([extended])["groups"], original)
        self.assertEqual(aggregate([extended])["groups"][0]["comparable"]["signed_error_tokens"], -200)

    def test_account_high_hit_filter_aggregates_models_transports_and_known_skips(self):
        records = [
            event(input_tokens=100, upstream_cache_read_tokens=100),
            event(model="other", transport="ws", input_tokens=900, upstream_cache_read_tokens=0),
            event(account_id=7, upstream_cache_read_tokens=900, comparable=False,
                  decision="skipped", skip_reason="unsupported_body"),
            event(account_id=7, model="other", upstream_cache_read_tokens=900),
            event(account_id=8, upstream_cache_known=False, upstream_cache_read_tokens=None),
        ]
        report = aggregate(records, Decimal("0.8"))
        self.assertEqual({group["account_id"] for group in report["groups"]}, {"7"})
        self.assertEqual(report["selected_events"], 2)
        self.assertEqual(report["high_hit_filtered_events"], 3)
        accounts = {account["account_id"]: account for account in report["account_actual_usage"]}
        self.assertEqual(accounts["42"]["real_hit_rate"], 0.1)
        self.assertEqual(accounts["7"]["real_hit_rate"], 0.9)
        self.assertIsNone(accounts["8"]["real_hit_rate"])
        self.assertFalse(accounts["8"]["selected"])

    def test_threshold_is_inclusive_and_unknown_not_high_hit_even_at_zero(self):
        records = [event(), event(account_id=8, upstream_cache_known=False)]
        self.assertEqual(len(aggregate(records, Decimal("0.8"))["groups"]), 1)
        self.assertEqual(len(aggregate(records, Decimal("0.80000000000000000001"))["groups"]), 0)
        self.assertEqual(len(aggregate(records, Decimal("0"))["groups"]), 1)

    def test_account_filter_and_group_dimensions(self):
        report = aggregate([
            event(), event(account_id="7"), event(account_id=8),
            event(model="other"), event(transport="ws"), event(instance_id="another-process"),
        ], account_ids=["42", "7"])
        self.assertEqual(len(report["groups"]), 4)
        self.assertEqual(report["counts"]["account_filtered_events"], 1)
        combined = next(group for group in report["groups"] if group["account_id"] == "42"
                        and group["model"] == "synthetic-model" and group["transport"] == "http")
        self.assertEqual(combined["events"], 2)


class ParsingAndBoundsTests(unittest.TestCase):
    def test_mixed_json_lines_and_common_wrappers(self):
        message_event = event()
        message_event["message"] = message_event.pop("msg")
        records = [
            event(), message_event, {"log": json.dumps(event()) + "\n"},
            {"logs": event()}, {"payload": {"jsonPayload": event()}},
            {"message": json.dumps(event())}, {"msg": json.dumps(event())},
            {"msg": "unrelated", "component": analyze.COMPONENT},
            event(component="other"), [event()], None,
            event(schema_version=4), event(state_scope="shared"),
        ]
        analyzer = analyze.Analyzer()
        analyzer.consume(io.BytesIO(jsonl(records) + b'\nnot-json\n{"log":"{broken"}\n\xff\n'))
        report = analyzer.report()
        self.assertEqual(report["counts"]["lines"], 17)
        self.assertEqual(report["counts"]["analyzed_events"], 7)
        self.assertEqual(report["counts"]["ignored_lines"], 4)
        self.assertEqual(report["counts"]["malformed_json_lines"], 3)
        self.assertEqual(report["counts"]["blank_lines"], 1)
        self.assertEqual(report["counts"]["unsupported_schema_lines"], 1)
        self.assertEqual(report["counts"]["invalid_event_lines"], 1)

    def test_deep_junk_branch_does_not_hide_shallow_valid_collector_event(self):
        junk = {"unrelated": True}
        for _ in range(10):
            junk = {"log": junk}
        report = aggregate([
            {"log": junk, "jsonPayload": event()},
            {"log": junk, "jsonPayload": event(account_id=7)},
        ], account_ids=["42"])
        self.assertEqual(report["counts"]["wrapper_limit_lines"], 0)
        self.assertEqual(report["counts"]["analyzed_events"], 1)
        self.assertEqual(report["counts"]["account_filtered_events"], 1)
        self.assertEqual(report["groups"][0]["events"], 1)

    def test_wrapper_object_budget_is_bounded(self):
        branch = {"unrelated": True}
        for _ in range(3):
            branch = {key: branch for key in ("log", "payload", "jsonPayload", "message")}
        report = aggregate([branch, event()])
        self.assertEqual(report["counts"]["wrapper_limit_lines"], 1)
        self.assertEqual(report["counts"]["analyzed_events"], 1)

    def test_invalid_fields_do_not_crash_or_pollute_totals(self):
        records = [event(**change) for change in (
            {"input_tokens": True}, {"input_tokens": -1}, {"input_tokens": 1.5},
            {"simulated_cache_read_tokens": "600"}, {"upstream_cache_known": "false"},
            {"comparable": 1}, {"decision": []}, {"model": None}, {"transport": ""},
            {"account_id": []}, {"skip_reason": {}}, {"instance_id": None},
            {"upstream_cache_read_tokens": 1001}, {"input_tokens": 1 << 63},
            {"model": "x" * 513}, {"skip_reason": "x" * 513},
        )]
        report = aggregate(records + [event()])
        self.assertEqual(report["counts"]["invalid_event_lines"], len(records))
        self.assertEqual(report["groups"][0]["events"], 1)

    def test_nonfinite_and_overly_nested_json_are_counted_as_malformed(self):
        analyzer = analyze.Analyzer()
        analyzer.consume(io.BytesIO(b'{"x":NaN}\n' + b"[" * 2000 + b"]" * 2000 + b"\n"))
        self.assertEqual(analyzer.counts["malformed_json_lines"], 2)

    def test_giant_integer_in_irrelevant_field_is_rejected_before_conversion(self):
        giant = "9" * 10000
        for sign in ("", "-"):
            raw = '{"unknown":' + sign + giant + '}'
            with self.subTest(sign=sign):
                # The pre-conversion digit guard must work even without CPython's
                # configurable integer-string safety limit (e.g. Python 3.9).
                with mock.patch.object(analyze, "int", side_effect=AssertionError("int called"), create=True):
                    with self.assertRaisesRegex(ValueError, "digit limit"):
                        analyze.decode_json(raw)
        analyzer = analyze.Analyzer()
        analyzer.consume(io.BytesIO(('{"unknown":' + giant + '}\n').encode()
                                    + jsonl([{"log": '{"unknown":' + giant + '}'}])
                                    + jsonl([event()])))
        self.assertEqual(analyzer.counts["malformed_json_lines"], 2)
        self.assertEqual(analyzer.counts["analyzed_events"], 1)

    def test_json_nesting_guard_ignores_escaped_braces_inside_strings(self):
        record = event(prompt='[{"' * 2000 + '\\"}]' * 2000)
        report = aggregate([record])
        self.assertEqual(report["counts"]["malformed_json_lines"], 0)
        self.assertEqual(report["counts"]["analyzed_events"], 1)

    def test_oversized_line_is_drained_and_next_line_survives(self):
        limit = 4096

        class GuardedStream(io.BytesIO):
            def readline(self, size=-1):
                if not 0 < size <= limit + 1:
                    raise AssertionError("unbounded read")
                return super().readline(size)

        analyzer = analyze.Analyzer(max_line_bytes=limit)
        analyzer.consume(GuardedStream(b"x" * (limit * 4) + b"\n" + jsonl([event()])
                                       + b"x" * (limit + 2)))
        self.assertEqual(analyzer.counts["lines"], 3)
        self.assertEqual(analyzer.counts["oversized_lines"], 2)
        self.assertEqual(analyzer.counts["analyzed_events"], 1)

    def test_group_limit_fails_instead_of_silently_filtering_accounts(self):
        with self.assertRaisesRegex(analyze.LimitError, "max-groups"):
            aggregate([event(), event(model="other")], max_groups=1)
        report = aggregate([event(), event(account_id=7)], max_groups=1, account_ids=["42"])
        self.assertEqual(len(report["groups"]), 1)

    def test_reason_counter_overflow_is_visible(self):
        group = aggregate([event(comparable=False, decision="skipped", skip_reason=reason)
                           for reason in ("a", "b", "c", "a")], max_reasons=2)["groups"][0]
        self.assertEqual(group["skip_reason_counts"], {"counts": {"a": 2, "b": 1}, "other_events": 1})

    def test_reservoir_is_bounded_repeatable_and_all_other_metrics_remain_exact(self):
        def run():
            analyzer = analyze.Analyzer(quantile_samples=8)
            for value in range(100):
                analyzer.consume(io.BytesIO(jsonl([event(upstream_cache_read_tokens=0,
                                                       simulated_cache_read_tokens=value)])))
            group = next(iter(analyzer.groups.values()))
            self.assertEqual(len(group.comparable.errors), 8)
            self.assertEqual(len(group.warm_only.errors), 8)
            self.assertEqual(len(group.comparable.relative_errors), 8)
            self.assertEqual(len(group.warm_only.relative_errors), 8)
            self.assertTrue(group.warm_only.report()["relative_quantiles"]["approximate"])
            return analyzer.report()["groups"][0]["comparable"]

        stats = run()
        self.assertEqual(stats, run())
        self.assertTrue(stats["quantiles"]["approximate"])
        self.assertEqual(stats["samples"], 100)
        self.assertEqual(stats["mae_tokens"], 49.5)
        self.assertEqual(stats["simulated_cache_read_tokens"], 4950)
        self.assertTrue(stats["relative_quantiles"]["approximate"])
        self.assertEqual(stats["relative_quantiles"]["retained_samples"], 8)

    def test_relative_reservoir_has_separate_population_and_precision_marker(self):
        records = [event(input_tokens=0, upstream_cache_read_tokens=0,
                         simulated_cache_read_tokens=50) for _ in range(10)]
        records += [event(input_tokens=100, upstream_cache_read_tokens=0,
                          simulated_cache_read_tokens=10 * index) for index in (1, 2)]
        analyzer = analyze.Analyzer(quantile_samples=2)
        analyzer.consume(io.BytesIO(jsonl(records)))
        group = next(iter(analyzer.groups.values()))
        for cohort in (group.comparable, group.warm_only):
            self.assertEqual(cohort.relative_errors, [0.1, 0.2])
            stats = cohort.report()
            self.assertTrue(stats["quantiles"]["approximate"])
            self.assertFalse(stats["relative_quantiles"]["approximate"])
            self.assertEqual(stats["relative_error_samples"], 2)
            self.assertEqual(stats["p50_absolute_relative_error"], 0.1)
            self.assertEqual(stats["p95_absolute_relative_error"], 0.2)

    def test_wrapper_depth_is_bounded(self):
        obj = event()
        for _ in range(6):
            obj = {"payload": obj}
        report = aggregate([obj, event()])
        self.assertEqual(report["counts"]["wrapper_limit_lines"], 1)
        self.assertEqual(report["counts"]["analyzed_events"], 1)

    def test_report_does_not_retain_prompts_or_request_identifiers(self):
        report = aggregate([event(prompt="DO_NOT_RETAIN_PROMPT", request_id="PRIVATE_REQUEST",
                                  api_key_id=987654321, messages=[{"content": "PRIVATE_MESSAGE"}])])
        rendered = json.dumps(report)
        for private in ("DO_NOT_RETAIN_PROMPT", "PRIVATE_REQUEST", "987654321", "PRIVATE_MESSAGE"):
            self.assertNotIn(private, rendered)


class CLITests(unittest.TestCase):
    def run_cli(self, args=(), data=b""):
        return subprocess.run([sys.executable, "-B", str(HERE / "analyze.py"), *args],
                              input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)

    def test_multiple_files_stdin_repeated_accounts_and_json(self):
        with tempfile.TemporaryDirectory(dir=HERE) as temporary:
            first = Path(temporary) / "first.jsonl"
            second = Path(temporary) / "second.jsonl"
            first.write_bytes(jsonl([event(), event(account_id=8)]))
            second.write_bytes(jsonl([event(account_id=7)]))
            result = self.run_cli(["--json", "--account-id", "42", "--account-id", "7",
                                   str(first), str(second), "-"], jsonl([event(account_id=7)]))
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        report = json.loads(result.stdout)
        self.assertEqual(report["counts"]["analyzed_events"], 3)
        self.assertEqual(report["counts"]["account_filtered_events"], 1)
        self.assertEqual(report["selected_events"], 3)

    def test_stdin_default_and_text_unknown(self):
        result = self.run_cli(data=jsonl([event(upstream_cache_known=False)]))
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assertIn(b"real_hit_rate=unknown", result.stdout)
        self.assertIn(b"real_cache=unknown", result.stdout)

    def test_text_shows_group_actual_usage_even_for_skipped_only_model(self):
        result = self.run_cli(data=jsonl([
            event(model="observed"),
            event(model="skipped-only", input_tokens=2000, upstream_cache_read_tokens=1800,
                  simulated_cache_read_tokens=None, comparable=False, decision="skipped",
                  skip_reason="unsupported_body"),
        ]))
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        skipped = result.stdout.split(b'model="skipped-only"', 1)[1]
        self.assertIn(b"actual_known: samples=1 input=2000 real_cache=1800 real_hit_rate=0.9", skipped)
        self.assertIn(b"comparable: samples=0 input=unknown real_cache=unknown", skipped)

    def test_json_output_streams_without_dumps_copy(self):
        data = jsonl([event(model="synthetic-\U0001f30d", skip_reason="\U0001f30d" * 512,
                            comparable=False, decision="skipped")])
        output = io.StringIO()
        with mock.patch.object(analyze.sys, "stdin", SimpleNamespace(buffer=io.BytesIO(data))), \
                mock.patch.object(analyze.sys, "stdout", output), \
                mock.patch.object(analyze.json, "dumps", side_effect=AssertionError("full JSON copy")):
            status = analyze.main(["--json"])
        self.assertEqual(status, 0)
        self.assertTrue(output.getvalue().endswith("\n"))
        self.assertEqual(json.loads(output.getvalue())["selected_events"], 1)

    def test_text_output_streams_without_full_render_and_matches_wrapper(self):
        data = jsonl([event(model="synthetic-\U0001f30d-" + str(index), skip_reason="\U0001f30d" * 512,
                            comparable=False, decision="skipped") for index in range(3)])
        analyzer = analyze.Analyzer()
        analyzer.consume(io.BytesIO(data))
        expected = analyze.render_text(analyzer.report()) + "\n"

        class BoundedWriter:
            def __init__(self):
                self.chunks = []

            def write(self, value):
                self.chunks.append(value)
                if len(value) > len(expected) / 2:
                    raise AssertionError("report should be written incrementally")

        output = BoundedWriter()
        with mock.patch.object(analyze.sys, "stdin", SimpleNamespace(buffer=io.BytesIO(data))), \
                mock.patch.object(analyze.sys, "stdout", output), \
                mock.patch.object(analyze, "render_text", side_effect=AssertionError("full text copy")):
            status = analyze.main([])
        self.assertEqual(status, 0)
        self.assertGreater(len(output.chunks), 10)
        self.assertEqual("".join(output.chunks), expected)

    def test_json_high_hit_filter(self):
        result = self.run_cli(["--json", "--min-real-hit-rate", "0.8", "-"],
                              jsonl([event(), event(account_id=7, upstream_cache_read_tokens=100)]))
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assertEqual([g["account_id"] for g in json.loads(result.stdout)["groups"]], ["42"])

    def test_invalid_arguments_and_failed_file_have_no_partial_report(self):
        for value in ("-0.1", "1.01", "nan", "inf", "word", "1e-100000000"):
            with self.subTest(value=value):
                result = self.run_cli(["--min-real-hit-rate=" + value])
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout, b"")
        for args in (["--max-groups", "0"], ["-", "-"], ["--quantile-samples", "0"],
                     ["--max-line-bytes", "-1"], ["--max-reasons", "0"],
                     ["--max-line-bytes", str(sys.maxsize)],
                     ["--max-line-bytes", str(analyze.MAX_LINE_BYTES + 1)],
                     ["--max-groups", "9" * 1000],
                     ["--account-id", "   "], ["--account-id", "x" * 513]):
            with self.subTest(args=args):
                result = self.run_cli(args)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout, b"")
                self.assertNotIn(b"Traceback", result.stderr)
        with tempfile.TemporaryDirectory(dir=HERE) as temporary:
            missing = str(Path(temporary) / "missing.jsonl")
            result = self.run_cli(["-", missing], jsonl([event()]))
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, b"")
        limited = self.run_cli(["--json", "--max-groups", "1"], jsonl([event(), event(model="other")]))
        self.assertEqual(limited.returncode, 1)
        self.assertEqual(limited.stdout, b"")

    def test_empty_input_is_valid_and_contains_no_nan(self):
        result = self.run_cli(["--json"])
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        report = json.loads(result.stdout)
        self.assertEqual(report["groups"], [])
        self.assertEqual(report["counts"]["lines"], 0)


if __name__ == "__main__":
    unittest.main()
