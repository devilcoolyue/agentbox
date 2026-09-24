#!/usr/bin/env python3
"""Uninstaller boundary tests; no host service or file removal."""
import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest.mock import patch

source = (Path(__file__).resolve().parent.parent / 'uninstall.sh').read_text().split("<<'PY'\n",1)[1].split('\nPY\n',1)[0]
m = types.ModuleType('uninstall')
exec(compile(source, 'uninstall.sh', 'exec'), m.__dict__)

class Uninstall(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(); self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name).resolve()
        for name in ('APP','CONFIG','DATA','CACHE','BACKUPS'):
            setattr(m,name,root/name.lower())
        m.UNIT = root/'agentbox.service'
        m.CONFIG.mkdir()
        self.cfg = {'data_dir':str(m.DATA),'cache_dir':str(m.CACHE)}
        (m.CONFIG/'config.json').write_text(json.dumps(self.cfg))
        m.UNIT.write_text(f'WorkingDirectory={m.APP}\nExecStart={m.APP}/current/agentbox -config {m.CONFIG}/config.json\n')
    def validate(self):
        with patch.object(m,'run',side_effect=[str(m.UNIT),'']):return m.validate()
    def test_accepts_default_layout(self):
        self.assertEqual(self.validate(),self.cfg)
    def test_refuses_foreign_unit(self):
        m.UNIT.write_text('ExecStart=/other/app\n')
        with self.assertRaises(ValueError):self.validate()
    def test_refuses_custom_data(self):
        (m.CONFIG/'config.json').write_text(json.dumps(dict(self.cfg,data_dir='/other/data')))
        with self.assertRaises(ValueError):self.validate()
    def test_refuses_symlinked_directory(self):
        m.DATA.symlink_to(m.CONFIG,target_is_directory=True)
        with self.assertRaises(ValueError):self.validate()
    def test_selects_only_owned_mounted_containers(self):
        rows=[{'Id':'ours','Config':{'Labels':{'agentbox.session':'fixture'}},'Mounts':[{'Type':'bind','Source':str(m.DATA/'users/fixture')}]},
              {'Id':'other','Config':{},'Mounts':[{'Type':'bind','Source':'/unrelated'}]}]
        with patch.object(m,'run',side_effect=['ours other',json.dumps(rows)]):self.assertEqual(m.containers(),['ours'])
        rows[0]['Config']={}
        with patch.object(m,'run',side_effect=['ours other',json.dumps(rows)]),self.assertRaises(ValueError):m.containers()
    def test_refuses_ancestor_mount(self):
        rows=[{'Id':'other','Config':{'Labels':{'agentbox.session':'other'}},'Mounts':[{'Type':'bind','Source':str(m.DATA.parent)}]}]
        with patch.object(m,'run',side_effect=['other',json.dumps(rows)]),self.assertRaises(ValueError):m.containers()

if __name__=='__main__':unittest.main()
