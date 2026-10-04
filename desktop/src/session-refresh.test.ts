import { afterEach, describe, expect, it, vi } from 'vitest';
import { SessionRefresh } from './session-refresh';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function harness() {
  vi.useFakeTimers();
  const driver = {
    load: vi.fn(async () => 'running'),
    apply: vi.fn(),
    error: vi.fn(),
    loading: vi.fn(),
    visible: vi.fn(() => true),
  };
  return { driver, refresh: new SessionRefresh(driver) };
}

afterEach(() => vi.useRealTimers());

describe('workspace status refresh', () => {
  it('starts polling after five seconds and waits five seconds after each completion', async () => {
    const h = harness();
    const slow = deferred<string>();
    h.driver.load.mockReturnValueOnce(slow.promise);
    h.refresh.start();
    expect(h.driver.load).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(4999);
    expect(h.driver.load).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(h.driver.load).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.driver.load).toHaveBeenCalledTimes(1);
    slow.resolve('running');
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(4999);
    expect(h.driver.load).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.driver.load).toHaveBeenCalledTimes(2);
    h.refresh.stop();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.driver.load).toHaveBeenCalledTimes(2);
  });

  it('reads again after terminal startup overtakes an old stopped snapshot', async () => {
    const h = harness();
    const loginList = deferred<string>();
    const terminalList = deferred<string>();
    h.driver.load.mockReturnValueOnce(loginList.promise).mockReturnValueOnce(terminalList.promise);
    h.refresh.start();
    const login = h.refresh.refresh();
    const terminal = h.refresh.refresh(true);
    const extra = h.refresh.refresh(true);
    expect(extra).toBe(terminal);
    expect(h.driver.load).toHaveBeenCalledTimes(1);
    let terminalSettled = false;
    void terminal.then(() => { terminalSettled = true; });
    loginList.resolve('stopped');
    await login;
    expect(h.driver.load).toHaveBeenCalledTimes(2);
    expect(terminalSettled).toBe(false);
    expect(h.driver.apply).toHaveBeenLastCalledWith('stopped');
    terminalList.resolve('running');
    await terminal;
    expect(h.driver.apply).toHaveBeenLastCalledWith('running');
    expect(h.driver.loading.mock.calls).toEqual([[true], [false]]);
    h.refresh.stop();
  });

  it('coalesces bursts into one trailing request without overlapping reads', async () => {
    const h = harness();
    const first = deferred<string>();
    h.driver.load.mockReturnValueOnce(first.promise);
    h.refresh.start();
    const initial = h.refresh.refresh();
    const pending = Array.from({ length: 20 }, () => h.refresh.refresh(true));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.driver.load).toHaveBeenCalledTimes(1);
    first.resolve('stopped');
    await Promise.all([initial, ...pending]);
    expect(h.driver.load).toHaveBeenCalledTimes(2);
    h.refresh.stop();
  });

  it.each(['resolve', 'reject'] as const)('rejects an old account result after stop/start (%s)', async outcome => {
    const h = harness();
    const old = deferred<string>();
    h.driver.load.mockReturnValueOnce(old.promise).mockResolvedValueOnce('new-account');
    h.refresh.start();
    const oldRead = h.refresh.refresh();
    const oldTrailing = h.refresh.refresh();
    h.refresh.stop();
    await Promise.all([oldRead, oldTrailing]);
    h.refresh.start();
    const newRead = h.refresh.refresh();
    expect(h.driver.load).toHaveBeenCalledTimes(1);
    if (outcome === 'resolve') old.resolve('old-account');
    else old.reject(new Error('old-account failure'));
    await newRead;
    expect(h.driver.load).toHaveBeenCalledTimes(2);
    expect(h.driver.apply).toHaveBeenCalledExactlyOnceWith('new-account');
    expect(h.driver.error).not.toHaveBeenCalled();
    h.refresh.stop();
  });

  it('invalidates old requests when start establishes another generation directly', async () => {
    const h = harness();
    const old = deferred<string>();
    h.driver.load.mockReturnValueOnce(old.promise).mockResolvedValueOnce('new-account');
    h.refresh.start();
    const oldRead = h.refresh.refresh();
    h.refresh.start();
    await oldRead;
    const newRead = h.refresh.refresh();
    old.resolve('old-account');
    await newRead;
    expect(h.driver.apply).toHaveBeenCalledExactlyOnceWith('new-account');
    h.refresh.stop();
  });

  it('skips hidden polling but allows explicit refresh and resumes after becoming visible', async () => {
    const h = harness();
    h.refresh.start();
    h.driver.visible.mockReturnValue(false);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.driver.load).not.toHaveBeenCalled();
    await h.refresh.refresh(true);
    expect(h.driver.load).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.driver.load).toHaveBeenCalledTimes(1);
    h.driver.visible.mockReturnValue(true);
    await h.refresh.refresh(true);
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.driver.load).toHaveBeenCalledTimes(3);
    h.refresh.stop();
  });

  it('keeps the last data when a silent poll fails and continues polling', async () => {
    const h = harness();
    const offline = new Error('offline');
    h.refresh.start();
    await h.refresh.refresh();
    h.driver.load.mockRejectedValueOnce(offline);
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.driver.apply).toHaveBeenCalledExactlyOnceWith('running');
    expect(h.driver.error).not.toHaveBeenCalled();
    expect(h.driver.loading).toHaveBeenLastCalledWith(false);
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.driver.load).toHaveBeenCalledTimes(3);
    h.refresh.stop();
  });

  it('reports explicit errors without replacing successful data', async () => {
    const h = harness();
    h.refresh.start();
    await h.refresh.refresh();
    const offline = new Error('offline');
    h.driver.load.mockRejectedValueOnce(offline);
    await h.refresh.refresh();
    expect(h.driver.apply).toHaveBeenCalledExactlyOnceWith('running');
    expect(h.driver.error).toHaveBeenCalledExactlyOnceWith(offline);
    expect(h.driver.loading).toHaveBeenLastCalledWith(false);
    h.refresh.stop();
  });

  it('preserves explicit error reporting when its pending read coalesces with silent events', async () => {
    const h = harness();
    const first = deferred<string>();
    const offline = new Error('offline');
    h.driver.load.mockReturnValueOnce(first.promise).mockRejectedValueOnce(offline);
    h.refresh.start();
    const active = h.refresh.refresh(true);
    const quiet = h.refresh.refresh(true);
    const explicit = h.refresh.refresh();
    const extra = h.refresh.refresh(true);
    expect(quiet).toBe(explicit);
    expect(extra).toBe(explicit);
    first.resolve('running');
    await Promise.all([active, explicit]);
    expect(h.driver.error).toHaveBeenCalledExactlyOnceWith(offline);
    h.refresh.stop();
  });

  it('does no work before start or after stop, including a queued trailing read', async () => {
    const h = harness();
    await h.refresh.refresh();
    expect(h.driver.load).not.toHaveBeenCalled();
    const first = deferred<string>();
    h.driver.load.mockReturnValueOnce(first.promise);
    h.refresh.start();
    const active = h.refresh.refresh();
    const trailing = h.refresh.refresh();
    h.refresh.stop();
    await Promise.all([active, trailing, h.refresh.refresh()]);
    first.resolve('stale');
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.driver.load).toHaveBeenCalledTimes(1);
    expect(h.driver.apply).not.toHaveBeenCalled();
    expect(h.driver.error).not.toHaveBeenCalled();
    expect(h.driver.loading).toHaveBeenLastCalledWith(false);
  });
});
