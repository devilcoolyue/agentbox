#!/usr/bin/env python3
"""Synthetic repair checks; no production data or credentials."""
import importlib.util
import json
from pathlib import Path
import sqlite3
import unittest

spec = importlib.util.spec_from_file_location('repair', Path(__file__).with_name('repair-claude-cumulative.py'))
repair = importlib.util.module_from_spec(spec)
spec.loader.exec_module(repair)


def database():
    conn = sqlite3.connect(':memory:')
    conn.row_factory = sqlite3.Row
    conn.executescript('''
      CREATE TABLE usage_events(id INTEGER PRIMARY KEY, user TEXT, session_id TEXT, thread_id TEXT,
        turn_id TEXT, model TEXT, agent TEXT, kind TEXT, raw TEXT, price_snapshot TEXT,
        input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
        cache_write_tokens INTEGER, cost_micro_usd INTEGER);
      CREATE TABLE quotas(user TEXT PRIMARY KEY, balance_micro_usd INTEGER,
        granted_micro_usd INTEGER, spent_micro_usd INTEGER, created_at TEXT, updated_at TEXT);
      CREATE TABLE credit_ledger(id INTEGER PRIMARY KEY, ts TEXT, user TEXT, ref TEXT UNIQUE,
        reason TEXT, delta_micro_usd INTEGER, balance_after INTEGER, note TEXT, actor TEXT);
      INSERT INTO quotas VALUES('u',0,0,0,'2026-01-01T00:00:00Z','');
    ''')
    return conn


def add(conn, nid, cost, output, current_output, turn=None):
    turn = turn or 't' + str(nid)
    raw = {'type': 'result', 'session_id': 'provider',
           'usage': {'output_tokens': current_output},
           'modelUsage': {'opus': {'outputTokens': output, 'costUSD': cost / 1e6}}}
    conn.execute('INSERT INTO usage_events VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)',
                 (nid, 'u', 's', 'thread', turn, 'opus', 'claude', 'chat', json.dumps(raw), '', 0, output, 0, 0, cost))
    q = conn.execute('SELECT balance_micro_usd FROM quotas').fetchone()[0]
    conn.execute('INSERT INTO credit_ledger(ts,user,ref,reason,delta_micro_usd,balance_after) VALUES(?,?,?,?,?,?)',
                 ('2026-02-01T00:00:00Z', 'u', 'usage:' + str(nid), 'spend', -cost, q-cost))
    conn.execute('UPDATE quotas SET balance_micro_usd=balance_micro_usd-?,spent_micro_usd=spent_micro_usd+?', (cost, cost))
    conn.commit()


class RepairTest(unittest.TestCase):
    def test_delta_idempotent_and_ledger_counters(self):
        original, live = database(), database()
        for c in (original, live):
            add(c, 1, 500000, 10, 10)
            add(c, 2, 700000, 15, 5)
        p = repair.plan(original, live)
        self.assertEqual(p['usage_changes'][0]['after'], (0, 5, 0, 0, 200000))
        self.assertEqual(p['credits'][0]['delta_micro_usd'], 500000)
        repair.apply(live, p)
        q = live.execute('SELECT * FROM quotas').fetchone()
        self.assertEqual((q['balance_micro_usd'], q['granted_micro_usd'], q['spent_micro_usd']), (-700000, 500000, 1200000))
        again = repair.plan(original, live)
        for k in ('usage_changes', 'credits', 'quota_changes'):
            self.assertEqual(again[k], [])

    def test_single_turn_and_reset_are_not_subtracted(self):
        original, live = database(), database()
        for c in (original, live):
            add(c, 1, 500000, 10, 10)
            add(c, 2, 700000, 15, 15)
            add(c, 3, 100000, 2, 2)
        p = repair.plan(original, live)
        self.assertEqual(p['usage_changes'], [])
        self.assertEqual(p['credits'], [])

    def test_duplicate_result_previously_refunded(self):
        original, live = database(), database()
        for c in (original, live):
            add(c, 1, 500000, 10, 10, 'same-turn')
            add(c, 2, 500000, 10, 10, 'same-turn')
            c.execute("INSERT INTO credit_ledger(ts,user,ref,reason,delta_micro_usd,balance_after) VALUES('2026-02-02T00:00:00Z','u','refund:turn:same-turn','adjust',500000,-500000)")
            c.execute('UPDATE quotas SET balance_micro_usd=-500000,spent_micro_usd=500000')
            c.commit()
        p = repair.plan(original, live)
        self.assertEqual(p['usage_changes'][0]['after'], (0, 0, 0, 0, 0))
        self.assertEqual(p['credits'], [])
        repair.apply(live, p)
        self.assertEqual(live.execute('SELECT granted_micro_usd,spent_micro_usd FROM quotas').fetchone()[:], (500000, 1000000))

    def test_round_cumulative_before_subtraction(self):
        self.assertEqual(repair.micro(.1134145), 113415)
        self.assertEqual(repair.micro(.2000005) - repair.micro(.1000005), 100000)
        self.assertEqual(repair.instant('2026-07-31T02:20:17.747716949-07:00'), repair.instant('2026-07-31T09:20:17Z'))

    def test_forged_debit_rolls_back(self):
        original, live = database(), database()
        for c in (original, live):
            add(c, 1, 500000, 10, 10)
            add(c, 2, 700000, 15, 5)
        live.execute("UPDATE credit_ledger SET delta_micro_usd=-1 WHERE ref='usage:2'")
        with self.assertRaises(ValueError):
            repair.plan(original, live)


if __name__ == '__main__':
    unittest.main()
