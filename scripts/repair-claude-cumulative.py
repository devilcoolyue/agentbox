#!/usr/bin/env python3
"""Offline, audited repair using an immutable pre-repair SQLite snapshot.

No current price table is used. Only exact token evidence authorizes a
cumulative-to-incremental correction. Original raw events and debit entries
are retained; compensating ledger entries carry stable idempotency refs.
"""
import argparse
import collections
import datetime
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import sqlite3

TOKEN_KEYS = ('inputTokens', 'outputTokens', 'cacheReadInputTokens', 'cacheCreationInputTokens')
TOP_KEYS = ('input_tokens', 'output_tokens', 'cache_read_input_tokens', 'cache_creation_input_tokens')
COLS = ('input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens', 'cost_micro_usd')
PREFIX = 'repair:claude-cumulative:v2:'


def instant(value):
    # Python 3.6 on older production hosts cannot parse Go's nanoseconds.
    value = re.sub(r'\.\d+', '', value).replace('Z', '+00:00')
    value = re.sub(r'([+-]\d\d):(\d\d)$', r'\1\2', value)
    return datetime.datetime.strptime(value, '%Y-%m-%dT%H:%M:%S%z')


def micro(value):
    # Match Go math.Round for nonnegative costs, then subtract integers. This
    # telescopes to the original cumulative total without float delta drift.
    if not math.isfinite(value) or value < 0:
        raise ValueError('invalid reported cost')
    return math.floor(value * 1_000_000 + 0.5)


def vector(model):
    return tuple(model.get(k, 0) for k in TOKEN_KEYS)


def read_groups(conn):
    groups = collections.OrderedDict()
    for row in conn.execute("SELECT * FROM usage_events WHERE agent='claude' AND kind='chat' ORDER BY id"):
        row = dict(row)
        groups.setdefault((row['session_id'], row['thread_id'], row['turn_id']), []).append(row)
    return groups


def targets(original):
    previous, result, uncertain = {}, [], []
    for key, rows in read_groups(original).items():
        raw_rows = [r for r in rows if r['raw']]
        if not raw_rows:
            uncertain.append({'ids': [r['id'] for r in rows], 'reason': 'missing_raw'})
            continue
        raw = json.loads(raw_rows[-1]['raw'])
        models = raw.get('modelUsage') or {}
        sid = raw.get('session_id')
        if not models or not sid:
            continue  # Includes new message-based receipts, not repair targets.
        old_models = previous.get((key[0], sid), {})
        previous[(key[0], sid)] = models
        top = tuple((raw.get('usage') or {}).get(k, 0) for k in TOP_KEYS)
        by_model = collections.defaultdict(list)
        for row in rows:
            by_model[row['model']].append(row)
        delta = {m: tuple(a-b for a, b in zip(vector(u), vector(old_models.get(m, {}))))
                 for m, u in models.items()}
        retained = [m for m in old_models if m in models and any(vector(old_models[m]))]
        cumulative = (bool(retained) and all(v >= 0 for d in delta.values() for v in d)
                      and tuple(sum(d[i] for d in delta.values()) for i in range(4)) == top
                      and all(micro(u.get('costUSD', 0)) >= micro(old_models.get(m, {}).get('costUSD', 0))
                              for m, u in models.items()))
        duplicate = any(len(rs) > 1 for rs in by_model.values())
        if set(models) != set(by_model):
            uncertain.append({'ids': [r['id'] for r in rows], 'reason': 'model_rows_mismatch'})
            continue
        if duplicate:
            # Only identical repeated result snapshots are safe to collapse.
            if any(json.loads(r['raw']).get('modelUsage') != models for r in raw_rows):
                uncertain.append({'ids': [r['id'] for r in rows], 'reason': 'changing_duplicate_results'})
                continue
            if any(any(tuple(r[k] for k in COLS) != tuple(rs[0][k] for k in COLS) for r in rs) for rs in by_model.values()):
                uncertain.append({'ids': [r['id'] for r in rows], 'reason': 'different_duplicate_rows'})
                continue
        if not cumulative and not duplicate:
            if old_models and top not in [vector(u) for u in models.values()] and top != tuple(sum(vector(u)[i] for u in models.values()) for i in range(4)):
                uncertain.append({'ids': [r['id'] for r in rows], 'reason': 'not_proven_cumulative'})
            continue
        corrected = {}
        valid = True
        for model, rs in by_model.items():
            row = rs[-1]
            snapshot = json.loads(row['price_snapshot'] or '{}')
            if (snapshot.get('source', 'provider') != 'provider' or
                    tuple(row[k] for k in COLS[:4]) != vector(models[model]) or
                    abs(row['cost_micro_usd'] - micro(models[model].get('costUSD', 0))) > 1):
                valid = False
                break
            cost = row['cost_micro_usd']
            tokens = vector(models[model])
            if cumulative:
                tokens = delta[model]
                cost -= micro(old_models.get(model, {}).get('costUSD', 0))
            if cost < 0:
                valid = False
                break
            for duplicate_row in rs[:-1]:
                corrected[duplicate_row['id']] = (0, 0, 0, 0, 0)
            corrected[row['id']] = tokens + (cost,)
        if valid:
            result.append({'key': key, 'rows': rows, 'corrected': corrected,
                           'basis': 'session_delta' if cumulative else 'duplicate_result'})
        else:
            uncertain.append({'ids': [r['id'] for r in rows], 'reason': 'stored_usage_or_pricing_differs'})
    return result, uncertain


def plan(original, current):
    repairs, uncertain = targets(original)
    targeted = {r['id'] for g in repairs for r in g['rows']}
    # Restore any earlier repair of an unproven row (e.g. a one-micro-dollar
    # rounding-only change made without a cumulative baseline).
    for key, rows in read_groups(original).items():
        if any(r['id'] in targeted for r in rows):
            continue
        prior = current.execute("SELECT 1 FROM credit_ledger WHERE ref=?",
                                ('repair:claude-message-dedup:v1:' + key[0] + ':' + key[2],)).fetchone()
        if prior:
            repairs.append({'key': key, 'rows': rows,
                            'corrected': {r['id']: tuple(r[k] for k in COLS) for r in rows},
                            'basis': 'restore_unproven_change'})
    changes, credits = [], []
    for group in repairs:
        key, originals = group['key'], group['rows']
        user = originals[0]['user']
        quota = current.execute('SELECT * FROM quotas WHERE user=?', (user,)).fetchone()
        expected_refund = 0
        for row in originals:
            live = current.execute('SELECT * FROM usage_events WHERE id=?', (row['id'],)).fetchone()
            if live is None or any(live[k] != row[k] for k in ('raw', 'user', 'session_id', 'thread_id', 'turn_id', 'model', 'price_snapshot')):
                raise ValueError('original identity or evidence changed for row ' + str(row['id']))
            after = group['corrected'][row['id']]
            before = tuple(live[k] for k in COLS)
            if before != after:
                changes.append({'id': row['id'], 'user': user, 'model': row['model'], 'turn': key[2],
                                'before': before, 'after': after, 'basis': group['basis'],
                                'raw_sha256': hashlib.sha256(row['raw'].encode()).hexdigest()})
            debit = current.execute('SELECT * FROM credit_ledger WHERE ref=?', ('usage:' + str(row['id']),)).fetchone()
            if debit and quota and instant(debit['ts']) >= instant(quota['created_at']):
                if debit['user'] != user or debit['delta_micro_usd'] != -row['cost_micro_usd']:
                    raise ValueError('unexpected original debit for row ' + str(row['id']))
                expected_refund += row['cost_micro_usd'] - after[-1]
        references = ['refund:turn:' + key[2], 'repair:claude-message-dedup:v1:' + key[0] + ':' + key[2], PREFIX + key[0] + ':' + key[2]]
        already = 0
        for ref in references:
            receipt = current.execute('SELECT * FROM credit_ledger WHERE ref=?', (ref,)).fetchone()
            if receipt and quota and instant(receipt['ts']) >= instant(quota['created_at']):
                if receipt['user'] != user:
                    raise ValueError('refund owner mismatch')
                already += receipt['delta_micro_usd']
        delta = expected_refund - already
        if delta:
            if quota is None:
                raise ValueError('cannot adjust removed quota')
            ref = PREFIX + key[0] + ':' + key[2]
            if current.execute('SELECT 1 FROM credit_ledger WHERE ref=?', (ref,)).fetchone():
                raise ValueError('conflicting prior repair: ' + ref)
            credits.append({'user': user, 'ref': ref, 'delta_micro_usd': delta,
                            'basis': group['basis'], 'expected_total_refund': expected_refund,
                            'previous_refund': already})
    quotas = []
    for q in current.execute('SELECT * FROM quotas'):
        entries = [dict(r) for r in current.execute('SELECT * FROM credit_ledger WHERE user=?', (q['user'],))
                   if instant(r['ts']) >= instant(q['created_at'])]
        balance = sum(r['delta_micro_usd'] for r in entries)
        if balance != q['balance_micro_usd']:
            raise ValueError('pre-existing balance/ledger mismatch: ' + q['user'])
        granted = sum(max(r['delta_micro_usd'], 0) for r in entries)
        spent = sum(max(-r['delta_micro_usd'], 0) for r in entries)
        for change in credits:
            if change['user'] == q['user']:
                delta = change['delta_micro_usd']
                balance += delta
                granted += max(delta, 0)
                spent += max(-delta, 0)
        before = (q['balance_micro_usd'], q['granted_micro_usd'], q['spent_micro_usd'])
        after = (balance, granted, spent)
        if before != after:
            quotas.append({'user': q['user'], 'before': before, 'after': after})
    return {'usage_changes': changes, 'credits': credits, 'quota_changes': quotas, 'unresolved': uncertain}


def apply(conn, report):
    now = datetime.datetime.now(datetime.timezone.utc).isoformat().replace('+00:00', 'Z')
    for row in report['usage_changes']:
        conn.execute('UPDATE usage_events SET input_tokens=?,output_tokens=?,cache_read_tokens=?,cache_write_tokens=?,cost_micro_usd=? WHERE id=?', tuple(row['after']) + (row['id'],))
    for change in report['credits']:
        user, delta = change['user'], change['delta_micro_usd']
        balance = conn.execute('SELECT balance_micro_usd FROM quotas WHERE user=?', (user,)).fetchone()[0] + delta
        conn.execute('UPDATE quotas SET balance_micro_usd=? WHERE user=?', (balance, user))
        conn.execute('INSERT INTO credit_ledger(ts,user,ref,reason,delta_micro_usd,balance_after,note,actor) VALUES(?,?,?,?,?,?,?,?)',
                     (now, user, change['ref'], 'adjust', delta, balance,
                      'Claude 历史计费核对：' + change['basis'] + '；保留原始事件及扣款，按整数微美元冲正', 'boxadmin'))
    for quota in report['quota_changes']:
        conn.execute('UPDATE quotas SET balance_micro_usd=?,granted_micro_usd=?,spent_micro_usd=?,updated_at=? WHERE user=?',
                     tuple(quota['after']) + (now, quota['user']))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--original-db', required=True)
    parser.add_argument('--db', required=True)
    parser.add_argument('--report', required=True)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    original_path, current_path = Path(args.original_db).resolve(), Path(args.db).resolve()
    if original_path == current_path:
        parser.error('original must be an immutable, separate pre-repair snapshot')
    original = sqlite3.connect(original_path.as_uri() + '?mode=ro', uri=True)
    original.row_factory = sqlite3.Row
    lock = None
    if args.apply:
        lock = open(str(current_path.parent / 'agentbox.lock'), 'a')
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    conn = sqlite3.connect(current_path.as_uri() + ('?mode=rw' if args.apply else '?mode=ro'), uri=True)
    conn.row_factory = sqlite3.Row
    try:
        conn.execute('BEGIN IMMEDIATE' if args.apply else 'BEGIN')
        report = plan(original, conn)
        report.update({'original_sha256': hashlib.sha256(original_path.read_bytes()).hexdigest(), 'applied': args.apply})
        # Persist before mutation; never overwrite an earlier audit receipt.
        fd = os.open(args.report, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, 'w') as f:
            json.dump(report, f, ensure_ascii=False, indent=2)
            f.write('\n'); f.flush(); os.fsync(f.fileno())
        if args.apply:
            apply(conn, report)
            check = plan(original, conn)
            if any(check[k] for k in ('usage_changes', 'credits', 'quota_changes')):
                raise ValueError('repair did not converge to an idempotent result')
            conn.commit()
        else:
            conn.rollback()
        print(json.dumps({'applied': args.apply, 'usage_changes': len(report['usage_changes']),
                          'credit_delta': sum(c['delta_micro_usd'] for c in report['credits']),
                          'quota_changes': report['quota_changes'], 'unresolved': report['unresolved']}, ensure_ascii=False))
    finally:
        conn.close(); original.close()
        if lock:
            lock.close()


if __name__ == '__main__':
    main()
