#!/usr/bin/env python3
"""Portable negative-contract tests; these do not install or emulate WebView2."""
import copy
import importlib.util
import json
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch

import webview2_support as support


def load(name):
    specification=importlib.util.spec_from_file_location(name,Path(__file__).with_name(name+'.py'))
    module=importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


package=load('test-webview2-package')
offline=load('test-webview2-offline')


class WebView2HarnessTests(unittest.TestCase):
    def inventory(self):
        return {'registry':[{'hive':hive,'view':view,'exists':False,'values':{}} for hive in ('HKCU','HKLM') for view in ('32','64')],
                'loader':{'hresult':'0x80070002','version':None},'runtime_or_preview_files':[]}

    def test_declared_offline_policy_and_notice_are_consistent(self):
        root=Path(__file__).resolve().parents[1]
        config=json.loads((root/'src-tauri/tauri.conf.json').read_text())
        self.assertEqual(config['bundle']['windows']['webviewInstallMode'],{'type':'offlineInstaller','silent':True})
        self.assertEqual(config['bundle']['resources']['../vendor-notices/'],'third-party/vendor/')
        text=(root/'vendor-notices/Microsoft-WebView2.txt').read_text()
        self.assertIn('Microsoft proprietary software',text)
        self.assertIn('not uninstall',text)

    def test_registry_removal_alone_cannot_fabricate_runtime_absence(self):
        missing=self.inventory()
        self.assertTrue(support.runtime_is_absent(missing))
        for mutation in ('registry','loader','files'):
            present=copy.deepcopy(missing)
            if mutation=='registry':present['registry'][0]['values']['pv']='150.0.1.2'
            elif mutation=='loader':present['loader']={'hresult':'0x00000000','version':'150.0.1.2 dev'}
            else:present['runtime_or_preview_files']=['C:/runtime/msedgewebview2.exe']
            self.assertFalse(support.runtime_is_absent(present),mutation)
        missing['registry'][0]['values']['pv']='malformed'
        with self.assertRaises(ValueError):support.runtime_is_absent(missing)
        missing=self.inventory();missing['registry'].pop()
        with self.assertRaises(ValueError):support.runtime_is_absent(missing)

    def test_network_evidence_must_be_complete_and_disconnected(self):
        loopback={'name':'Loopback','type':'Loopback','status':'Up'}
        support.require_offline([loopback,{'name':'Ethernet','type':'Ethernet','status':'Down'}])
        for invalid in ([],{},[{}],[loopback,{'name':'Ethernet','type':'Ethernet','status':'Up'}],
                        [loopback,{'name':'adapter','type':'Unknown','status':'Down'}]):
            with self.assertRaises(ValueError):support.require_offline(invalid)

    def test_only_full_x64_offline_nsis_source_is_accepted(self):
        good='!define ARCH "x64"\n!define INSTALLWEBVIEW2MODE "offlineInstaller"\n!define WEBVIEW2INSTALLERPATH "C:\\cache\\x64\\12345678-1234-1234-1234-123456789abc\\MicrosoftEdgeWebView2RuntimeInstallerX64.exe"\n'
        source,url=support.nsis_source(good)
        self.assertTrue(url.startswith(support.SOURCE_PREFIX))
        self.assertTrue(source.endswith(support.VENDOR_FILENAME))
        for invalid in (good.replace('"x64"','"x86"'),good.replace('offlineInstaller','downloadBootstrapper'),
                        good.replace('RuntimeInstallerX64','RuntimeInstallerX86'),good.replace('123456789abc','path-traversal'),
                        good.replace('C:\\cache','$TEMP')):
            with self.assertRaises(ValueError):support.nsis_source(invalid)

    def test_archive_payload_is_unique_bounded_and_exact(self):
        entry=f'Path = $TEMP\\{support.EMBEDDED_FILENAME}\nSize = 123\n'
        self.assertEqual(package.archive_entry(entry,support.EMBEDDED_FILENAME)[1],123)
        for invalid in ('',entry+'\n'+entry,entry.replace('123','0'),entry.replace('123',str(support.MAX_PACKAGE+1))):
            with self.assertRaises(ValueError):package.archive_entry(invalid,support.EMBEDDED_FILENAME)

    def test_publisher_is_verified_not_just_signed(self):
        with patch.object(support,'powershell',return_value={'status':'Valid','subject':'CN=Other, O=Other'}):
            with self.assertRaises(ValueError):support.authenticode(Path('unused'))
        with patch.object(support,'powershell',return_value={'status':'HashMismatch','subject':'O=Microsoft Corporation'}):
            with self.assertRaises(ValueError):support.authenticode(Path('unused'))
        with patch.object(support,'powershell',return_value={'status':'Valid','subject':'CN=Microsoft Corporation, O=Microsoft Corporation, C=US'}):
            self.assertEqual(support.authenticode(Path('unused'))['status'],'Valid')

    def test_microsoft_source_mismatch_fails_even_with_valid_metadata(self):
        class Response:
            status=200
            def __enter__(self):return self
            def __exit__(self,*args):pass
            def geturl(self):return support.SOURCE_PREFIX+'guid/'+support.VENDOR_FILENAME
            def read(self,_):return b''
        with patch.object(package.urllib.request,'urlopen',return_value=Response()):
            with self.assertRaises(ValueError):package.verify_microsoft_source(Response().geturl(),'a'*64,123)

    def test_package_audit_is_bound_to_actual_artifact_not_config(self):
        with tempfile.TemporaryDirectory() as directory:
            artifact=Path(directory)/'candidate.exe';artifact.write_bytes(b'candidate')
            audit={'status':'passed','scope':'actual_full_nsis_payload_audit','package':support.bounded_file(artifact),
                   'target':'x86_64-pc-windows-msvc','source_bytes_match':True,'proprietary_notice_present':True,'microsoft_signature':{'status':'Valid'}}
            offline.validate_package_audit(artifact,audit)
            artifact.write_bytes(b'changed')
            with self.assertRaises(ValueError):offline.validate_package_audit(artifact,audit)
            audit['scope']='configuration_checked'
            with self.assertRaises(ValueError):offline.validate_package_audit(artifact,audit)

    def test_pe_architecture_is_read_from_actual_payload(self):
        with tempfile.TemporaryDirectory() as directory:
            file=Path(directory)/'app.exe'
            head=bytearray(64);head[:2]=b'MZ';struct.pack_into('<I',head,60,64)
            file.write_bytes(head+b'PE\0\0'+struct.pack('<H',0x8664))
            self.assertEqual(support.pe_machine(file),0x8664)
            file.write_bytes(b'not a PE')
            with self.assertRaises(ValueError):support.pe_machine(file)

    def test_nsis_unicode_destination_is_last_unquoted_remainder(self):
        command=offline.install_command(r'C:\installer dir\Agentbox.exe',r'C:\guest dir\Agentbox 中文')
        self.assertTrue(command.endswith('/D=C:\\guest dir\\Agentbox 中文'))
        self.assertIn('"C:\\installer dir\\Agentbox.exe" /S',command)
        with self.assertRaises(ValueError):offline.install_command('installer.exe','bad\npath')


if __name__=='__main__':unittest.main()
