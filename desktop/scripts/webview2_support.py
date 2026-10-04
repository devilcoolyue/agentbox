"""Strict read-only primitives shared by the Windows WebView2 acceptance tools."""
import ctypes
import hashlib
import json
import os
from pathlib import Path, PureWindowsPath
import re
import shutil
import struct
import subprocess
import tomllib
import uuid

MAX_PACKAGE = 512 * 1024 * 1024
RUNTIME_GUID = '{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}'
RUNTIME_KEY = 'Software\\Microsoft\\EdgeUpdate\\Clients\\' + RUNTIME_GUID
UNINSTALL_KEY = r'Software\Microsoft\Windows\CurrentVersion\Uninstall\Agentbox'
VENDOR_FILENAME = 'MicrosoftEdgeWebView2RuntimeInstallerX64.exe'
EMBEDDED_FILENAME = 'MicrosoftEdgeWebView2RuntimeInstaller.exe'
REPAIR_FILENAME = 'AgentboxWebView2Repair.exe'
SOURCE_PREFIX = 'https://msedge.sf.dl.delivery.mp.microsoft.com/filestreamingservice/files/'
SELECTION_URL = 'https://go.microsoft.com/fwlink/?linkid=2124701'


def install_command(package, directory):
    if any(character in str(path) for path in (package, directory) for character in ('"', '\r', '\n', '\0')):
        raise ValueError('Invalid NSIS path')
    # /D is the final unquoted remainder, including spaces. Pass this string
    # directly to Windows CreateProcess with shell=False, never as an argv list.
    return subprocess.list2cmdline([str(package), '/S']) + ' /D=' + str(directory)


def digest(path):
    with Path(path).open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def bounded_file(path, maximum=MAX_PACKAGE):
    path = Path(path)
    if path.is_symlink() or not path.is_file() or not 0 < path.stat().st_size <= maximum:
        raise ValueError('Missing, linked, empty, or oversized artifact: ' + path.name)
    return {'bytes': path.stat().st_size, 'sha256': digest(path)}


def pe_machine(path):
    with Path(path).open('rb') as source:
        head = source.read(64)
        if len(head) != 64 or head[:2] != b'MZ':
            raise ValueError('Artifact is not a Windows PE file')
        offset = struct.unpack_from('<I', head, 60)[0]
        if offset < 64 or offset > 16 * 1024 * 1024:
            raise ValueError('Invalid PE header offset')
        source.seek(offset)
        header = source.read(6)
        if len(header) != 6 or header[:4] != b'PE\0\0':
            raise ValueError('Invalid PE signature')
        return struct.unpack_from('<H', header, 4)[0]


def nsis_source(script):
    definitions = dict(re.findall(r'^!define\s+(\w+)\s+"(.*)"\s*$', script, re.M))
    if definitions.get('ARCH') != 'x64' or definitions.get('INSTALLWEBVIEW2MODE') != 'offlineInstaller':
        raise ValueError('Require the x64 full NSIS installer with offlineInstaller; updater-only NSIS is different')
    source = definitions.get('WEBVIEW2INSTALLERPATH', '').replace('$$', '$')
    if '$' in source or '\x00' in source:
        raise ValueError('Unresolved NSIS variable in vendor source path')
    path = PureWindowsPath(source)
    if path.name != VENDOR_FILENAME or path.parent.parent.name != 'x64':
        raise ValueError('Vendor source is not Tauri\'s x64 standalone installer cache')
    try:
        guid = str(uuid.UUID(path.parent.name))
    except ValueError as error:
        raise ValueError('Invalid vendor download GUID') from error
    if guid != path.parent.name.lower():
        raise ValueError('Non-canonical vendor download GUID')
    return source, SOURCE_PREFIX + path.parent.name + '/' + VENDOR_FILENAME


def powershell(script, values=None):
    if os.name != 'nt':
        raise ValueError('This check requires actual Windows')
    host = shutil.which('pwsh')
    environment = dict(os.environ)
    environment.update(values or {})
    if not host:
        host = shutil.which('powershell.exe')
        # PowerShell 7's module directories can contain assemblies that Windows
        # PowerShell 5 cannot load. Let the fallback host initialize its own path.
        environment = {key: value for key, value in environment.items() if key.upper() != 'PSMODULEPATH'}
    if not host:
        raise ValueError('No PowerShell host is available for Windows verification')

    def failure(reason, code, stderr):
        if isinstance(stderr, bytes):
            stderr = stderr.decode('utf-8', errors='replace')
        # Keep a bounded UTF-8 diagnostic, not the command or inherited environment.
        detail = (stderr or '').encode('utf-8', errors='replace')[-4096:].decode('utf-8', errors='ignore').strip()
        return ValueError(f'{PureWindowsPath(host).name}: {reason}; exit_code={code}; stderr={detail or "<empty>"}')

    try:
        result = subprocess.run([host, '-NoProfile', '-NonInteractive', '-Command',
                                 "$ErrorActionPreference='Stop'; [Console]::OutputEncoding=[Text.UTF8Encoding]::new(); " + script],
                                env=environment, capture_output=True, text=True, encoding='utf-8', errors='replace', timeout=45)
    except subprocess.TimeoutExpired as error:
        raise failure('verification timed out', 'unavailable', error.stderr) from error
    except OSError as error:
        raise failure('verification host could not start', 'unavailable', str(error)) from error
    if result.returncode != 0:
        raise failure('verification failed', result.returncode, result.stderr)
    try:
        def reject_constant(value):
            raise ValueError('Non-JSON numeric constant: ' + value)
        return json.loads(result.stdout.lstrip('\ufeff'), parse_constant=reject_constant)
    except ValueError as error:
        raise failure('verification returned invalid JSON', result.returncode, result.stderr) from error


def authenticode(path):
    result = powershell("$s=Get-AuthenticodeSignature -LiteralPath $env:AGENTBOX_WEBVIEW_AUDIT_FILE; "
                        "$v=(Get-Item -LiteralPath $env:AGENTBOX_WEBVIEW_AUDIT_FILE).VersionInfo; "
                        "@{status=[string]$s.Status; subject=$s.SignerCertificate.Subject; "
                        "thumbprint=$s.SignerCertificate.Thumbprint; product_version=$v.ProductVersion} | ConvertTo-Json -Compress",
                        {'AGENTBOX_WEBVIEW_AUDIT_FILE': str(Path(path).resolve())})
    if result.get('status') != 'Valid' or not re.search(r'(?:^|,\s*)O=Microsoft Corporation(?:,|$)', result.get('subject') or ''):
        raise ValueError('Embedded WebView2 installer must have a valid Microsoft Authenticode signature')
    return result


def registry_values(key, names):
    import winreg
    records = []
    for hive_name, hive in [('HKCU', winreg.HKEY_CURRENT_USER), ('HKLM', winreg.HKEY_LOCAL_MACHINE)]:
        for view_name, view in [('32', winreg.KEY_WOW64_32KEY), ('64', winreg.KEY_WOW64_64KEY)]:
            item = {'hive': hive_name, 'view': view_name, 'key': key, 'exists': False, 'values': {}}
            try:
                with winreg.OpenKey(hive, key, 0, winreg.KEY_READ | view) as opened:
                    item['exists'] = True
                    for name in names:
                        try:
                            value, kind = winreg.QueryValueEx(opened, name)
                            if kind != winreg.REG_SZ or not isinstance(value, str):
                                raise ValueError('Registry field has an unexpected type: ' + name)
                            item['values'][name] = value
                        except FileNotFoundError:
                            pass
            except FileNotFoundError:
                pass
            # Permission, I/O and type errors are not evidence of absence.
            records.append(item)
    return records


def loader_version(loader):
    if os.name != 'nt' or ctypes.sizeof(ctypes.c_void_p) != 8 or pe_machine(loader) != 0x8664:
        raise ValueError('Require actual Windows x64 Python and the x64 WebView2Loader DLL')
    if any(key.upper().startswith('WEBVIEW2_') for key in os.environ):
        raise ValueError('WebView2 environment overrides invalidate runtime detection')
    library = ctypes.WinDLL(str(Path(loader).resolve()), winmode=0x900)
    function = library.GetAvailableCoreWebView2BrowserVersionString
    function.argtypes = [ctypes.c_wchar_p, ctypes.POINTER(ctypes.c_void_p)]
    function.restype = ctypes.c_int32
    pointer = ctypes.c_void_p()
    result = function(None, ctypes.byref(pointer)) & 0xffffffff
    version = None
    if pointer.value:
        try:
            version = ctypes.wstring_at(pointer)
        finally:
            ole32 = ctypes.WinDLL('ole32', winmode=0x800)
            ole32.CoTaskMemFree.argtypes = [ctypes.c_void_p]
            ole32.CoTaskMemFree(pointer)
    if result not in (0, 0x80070002) or result == 0 and not version or result != 0 and version:
        raise ValueError('WebView2 loader detection failed; do not treat it as an absent runtime')
    return {'hresult': f'0x{result:08x}', 'version': version, 'loader_sha256': digest(loader)}


def runtime_inventory(loader):
    records = registry_values(RUNTIME_KEY, ['pv', 'name', 'location'])
    files = []
    for variable in ('ProgramFiles', 'ProgramFiles(x86)', 'LOCALAPPDATA'):
        root = os.environ.get(variable)
        if not root:
            raise ValueError('Missing Windows system directory variable: ' + variable)
        for product in ('EdgeWebView', 'Edge Beta', 'Edge Dev', 'Edge SxS'):
            directory = Path(root) / 'Microsoft' / product / 'Application'
            files.extend(runtime_files(directory))
    return {'registry': records, 'loader': loader_version(loader), 'runtime_or_preview_files': sorted(set(files))}


def runtime_files(directory):
    # Path.glob/is_file can suppress an inaccessible directory and make it look
    # empty. Only an actual not-found result is evidence of absence here.
    try:
        with os.scandir(directory) as scan:
            entries = []
            for entry in scan:
                if len(entries) >= 256:
                    raise ValueError('Unexpectedly large Runtime directory inventory')
                entries.append(entry)
    except FileNotFoundError:
        return []
    directories = [Path(directory)] + [Path(entry.path) for entry in entries if entry.is_dir()]
    found = []
    for folder in directories:
        for name in ('msedgewebview2.exe', 'msedge.exe'):
            path = folder / name
            try:
                os.lstat(path)
            except FileNotFoundError:
                continue
            found.append(str(path))
    return found


def runtime_is_absent(inventory):
    records = inventory['registry']
    if len(records) != 4 or {(row['hive'],row['view']) for row in records} != {('HKCU','32'),('HKCU','64'),('HKLM','32'),('HKLM','64')}:
        raise ValueError('Incomplete Runtime registry inventory is not absence')
    for record in records:
        version = record['values'].get('pv', '')
        if version and not re.fullmatch(r'\d+\.\d+\.\d+\.\d+', version):
            raise ValueError('Unrecognized runtime registry version is not absence')
        if version and any(int(part) for part in version.split('.')):
            return False
    return (inventory['loader']['hresult'] == '0x80070002'
            and inventory['loader']['version'] is None
            and not inventory['runtime_or_preview_files'])


def network_inventory():
    return powershell("$items=@([Net.NetworkInformation.NetworkInterface]::GetAllNetworkInterfaces() | ForEach-Object { "
                      "@{name=$_.Name; type=[string]$_.NetworkInterfaceType; status=[string]$_.OperationalStatus} }); "
                      "ConvertTo-Json -InputObject $items -Compress")


def require_offline(network):
    if not isinstance(network, list) or not network or any(not isinstance(item, dict) or not {'name','type','status'} <= item.keys() for item in network):
        raise ValueError('Invalid network inventory')
    if any(item['status'] not in ('Up','Down','Dormant','NotPresent','LowerLayerDown') or item['type'] in ('Unknown','') for item in network):
        raise ValueError('Unknown interface state is not evidence of an offline guest')
    if any(item['type'] != 'Loopback' and item['status'] == 'Up' for item in network):
        raise ValueError('Guest has an active network interface; host/runner networking will not be changed')


def sdk_loader():
    desktop = Path(__file__).resolve().parents[1]
    lock = tomllib.loads((desktop / 'src-tauri/Cargo.lock').read_text(encoding='utf-8'))
    versions = [package['version'] for package in lock['package'] if package['name'] == 'webview2-com-sys']
    if len(versions) != 1:
        raise ValueError('Require one locked WebView2 SDK version or supply an explicit reviewed loader')
    registry = Path(os.environ.get('CARGO_HOME', Path.home() / '.cargo')) / 'registry/src'
    matches = list(registry.glob(f'*/webview2-com-sys-{versions[0]}/x64/WebView2Loader.dll'))
    if len(matches) != 1:
        raise ValueError('Cannot locate the locked x64 SDK loader; supply --loader explicitly')
    return matches[0]


def write_report(path, report):
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    Path(path).write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
