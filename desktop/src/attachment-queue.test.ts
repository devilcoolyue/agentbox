import { describe, expect, it, vi } from 'vitest';
import { AttachmentQueue } from './attachment-queue';

function file(name: string, body: string) {
  return { name, type: 'text/plain', size: body.length, arrayBuffer: async () => new TextEncoder().encode(body).buffer };
}

describe('attachment queue', () => {
  it('uploads files sequentially and reports bounded progress', async () => {
    const upload = vi.fn(async ({ name }: { name: string }) => ({ path: `/shared/.file/${name}`, name }));
    const progress: string[] = [];
    const result = await new AttachmentQueue(upload).run([file('a.txt', 'a'), file('b.txt', 'bb')], (current, total, name) => progress.push(`${current}/${total}:${name}`));
    expect(result.map(value => value.path)).toEqual(['/shared/.file/a.txt', '/shared/.file/b.txt']);
    expect(upload).toHaveBeenCalledTimes(2);
    expect(progress).toEqual(['0/2:a.txt', '1/2:a.txt', '1/2:b.txt', '2/2:b.txt']);
  });

  it('cancels between reads and never starts the next upload', async () => {
    let resolve!: (value: ArrayBuffer) => void;
    const second = file('b.txt', 'b');
    const first = { name: 'a.txt', type: 'text/plain', size: 1, arrayBuffer: () => new Promise<ArrayBuffer>(r => { resolve = r; }) };
    const upload = vi.fn(async ({ name }: { name: string }) => ({ path: name, name }));
    const queue = new AttachmentQueue(upload);
    const pending = queue.run([first, second], () => {});
    queue.cancel();
    resolve(new Uint8Array([1]).buffer);
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
    expect(upload).not.toHaveBeenCalled();
  });

  it('rejects unsafe names and oversized files before invoking native upload', async () => {
    const upload = vi.fn();
    await expect(new AttachmentQueue(upload).run([file('../secret', 'x')], () => {})).rejects.toThrow('文件名无效');
    await expect(new AttachmentQueue(upload).run([{ ...file('large.bin', 'x'), size: 20 * 1024 * 1024 + 1 }], () => {})).rejects.toThrow('超过 19 MiB');
    expect(upload).not.toHaveBeenCalled();
  });
  it('uses native tickets without reading local bytes in the renderer', async()=>{
    const upload=vi.fn(async()=>({path:'/shared/.file/a.bin',name:'a.bin'}));
    const arrayBuffer=vi.fn();
    await new AttachmentQueue(upload).run([{name:'a.bin',type:'',size:4,ticket:'native-ticket',arrayBuffer}],()=>{});
    expect(arrayBuffer).not.toHaveBeenCalled();expect(upload).toHaveBeenCalledWith({name:'a.bin',type:'application/octet-stream',bytes:undefined,ticket:'native-ticket'});
  });
  it('retains successful earlier results and suppresses a canceled late result',async()=>{
    let finish!:(value:{path:string;name:string})=>void;
    const upload=vi.fn().mockResolvedValueOnce({path:'/shared/.file/a',name:'a'}).mockImplementationOnce(()=>new Promise(resolve=>{finish=resolve;}));
    const completed=vi.fn();const queue=new AttachmentQueue(upload);
    const pending=queue.run([file('a','a'),file('b','b')],()=>{},completed);
    await vi.waitFor(()=>expect(upload).toHaveBeenCalledTimes(2));queue.cancel();finish({path:'/shared/.file/b',name:'b'});
    await expect(pending).rejects.toMatchObject({name:'AbortError'});
    expect(completed).toHaveBeenCalledTimes(1);expect(completed).toHaveBeenCalledWith({path:'/shared/.file/a',name:'a'});
  });

});
