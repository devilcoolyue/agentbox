import type { ShortcutEvent } from './app-zoom';

export function terminalShortcut(event: ShortcutEvent, mac: boolean, composing = false): 'copy' | 'paste' | 'menu' | undefined {
  if (composing || event.isComposing || event.keyCode === 229 || event.altKey) return;
  if (!event.ctrlKey && !event.metaKey && (event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10'))) return 'menu';
  const clipboard = mac ? event.metaKey && !event.ctrlKey : event.ctrlKey && event.shiftKey && !event.metaKey;
  if (clipboard && event.key.toLowerCase() === 'c') return 'copy';
  if (clipboard && event.key.toLowerCase() === 'v') return 'paste';
}
