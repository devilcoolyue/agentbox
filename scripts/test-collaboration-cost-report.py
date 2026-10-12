#!/usr/bin/env python3
"""Local accounting fixtures only; no providers or existing database instances."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("cost_report", ROOT / "scripts/collaboration-cost-report.py")
cost = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cost)
BASELINE = "a" * 40
NOW = "2026-10-12T00:00:00Z"
RUN = "b" * 32


class ReportTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "state.db"
        self.db = sqlite3.connect(self.path)
        self.addCleanup(self.db.close)
        for name in ("001_baseline.sql", "002_usage_price.sql", "012_chat_requests.sql", "013_collaborations.sql", "015_collaboration_decisions.sql"):
            self.db.executescript((ROOT / "internal/store/migrations" / name).read_text())

    def insert(self, table, **values):
        self.db.execute(f"INSERT INTO {table} ({','.join(values)}) VALUES ({','.join('?' for _ in values)})", list(values.values()))

    def usage(self, turn, amount, user="alice", session="single", thread="", kind="chat", source="table", **extra):
        self.insert("usage_events", ts=NOW, user=user, session_id=session, thread_id=thread, turn_id=turn,
                    kind=kind, model="synthetic", input_tokens=100, output_tokens=1000,
                    cache_read_tokens=100, cache_write_tokens=50, cost_micro_usd=amount,
                    wall_ms=100, price_snapshot=json.dumps({"version": 1, "source": source}),
                    raw="PRIVATE_RAW_SENTINEL", **extra)

    def chat(self, turn, amount, state="completed", **extra):
        self.insert("chat_requests", user="alice", user_created_at=NOW, session_id="single", request_id=turn,
                    thread_id="chat", turn_id=turn, fingerprint="synthetic", request_json='{"text":"PRIVATE_PROMPT_SENTINEL"}',
                    state=state, created_at=NOW, updated_at=NOW)
        self.usage(turn, amount, thread="chat", **extra)

    def collaboration(self):
        self.insert("collaborations", id=RUN, user="alice", user_created_at=NOW, source_session_id="source", request_id=RUN,
                    fingerprint="synthetic", request_json=json.dumps({"baseline": BASELINE, "text": "PRIVATE_PROMPT_SENTINEL"}),
                    state="completed", repairs_used=1, created_at=NOW, updated_at="2026-10-12T00:00:10Z")
        for role in ("coordinator", "executor"):
            self.insert("collaboration_participants", run_id=RUN, role=role, generation=1, config_version=1,
                        session_id=role, provider_session="PRIVATE_PROVIDER_SESSION", created_at=NOW)
        for i, (role, kind, amount) in enumerate((("coordinator", "plan", 100), ("executor", "execute", 50), ("executor", "repair", 30), ("coordinator", "review", 20))):
            turn = f"{i + 1:012x}"
            self.insert("collaboration_steps", id=turn, run_id=RUN, kind=kind, role=role, participant_generation=1,
                        config_version=1, plan_version=1, turn_id=turn, input_fingerprint="synthetic", input_json="{}",
                        state="completed", created_at=NOW, updated_at=NOW)
            self.usage(turn, amount, session=role, thread=RUN)
        self.insert("decision_attempts", id="d" * 32, run_id=RUN, provider="jev", model="jev", state="settled", reserved_micro_usd=0,
                    cost_micro_usd=7, input_tokens=20, output_tokens=0,
                    evidence_json='{"known":true,"input_tokens":20,"output_tokens":0,"cost_status":"known"}', created_at=NOW, updated_at=NOW)
        self.usage("d" * 32, 7, session="source", thread=RUN, kind="decision")
        # Same turn but foreign participant/user/thread must not enter the total.
        self.usage("000000000001", 9000, user="mallory", session="coordinator", thread=RUN)
        self.usage("000000000001", 8000, session="another", thread=RUN)
        self.usage("000000000001", 7000, session="coordinator", thread="foreign")

    def item(self, identifier="run", variant="reviewed", task="task", accepted=True, **source):
        return dict(id=identifier, task_id=task, variant=variant, baseline=BASELINE, acceptance_id="checks-v1", accepted=accepted, **(source or {"run_id": RUN}))

    def report(self, items):
        self.db.commit()
        with cost.open_snapshot(self.path) as db:
            return cost.report(db, {"version": 1, "attempts": items})

    def test_real_schema_exact_join_and_no_decision_double_charge(self):
        self.collaboration()
        self.chat("f" * 12, 300)
        report = self.report([self.item(), self.item("strong", "strong_single", user="alice", session_id="single", turn_ids=["f" * 12])])
        reviewed = report["variants"]["reviewed"]
        self.assertEqual(reviewed["total_cost_micro_usd"], 207)
        self.assertEqual(reviewed["cost_per_successful_delivery_micro_usd"], "207")
        self.assertEqual(reviewed["repairs"], 1)
        self.assertEqual(reviewed["model_wall_ms"], 400)
        self.assertEqual(reviewed["elapsed_ms"], 10000)
        self.assertEqual(reviewed["cache_read_fraction_of_input"], .4)
        self.assertEqual(reviewed["recorded_decision_input_tokens"], 20)
        self.assertTrue(report["comparison"]["fully_paired"])
        self.assertEqual(report["cost_scope"], "selected_coding_turns_and_decisions")
        self.assertEqual(report["excluded_kinds"], ["title", "terminal"])
        self.assertNotIn("PRIVATE", json.dumps(report))
        self.assertNotIn("provider_session", json.dumps(report))

    def test_failed_attempt_cost_is_included_in_each_successful_delivery(self):
        for turn, amount in (("1" * 12, 50), ("2" * 12, 80)):
            self.chat(turn, amount)
        rows = [self.item(str(i), "economy_single", accepted=bool(i), user="alice", session_id="single", turn_ids=[turn]) for i, turn in enumerate(("1" * 12, "2" * 12))]
        variant = self.report(rows)["variants"]["economy_single"]
        self.assertEqual(variant["acceptance_rate"], .5)
        self.assertEqual(variant["acceptance_coverage"], 1)
        self.assertEqual(variant["total_cost_micro_usd"], 130)
        self.assertEqual(variant["cost_per_successful_delivery_micro_usd"], "130")

    def test_unknown_unpriced_or_partial_is_never_zero_cost_success(self):
        for index, (state, source) in enumerate((("failed", "table"), ("completed", "unpriced"))):
            turn = f"{index + 10:012x}"
            self.chat(turn, 40, state=state, source=source)
            row = self.item(str(index), "single", user="alice", session_id="single", turn_ids=[turn])
            report = self.report([row])
            self.assertEqual(report["variants"]["single"]["recorded_cost_micro_usd"], 40)
            self.assertIsNone(report["variants"]["single"]["total_cost_micro_usd"])
            self.assertIsNone(report["variants"]["single"]["cost_per_successful_delivery_micro_usd"])
        self.collaboration()
        self.db.execute("UPDATE decision_attempts SET state='unknown', reserved_micro_usd=12")
        report = self.report([self.item()])
        self.assertIsNone(report["attempts"][0]["total_cost_micro_usd"])
        self.assertEqual(report["attempts"][0]["recorded_cost_micro_usd"], 200)
        self.assertEqual(report["attempts"][0]["decision_reserved_micro_usd"], 12)
        self.db.execute("UPDATE decision_attempts SET state='settled',cost_micro_usd=0,reserved_micro_usd=0,evidence_json='null'")
        row = self.report([self.item()])["attempts"][0]
        self.assertEqual(row["total_cost_micro_usd"], 200)
        self.assertFalse(row["decision_tokens_complete"], "free does not imply known tokens")

    def test_missing_receipt_usage_or_acceptance_is_explicit(self):
        item = self.item("single", "single", accepted=None, user="alice", session_id="single", turn_ids=["f" * 12])
        self.usage("f" * 12, 7000, thread="unconfirmed")
        report = self.report([item])
        self.assertEqual(report["attempts"][0]["recorded_cost_micro_usd"], 0, "missing receipt cannot authorize a guess at thread ownership")
        self.assertIsNone(report["variants"]["single"]["acceptance_rate"])
        self.assertIsNone(report["variants"]["single"]["total_cost_micro_usd"])
        self.assertIn("usage_missing", report["attempts"][0]["issues"])
        self.assertIsNone(report["attempts"][0]["repairs"])
        self.assertIsNone(report["attempts"][0]["model_wall_ms"])
        self.assertEqual(report["attempts"][0]["recorded_model_wall_ms"], 0)
        self.assertFalse(report["comparison"]["fully_paired"])
        self.assertIsNone(report["comparison"]["savings_percent"])

    def test_duplicate_sources_baselines_and_unknown_fields_are_rejected(self):
        base = self.item()
        for items in ([base, {**base, "id": "duplicate"}], [base, {**base, "id": "changed", "baseline": "c" * 40}],
                      [{**base, "accepted": 1}], [{**base, "raw": "secret"}], [{**base, "human_ms": True}],
                      [self.item("single", "single", user="alice", session_id="single", turn_ids=["f" * 12, "f" * 12])]):
            with self.assertRaises(ValueError):
                cost.manifest_rows({"version": 1, "attempts": items})
        self.collaboration()
        with self.assertRaisesRegex(ValueError, "baseline"):
            self.report([{**base, "baseline": "c" * 40}])
        # Detect overlap even across collaboration and scoped single-agent sources.
        with self.assertRaisesRegex(ValueError, "same model turn"):
            self.report([base, self.item("overlap", "single", user="alice", session_id="coordinator", turn_ids=["000000000001"])])

    def test_turn_wall_time_not_multiplied_by_model_aggregates(self):
        turn = "f" * 12
        self.chat(turn, 10)
        self.usage(turn, 20, thread="chat")
        self.usage(turn, 9000, thread="foreign")
        self.usage(turn, 8000, thread="")
        row = self.report([self.item("single", "single", user="alice", session_id="single", turn_ids=[turn])])["attempts"][0]
        self.assertEqual(row["recorded_cost_micro_usd"], 30)
        self.assertEqual(row["model_wall_ms"], 100)

    def test_read_only_snapshot_and_exclusive_output(self):
        self.collaboration()
        self.db.commit()
        before = self.path.read_bytes(), self.path.stat().st_mtime_ns
        names = {p.name for p in self.path.parent.iterdir()}
        with cost.open_snapshot(self.path) as db:
            with self.assertRaises(sqlite3.OperationalError):
                db.execute("DELETE FROM usage_events")
            cost.report(db, {"version": 1, "attempts": [self.item()]})
        self.assertEqual(before, (self.path.read_bytes(), self.path.stat().st_mtime_ns))
        self.assertEqual(names, {p.name for p in self.path.parent.iterdir()})
        manifest, output = self.path.parent / "manifest.json", self.path.parent / "report.json"
        manifest.write_text(json.dumps({"version": 1, "attempts": [self.item()]}))
        argv = ["--db", str(self.path), "--manifest", str(manifest), "--output", str(output)]
        self.assertEqual(cost.main(argv), 0)
        original = output.read_bytes()
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(cost.main(argv), 1)
        self.assertEqual(original, output.read_bytes())
        Path(str(self.path) + "-wal").write_bytes(b"uncheckpointed")
        with self.assertRaisesRegex(ValueError, "WAL"):
            with cost.open_snapshot(self.path):
                self.fail("uncheckpointed database was accepted")


if __name__ == "__main__":
    unittest.main()
