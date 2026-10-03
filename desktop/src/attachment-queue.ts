export const MAX_ATTACHMENT_BYTES = 19 * 1024 * 1024;

export interface AttachmentInput {
  name: string;
  type: string;
  size: number;
  ticket?: string;
  arrayBuffer?: () => Promise<ArrayBuffer>;
}

export interface AttachmentResult {
  path: string;
  name: string;
  orig?: string;
}

export type AttachmentUploader = (
  input: { name: string; type: string; bytes?: number[]; ticket?: string },
) => Promise<AttachmentResult>;

export class AttachmentQueue {
  private generation = 0;
  private active = false;

  constructor(private readonly upload: AttachmentUploader) {}

  get running() {
    return this.active;
  }

  cancel() {
    this.generation++;
  }

  async run(
    files: readonly AttachmentInput[],
    progress: (current: number, total: number, name: string) => void,
    completed: (result: AttachmentResult) => void = () => {},
  ): Promise<AttachmentResult[]> {
    if (files.length > 32 || files.reduce((total,file)=>total+file.size,0)>64*1024*1024) throw new Error('每次最多 32 个附件，总计 64 MiB');
    if (this.active) throw new Error('附件上传正在进行');
    const generation = ++this.generation;
    this.active = true;
    const results: AttachmentResult[] = [];
    try {
      for (let index = 0; index < files.length; index++) {
        if (generation !== this.generation) throw new DOMException('上传已取消', 'AbortError');
        const file = files[index];
        if (!file.name || file.name === '.' || file.name === '..' || /[\\/\0\r\n]/.test(file.name)) {
          throw new Error(`文件名无效：${file.name || '（空）'}`);
        }
        if (!Number.isSafeInteger(file.size) || file.size < 0 || file.size > MAX_ATTACHMENT_BYTES) {
          throw new Error(`附件超过 19 MiB 限制：${file.name}`);
        }
        progress(index, files.length, file.name);
        let bytes: number[]|undefined;
        if (!file.ticket) {
          if(!file.arrayBuffer)throw new Error('附件内容缺失');
          bytes = Array.from(new Uint8Array(await file.arrayBuffer()));
          if (generation !== this.generation) throw new DOMException('上传已取消', 'AbortError');
          if (bytes.length !== file.size) throw new Error(`读取附件大小变化：${file.name}`);
        }
        results.push(await this.upload({ name: file.name, type: file.type || 'application/octet-stream', bytes, ticket:file.ticket }));
        if (generation !== this.generation) throw new DOMException('上传已取消，服务器可能已保存附件', 'AbortError');
        completed(results[results.length-1]);
        progress(index + 1, files.length, file.name);
      }
      return results;
    } finally {
      this.active = false;
    }
  }
}
