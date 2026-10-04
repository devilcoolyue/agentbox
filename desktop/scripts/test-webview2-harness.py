#!/usr/bin/env python3
"""Portable negative-contract tests; these do not install or emulate WebView2."""
import copy
import importlib.util
import json
from pathlib import Path
import struct
import subprocess
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
        config=json.loads((root/'src-tauri/tauri.conf.json').read_text(encoding='utf-8'))
        self.assertEqual(config['bundle']['windows']['webviewInstallMode'],{'type':'offlineInstaller','silent':True})
        self.assertEqual(config['bundle']['windows']['nsis']['installerHooks'],'windows/webview2-hooks.nsh')
        self.assertEqual(config['bundle']['resources']['../vendor-notices/'],'third-party/vendor/')
        text=(root/'vendor-notices/Microsoft-WebView2.txt').read_text(encoding='utf-8')
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

    def test_runtime_directory_read_failure_is_not_absence(self):
        with patch.object(support.os,'scandir',side_effect=PermissionError('denied')):
            with self.assertRaises(PermissionError):support.runtime_files(Path('unreadable'))
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);version=root/'154.0.1.2';version.mkdir()
            binary=version/'msedgewebview2.exe';binary.write_bytes(b'presence')
            self.assertEqual(support.runtime_files(root),[str(binary)])

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
        self.assertIsNone(package.archive_entry(entry.replace('123',''),support.EMBEDDED_FILENAME)[1])
        for invalid in ('',entry+'\n'+entry,entry.replace('123','0'),entry.replace('123',str(support.MAX_PACKAGE+1))):
            with self.assertRaises(ValueError):package.archive_entry(invalid,support.EMBEDDED_FILENAME)

    def test_publisher_is_verified_not_just_signed(self):
        with patch.object(support,'powershell',return_value={'status':'Valid','subject':'CN=Other, O=Other'}):
            with self.assertRaises(ValueError):support.authenticode(Path('unused'))
        with patch.object(support,'powershell',return_value={'status':'HashMismatch','subject':'O=Microsoft Corporation'}):
            with self.assertRaises(ValueError):support.authenticode(Path('unused'))
        with patch.object(support,'powershell',return_value={'status':'Valid','subject':'CN=Microsoft Corporation, O=Microsoft Corporation, C=US'}):
            self.assertEqual(support.authenticode(Path('unused'))['status'],'Valid')

    def test_powershell_prefers_available_pwsh_and_preserves_its_modules(self):
        host=r'C:\Program Files\PowerShell\7\pwsh.exe'
        result=subprocess.CompletedProcess([],0,'\ufeff{"status":"Valid"}','')
        with patch.object(support.os,'name','nt'), patch.dict(support.os.environ,{'PSModulePath':'pwsh-modules'},clear=True), \
                patch.object(support.shutil,'which',return_value=host) as locate, \
                patch.object(support.subprocess,'run',return_value=result) as execute:
            self.assertEqual(support.powershell('verification',{'FIXTURE_VALUE':'中文'}),{'status':'Valid'})
            locate.assert_called_once_with('pwsh')
            self.assertEqual(execute.call_args.args[0][0],host)
            self.assertEqual({key.upper():value for key,value in execute.call_args.kwargs['env'].items()},
                             {'PSMODULEPATH':'pwsh-modules','FIXTURE_VALUE':'中文'})

    def test_windows_powershell_fallback_initializes_its_own_module_path(self):
        host=r'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe'
        result=subprocess.CompletedProcess([],0,'[]','')
        with patch.object(support.os,'name','nt'), patch.dict(support.os.environ,{'PSModulePath':'incompatible','KEEP':'value'},clear=True), \
                patch.object(support.shutil,'which',side_effect=[None,host]) as locate, \
                patch.object(support.subprocess,'run',return_value=result) as execute:
            self.assertEqual(support.powershell('verification',{'psmodulepath':'also incompatible'}),[])
            self.assertEqual([call.args[0] for call in locate.call_args_list],['pwsh','powershell.exe'])
            self.assertEqual(execute.call_args.args[0][0],host)
            self.assertEqual(execute.call_args.kwargs['env'],{'KEEP':'value'})

    def test_powershell_failure_keeps_bounded_utf8_diagnostic_without_retry(self):
        diagnostic='discarded prefix '+'中'*5000+' module loading failed'
        result=subprocess.CompletedProcess([],7,'{"status":"Valid"}',diagnostic)
        with patch.object(support.os,'name','nt'), patch.object(support.shutil,'which',return_value='pwsh.exe'), \
                patch.object(support.subprocess,'run',return_value=result) as execute:
            with self.assertRaises(ValueError) as raised:
                support.powershell('verification')
            message=str(raised.exception)
            self.assertIn('exit_code=7',message)
            self.assertIn('module loading failed',message)
            self.assertNotIn('discarded prefix',message)
            self.assertLessEqual(len(message.split('stderr=',1)[1].encode('utf-8')),4096)
            execute.assert_called_once()

    def test_powershell_timeout_missing_host_and_invalid_json_fail_closed(self):
        with patch.object(support.os,'name','nt'), patch.object(support.shutil,'which',return_value='pwsh.exe'), \
                patch.object(support.subprocess,'run',side_effect=subprocess.TimeoutExpired('fixture',45,stderr='超时详情'.encode('utf-8'))):
            with self.assertRaisesRegex(ValueError,'timed out.*stderr=超时详情'):
                support.powershell('verification')
        with patch.object(support.os,'name','nt'), patch.object(support.shutil,'which',return_value=None), \
                patch.object(support.subprocess,'run') as execute:
            with self.assertRaisesRegex(ValueError,'No PowerShell host'):
                support.powershell('verification')
            execute.assert_not_called()
        for output in ('not json','{"status":"Valid"} trailing','NaN'):
            with self.subTest(output=output), patch.object(support.os,'name','nt'), \
                    patch.object(support.shutil,'which',return_value='pwsh.exe'), \
                    patch.object(support.subprocess,'run',return_value=subprocess.CompletedProcess([],0,output,'parse detail')):
                with self.assertRaisesRegex(ValueError,'invalid JSON; exit_code=0; stderr=parse detail'):
                    support.powershell('verification')

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
                   'target':'x86_64-pc-windows-msvc','source_bytes_match':True,'proprietary_notice_present':True,'zero_version_repair_payload_matches':True,'microsoft_signature':{'status':'Valid'}}
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
        self.assertIs(offline.install_command,support.install_command)
        self.assertTrue(command.endswith('/D=C:\\guest dir\\Agentbox 中文'))
        self.assertIn('"C:\\installer dir\\Agentbox.exe" /S',command)
        for character in ('"','\r','\n','\0'):
            for package,directory in [('installer.exe','bad'+character+'path'),('bad'+character+'installer.exe','directory')]:
                with self.assertRaises(ValueError):support.install_command(package,directory)


if __name__=='__main__':unittest.main()
