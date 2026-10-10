#!/usr/bin/env python3
"""Release retention policy regression on synthetic temporary packages."""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('retention',Path(__file__).resolve().parents[1]/'deploy/retention.py')
retention=importlib.util.module_from_spec(spec)
spec.loader.exec_module(retention)

class RetentionTests(unittest.TestCase):
    def test_current_recent_pinned_and_process_references(self):
        with tempfile.TemporaryDirectory() as td:
            app=Path(td).resolve();releases=app/'releases';releases.mkdir()
            versions=[]
            for i in range(7):
                p=releases/f'v0.1.{i}';p.mkdir(mode=0o700);(p/'agentbox').write_text('fixture')
                os.utime(p,(100+i,100+i));versions.append(p)
            (app/'current').symlink_to('releases/v0.1.0')
            (app/'rollback').symlink_to('releases/v0.1.1')
            (releases/'not-a-package').mkdir()
            result=retention.plan(app,2,['v0.1.2'],[versions[3]/'agentbox'])
            self.assertEqual(result,[versions[4]])
            self.assertEqual(retention.plan(app,0),[])
            with self.assertRaises(ValueError):retention.plan(app,1)
            with self.assertRaises(ValueError):retention.plan(app,2,['../escape'])

    def test_symlink_root_is_rejected(self):
        with tempfile.TemporaryDirectory() as td:
            root=Path(td).resolve();(root/'real').mkdir();(root/'link').symlink_to(root/'real')
            with self.assertRaises(ValueError):retention.plan(root/'link',5)

if __name__=='__main__':unittest.main()
