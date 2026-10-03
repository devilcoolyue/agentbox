export const zoomLevels = [80, 90, 100, 110, 125, 150, 175, 200] as const;
export const zoomPreference = 'agentbox.zoom';
export type ZoomAction = 'in' | 'out' | 'reset';
export type ShortcutEvent = Pick<KeyboardEvent, 'key' | 'ctrlKey' | 'metaKey' | 'altKey' | 'shiftKey' | 'isComposing' | 'keyCode'>;

export function savedZoom(value: string | null): number {
  const parsed = Number(value);
  return zoomLevels.some(level => level === parsed) ? parsed : 100;
}

export function nextZoom(current: number, action: ZoomAction): number {
  if (action === 'reset') return 100;
  if (action === 'in') return zoomLevels.find(level => level > current) ?? 200;
  return [...zoomLevels].reverse().find(level => level < current) ?? 80;
}

export function zoomShortcut(event: ShortcutEvent, mac: boolean, composing = false): ZoomAction | undefined {
  if (composing || event.isComposing || event.keyCode === 229 || event.altKey) return;
  if (mac ? !event.metaKey || event.ctrlKey : !event.ctrlKey || event.metaKey) return;
  if (event.key === '+' || event.key === '=') return 'in';
  if (event.key === '-' && !event.shiftKey) return 'out';
  if (event.key === '0' && !event.shiftKey) return 'reset';
}

// Serialize native changes so rapid clicks cannot apply older preferences last.
// Only a successfully applied zoom is persisted, never a failed request.
export class AppZoom {
  desired = 100;
  applied = 100;
  private pending: Promise<void> | undefined;
  constructor(
    private readonly apply: (percent: number) => Promise<void>,
    private readonly changed: (percent: number) => void,
    private readonly failed: (error: unknown) => void,
  ) {}

  set(percent: number): Promise<void> {
    this.desired = savedZoom(String(percent));
    if (this.desired === this.applied && !this.pending) return Promise.resolve();
    this.pending ??= this.flush().finally(() => { this.pending = undefined; });
    return this.pending;
  }

  change(action: ZoomAction): Promise<void> { return this.set(nextZoom(this.desired, action)); }

  private async flush(): Promise<void> {
    while (this.applied !== this.desired) {
      const target = this.desired;
      try {
        await this.apply(target);
        this.applied = target;
        this.changed(target);
      } catch (error) {
        this.desired = this.applied;
        this.failed(error);
        return;
      }
    }
  }
}
