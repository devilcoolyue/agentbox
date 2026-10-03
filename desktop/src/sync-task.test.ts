import { describe, expect, it, vi } from 'vitest';
import { SyncTask, type SyncProgress } from './sync-task';
function harness(){
 const callbacks:((p:SyncProgress)=>void)[]=[];
 let resolve!:(value:unknown)=>void;
 const invoke=vi.fn((name:string)=>name==='sync_progress_ack'||name==='sync_cancel'?Promise.resolve():new Promise(r=>{resolve=r;}));
 const update=vi.fn();
 const task=new SyncTask(update,{invoke,channel:(cb:(p:SyncProgress)=>void)=>{callbacks.push(cb);return {};}} as never);
 const event=(sequence:number,task='native-1')=>({task,sequence,stage:'applying',path:'file',completed:0,total:1,bytes:10,total_bytes:10} as SyncProgress);
 return {task,callbacks,invoke,update,event,finish:()=>resolve({ok:true})};
}
describe('sync progress ownership',()=>{
 it('ignores duplicate/out-of-order progress and acknowledges consumed events',async()=>{
  const h=harness();const result=h.task.run('sync_apply');h.callbacks[0](h.event(2));h.callbacks[0](h.event(1));h.callbacks[0](h.event(3,'wrong-task'));
  expect(h.update.mock.calls.filter(([p])=>p)).toHaveLength(1);
  expect(h.invoke).toHaveBeenCalledWith('sync_progress_ack',{task:'native-1',sequence:2});
  h.finish();await result;h.callbacks[0](h.event(4));expect(h.update).toHaveBeenLastCalledWith(null);
 });
 it('cancellation and a new task never accept the previous callback',async()=>{
  const h=harness();const first=h.task.run('sync_apply');await h.task.cancel();h.callbacks[0](h.event(1));h.finish();await first;
  const second=h.task.run('sync_review');h.callbacks[0](h.event(2));h.callbacks[1](h.event(1,'native-2'));
  expect(h.update.mock.calls.filter(([p])=>p)).toEqual([[h.event(1,'native-2')]]);
  h.finish();await second;
 });
 it('unmount cancels work and never starts or paints another request',async()=>{
  const h=harness();const result=h.task.run('sync_preview');h.task.close();h.callbacks[0](h.event(1));h.finish();await result;
  expect(h.update.mock.calls.filter(([p])=>p)).toHaveLength(0);
  expect(h.invoke).toHaveBeenCalledWith('sync_cancel');await expect(h.task.run('sync_list')).rejects.toThrow('closed');
 });
});

it('canceling a queued project does not cancel or dispatch another native task',async()=>{
 const first=harness();const second=harness();
 const running=first.task.run('sync_apply');const queued=second.task.run('sync_preview');
 const rejected=expect(queued).rejects.toThrow('canceled before dispatch');
 await second.task.cancel();expect(second.invoke).not.toHaveBeenCalled();
 first.finish();await running;await rejected;
 expect(second.invoke).not.toHaveBeenCalled();
});
