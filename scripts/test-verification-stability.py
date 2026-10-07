#!/usr/bin/env python3
"""Synthetic aggregation tests, not actual PR stability samples."""
import copy
import subprocess
import unittest
from unittest.mock import patch
import verify
import verification_stability as stability
from verification_catalog import PROFILES

class StabilityTests(unittest.TestCase):
    def sample(self):
        steps=[]
        for name in PROFILES['pr']:
            step={'id':name,'status':'passed'}
            if name in ('go.unit','go.server'):
                step['go_tests']={'cached_packages':[],'counts':{'passed':1}}
                step['command']=['_go_nonserver_fresh' if name=='go.unit' else '_go_server_fresh']
            steps.append(step)
        return {'status':'passed','profile':'pr','platform':'Linux-synthetic','source':{'worktree_sha256':'a'*64},'tools':{'go':'fixture'},'source_changed':False,'elapsed_seconds':100,'steps':steps}
    def test_twenty_comparable_uncached_runs_required(self):
        reports=[self.sample() for _ in range(20)]
        self.assertEqual(stability.summarize(reports)['status'],'passed')
        self.assertEqual(stability.summarize(reports[:19])['status'],'incomplete')
        for change in ['source','tools','cache','failed','slow','skipped','platform']:
            rows=copy.deepcopy(reports)
            if change=='source':rows[5]['source_changed']=True
            if change=='tools':rows[5]['tools']={}
            if change=='cache':next(s for s in rows[5]['steps'] if s['id']=='go.unit')['go_tests']['cached_packages']=['fixture']
            if change=='failed':rows[5]['status']='incomplete';rows[5]['steps'][0]['status']='failed'
            if change=='slow':rows[5]['elapsed_seconds']=601
            if change=='skipped':rows[5]['steps'][0]['status']='not_run'
            if change=='platform':rows[5]['platform']='Darwin'
            self.assertEqual(stability.summarize(rows)['status'],'incomplete',change)
    def test_fresh_server_keeps_linux_permissions(self):
        with patch.object(verify.sys,'platform','linux'),patch.object(verify.os,'geteuid',return_value=1001),patch.object(verify.subprocess,'run',return_value=subprocess.CompletedProcess([],0)) as run:
            self.assertEqual(verify.helper('_go_server_fresh'),0)
            self.assertEqual(run.call_args.args[0],['go','test','-json','-count=1','-exec','sudo -n --','./internal/server'])
        for step in stability.fresh_steps():
            if step['id'] in ('go.unit','go.server'):self.assertTrue(step['argv'][-1].endswith('_fresh'))

if __name__=='__main__':unittest.main()
