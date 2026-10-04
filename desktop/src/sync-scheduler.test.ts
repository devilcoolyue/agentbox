import {describe,it,expect,vi} from 'vitest';
import {SyncScheduler} from './sync-scheduler';
describe('shared sync scheduler',()=>{
 it('serializes projects and starts the next after failure',async()=>{
  const scheduler=new SyncScheduler();let fail!:(e:Error)=>void;
  const first=scheduler.run(()=>new Promise<void>((_,reject)=>{fail=reject;}));
  const checked=expect(first).rejects.toThrow('offline');
  const work=vi.fn(async()=>42);const second=scheduler.run(work);
  expect(work).not.toHaveBeenCalled();fail(new Error('offline'));await checked;
  expect(await second).toBe(42);expect(work).toHaveBeenCalledTimes(1);
 });
});
