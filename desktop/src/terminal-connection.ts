import { bridge, errorMessage, retryable, type TerminalEvent } from './bridge';

export function reconnectClose(code: number): boolean {
  return code === 1001 || code === 1006 || code === 1011 || code === 1012 || code === 1013;
}

// A generation owns its channel, timers and input queue. Late events cannot target a new terminal.
export class TerminalConnection {
  private generation = 0;
  private id: number | null = null;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private stable: ReturnType<typeof setTimeout> | undefined;
  private stopped = true;
  private attempts = 0;
  private policyClosed = false;
  private queued = 0;
  private inputChain: Promise<void> = Promise.resolve();
  private session = '';
  private terminal: string | null = null;

  constructor(
    private output: (bytes: Uint8Array, done: () => void) => void,
    private status: (message: string, connected: boolean) => void,
    private size: () => { cols: number; rows: number },
    private transport = bridge,
  ) {}

  async open(session: string, terminal: string | null = null): Promise<void> {
    this.close();
    this.stopped = false;
    this.session = session;
    this.terminal = terminal;
    this.attempts = 0;
    this.policyClosed = false;
    await this.attach();
  }

  close(): void {
    this.stopped = true;
    this.generation++;
    clearTimeout(this.timer);
    clearTimeout(this.stable);
    if (this.id !== null) void this.transport.invoke('terminal_close', { id: this.id }).catch(() => {});
    this.id = null;
    this.queued = 0;
    this.inputChain = Promise.resolve();
  }

  private async attach(): Promise<void> {
    const generation = ++this.generation;
    let closed = false;
    this.status(this.attempts ? `正在重连（${this.attempts}/10）…` : '正在连接…', false);
    // Events can arrive before terminal_open resolves. ACK waits for the native handle.
    let opened: Promise<number>;
    const events = this.transport.channel((event: TerminalEvent) => {
      if (generation !== this.generation || this.stopped) return;
      if (event.type === 'data') {
        this.output(new Uint8Array(event.bytes), () => {
          void opened.then(id => {
            if (generation === this.generation && !this.stopped) return this.transport.invoke('terminal_ack', { id, sequence: event.sequence });
          }).catch(() => {});
        });
      } else {
        closed = true;
        void opened.then(id => this.transport.invoke('terminal_close', { id })).catch(() => {});
        this.id = null;
        clearTimeout(this.stable);
        this.policyClosed = !reconnectClose(event.code) && event.code !== 4008;
        this.status(event.message, false);
        if (reconnectClose(event.code)) this.schedule(generation);
      }
    });
    opened = this.transport.invoke<number>('terminal_open', { session: this.session, terminal: this.terminal, events });
    try {
      const id = await opened;
      if (generation !== this.generation || this.stopped) {
        await this.transport.invoke('terminal_close', { id });
        return;
      }
      if (closed) return;
      this.id = id;
      this.status('已连接', true);
      this.resize();
      this.stable = setTimeout(() => { this.attempts = 0; }, 30_000);
    } catch (error) {
      if (generation !== this.generation || this.stopped) return;
      this.status(errorMessage(error), false);
      this.policyClosed=!retryable(error);
      if (retryable(error)) this.schedule(generation);
    }
  }

  private schedule(generation: number): void {
    if (this.stopped || this.attempts >= 10) return;
    clearTimeout(this.timer);
    const delay = Math.min(30_000, 1000 * 2 ** this.attempts++) + Math.random() * 250;
    this.timer = setTimeout(() => {
      if (generation === this.generation && !this.stopped) void this.attach();
    }, delay);
  }

  resumeAfterSleep(): void {
    if(this.stopped||this.policyClosed)return;
    // Reopen the same remote terminal, discard the old local transport/input
    // queue, and never replay user keystrokes after sleep.
    void this.open(this.session,this.terminal);
  }

  resize(): void {
    if (this.id === null) return;
    const { cols, rows } = this.size();
    void this.transport.invoke('terminal_resize', { id: this.id, cols: Math.min(1000, cols), rows: Math.min(1000, rows) }).catch(error => this.status(errorMessage(error), this.id !== null));
  }

  input(bytes: Uint8Array): void {
    if (this.id === null) { this.status('终端未连接，输入未发送', false); return; }
    if (this.queued + bytes.length > 128 * 1024) { this.status('输入超过缓冲限制，请分段粘贴', true); return; }
    const generation = this.generation;
    const id = this.id;
    this.queued += bytes.length;
    this.inputChain = this.inputChain.then(async () => {
      for (let start = 0; start < bytes.length; start += 16 * 1024) {
        if (generation !== this.generation || id !== this.id) return;
        await this.transport.invoke('terminal_input', { id, bytes: Array.from(bytes.subarray(start, start + 16 * 1024)) });
      }
    }).catch(error => {
      if (generation === this.generation) { this.close(); this.status(`${errorMessage(error)}；输入可能只发送了一部分，请检查后重连`, false); }
    }).finally(() => { if (generation === this.generation) this.queued -= bytes.length; });
  }
}
