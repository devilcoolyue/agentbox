import { describe, expect, it, vi } from 'vitest';
import { AppZoom, nextZoom, savedZoom, zoomShortcut, type ShortcutEvent } from './app-zoom';
import { terminalShortcut } from './terminal-shortcuts';

function key(key: string, fields: Partial<ShortcutEvent> = {}): ShortcutEvent {
  return { key, ctrlKey: false, metaKey: false, altKey: false, shiftKey: false, isComposing: false, keyCode: 0, ...fields };
}

describe('application and terminal shortcuts', () => {
  it('reserves only platform zoom combinations and preserves terminal interrupt keys', () => {
    expect(zoomShortcut(key('+', { metaKey: true, shiftKey: true }), true)).toBe('in');
    expect(zoomShortcut(key('=', { ctrlKey: true }), false)).toBe('in');
    expect(zoomShortcut(key('-', { ctrlKey: true }), false)).toBe('out');
    expect(zoomShortcut(key('0', { metaKey: true }), true)).toBe('reset');
    for (const shortcut of [key('c', { ctrlKey: true }), key('-', { ctrlKey: true, altKey: true }), key('_', { ctrlKey: true, shiftKey: true }), key('+'), key('0', { ctrlKey: true, metaKey: true })]) {
      expect(zoomShortcut(shortcut, false)).toBeUndefined();
      expect(terminalShortcut(shortcut, false)).toBeUndefined();
    }
    expect(zoomShortcut(key('-', { ctrlKey: true }), true)).toBeUndefined();
    expect(zoomShortcut(key('-', { metaKey: true }), false)).toBeUndefined();
    expect(terminalShortcut(key('c', { ctrlKey: true, shiftKey: true }), false)).toBe('copy');
    expect(terminalShortcut(key('v', { metaKey: true }), true)).toBe('paste');
    expect(terminalShortcut(key('F10', { shiftKey: true }), false)).toBe('menu');
    expect(terminalShortcut(key('ContextMenu'), false)).toBe('menu');
  });

  it('leaves IME composition and keyCode 229 to the input method', () => {
    for (const state of [{ isComposing: true }, { keyCode: 229 }]) {
      expect(zoomShortcut(key('+', { ctrlKey: true, ...state }), false)).toBeUndefined();
      expect(terminalShortcut(key('v', { metaKey: true, ...state }), true)).toBeUndefined();
    }
    expect(zoomShortcut(key('+', { metaKey: true }), true, true)).toBeUndefined();
    expect(terminalShortcut(key('v', { metaKey: true }), true, true)).toBeUndefined();
  });
});

describe('application zoom preferences', () => {
  it('rejects stale or corrupt values and bounds repeated adjustments', () => {
    for (const value of [null, '', 'NaN', 'Infinity', '79', '201', '1.25']) expect(savedZoom(value)).toBe(100);
    expect(savedZoom('125')).toBe(125);
    expect(nextZoom(200, 'in')).toBe(200);
    expect(nextZoom(80, 'out')).toBe(80);
    expect(nextZoom(175, 'reset')).toBe(100);
  });

  it('serializes rapid adjustments and records only native successes', async () => {
    const releases: (() => void)[] = [];
    const apply = vi.fn(() => new Promise<void>(resolve => releases.push(resolve)));
    const changed = vi.fn(); const failed = vi.fn();
    const zoom = new AppZoom(apply, changed, failed);
    await zoom.set(100);
    const pending = zoom.change('in');
    void zoom.change('in'); void zoom.change('in');
    expect(apply.mock.calls).toEqual([[110]]);
    expect(changed).not.toHaveBeenCalled();
    releases.shift()!();
    await vi.waitFor(() => expect(apply.mock.calls).toEqual([[110], [150]]));
    releases.shift()!(); await pending;
    expect(changed.mock.calls).toEqual([[110], [150]]);
    expect(zoom.applied).toBe(150); expect(zoom.desired).toBe(150);
    expect(failed).not.toHaveBeenCalled();
  });

  it('keeps the last working size when native zoom fails and allows retry', async () => {
    const error = { message: 'zoom failed' };
    const apply = vi.fn().mockResolvedValueOnce(undefined).mockRejectedValueOnce(error).mockResolvedValue(undefined);
    const changed = vi.fn(); const failed = vi.fn();
    const zoom = new AppZoom(apply, changed, failed);
    await zoom.set(125); await zoom.set(150);
    expect(zoom.applied).toBe(125); expect(zoom.desired).toBe(125);
    expect(changed.mock.calls).toEqual([[125]]); expect(failed).toHaveBeenCalledWith(error);
    await zoom.set(150); expect(zoom.applied).toBe(150);
  });
});
