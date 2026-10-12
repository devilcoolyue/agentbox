#!/usr/bin/env python3
"""Read-only ledger comparison; never launches agents, reprices, or infers acceptance."""
import argparse
from collections import defaultdict
from contextlib import contextmanager
from datetime import datetime
from decimal import Decimal
import json
from pathlib import Path
import re
import sqlite3
import sys

LABEL = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}\Z")
HASH = re.compile(r"(?:[a-f0-9]{40}|[a-f0-9]{64})\Z")
TOKENS = ("input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens")
USAGE_COLUMNS = "id,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_micro_usd,price_snapshot,wall_ms"


def integer(value):
    return type(value) is int and value >= 0


def manifest_rows(value):
    if not isinstance(value, dict) or set(value) != {"version", "attempts"} or type(value["version"]) is not int or value["version"] != 1:
        raise ValueError("manifest requires version=1 and attempts")
    rows = value["attempts"]
    if not isinstance(rows, list) or not 1 <= len(rows) <= 5000:
        raise ValueError("manifest requires 1..5000 attempts")
    required = {"id", "task_id", "variant", "baseline", "acceptance_id", "accepted"}
    allowed = required | {"run_id", "user", "session_id", "turn_ids", "elapsed_ms", "model_repairs", "human_repairs", "human_ms"}
    identities, conditions, sources = set(), {}, set()
    for row in rows:
        if not isinstance(row, dict) or not required <= row.keys() or row.keys() - allowed:
            raise ValueError("attempt contains missing or unsupported fields")
        for name in ("id", "task_id", "variant", "acceptance_id"):
            if not isinstance(row[name], str) or not LABEL.fullmatch(row[name]):
                raise ValueError("attempt labels must be short identifiers, not task text")
        if row["id"] in identities:
            raise ValueError("duplicate attempt id")
        identities.add(row["id"])
        if not isinstance(row["baseline"], str) or not HASH.fullmatch(row["baseline"]):
            raise ValueError("baseline must be a Git commit hash")
        condition = (row["baseline"], row["acceptance_id"])
        if conditions.setdefault(row["task_id"], condition) != condition:
            raise ValueError("same task has different baseline or acceptance criteria")
        if row["accepted"] is not None and type(row["accepted"]) is not bool:
            raise ValueError("accepted must be explicit true, false, or null")
        for key in ("elapsed_ms", "model_repairs", "human_repairs", "human_ms"):
            if key in row and not integer(row[key]):
                raise ValueError("attempt counters must be nonnegative integers")
        if "run_id" in row:
            if not isinstance(row["run_id"], str) or not re.fullmatch(r"[a-f0-9]{32}", row["run_id"]) or any(k in row for k in ("user", "session_id", "turn_ids", "model_repairs")):
                raise ValueError("use one run_id or scoped chat turn_ids")
            keys = [("run", row["run_id"])]
        else:
            if not all(isinstance(row.get(k), str) and 0 < len(row[k]) <= 128 and "\0" not in row[k] for k in ("user", "session_id")):
                raise ValueError("chat attempts require user and session_id")
            turns = row.get("turn_ids")
            if not isinstance(turns, list) or not 1 <= len(turns) <= 1000 or any(not isinstance(t, str) or not re.fullmatch(r"[a-f0-9]{12,32}", t) for t in turns):
                raise ValueError("turn_ids must be bounded persistent turn identities")
            keys = [("turn", row["user"], row["session_id"], t) for t in turns]
        for key in keys:
            if key in sources:
                raise ValueError("same run or scoped turn appears more than once")
            sources.add(key)
    return rows


@contextmanager
def open_snapshot(path):
    path = Path(path).resolve(strict=True)
    wal = Path(str(path) + "-wal")
    if not path.is_file() or wal.exists() and wal.stat().st_size:
        raise ValueError("use a standalone SQLite backup; an uncheckpointed WAL is not a snapshot")
    # immutable prevents SQLite from creating journal/shared-memory side files.
    # Therefore a nonempty WAL is rejected instead of silently being ignored.
    db = sqlite3.connect(path.as_uri() + "?mode=ro&immutable=1", uri=True)
    db.row_factory = sqlite3.Row
    try:
        db.execute("PRAGMA query_only=ON")
        db.execute("BEGIN")
        yield db
    finally:
        db.rollback()
        db.close()


def rows_for_turn(db, user, session, turn, thread):
    where = "user=? AND session_id=? AND turn_id=? AND thread_id=? AND kind='chat'"
    params = [user, session, turn, thread]
    return db.execute(f"SELECT {USAGE_COLUMNS} FROM usage_events WHERE {where} ORDER BY id", params).fetchall()


def elapsed(start, end):
    try:
        a, b = (datetime.fromisoformat(v.replace("Z", "+00:00")) for v in (start, end))
        return round((b - a).total_seconds() * 1000) if b >= a else None
    except (ValueError, TypeError):
        return None


def summarize_attempt(db, item, claimed_usage, claimed_turns):
    result = {k: item[k] for k in ("id", "task_id", "variant", "accepted")}
    issues, usage, turns, model_wall = set(), {}, [], 0
    result.update({"repairs": item.get("model_repairs"), "decision_calls": 0, "decision_cost_micro_usd": 0, "decision_reserved_micro_usd": 0, "decision_tokens_complete": True, "decision_input_tokens": 0, "decision_output_tokens": 0})
    elapsed_ms = None
    if "run_id" in item:
        run = db.execute("SELECT user,state,repairs_used,created_at,updated_at,json_extract(request_json,'$.baseline') AS baseline FROM collaborations WHERE id=?", (item["run_id"],)).fetchone()
        if run is None:
            raise ValueError("manifest references an unknown collaboration")
        if run["baseline"] != item["baseline"]:
            raise ValueError("collaboration baseline does not match the experiment manifest")
        if run["state"] not in ("completed", "failed", "cancelled"):
            issues.add("run_not_final")
        if not integer(run["repairs_used"]):
            raise ValueError("invalid repair counter")
        result["repairs"] = run["repairs_used"]
        elapsed_ms = elapsed(run["created_at"], run["updated_at"])
        steps = db.execute("SELECT s.turn_id,s.state,p.session_id FROM collaboration_steps s LEFT JOIN collaboration_participants p ON p.run_id=s.run_id AND p.role=s.role AND p.generation=s.participant_generation WHERE s.run_id=? ORDER BY s.created_at,s.rowid", (item["run_id"],)).fetchall()
        if not steps:
            issues.add("no_model_steps")
        for step in steps:
            if not step["session_id"] or not step["turn_id"]:
                issues.add("missing_participant_or_turn")
                continue
            turns.append((run["user"], step["session_id"], step["turn_id"], item["run_id"], step["state"]))
        for decision in db.execute("SELECT state,cost_micro_usd,reserved_micro_usd,error_code,input_tokens,output_tokens,json_extract(evidence_json,'$.known') AS tokens_known,json_extract(evidence_json,'$.input_tokens') AS evidence_input,json_extract(evidence_json,'$.output_tokens') AS evidence_output FROM decision_attempts WHERE run_id=?", (item["run_id"],)):
            if not integer(decision["cost_micro_usd"]) or not integer(decision["reserved_micro_usd"]):
                raise ValueError("invalid decision ledger amount")
            result["decision_reserved_micro_usd"] += decision["reserved_micro_usd"]
            if decision["state"] == "not_dispatched":
                continue
            result["decision_calls"] += 1
            if decision["tokens_known"] == 1 and all(integer(decision[k]) for k in ("input_tokens", "output_tokens")) and decision["evidence_input"] == decision["input_tokens"] and decision["evidence_output"] == decision["output_tokens"]:
                result["decision_input_tokens"] += decision["input_tokens"]
                result["decision_output_tokens"] += decision["output_tokens"]
            else:
                result["decision_tokens_complete"] = False
            if decision["state"] == "settled" and not decision["error_code"]:
                result["decision_cost_micro_usd"] += decision["cost_micro_usd"]
            else:
                issues.add("decision_cost_unknown")
    else:
        for turn in item["turn_ids"]:
            receipt = db.execute("SELECT state,thread_id FROM chat_requests WHERE user=? AND session_id=? AND turn_id=?", (item["user"], item["session_id"], turn)).fetchone()
            if receipt is None:
                issues.add("chat_completion_unconfirmed")
            turns.append((item["user"], item["session_id"], turn, receipt["thread_id"] if receipt else None, receipt["state"] if receipt else "unknown"))
    for user, session, turn, thread, state in turns:
        key = (user, session, turn)
        if key in claimed_turns:
            raise ValueError("same model turn is claimed by multiple attempts or steps")
        claimed_turns.add(key)
        records = rows_for_turn(db, user, session, turn, thread) if thread is not None else []
        if any(not integer(r[k]) for r in records for k in (*TOKENS, "cost_micro_usd", "wall_ms")):
            raise ValueError("invalid nonnegative ledger counter")
        if not records:
            issues.add("usage_missing")
        if state != "completed":
            issues.add("turn_usage_partial")
        # A turn can have several model rows, each repeating the same wall time.
        model_wall += max((r["wall_ms"] for r in records), default=0)
        for record in records:
            if record["id"] in claimed_usage:
                raise ValueError("same usage row is claimed by multiple attempts")
            claimed_usage.add(record["id"])
            usage[record["id"]] = record
    token_totals = {k: 0 for k in TOKENS}
    recorded = result["decision_cost_micro_usd"]
    for row in usage.values():
        for key in (*TOKENS, "cost_micro_usd", "wall_ms"):
            if not integer(row[key]):
                raise ValueError("invalid nonnegative ledger counter")
        recorded += row["cost_micro_usd"]
        for key in TOKENS:
            token_totals[key] += row[key]
        try:
            price = json.loads(row["price_snapshot"])
        except (ValueError, TypeError):
            price = None
        if not isinstance(price, dict) or price.get("source") not in ("table", "provider"):
            issues.add("unpriced_usage")
    denom = token_totals["input_tokens"] + token_totals["cache_read_tokens"] + token_totals["cache_write_tokens"]
    tokens_complete = not issues.intersection({"usage_missing", "turn_usage_partial", "missing_participant_or_turn", "no_model_steps", "chat_completion_unconfirmed"})
    result.update({"recorded_cost_micro_usd": recorded, "total_cost_micro_usd": recorded if not issues else None,
                   "cost_complete": not issues, "issues": sorted(issues), "model_turns": len(turns), "usage_rows": len(usage),
                   "coding_tokens_complete": tokens_complete, "recorded_tokens": token_totals, "cache_read_fraction_of_input": round(token_totals["cache_read_tokens"] / denom, 6) if denom else None,
                   "model_wall_ms": model_wall if tokens_complete else None, "recorded_model_wall_ms": model_wall, "elapsed_ms": item.get("elapsed_ms", elapsed_ms),
                   "elapsed_source": "manifest" if "elapsed_ms" in item else "run_timestamps" if elapsed_ms is not None else "unavailable",
                   "human_repairs": item.get("human_repairs"), "human_ms": item.get("human_ms")})
    return result


def aggregate(rows):
    successes = {r["task_id"] for r in rows if r["accepted"] is True}
    known = sum(r["accepted"] is not None for r in rows)
    accepted = sum(r["accepted"] is True for r in rows)
    complete = all(r["cost_complete"] for r in rows)
    recorded = sum(r["recorded_cost_micro_usd"] for r in rows)
    buckets = {k: sum(r["recorded_tokens"][k] for r in rows) for k in TOKENS}
    denominator = sum(buckets[k] for k in ("input_tokens", "cache_read_tokens", "cache_write_tokens"))
    return {"attempts": len(rows), "acceptance_known": known, "acceptance_coverage": round(known / len(rows), 6), "accepted_attempts": accepted,
            "acceptance_rate": round(accepted / known, 6) if known else None,
            "successful_deliveries": len(successes), "cost_complete": complete,
            "recorded_cost_micro_usd": recorded, "total_cost_micro_usd": recorded if complete else None,
            "cost_per_successful_delivery_micro_usd": str(Decimal(recorded) / len(successes)) if complete and successes and known == len(rows) else None,
            "incomplete_attempts": sum(not r["cost_complete"] for r in rows),
            "coding_tokens_complete": all(r["coding_tokens_complete"] for r in rows), "recorded_tokens": buckets, "cache_read_fraction_of_input": round(buckets["cache_read_tokens"] / denominator, 6) if denominator else None,
            "repairs": sum(r["repairs"] for r in rows) if all(r["repairs"] is not None for r in rows) else None, "decision_calls": sum(r["decision_calls"] for r in rows),
            "decision_cost_micro_usd": sum(r["decision_cost_micro_usd"] for r in rows),
            "decision_tokens_complete": all(r["decision_tokens_complete"] for r in rows),
            "recorded_decision_input_tokens": sum(r["decision_input_tokens"] for r in rows),
            "recorded_decision_output_tokens": sum(r["decision_output_tokens"] for r in rows),
            "decision_reserved_micro_usd": sum(r["decision_reserved_micro_usd"] for r in rows),
            "model_wall_ms": sum(r["model_wall_ms"] for r in rows) if all(r["model_wall_ms"] is not None for r in rows) else None,
            "recorded_model_wall_ms": sum(r["recorded_model_wall_ms"] for r in rows),
            **{key: sum(r[key] for r in rows) if all(r[key] is not None for r in rows) else None for key in ("elapsed_ms", "human_repairs", "human_ms")}}


def report(db, manifest):
    inputs = manifest_rows(manifest)
    claimed_usage, claimed_turns = set(), set()
    attempts = [summarize_attempt(db, row, claimed_usage, claimed_turns) for row in inputs]
    groups, tasks = defaultdict(list), defaultdict(set)
    for row in attempts:
        groups[row["variant"]].append(row)
        tasks[row["task_id"]].add(row["variant"])
    variants = sorted(groups)
    paired = sorted(task for task, present in tasks.items() if len(variants) > 1 and present == set(variants))
    return {"version": 1, "money_basis": "recorded_platform_ledger_micro_usd",
            "cost_scope": "selected_coding_turns_and_decisions", "excluded_kinds": ["title", "terminal"], "acceptance_basis": "explicit_external_manifest",
            "cache_basis": "coding_model_input_excluding_output_and_decision_tokens", "attempts": attempts,
            "variants": {key: aggregate(groups[key]) for key in variants},
            "comparison": {"paired_task_ids": paired, "fully_paired": len(variants) > 1 and len(paired) == len(tasks),
                           "savings_percent": None, "note": "No causal savings estimate; use matched tasks and independent acceptance. Subscription cash costs are not inferred."}}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", required=True, type=Path)
    parser.add_argument("--manifest", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args(argv)
    try:
        if args.manifest.stat().st_size > 4 * 1024 * 1024:
            raise ValueError("manifest exceeds 4 MiB")
        manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
        with open_snapshot(args.db) as db:
            value = report(db, manifest)
        # Exclusive create preserves both input databases and earlier reports.
        with args.output.open("x", encoding="utf-8") as output:
            json.dump(value, output, ensure_ascii=False, indent=2, allow_nan=False)
            output.write("\n")
        return 0
    except (ValueError, OSError, sqlite3.Error) as error:
        # Never print SQLite payloads, paths, request JSON or arbitrary DB errors.
        message = str(error) if type(error) is ValueError else "cannot read the snapshot/manifest or create a new output; required accounting schema must be present"
        print("Cost report failed: " + message, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
