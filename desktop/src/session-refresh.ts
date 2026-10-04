export interface SessionRefreshDriver<T> {
  load: () => Promise<T>;
  apply: (value: T) => void;
  error: (error: unknown) => void;
  loading: (loading: boolean) => void;
  visible: () => boolean;
}

interface RefreshBatch {
  generation: number;
  quiet: boolean;
  promise: Promise<void>;
  done: () => void;
}

// A terminal can start its workspace while an older list request is in flight.
// Keep one follow-up read instead of dropping that terminal's refresh event.
export class SessionRefresh<T> {
  private generation = 0;
  private enabled = false;
  private loading = false;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private active: RefreshBatch | undefined;
  private pending: RefreshBatch | undefined;

  constructor(private driver: SessionRefreshDriver<T>) {}

  start(): void {
    this.stop();
    this.enabled = true;
    this.schedule();
  }

  stop(): void {
    this.enabled = false;
    this.generation++;
    this.clearTimer();
    this.pending?.done();
    this.pending = undefined;
    // The transport cannot be canceled here. Retain its slot until it settles,
    // but release callers and invalidate its results before changing accounts.
    this.active?.done();
    this.setLoading(false);
  }

  refresh(quiet = false): Promise<void> {
    if (!this.enabled) return Promise.resolve();
    this.clearTimer();
    if (this.pending) {
      this.pending.quiet = this.pending.quiet && quiet;
      return this.pending.promise;
    }
    let done!: () => void;
    const promise = new Promise<void>(resolve => { done = resolve; });
    this.pending = { generation: this.generation, quiet, promise, done };
    this.setLoading(true);
    this.drain();
    return promise;
  }

  private setLoading(value: boolean): void {
    if (this.loading === value) return;
    this.loading = value;
    this.driver.loading(value);
  }

  private clearTimer(): void {
    clearTimeout(this.timer);
    this.timer = undefined;
  }

  private current(batch: RefreshBatch): boolean {
    return this.enabled && batch.generation === this.generation;
  }

  private drain(): void {
    if (this.active || !this.pending) return;
    const batch = this.pending;
    this.pending = undefined;
    this.active = batch;
    void this.run(batch);
  }

  private async run(batch: RefreshBatch): Promise<void> {
    try {
      const value = await this.driver.load();
      if (this.current(batch)) this.driver.apply(value);
    } catch (error) {
      if (this.current(batch) && !batch.quiet) this.driver.error(error);
    } finally {
      this.active = undefined;
      batch.done();
      if (this.pending) {
        this.drain();
      } else {
        this.setLoading(false);
        this.schedule();
      }
    }
  }

  private schedule(): void {
    this.clearTimer();
    if (!this.enabled || this.active || this.pending || !this.driver.visible()) return;
    this.timer = setTimeout(() => {
      this.timer = undefined;
      if (this.enabled && this.driver.visible()) void this.refresh(true);
    }, 5000);
  }
}
