#!/usr/bin/env python3
"""Collect repeated PR checks in ONE runner invocation; never retry/hide failures."""
import argparse
import copy
import json
import math
import os
from pathlib import Path
import platform
import statistics
import sys
import uuid

import verify
from verification_catalog import PROFILES


def summarize(reports, required=20, budget_seconds=600):
    expected=PROFILES['pr']
    elapsed=[r['elapsed_seconds'] for r in reports if r.get('status')=='passed' and isinstance(r.get('elapsed_seconds'),(int,float))]
    comparable=bool(reports) and bool(reports[0].get('source',{}).get('worktree_sha256')) and all(r.get('source')==reports[0].get('source') and r.get('tools')==reports[0].get('tools') and r.get('platform')==reports[0].get('platform') and r.get('source_changed') is False for r in reports)
    valid=bool(reports)
    outcomes={name:set() for name in expected}
    for report in reports:
        steps=report.get('steps',[])
        if [s['id'] for s in steps]!=expected or report.get('profile')!='pr' or not report.get('platform','').startswith('Linux'):valid=False
        for step in steps:
            if step['id'] in outcomes:outcomes[step['id']].add(step['status'])
            if step['status']!='passed':valid=False
            if step['id'] in ('go.unit','go.server'):
                evidence=step.get('go_tests',{})
                command=step.get('command',[])
                helper='_go_nonserver_fresh' if step['id']=='go.unit' else '_go_server_fresh'
                if evidence.get('cached_packages') or evidence.get('counts',{}).get('passed',0)==0 or helper not in command:valid=False
    qualified=len(reports)>=required and len(elapsed)==len(reports) and comparable and valid and max(elapsed,default=math.inf)<=budget_seconds
    return {'status':'passed' if qualified else 'incomplete','completed_runs':len(reports),'required_runs':required,
            'comparable':comparable,'all_steps_executed_without_test_cache':valid,
            'failed_run_rate':sum(r.get('status')!='passed' for r in reports)/len(reports) if reports else None,
            'median_seconds':statistics.median(elapsed) if elapsed else None,
            'p95_seconds':sorted(elapsed)[math.ceil(.95*len(elapsed))-1] if elapsed else None,
            'max_seconds':max(elapsed) if elapsed else None,'budget_seconds':budget_seconds,
            'mixed_outcome_steps':[name for name,values in outcomes.items() if len(values)>1],
            'scope_note':'Mixed outcomes are investigation leads, not proof of nondeterminism. Go result caching is forbidden; compiler caches may warm. Skipped live/manual tests remain outside acceptance.'}


def fresh_steps():
    steps=copy.deepcopy([verify.BY_ID[name] for name in PROFILES['pr']])
    for step in steps:
        if step['id'] in ('go.unit','go.server'):step['argv'][-1]+='_fresh'
    return steps


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runs',type=int,default=20)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--require-ci',action='store_true')
    args=parser.parse_args()
    if not 1<=args.runs<=100:parser.error('--runs must be 1..100; fewer than 20 cannot pass acceptance')
    if args.require_ci and (os.environ.get('GITHUB_ACTIONS')!='true' or sys.platform!='linux'):parser.error('requires the explicit Linux CI job')
    args.output.mkdir(parents=True,exist_ok=False,mode=0o700)
    runner={'batch_id':uuid.uuid4().hex,'platform':platform.platform(),'machine':platform.machine(),'cpu_count':os.cpu_count(),
            'provider':'github-actions' if os.environ.get('GITHUB_ACTIONS')=='true' else 'local',
            'image_version':os.environ.get('ImageVersion','unknown')}
    reports=[]
    try:
        for index in range(args.runs):
            target=args.output/f'run-{index+1:02}'
            code=verify.run_steps(fresh_steps(),target,{'python':sys.executable,'timeout':1800},'pr')
            reports.append(json.loads((target/'report.json').read_text()))
            result={'runner':runner,**summarize(reports)}
            verify.save_report(args.output/'summary.json',result)
            if code in (2,130) or reports[-1].get('source_changed'):break
    finally:
        # Incomplete/failed runs stay on disk. No rerun substitutes a green report.
        result={'runner':runner,**summarize(reports)}
        verify.save_report(args.output/'summary.json',result)
    print(json.dumps(result,ensure_ascii=False,indent=2))
    return 0 if result['status']=='passed' else 1


if __name__=='__main__':raise SystemExit(main())
