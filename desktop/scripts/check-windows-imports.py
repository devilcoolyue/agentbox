#!/usr/bin/env python3
"""Diagnose Windows executable imports before tests, without running the executable."""
import argparse
import ctypes
import json
import os
from pathlib import Path
import struct
import sys


def imports(binary):
    data = binary.read_bytes()
    if len(data) < 64 or data[:2] != b'MZ':
        raise ValueError('Expected a PE executable')
    pe, = struct.unpack_from('<I', data, 60)
    if data[pe:pe + 4] != b'PE\0\0':
        raise ValueError('Invalid PE header')
    sections, = struct.unpack_from('<H', data, pe + 6)
    optional_size, = struct.unpack_from('<H', data, pe + 20)
    optional = pe + 24
    magic, = struct.unpack_from('<H', data, optional)
    if magic not in (0x10b, 0x20b):
        raise ValueError('Unsupported PE optional header')
    wide = magic == 0x20b
    address, size = struct.unpack_from('<II', data, optional + (112 if wide else 96) + 8)
    headers, = struct.unpack_from('<I', data, optional + 60)
    ranges = []
    for index in range(sections):
        virtual_size, virtual, raw_size, raw = struct.unpack_from('<IIII', data, optional + optional_size + index * 40 + 8)
        ranges.append((virtual, min(virtual_size, raw_size) if virtual_size else raw_size, raw))

    def offset(rva):
        if rva < headers:
            return rva
        for start, length, raw in ranges:
            if start <= rva < start + length:
                return raw + rva - start
        raise ValueError('PE address outside file-backed section')

    def name(rva):
        start = offset(rva)
        end = data.index(b'\0', start, min(start + 65536, len(data)))
        return data[start:end].decode('ascii')

    if not address:
        return []
    result = []
    step, flag, fmt = (8, 1 << 63, '<Q') if wide else (4, 1 << 31, '<I')
    for index in range(min(size // 20, 4096)):
        thunk, _, _, module, first = struct.unpack_from('<IIIII', data, offset(address + index * 20))
        if not module:
            break
        functions = []
        for item in range(65536):
            value, = struct.unpack_from(fmt, data, offset((thunk or first) + item * step))
            if not value:
                break
            functions.append(value & 0xffff if value & flag else name(value + 2))
        result.append((name(module), functions))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory', type=Path)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    if sys.platform != 'win32':
        raise SystemExit('Native Windows loader required')
    binaries = sorted(args.directory.glob('agentbox_desktop-*.exe'))
    if not binaries:
        raise SystemExit('No compiled native desktop test executable')
    kernel = ctypes.WinDLL('kernel32', use_last_error=True)
    kernel.GetProcAddress.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
    kernel.GetProcAddress.restype = ctypes.c_void_p
    kernel.GetModuleFileNameW.argtypes = [ctypes.c_void_p, ctypes.c_wchar_p, ctypes.c_uint32]
    kernel.GetModuleFileNameW.restype = ctypes.c_uint32
    kernel.SetErrorMode(0x0001 | 0x0002 | 0x8000)
    rows, missing = [], []
    for binary in binaries:
        for module, functions in imports(binary):
            row = {'binary': binary.name, 'module': module, 'missing_exports': []}
            try:
                library = ctypes.WinDLL(module, use_last_error=True)
                location = ctypes.create_unicode_buffer(32768)
                kernel.GetModuleFileNameW(library._handle, location, len(location))
                row['resolved_path'] = location.value
                for function in functions:
                    symbol = ctypes.c_void_p(function) if isinstance(function, int) else ctypes.cast(ctypes.c_char_p(function.encode('ascii')), ctypes.c_void_p)
                    if not kernel.GetProcAddress(library._handle, symbol):
                        row['missing_exports'].append(function)
                if row['missing_exports']:
                    missing.append(row)
            except OSError as error:
                row['load_error'] = str(error)
                missing.append(row)
            rows.append(row)
    result = {'os': sys.getwindowsversion().platform_version, 'imports': rows, 'failures': missing}
    args.report.write_text(json.dumps(result, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'binaries': [p.name for p in binaries], 'failures': missing}, indent=2))
    if missing:
        raise SystemExit('Windows loader imports are unavailable')


if __name__ == '__main__':
    main()
