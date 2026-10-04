import { listen } from '@tauri-apps/api/event';
import { bridge } from './bridge';
import type { AttachmentInput } from './attachment-queue';

export interface AttachmentDrop { files: AttachmentInput[]; x: number; y: number }
interface DropTarget {
  accepts: (x: number, y: number) => boolean;
  receive: (files: AttachmentInput[]) => void;
}

// Exactly one visible terminal owns a native selection. A window with no
// terminal, or a drop over another panel/modal, must release its native handles.
export class AttachmentDropRouter {
  private readonly targets = new Set<DropTarget>();
  constructor(private readonly release: (files: AttachmentInput[]) => Promise<void>) {}
  register(target: DropTarget): () => void {
    this.targets.add(target);
    return () => { this.targets.delete(target); };
  }
  async route(drop: AttachmentDrop): Promise<boolean> {
    try {
      if (Number.isFinite(drop.x) && Number.isFinite(drop.y)) {
        for (const target of this.targets) {
          if (target.accepts(drop.x, drop.y)) {
            target.receive(drop.files);
            return true;
          }
        }
      }
    } catch {
      // A disappearing receiver must not strand OS file handles.
    }
    await this.release(drop.files);
    return false;
  }
}

export const attachmentDrops = new AttachmentDropRouter(async files => {
  const tickets = files.flatMap(file => file.ticket ? [file.ticket] : []);
  if (tickets.length) await bridge.invoke('release_attachments', { tickets }).catch(() => {});
});

export async function listenForAttachmentDrops(warn: (message: string) => void): Promise<() => void> {
  const stops: (() => void)[] = [];
  try {
    stops.push(await listen<AttachmentDrop>('attachment-drop', ({payload}) => {
      void attachmentDrops.route(payload).then(accepted => {
        if (!accepted) warn('请将文件拖到终端区域，或在文件页使用上传按钮。');
      });
    }));
    stops.push(await listen<{message: string}>('attachment-drop-error', ({payload}) => warn(payload.message)));
  } catch (error) {
    stops.forEach(stop => stop());
    throw error;
  }
  return () => stops.forEach(stop => stop());
}
