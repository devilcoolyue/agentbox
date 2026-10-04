import { afterEach, expect, it, vi } from 'vitest';
import { newTaskId } from './task-id';

afterEach(() => vi.unstubAllGlobals());

it('creates independent UUID v4 task IDs when the platform has no randomUUID', () => {
  const source = globalThis.crypto;
  const secureBytes = vi.fn((bytes: Uint8Array) => source.getRandomValues(bytes));
  vi.stubGlobal('crypto', { getRandomValues: secureBytes });
  expect('randomUUID' in crypto).toBe(false);
  const first = newTaskId();
  const second = newTaskId();
  const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
  expect(first).toMatch(uuidV4);
  expect(second).toMatch(uuidV4);
  expect(second).not.toBe(first);
  expect(secureBytes).toHaveBeenCalledTimes(2);
});
