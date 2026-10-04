#!/usr/bin/env python3
"""Compile the real hook with full/updater contexts; never execute a Windows installer."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile


def compile_hook(makensis, hook):
    results=[]
    with tempfile.TemporaryDirectory(prefix='agentbox-nsis-hook-') as temporary:
        directory=Path(temporary)
        source=directory/'vendor.exe'
        source.write_bytes(b'synthetic compile-only vendor bytes; not an executable')
        for mode in ('offlineInstaller','downloadBootstrapper'):
            # Match Tauri's ordering: hook definitions are included before the
            # generated constants, and expanded inside the Install section.
            output=directory/(mode+'.exe')
            lines=['Unicode true','!include LogicLib.nsh','!include x64.nsh','!include WordFunc.nsh',
                   '!include "'+str(hook)+'"',
                   '!define WEBVIEW2APPGUID "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"',
                   '!define INSTALLWEBVIEW2MODE "'+mode+'"','!define WEBVIEW2INSTALLERARGS "/silent"',
                   'Name "Agentbox synthetic hook compiler test"','OutFile "'+str(output)+'"',
                   'RequestExecutionLevel user','Var UpdateMode']
            if mode=='offlineInstaller':lines.append('!define WEBVIEW2INSTALLERPATH "'+str(source)+'"')
            # The updater context deliberately lacks WEBVIEW2INSTALLERPATH;
            # compilation must not try to embed or invoke an offline payload.
            lines+=['Section','StrCpy $UpdateMode 0','!insertmacro NSIS_HOOK_PREINSTALL','SectionEnd']
            script=directory/(mode+'.nsi')
            script.write_text('\n'.join(lines)+'\n',encoding='utf-8-sig')
            subprocess.run([str(makensis),'-V2',str(script)],check=True,timeout=60)
            if not output.is_file() or output.stat().st_size==0:raise ValueError('NSIS did not emit the fixture installer')
            results.append({'mode':mode,'compiled':True,'bytes':output.stat().st_size,'executed':False})
    return {'scope':'real_NSIS_compile_only','results':results,'windows_runtime_installation':'not_run'}


def controlflow_fixtures(makensis,hook,execute=False):
    if execute and not (os.name=='nt' and (os.environ.get('GITHUB_ACTIONS')=='true' and os.environ.get('RUNNER_ENVIRONMENT')=='github-hosted' or os.environ.get('AGENTBOX_DISPOSABLE_WINDOWS_GUEST')=='1')):
        raise ValueError('NSIS control-flow execution requires a disposable Windows CI/guest')
    good='154.0.4258.37'
    def case(name,**changes):
        value=dict(name=name,machine=('0.0.0.0',0),user=('',2),after_user=(good,0),exit_code=0,update=0,
                   mode='offlineInstaller',vendor=True,app=True,decision='missing',choice='',errors=0,stage=None)
        value.update(changes)
        value.setdefault('after_machine',value['machine'])
        return value
    cases=[
        case('machine_priority',machine=(good,0),user=('155.0.1.2',0),vendor=False,decision='ready',choice=good),
        case('machine_zero_user_valid',user=(good,0),vendor=False,decision='ready',choice=good,errors=1),
        case('machine_zero_user_install',errors=1),
        case('both_keys_missing',machine=('',2)),
        case('machine_empty_user_zero',machine=('',0),user=('0.0.0.0',0)),
        case('bad_machine_valid_user',machine=('damaged',0),user=(good,0),vendor=False,decision='ready',choice=good),
        case('valid_machine_user_denied',machine=(good,0),user=('',5),vendor=False,decision='ready',choice=good),
        case('machine_access_denied',machine=('',5),vendor=False,app=False,decision='unknown'),
        case('machine_more_data',machine=('',234),vendor=False,app=False,decision='unknown'),
        case('user_wrong_type',user=('',1630),vendor=False,app=False,decision='unknown'),
        case('user_malformed',user=('1.bad.0.0',0),vendor=False,app=False,decision='unknown'),
        case('vendor_failure',exit_code=42,app=False),
        case('unknown_hresult_exit',exit_code=0x80004005,app=False),
        case('launch_failure',exit_code=None,vendor=False,app=False),
        case('missing_after_success',after_user=('0.0.0.0',0),app=False),
        case('malformed_after_success',after_user=('1.invalid.0.0',0),app=False),
        case('overflow_after_success',after_user=('70000.0.0.0',0),app=False),
        case('prepare_failure',stage='prepare',vendor=False,app=False),
        case('extract_failure_with_old_payload',stage='extract',vendor=False,app=False),
        case('update_mode',update=1,vendor=False,errors=1),
        case('dedicated_updater',mode='downloadBootstrapper',vendor=False,errors=1),
    ]
    results=[]
    quote=lambda value:str(value).replace('$','$$').replace('"','$\\"')
    registers=['$'+str(i) for i in range(10)]+['$R'+str(i) for i in range(10)]
    with tempfile.TemporaryDirectory(prefix='agentbox-hook-controlflow-') as temporary:
        root=Path(temporary)
        for c in cases:
            name=c['name'];directory=root/name;directory.mkdir()
            ini=directory/'runtime.ini';evidence=directory/'evidence.ini'
            ini.write_text(''.join('['+hive+']\npv='+c[hive][0]+'\nstatus='+str(c[hive][1])+'\n' for hive in ('machine','user')),encoding='utf-8')
            vendor=directory/'vendor.exe';vendor_marker=directory/'vendor-ran';app_marker=directory/'app-copied'
            vendor_script=directory/'vendor.nsi'
            lines=['Unicode true','SilentInstall silent','RequestExecutionLevel user','Name "Synthetic vendor fixture"',
                   'OutFile "'+quote(vendor)+'"','Section']
            for hive in ('machine','user'):
                value,status=c['after_'+hive]
                lines+=['WriteINIStr "'+quote(ini)+'" "'+hive+'" "pv" "'+quote(value)+'"',
                        'WriteINIStr "'+quote(ini)+'" "'+hive+'" "status" "'+str(status)+'"']
            lines+=['FileOpen $0 "'+quote(vendor_marker)+'" w','FileWrite $0 "executed"','FileClose $0',
                    'SetErrorLevel '+str(c['exit_code'] or 0),'SectionEnd','']
            vendor_script.write_text('\n'.join(lines),encoding='utf-8-sig')
            subprocess.run([str(makensis),'-V2',str(vendor_script)],check=True,timeout=60)
            if c['exit_code'] is None:vendor.write_bytes(b'not an executable; controlled launch failure')
            installer=directory/'installer.exe';script=directory/'installer.nsi'
            lines=['Unicode true','SilentInstall silent','RequestExecutionLevel user',
                   '!include LogicLib.nsh','!include x64.nsh','!include WordFunc.nsh','!include "'+quote(hook)+'"',
                   # Replace only individual hive I/O. Actual dual-hive reader,
                   # status handling, selector and installer hook remain shared.
                   '!macroundef AgentboxReadWebView2Value','!macro AgentboxReadWebView2Value HIVE ROOT KEY FLAGS',
                   'ReadINIStr $0 "'+quote(ini)+'" "${HIVE}" "pv"',
                   'ReadINIStr $1 "'+quote(ini)+'" "${HIVE}" "status"','!macroend',
                   '!define WEBVIEW2APPGUID "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"',
                   '!define INSTALLWEBVIEW2MODE "'+c['mode']+'"','!define WEBVIEW2INSTALLERARGS "/silent"',
                   '!define WEBVIEW2INSTALLERPATH "'+quote(vendor)+'"',
                   'Name "Synthetic Agentbox hook flow"','OutFile "'+quote(installer)+'"','Var UpdateMode']
            if c['stage']=='prepare':
                lines+=['!macroundef AgentboxPrepareWebView2Directory','!macro AgentboxPrepareWebView2Directory','InitPluginsDir','SetErrors','!macroend']
            if c['stage']=='extract':
                lines+=['!macroundef AgentboxExtractWebView2Installer','!macro AgentboxExtractWebView2Installer',
                        'File "/oname=$PLUGINSDIR\\AgentboxWebView2Repair.exe" "${WEBVIEW2INSTALLERPATH}"',
                        'SetErrors','!macroend']
            lines+=['Section','StrCpy $UpdateMode '+str(c['update']),
                    '!insertmacro AgentboxReadWebView2Versions $R7 $7 $R8 $8',
                    '!insertmacro AgentboxSelectWebView2Version $R7 $7 $R8 $8 $5 $6 fixture']
            for key,var in [('machine','$R7'),('machine_status','$7'),('user','$R8'),('user_status','$8'),('choice','$5'),('decision','$6')]:
                lines.append('WriteINIStr "'+quote(evidence)+'" "before" "'+key+'" "'+var+'"')
            for var in registers:lines.append('StrCpy '+var+' "caller-'+var[1:]+'"')
            lines+=['Push "caller-stack"','SetErrors' if c['errors'] else 'ClearErrors','!insertmacro NSIS_HOOK_PREINSTALL',
                    'IfErrors '+('flag_ok flag_bad' if c['errors'] else 'flag_bad flag_ok'),
                    'flag_bad:','SetErrorLevel 91','Quit','flag_ok:']
            for var in registers:lines.append('StrCmp '+var+' "caller-'+var[1:]+'" 0 state_bad')
            lines+=['Pop $9','StrCmp $9 "caller-stack" 0 state_bad',
                    'FileOpen $0 "'+quote(app_marker)+'" w','FileWrite $0 "copied"','FileClose $0','Goto state_done',
                    'state_bad:','SetErrorLevel 92','Quit','state_done:','SectionEnd','']
            script.write_text('\n'.join(lines),encoding='utf-8-sig')
            subprocess.run([str(makensis),'-V2',str(script)],check=True,timeout=60)
            result={'case':name,'compiled':True,'executed':execute,'hives_before':{hive:{'pv':c[hive][0],'status':c[hive][1]} for hive in ('machine','user')}}
            if execute:
                completed=subprocess.run([str(installer),'/S'],timeout=30,check=False)
                if vendor_marker.exists()!=c['vendor'] or app_marker.exists()!=c['app'] or (completed.returncode==0)!=c['app']:
                    raise ValueError('NSIS hook flow/state preservation disagrees with expected outcome: '+name+' exit='+str(completed.returncode))
                import configparser
                actual=configparser.ConfigParser();actual.read(evidence,encoding='utf-8-sig')
                if actual['before']['choice']!=c['choice'] or actual['before']['decision']!=c['decision']:
                    raise ValueError('Actual dual-hive selector disagrees with expected outcome: '+name)
                result.update(vendor_invoked=vendor_marker.exists(),app_copy_reached=app_marker.exists(),exit_code=completed.returncode,
                              selection=dict(actual['before']),caller_state_preserved=bool(c['app']))
            results.append(result)
    return {'scope':'NSIS_controlflow_fixture','results':results,'runtime_registry_modified':False,'real_runtime_installed':False}


def native_registry_probe(makensis,hook,execute=False):
    """Exercise the unmodified RegGetValueW ABI, read-only, on actual Windows."""
    quote=lambda value:str(value).replace('$','$$').replace('"','$\\"')
    with tempfile.TemporaryDirectory(prefix='agentbox-nsis-registry-') as temporary:
        root=Path(temporary);output=root/'reader.exe';evidence=root/'reader.ini';script=root/'reader.nsi'
        evidence.write_text('',encoding='utf-16')
        lines=['Unicode true','SilentInstall silent','RequestExecutionLevel user','!include LogicLib.nsh','!include x64.nsh',
               '!include "'+quote(hook)+'"','!define WEBVIEW2APPGUID "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"',
               'Name "Read-only WebView2 registration probe"','OutFile "'+quote(output)+'"','Section',
               '!insertmacro AgentboxReadWebView2Versions $R7 $7 $R8 $8']
        for key,var in [('machine','$R7'),('machine_status','$7'),('user','$R8'),('user_status','$8')]:
            lines.append('WriteINIStr "'+quote(evidence)+'" "registry" "'+key+'" "'+var+'"')
        lines+=['SectionEnd','']
        script.write_text('\n'.join(lines),encoding='utf-8-sig')
        subprocess.run([str(makensis),'-V2',str(script)],check=True,timeout=60)
        result={'compiled':True,'executed':execute,'runtime_registry_modified':False}
        if execute:
            import configparser
            import winreg
            from webview2_support import RUNTIME_KEY
            subprocess.run([str(output),'/S'],check=True,timeout=30)
            actual=configparser.ConfigParser();actual.read(evidence,encoding='utf-16')
            expected={}
            for label,hive,view in [('machine',winreg.HKEY_LOCAL_MACHINE,winreg.KEY_WOW64_32KEY),('user',winreg.HKEY_CURRENT_USER,winreg.KEY_WOW64_64KEY)]:
                try:
                    with winreg.OpenKey(hive,RUNTIME_KEY,0,winreg.KEY_READ|view) as key:
                        value,kind=winreg.QueryValueEx(key,'pv')
                    if kind!=winreg.REG_SZ:value,status='',1630
                    elif len(value.encode('utf-16-le'))+2>2048:value,status='',234
                    else:status=0
                except OSError as error:value,status='',error.winerror
                expected[label]=value;expected[label+'_status']=str(status)
            result['hives']=dict(actual['registry'])
            if result['hives']!=expected:raise ValueError('NSIS native registry ABI differs from Windows registry API: '+json.dumps({'actual':result['hives'],'expected':expected}))
    return result


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--makensis',type=Path,required=True)
    parser.add_argument('--controlflow-fixtures',action='store_true',help='Compile synthetic INI/vendor control-flow cases too')
    parser.add_argument('--execute-fixtures',action='store_true',help='Actually execute those cases only in a disposable Windows CI/guest')
    parser.add_argument('--report',type=Path)
    args=parser.parse_args()
    hook=Path(__file__).resolve().parents[1]/'src-tauri/windows/webview2-hooks.nsh'
    result=compile_hook(args.makensis.resolve(),hook)
    if args.controlflow_fixtures or args.execute_fixtures:
        result['controlflow']=controlflow_fixtures(args.makensis.resolve(),hook,args.execute_fixtures)
        result['native_registry']=native_registry_probe(args.makensis.resolve(),hook,args.execute_fixtures)
    if args.report:args.report.write_text(json.dumps(result,indent=2)+'\n',encoding='utf-8')
    print(json.dumps(result,indent=2))


if __name__=='__main__':main()
