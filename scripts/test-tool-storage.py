#!/usr/bin/env python3
"""Offline publisher integrity/atomicity tests; no Docker or network."""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('tool_storage',Path(__file__).with_name('tool-storage.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class Tools(unittest.TestCase):
    def test_immutable_publication_and_tamper(self):
        with tempfile.TemporaryDirectory() as td:
            root=Path(td);m.init(root)
            source=root/'staging/one';source.mkdir();(source/'bin').mkdir();(source/'bin/go').write_text('binary');(source/'bin/go').chmod(0o755)
            target=root/'public/go/linux-amd64/go1.26.9'
            self.assertTrue(m.publish(source,target,{'kind':'go'}));m.verify(root)
            source=root/'staging/two';source.mkdir();(source/'bin').mkdir();(source/'bin/go').write_text('different');(source/'bin/go').chmod(0o755)
            with self.assertRaises(ValueError):m.publish(source,target,{})
            self.assertEqual((target/'bin/go').read_text(),'binary')
            (target/'bin/go').write_text('tampered')
            with self.assertRaises(ValueError):m.verify(root)

    def test_archive_boundaries(self):
        for name,link in [('go/../../outside',None),('go/link','/outside'),('go/link','../../outside')]:
            with self.subTest(name=name,link=link), tempfile.TemporaryDirectory() as td:
                root=Path(td);a=root/'archive.tar'
                with tarfile.open(a,'w') as t:
                    h=tarfile.TarInfo(name)
                    if link:h.type=tarfile.SYMTYPE;h.linkname=link;t.addfile(h)
                    else:h.size=1;t.addfile(h,io.BytesIO(b'x'))
                with self.assertRaises(ValueError):m.extract_go(a,root/'extract')

    def test_external_symlink_not_published(self):
        with tempfile.TemporaryDirectory() as td:
            root=Path(td);m.init(root);source=root/'staging/bad';source.mkdir();(source/'external').symlink_to('/etc/passwd')
            with self.assertRaises(ValueError):m.publish(source,root/'public/bad',{})
            self.assertFalse((root/'public/bad').exists())

if __name__=='__main__':unittest.main()
