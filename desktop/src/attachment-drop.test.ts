import { describe, expect, it, vi } from 'vitest';
import { AttachmentDropRouter } from './attachment-drop';

const files = [{ticket:'os-grant',name:'代码.png',size:10,type:'image/png'}];
const drop = {files,x:200,y:100};

describe('native drop ownership', () => {
  it('releases selections dropped outside every terminal, including with no open terminals', async () => {
    const release = vi.fn().mockResolvedValue(undefined);
    const router = new AttachmentDropRouter(release);
    expect(await router.route(drop)).toBe(false);
    router.register({accepts:()=>false,receive:vi.fn()});
    expect(await router.route(drop)).toBe(false);
    expect(release).toHaveBeenCalledTimes(2);
    expect(release).toHaveBeenLastCalledWith(files);
  });
  it('gives ownership to only one hit terminal and never to a hidden one', async () => {
    const release = vi.fn().mockResolvedValue(undefined);
    const hidden = vi.fn(); const selected = vi.fn(); const covered = vi.fn();
    const router = new AttachmentDropRouter(release);
    router.register({accepts:()=>false,receive:hidden});
    router.register({accepts:(x,y)=>x===200&&y===100,receive:selected});
    router.register({accepts:()=>true,receive:covered});
    expect(await router.route(drop)).toBe(true);
    expect(selected).toHaveBeenCalledExactlyOnceWith(files);
    expect(hidden).not.toHaveBeenCalled(); expect(covered).not.toHaveBeenCalled();
    expect(release).not.toHaveBeenCalled();
  });
  it('releases late selections after the terminal closes or if its receiver fails', async () => {
    const release = vi.fn().mockResolvedValue(undefined);
    const router = new AttachmentDropRouter(release);
    const receive = vi.fn();
    const unregister = router.register({accepts:()=>true,receive});
    unregister();
    expect(await router.route(drop)).toBe(false);
    expect(receive).not.toHaveBeenCalled();
    router.register({accepts:()=>true,receive:()=>{throw new Error('unmounted');}});
    expect(await router.route(drop)).toBe(false);
    expect(release).toHaveBeenCalledTimes(2);
  });
  it('does not deliver malformed native coordinates', async () => {
    const release = vi.fn().mockResolvedValue(undefined);
    const receive = vi.fn();const router = new AttachmentDropRouter(release);
    router.register({accepts:()=>true,receive});
    expect(await router.route({...drop,x:NaN})).toBe(false);
    expect(receive).not.toHaveBeenCalled();expect(release).toHaveBeenCalledWith(files);
  });
});
