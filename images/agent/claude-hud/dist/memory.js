import os from 'node:os';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { createDebug } from './debug.js';
const debug = createDebug('memory');
export function parseVmStat(output) {
    const pageSizeMatch = output.match(/page size of (\d+) bytes/);
    if (!pageSizeMatch)
        return null;
    const activeMatch = output.match(/Pages active:\s+(\d+)/);
    const wiredMatch = output.match(/Pages wired down:\s+(\d+)/);
    if (!activeMatch || !wiredMatch)
        return null;
    return {
        pageSize: Number(pageSizeMatch[1]),
        active: Number(activeMatch[1]),
        wired: Number(wiredMatch[1]),
    };
}
export function parseLinuxMeminfo(output) {
    const totalMatch = output.match(/^MemTotal:\s+(\d+)\s+kB/m);
    const availMatch = output.match(/^MemAvailable:\s+(\d+)\s+kB/m);
    if (!totalMatch || !availMatch)
        return null;
    const totalBytes = Number(totalMatch[1]) * 1024;
    const freeBytes = Number(availMatch[1]) * 1024;
    if (!Number.isFinite(totalBytes) || !Number.isFinite(freeBytes)) {
        return null;
    }
    return { totalBytes, freeBytes };
}
const readDefaultMemory = () => ({
    totalBytes: os.totalmem(),
    freeBytes: os.freemem(),
});
const readLinuxMemory = () => {
    try {
        const content = readFileSync('/proc/meminfo', 'utf8');
        return parseLinuxMeminfo(content) ?? readDefaultMemory();
    }
    catch (err) {
        debug('Failed to read /proc/meminfo:', err instanceof Error ? err.message : err);
        return readDefaultMemory();
    }
};
const readMacOSMemory = () => {
    try {
        const output = execFileSync('/usr/bin/vm_stat', {
            encoding: 'utf8',
            timeout: 5000,
        });
        const parsed = parseVmStat(output);
        if (!parsed)
            return readDefaultMemory();
        const totalBytes = os.totalmem();
        const usedBytes = (parsed.active + parsed.wired) * parsed.pageSize;
        return { totalBytes, freeBytes: totalBytes - usedBytes };
    }
    catch (err) {
        debug('Failed to run vm_stat:', err instanceof Error ? err.message : err);
        return readDefaultMemory();
    }
};
let readMemory = process.platform === 'darwin' ? readMacOSMemory :
    process.platform === 'linux' ? readLinuxMemory :
        readDefaultMemory;
export async function getMemoryUsage() {
    try {
        const { totalBytes, freeBytes } = readMemory();
        if (!Number.isFinite(totalBytes) || totalBytes <= 0) {
            return null;
        }
        const safeFreeBytes = Number.isFinite(freeBytes)
            ? Math.min(Math.max(freeBytes, 0), totalBytes)
            : 0;
        const usedBytes = totalBytes - safeFreeBytes;
        const usedPercent = Math.round((usedBytes / totalBytes) * 100);
        return {
            totalBytes,
            usedBytes,
            freeBytes: safeFreeBytes,
            usedPercent: Math.min(Math.max(usedPercent, 0), 100),
        };
    }
    catch (err) {
        debug('Failed to get memory usage:', err instanceof Error ? err.message : err);
        return null;
    }
}
export function formatBytes(bytes) {
    if (!Number.isFinite(bytes) || bytes <= 0) {
        return '0 B';
    }
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let value = bytes;
    let unitIndex = 0;
    while (value >= 1024 && unitIndex < units.length - 1) {
        value /= 1024;
        unitIndex += 1;
    }
    const fractionDigits = value >= 10 || unitIndex === 0 ? 0 : 1;
    return `${value.toFixed(fractionDigits)} ${units[unitIndex]}`;
}
export function _setMemoryReaderForTests(reader) {
    readMemory = reader ?? (process.platform === 'darwin' ? readMacOSMemory :
        process.platform === 'linux' ? readLinuxMemory :
            readDefaultMemory);
}
//# sourceMappingURL=memory.js.map