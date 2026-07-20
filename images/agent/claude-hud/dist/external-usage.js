import * as fs from 'node:fs';
import * as path from 'node:path';
import { createDebug } from './debug.js';
import { sanitizeDisplayText } from './utils/sanitize.js';
const debug = createDebug('external-usage');
const MAX_BALANCE_LABEL_LENGTH = 50;
export const EXTERNAL_USAGE_WRITE_THROTTLE_MS = 30_000;
const fsDeps = {
    chmodSync: fs.chmodSync,
    existsSync: fs.existsSync,
    readFileSync: fs.readFileSync,
    renameSync: fs.renameSync,
    rmSync: fs.rmSync,
    statSync: fs.statSync,
    writeFileSync: fs.writeFileSync,
};
function parseUsagePercent(value) {
    if (typeof value !== 'number' || !Number.isFinite(value)) {
        return null;
    }
    return Math.round(Math.min(100, Math.max(0, value)));
}
function sanitizeBalanceLabel(value) {
    if (typeof value !== 'string') {
        return null;
    }
    const sanitized = sanitizeDisplayText(value).trim();
    if (!sanitized) {
        return null;
    }
    if (sanitized.length <= MAX_BALANCE_LABEL_LENGTH) {
        return sanitized;
    }
    return `${sanitized.slice(0, MAX_BALANCE_LABEL_LENGTH - 3)}...`;
}
function parseDateValue(value) {
    if (typeof value === 'number') {
        if (!Number.isFinite(value) || value <= 0) {
            return null;
        }
        const millis = value > 1e12 ? value : value * 1000;
        const date = new Date(millis);
        return Number.isNaN(date.getTime()) ? null : date;
    }
    if (typeof value === 'string' && value.trim()) {
        const date = new Date(value);
        return Number.isNaN(date.getTime()) ? null : date;
    }
    return null;
}
function parseUpdatedAt(value) {
    const date = parseDateValue(value);
    return date ? date.getTime() : null;
}
function snapshotFromUsage(usage, now) {
    return {
        updated_at: new Date(now).toISOString(),
        five_hour: {
            used_percentage: usage.fiveHour,
            resets_at: usage.fiveHourResetAt?.toISOString() ?? null,
        },
        seven_day: {
            used_percentage: usage.sevenDay,
            resets_at: usage.sevenDayResetAt?.toISOString() ?? null,
        },
    };
}
function comparableSnapshot(snapshot) {
    if (!snapshot || typeof snapshot !== 'object' || Array.isArray(snapshot)) {
        return null;
    }
    const topLevelKeys = Object.keys(snapshot);
    if (topLevelKeys.length !== 3
        || !topLevelKeys.includes('updated_at')
        || !topLevelKeys.includes('five_hour')
        || !topLevelKeys.includes('seven_day')) {
        return null;
    }
    const value = snapshot;
    if (parseUpdatedAt(value.updated_at) === null) {
        return null;
    }
    const fiveHour = comparableWindow(value.five_hour);
    const sevenDay = comparableWindow(value.seven_day);
    if (fiveHour === null || sevenDay === null) {
        return null;
    }
    return {
        five_hour: fiveHour,
        seven_day: sevenDay,
    };
}
function comparableWindow(value) {
    if (!value || typeof value !== 'object' || Array.isArray(value)) {
        return null;
    }
    const keys = Object.keys(value);
    if (keys.length !== 2
        || !keys.includes('used_percentage')
        || !keys.includes('resets_at')) {
        return null;
    }
    const window = value;
    const usedPercentage = parseUsagePercent(window.used_percentage);
    if (window.used_percentage !== null && usedPercentage === null) {
        return null;
    }
    const resetAt = parseDateValue(window.resets_at);
    if (window.resets_at !== null && resetAt === null) {
        return null;
    }
    return {
        used_percentage: usedPercentage,
        resets_at: resetAt?.toISOString() ?? null,
    };
}
function shouldWriteSnapshot(snapshotPath, nextSnapshot, now, deps) {
    try {
        if (!deps.existsSync(snapshotPath)) {
            return true;
        }
        const stats = deps.statSync(snapshotPath);
        if (now - stats.mtimeMs > EXTERNAL_USAGE_WRITE_THROTTLE_MS) {
            return true;
        }
        const current = JSON.parse(deps.readFileSync(snapshotPath, 'utf8'));
        return JSON.stringify(comparableSnapshot(current)) !== JSON.stringify(comparableSnapshot(nextSnapshot));
    }
    catch (err) {
        debug('Failed to compare snapshot (will write):', err instanceof Error ? err.message : err);
        return true;
    }
}
function resolveSnapshotWritePath(snapshotPath) {
    if (!path.isAbsolute(snapshotPath)) {
        return null;
    }
    const parsed = path.parse(snapshotPath);
    if (!parsed.base || parsed.ext.toLowerCase() !== '.json') {
        return null;
    }
    return path.normalize(snapshotPath);
}
function directoryExists(dir, deps) {
    try {
        return deps.statSync(dir).isDirectory();
    }
    catch (err) {
        debug('Directory check failed for %s:', dir, err instanceof Error ? err.message : err);
        return false;
    }
}
export function writeExternalUsageSnapshot(config, usage, now = Date.now(), deps = fsDeps) {
    const snapshotPath = resolveSnapshotWritePath(config.display.externalUsageWritePath);
    if (!snapshotPath
        || !usage
        || (usage.fiveHour === null && usage.sevenDay === null)) {
        return false;
    }
    const snapshot = snapshotFromUsage(usage, now);
    const dir = path.dirname(snapshotPath);
    const base = path.basename(snapshotPath);
    const tmpPath = path.join(dir, `.${base}.${process.pid}.${now}.${Math.random().toString(36).slice(2)}.tmp`);
    try {
        if (!directoryExists(dir, deps)) {
            return false;
        }
        if (!shouldWriteSnapshot(snapshotPath, snapshot, now, deps)) {
            return false;
        }
        deps.writeFileSync(tmpPath, `${JSON.stringify(snapshot, null, 2)}\n`, {
            encoding: 'utf8',
            mode: 0o600,
            flag: 'wx',
        });
        deps.renameSync(tmpPath, snapshotPath);
        deps.chmodSync(snapshotPath, 0o600);
        return true;
    }
    catch (err) {
        debug('Failed to write usage snapshot:', err instanceof Error ? err.message : err);
        try {
            deps.rmSync(tmpPath, { force: true });
        }
        catch (cleanupErr) {
            debug('Failed to clean up temp file:', cleanupErr instanceof Error ? cleanupErr.message : cleanupErr);
        }
        return false;
    }
}
export function getUsageFromExternalSnapshot(config, now = Date.now()) {
    const snapshotPath = config.display.externalUsagePath;
    if (!snapshotPath || !path.isAbsolute(snapshotPath)) {
        return null;
    }
    try {
        const raw = fs.readFileSync(snapshotPath, 'utf8');
        const parsed = JSON.parse(raw);
        const updatedAt = parseUpdatedAt(parsed.updated_at);
        if (updatedAt === null) {
            return null;
        }
        const freshnessMs = config.display.externalUsageFreshnessMs;
        if (now - updatedAt > freshnessMs) {
            return null;
        }
        const fiveHour = parseUsagePercent(parsed.five_hour?.used_percentage);
        const sevenDay = parseUsagePercent(parsed.seven_day?.used_percentage);
        const balanceLabel = sanitizeBalanceLabel(parsed.balance_label);
        if (fiveHour === null && sevenDay === null && balanceLabel === null) {
            return null;
        }
        const fiveHourResetAt = parseDateValue(parsed.five_hour?.resets_at);
        const sevenDayResetAt = parseDateValue(parsed.seven_day?.resets_at);
        if (parsed.five_hour && parsed.five_hour.resets_at != null && fiveHourResetAt === null) {
            return null;
        }
        if (parsed.seven_day && parsed.seven_day.resets_at != null && sevenDayResetAt === null) {
            return null;
        }
        const usage = {
            fiveHour,
            sevenDay,
            fiveHourResetAt,
            sevenDayResetAt,
        };
        if (balanceLabel !== null) {
            usage.balanceLabel = balanceLabel;
        }
        return usage;
    }
    catch (err) {
        debug('Failed to read external usage snapshot:', err instanceof Error ? err.message : err);
        return null;
    }
}
//# sourceMappingURL=external-usage.js.map