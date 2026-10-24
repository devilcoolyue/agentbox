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
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('release',Path(__file__).resolve().parent.parent/'deploy/release.py')
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class Deployment(unittest.TestCase):
 def test_storage_capabilities_before_activation(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);binary=root/'agentbox';binary.write_bytes(b'old binary');cfg=root/'config.json'
   info={'schema_version':18,'compatibility_epoch':1}
   for policy in [{'storage_policy':{'mount':'/data'}},{'tool_storage':{'root':'/tools'}},
                  {'storage_policy':{'release_keep':3}}]:
    cfg.write_text(json.dumps(policy))
    with patch.object(m,'run',return_value=json.dumps(info)), self.assertRaisesRegex(ValueError,'storage capabilities'):
     m.compatible(binary,cfg,root)
    with patch.object(m,'run',return_value=json.dumps(info|{'storage_capabilities':{'storage_policy':1,'tool_storage':1}})):
     m.compatible(binary,cfg,root)
    with patch.object(m,'run',return_value=json.dumps(info|{'storage_capabilities':{'storage_policy':True,'tool_storage':'1'}})), self.assertRaises(ValueError):
     m.compatible(binary,cfg,root)
   cfg.write_text(json.dumps({'storage_policy':{'mount':'/data'}}))
   with patch.object(m,'LEGACY_STORAGE_BINARIES',{hashlib.sha256(binary.read_bytes()).hexdigest()}), patch.object(m,'run',return_value=json.dumps(info)):
    m.compatible(binary,cfg,root)
    binary.write_bytes(b'old binary tampered')
    with self.assertRaises(ValueError):m.compatible(binary,cfg,root)
   cfg.write_text('{}')
   with patch.object(m,'run',return_value=json.dumps(info)):m.compatible(binary,cfg,root)
 def test_bundled_clients_install_and_upgrade(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);package=root/'package';source=package/'clients';source.mkdir(parents=True);data=root/'data';data.mkdir()
   for name in m.CLIENT_NAMES:(source/name).write_bytes(('v1:'+name).encode())
   m.install_clients(package,data)
   for name in m.CLIENT_NAMES:self.assertEqual((data/'abox-link'/name).read_bytes(),('v1:'+name).encode())
   (source/m.CLIENT_NAMES[0]).write_bytes(b'v2')
   m.install_clients(package,data)
   self.assertEqual((data/'abox-link'/m.CLIENT_NAMES[0]).read_bytes(),b'v2')
   (source/m.CLIENT_NAMES[-1]).unlink()
   with self.assertRaises(ValueError):m.install_clients(package,data)
   self.assertEqual((data/'abox-link'/m.CLIENT_NAMES[0]).read_bytes(),b'v2')
 def test_selinux_context_restoration(self):
  with patch.object(m.Path,'exists',return_value=False), patch.object(m,'run') as run:
   m.restore_context(Path('/fixture/release'))
   run.assert_not_called()
  with patch.object(m.Path,'exists',return_value=True), patch.object(m.shutil,'which',return_value='/sbin/restorecon'), patch.object(m,'run') as run:
   m.restore_context(Path('/fixture/release'))
   run.assert_called_once_with('/sbin/restorecon','-R',Path('/fixture/release'))
  with patch.object(m.Path,'exists',return_value=True), patch.object(m.shutil,'which',return_value=None):
   with self.assertRaisesRegex(RuntimeError,'policycoreutils'):m.restore_context(Path('/fixture/release'))
 def test_failed_relabel_leaves_staging_retryable(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);package=root/'package';package.mkdir();app=root/'app'
   (package/'build.json').write_text(json.dumps({'program':'agentbox','os':'linux','version':'v1.0.0'}))
   (package/'agentbox').write_text('#!/bin/sh\necho fixture\n');(package/'agentbox').chmod(0o755)
   target=app/'releases/v1.0.0'
   def fail(path):
    self.assertEqual(path,target)
    self.assertTrue((path/'agentbox').is_file())
    raise subprocess.CalledProcessError(1,'restorecon')
   with patch.object(m,'restore_context',side_effect=fail):
    with self.assertRaises(subprocess.CalledProcessError):m.stage(package,app)
   self.assertFalse(target.exists())
   self.assertFalse((app/'current').exists())
   with patch.object(m,'restore_context') as restore:
    self.assertEqual(m.stage(package,app),'v1.0.0')
    restore.assert_called_once_with(target)
 def test_activation_relabel_failure_preserves_running_service(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);config=root/'config.json'
   (root/'agentbox.service').write_text(m.unit(root,config))
   with patch.object(m,'restore_context',side_effect=RuntimeError('restore failed')), patch.object(m,'run') as run:
    with self.assertRaisesRegex(RuntimeError,'restore failed'):m.activate(root,'v1.0.0',config,root/'backups',root)
    run.assert_not_called()
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
 def test_upgrade_marker_lasts_only_for_the_stop(self):
  # The stopping server reads <app>/upgrading.json to name the upgrade; a
  # stop that fails or one that succeeds must not leave it behind.
  for stop_fails in (False,True):
   with self.subTest(stop_fails=stop_fails), tempfile.TemporaryDirectory() as tmp:
    root=Path(tmp);config=root/'config.json';config.write_text('{}')
    (root/'agentbox.service').write_text(m.unit(root,config))
    seen=[]
    def run(*args,**kw):
     if args[:2]==('systemctl','stop'):
      seen.append(json.loads((root/'upgrading.json').read_text()))
      if stop_fails:raise subprocess.CalledProcessError(1,'systemctl')
    checks=iter([None,ValueError('after stop')])
    def compatible(*a):
     error=next(checks)
     if error:raise error
    with patch.object(m,'restore_context'), patch.object(m,'compatible',side_effect=compatible), patch.object(m,'run',side_effect=run):
     with self.assertRaises(subprocess.CalledProcessError if stop_fails else ValueError):m.activate(root,'v1.0.0',config,root/'backups',root)
    self.assertEqual(seen,[{'version':'v1.0.0'}])
    self.assertFalse((root/'upgrading.json').exists())
 def test_release_announcement_posts_waits_and_withdraws_on_failure(self):
  # The banner is posted with the target binary, naming the running version,
  # and waited out before the stop; a failure before the stop withdraws it.
  for fails in (False,True):
   with self.subTest(fails=fails), tempfile.TemporaryDirectory() as tmp:
    root=Path(tmp);config=root/'config.json';config.write_text('{}')
    (root/'agentbox.service').write_text(m.unit(root,config))
    (root/'releases'/'v0.9.0').mkdir(parents=True);(root/'current').symlink_to(root/'releases'/'v0.9.0')
    (root/'releases'/'v0.9.0'/'build.json').write_text('{"version":"v0.9.0"}')
    calls=[];slept=[];cleared=[]
    def run(*args,**kw):
     calls.append([str(a) for a in args])
     if args[:2]==('systemctl','stop'):raise subprocess.CalledProcessError(1,'systemctl')
    checks=iter([None,ValueError('changed while waiting') if fails else None])
    def compatible(*a):
     error=next(checks)
     if error:raise error
    with patch.object(m,'restore_context'),patch.object(m,'compatible',side_effect=compatible),patch.object(m,'run',side_effect=run),\
         patch.object(m.time,'sleep',side_effect=slept.append),patch.object(m.subprocess,'run',side_effect=lambda a,**k:cleared.append(a)):
     with self.assertRaises(ValueError if fails else subprocess.CalledProcessError):
      m.activate(root,'v1.0.0',config,root/'backups',root,notice={'minutes':5,'duration':2,'message':'hi'})
    post=calls[0]
    self.assertEqual(post[0],str(root/'releases'/'v1.0.0'/'agentbox'))
    self.assertEqual(post[1:],['announce','--config',str(config),'--release','--in','5m','--duration','2','--message','hi','--from-version','v0.9.0'])
    self.assertEqual(slept,[300])
    self.assertEqual(len(cleared),1 if fails else 0)
    if fails:
     self.assertEqual(cleared[0][1:],['announce','--config',str(config),'--clear'])
     self.assertFalse(any(c[:2]==['systemctl','stop'] for c in calls))
    else:
     self.assertTrue(any(c[:2]==['systemctl','stop'] for c in calls))

 def test_release_announcement_lead_time_bounded(self):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp)
   for minutes in (0,121):
    with patch.object(m,'run') as run:
     with self.assertRaises(ValueError):m.announce_release(root/'agentbox',root,root/'config.json',{'minutes':minutes})
     run.assert_not_called()

 def test_mount_selection_includes_ancestor_mounts(self):
  records=[{'Id':'match','Mounts':[{'Source':'/fixture/users/u'}]}, {'Id':'ancestor','Mounts':[{'Source':'/fixture'}]}, {'Id':'other','Mounts':[{'Source':'/unrelated'}]}]
  with patch.object(m,'run',side_effect=['match ancestor other',json.dumps(records)]):
   self.assertEqual([c['Id'] for c in m.mounted_containers([Path('/fixture/users')])],['match','ancestor'])

if __name__=='__main__':unittest.main()
