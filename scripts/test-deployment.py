#!/usr/bin/env python3
"""Portable deployment policy tests; systemctl is simulated, not a systemd test."""
import importlib.util
from pathlib import Path
import json
import tarfile
import io
import hashlib
import datetime
import os
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('release',Path(__file__).resolve().parent.parent/'deploy/release.py')
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class Deployment(unittest.TestCase):
 def test_cloned_users_verified_against_backup(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);tree=root/'users';tree.mkdir();(tree/'file').write_bytes(b'fixture')
   (tree/'link').symlink_to('/shared/file')
   entries=[]
   for path in [tree,tree/'file',tree/'link']:
    st=path.lstat(); name='data/users'+('/'+path.name if path!=tree else '')
    kind=50 if path.is_symlink() else 53 if path.is_dir() else 48
    stamp=datetime.datetime.fromtimestamp(st.st_mtime_ns//1000000000,datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')+f'.{st.st_mtime_ns%1000000000:09d}Z'
    entry={'name':name,'type':kind,'mode':st.st_mode&0o777,'uid':st.st_uid,'gid':st.st_gid,'mtime':stamp,'size':st.st_size}
    if kind==48:entry['sha256']=hashlib.sha256(path.read_bytes()).hexdigest()
    if kind==50:entry['link']=os.readlink(path)
    entries.append(entry)
   archive=root/'full.tar.gz'
   with tarfile.open(archive,'w:gz') as tar:
    data=json.dumps({'mode':'full','entries':entries}).encode();info=tarfile.TarInfo('manifest.json');info.size=len(data);tar.addfile(info,io.BytesIO(data))
   m.verify_cloned_users(archive,tree)
   (tree/'file').write_bytes(b'changed')
   with self.assertRaises(ValueError):m.verify_cloned_users(archive,tree)
   (tree/'new').write_text('unexpected')
   with self.assertRaises(ValueError):m.verify_cloned_users(archive,tree)
 def test_snapshot_integrity_and_independence(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);(root/'file').write_bytes(b'snapshot data')
   (root/'link').symlink_to('/outside')
   inventory=m.tree_inventory(root);inventory.pop('.')
   m.atomic_json(root/'snapshot-manifest.json',{'format_version':1,'mode':'full-reflink','entries':inventory})
   m.verify_snapshot(root)
   (root/'file').write_bytes(b'changed')
   with self.assertRaises(ValueError):m.verify_snapshot(root)
 def test_version_stage_and_atomic_switch(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp); package=root/'package';package.mkdir();app=root/'app'
   (package/'build.json').write_text(json.dumps({'program':'agentbox','os':'linux','version':'v1.0.0'}))
   (package/'agentbox').write_text('#!/bin/sh\necho fixture\n');(package/'agentbox').chmod(0o755)
   version=m.stage(package,app);m.switch(app,version)
   self.assertEqual((app/'current').resolve(),(app/'releases/v1.0.0').resolve())
   with self.assertRaises(ValueError):m.stage(package,app)
   with self.assertRaises(ValueError):m.switch(app,'../../bad')
   (package/'build.json').write_text(json.dumps({'program':'agentbox','os':'linux','version':'v1.1.0'}))
   m.stage(package,app);m.switch(app,'v1.1.0');m.switch(app,'v1.0.0')
   self.assertTrue((app/'releases/v1.1.0/agentbox').exists())
 def test_future_database_rejected_without_writes(self):
  with tempfile.TemporaryDirectory() as tmp:
   data=Path(tmp);db=data/'state.db'
   with sqlite3.connect(db) as conn:conn.execute('pragma user_version=3')
   before=db.read_bytes()
   with patch.object(m,'run',return_value=json.dumps({'schema_version':2,'compatibility_epoch':1})):
    with self.assertRaises(ValueError):m.compatible(Path('fixture'),Path('config'),data)
   self.assertEqual(db.read_bytes(),before)
 def test_source_and_destination_overlap_rejected(self):
  with self.assertRaises(ValueError):m.disjoint([Path('/tmp/data'),Path('/tmp/data/new')])
  with self.assertRaises(ValueError):m.absolute('/tmp/path%name')
 def test_stopped_service_is_not_hijacked(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);(root/'agentbox.service').write_text('legacy unit')
   with patch.object(m,'run') as run:
    with self.assertRaises(ValueError):m.activate(root,'v1.0.0',root/'config',root/'backups',root)
    run.assert_not_called()
 def test_mount_selection_includes_ancestor_mounts(self):
  records=[{'Id':'match','Mounts':[{'Source':'/fixture/users/u'}]}, {'Id':'ancestor','Mounts':[{'Source':'/fixture'}]}, {'Id':'other','Mounts':[{'Source':'/unrelated'}]}]
  with patch.object(m,'run',side_effect=['match ancestor other',json.dumps(records)]):
   self.assertEqual([c['Id'] for c in m.mounted_containers([Path('/fixture/users')])],['match','ancestor'])

if __name__=='__main__':unittest.main()
