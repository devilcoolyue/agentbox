import { afterEach, describe, expect, it, vi } from 'vitest';
import { TerminalConnection, reconnectClose } from './terminal-connection';
import type { TerminalEvent } from './bridge';

function harness() {
  const callbacks: ((event: TerminalEvent) => void)[] = [];
  let next = 0;
  const invoke = vi.fn(async (command: string, _args?: unknown) => command === 'terminal_open' ? ++next : undefined);
  const transport = { invoke, channel: (cb: (event: TerminalEvent) => void) => { callbacks.push(cb); return {}; } };
  const rendered: (() => void)[] = [];
  const status = vi.fn();
  const connection = new TerminalConnection((_bytes, done) => rendered.push(done), status, () => ({ cols: 80, rows: 24 }), transport as never);
  return { callbacks, invoke, rendered, status, connection };
}

afterEach(() => vi.useRealTimers());

describe('terminal connection ownership', () => {
  it('acknowledges output only after xterm consumed it', async () => {
    const h = harness(); await h.connection.open('s1');
    h.callbacks[0]({ type: 'data', sequence: 1, bytes: [0xe4, 0xb8, 0xad] });
    expect(h.invoke.mock.calls.filter(([name]) => name === 'terminal_ack')).toHaveLength(0);
    h.rendered[0](); await Promise.resolve();
    expect(h.invoke).toHaveBeenCalledWith('terminal_ack', { id: 1, sequence: 1 });
    h.connection.close();
  });

  it('ignores late data and closes from a replaced workspace', async () => {
    const h = harness(); await h.connection.open('old'); await h.connection.open('new');
    h.callbacks[0]({ type: 'data', sequence: 1, bytes: [65] });
    h.callbacks[0]({ type: 'closed', code: 4004, message: 'revoked' });
    expect(h.rendered).toHaveLength(0);
    expect(h.status).not.toHaveBeenCalledWith('revoked', false);
    h.connection.close();
  });

  it('never fights a shared tmux takeover, expired login, quota or revoked account', () => {
    for (const code of [1000, 1008, 4001, 4003, 4004, 4008]) expect(reconnectClose(code)).toBe(false);
    for (const code of [1001, 1006, 1011, 1012, 1013]) expect(reconnectClose(code)).toBe(true);
  });

  it('cancels scheduled reconnects on close', async () => {
    vi.useFakeTimers(); const h = harness(); await h.connection.open('s1');
    h.callbacks[0]({ type: 'closed', code: 1006, message: 'network' });
    h.connection.close(); await vi.advanceTimersByTimeAsync(60_000);
    expect(h.invoke.mock.calls.filter(([name]) => name === 'terminal_open')).toHaveLength(1);
  });

  it('keeps the remote terminal ID on reconnect and releases closed native handles', async () => {
    vi.useFakeTimers();const h=harness();await h.connection.open('workspace','123456abcdef');
    expect(h.invoke).toHaveBeenCalledWith('terminal_open',expect.objectContaining({session:'workspace',terminal:'123456abcdef'}));
    h.callbacks[0]({type:'closed',code:1006,message:'network'});
    await vi.advanceTimersByTimeAsync(1500);
    expect(h.invoke).toHaveBeenCalledWith('terminal_close',{id:1});
    const opened=h.invoke.mock.calls.filter(([name])=>name==='terminal_open');
    expect(opened).toHaveLength(2);
    expect(opened[1][1]).toEqual(expect.objectContaining({terminal:'123456abcdef'}));
    h.connection.close();
  });

  it('chunks accepted input in order and rejects oversized paste before sending', async () => {
    const h = harness(); await h.connection.open('s1');
    h.connection.input(new Uint8Array(128 * 1024 + 1)); await Promise.resolve();
    expect(h.invoke.mock.calls.filter(([name]) => name === 'terminal_input')).toHaveLength(0);
    h.connection.input(new Uint8Array(17 * 1024).fill(65));
    await vi.waitFor(() => expect(h.invoke.mock.calls.filter(([name]) => name === 'terminal_input')).toHaveLength(2));
    const calls = h.invoke.mock.calls.filter(([name]) => name === 'terminal_input');
    expect((calls[0][1] as { bytes: number[] }).bytes.length).toBe(16 * 1024);
    expect((calls[1][1] as { bytes: number[] }).bytes.length).toBe(1024);
    h.connection.close();
  });
  it('sleep recovery preserves the terminal identity and does not override policy closures',async()=>{
    const h=harness();await h.connection.open('space','123456abcdef');
    h.connection.resumeAfterSleep();await vi.waitFor(()=>expect(h.callbacks).toHaveLength(2));
    expect(h.invoke).toHaveBeenCalledWith('terminal_open',expect.objectContaining({session:'space',terminal:'123456abcdef'}));
    h.callbacks[1]({type:'closed',code:4003,message:'quota'});h.connection.resumeAfterSleep();await Promise.resolve();
    expect(h.callbacks).toHaveLength(2);h.connection.close();
  });

});
