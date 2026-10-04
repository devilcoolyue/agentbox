import { afterEach, describe, expect, it, vi } from 'vitest';
import { SyncLoop } from './sync-loop';
import type { SyncPreview } from './bridge';

function preview(overrides:Partial<SyncPreview['plan']>={},current=true):SyncPreview {
 return {baseline_current:current,state_revision:1,project_revision:1,plan:{digest:'digest',conflicts:[],operations:[],needs_confirmation:false,reasons:[],...overrides}};
}
function harness(value=preview()){
 vi.useFakeTimers();
 const driver={preview:vi.fn(async()=>value),apply:vi.fn(async(_value:SyncPreview)=>{}),update:vi.fn()};
 return {driver,loop:new SyncLoop(driver)};
}
afterEach(()=>vi.useRealTimers());
const transient={kind:'sync_preview_retryable',message:'temporary read failure'};
describe('opt-in continuous synchronization',()=>{
 it('does not start itself or create empty history while idle, and backs off',async()=>{
  const h=harness();await vi.advanceTimersByTimeAsync(60_000);expect(h.driver.preview).not.toHaveBeenCalled();
  h.loop.start();await vi.advanceTimersByTimeAsync(0);expect(h.driver.preview).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(2000);expect(h.driver.preview).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(3999);expect(h.driver.preview).toHaveBeenCalledTimes(2);
  expect(h.driver.apply).not.toHaveBeenCalled();h.loop.stop();await vi.advanceTimersByTimeAsync(60_000);expect(h.driver.preview).toHaveBeenCalledTimes(2);
 });
 it('commits an already converged changed tree once, then remains idle',async()=>{
  const h=harness(preview({},false));h.driver.apply.mockImplementation(async()=>{h.driver.preview.mockResolvedValue(preview());});
  h.loop.start();await vi.advanceTimersByTimeAsync(7000);
  expect(h.driver.apply).toHaveBeenCalledTimes(1);h.loop.stop();
 });
 it.each([
  preview({needs_confirmation:true,reasons:['initial_sync']}),
  preview({needs_confirmation:true,reasons:['mass_delete']}),
  preview({conflicts:[{path:'file',reason:'both_sides_changed'}]}),
 ])('pauses before any write that requires a decision',async(value)=>{
  const h=harness(value);h.loop.start();await vi.advanceTimersByTimeAsync(60_000);
  expect(h.driver.apply).not.toHaveBeenCalled();expect(h.driver.preview).toHaveBeenCalledTimes(1);
  expect(h.driver.update).toHaveBeenLastCalledWith('paused',value);
 });
 it('stopping during preview prevents late writes and a second overlapping round',async()=>{
  const h=harness();let finish!:(p:SyncPreview)=>void;
  h.driver.preview.mockImplementation(()=>new Promise(resolve=>{finish=resolve;}));
  h.loop.start();await vi.advanceTimersByTimeAsync(0);h.loop.stop();expect(h.loop.start()).toBe(false);
  finish(preview({operations:[{kind:'upload',path:'file'}]},false));await h.loop.settled();
  await vi.advanceTimersByTimeAsync(60_000);expect(h.driver.apply).not.toHaveBeenCalled();expect(h.driver.preview).toHaveBeenCalledTimes(1);
 });
 it('does not replay a failed write and pauses for reconciliation',async()=>{
  const h=harness(preview({operations:[{kind:'upload',path:'file'}]},false));
  const error=new Error('response lost');h.driver.apply.mockRejectedValue(error);
  h.loop.start();await vi.advanceTimersByTimeAsync(60_000);
  expect(h.driver.apply).toHaveBeenCalledTimes(1);expect(h.driver.update).toHaveBeenLastCalledWith('paused',undefined,error);
 });
 it('bounds preview retries to 1, 2, 4 and 8 seconds, then pauses without writes',async()=>{
  const h=harness();h.driver.preview.mockRejectedValue(transient);
  h.loop.start();await vi.advanceTimersByTimeAsync(0);
  for(const [index,delayMs] of [1000,2000,4000,8000].entries()){
   expect(h.driver.preview).toHaveBeenCalledTimes(index+1);
   expect(h.driver.update).toHaveBeenLastCalledWith('waiting',undefined,transient,{attempt:index+1,limit:4,delayMs});
   await vi.advanceTimersByTimeAsync(delayMs-1);
   expect(h.driver.preview).toHaveBeenCalledTimes(index+1);
   await vi.advanceTimersByTimeAsync(1);
  }
  expect(h.driver.preview).toHaveBeenCalledTimes(5);
  expect(h.driver.update).toHaveBeenLastCalledWith('paused',undefined,transient);
  await vi.advanceTimersByTimeAsync(120_000);
  expect(h.driver.preview).toHaveBeenCalledTimes(5);expect(h.driver.apply).not.toHaveBeenCalled();
 });
 it('rebuilds a fresh preview after recovery, applies it once and resets the retry budget',async()=>{
  const fresh=preview({operations:[{kind:'download',path:'new-file'}]},false);
  const h=harness();
  h.driver.preview.mockRejectedValueOnce(transient).mockRejectedValueOnce(transient).mockResolvedValueOnce(fresh).mockRejectedValueOnce(transient);
  h.loop.start();await vi.advanceTimersByTimeAsync(3000);
  expect(h.driver.preview).toHaveBeenCalledTimes(3);
  expect(h.driver.apply).toHaveBeenCalledExactlyOnceWith(fresh);
  await vi.advanceTimersByTimeAsync(1000);
  expect(h.driver.update).toHaveBeenLastCalledWith('waiting',undefined,transient,{attempt:1,limit:4,delayMs:1000});
  expect(h.driver.apply).toHaveBeenCalledTimes(1);h.loop.stop();
 });
 it.each([
  {kind:'unauthorized',message:'temporary read failure'},
  {kind:'sync_identity',message:'temporary read failure'},
  {kind:'sync_pending',message:'temporary read failure'},
  {kind:'sidecar_protocol',message:'temporary read failure'},
  {kind:'network',message:'certificate failure'},
  {kind:'sync',message:'sync_preview_retryable'},
  new Error('sync_preview_retryable'),
 ])('never infers retry permission from generic kinds or error messages',async(error)=>{
  const h=harness();h.driver.preview.mockRejectedValue(error);
  h.loop.start();await vi.advanceTimersByTimeAsync(60_000);
  expect(h.driver.preview).toHaveBeenCalledTimes(1);expect(h.driver.apply).not.toHaveBeenCalled();
  expect(h.driver.update).toHaveBeenLastCalledWith('paused',undefined,error);
 });
 it('pauses on conflict returned by the fresh retry preview',async()=>{
  const conflict=preview({conflicts:[{path:'file',reason:'both_sides_changed'}]});
  const h=harness();h.driver.preview.mockRejectedValueOnce(transient).mockResolvedValue(conflict);
  h.loop.start();await vi.advanceTimersByTimeAsync(60_000);
  expect(h.driver.preview).toHaveBeenCalledTimes(2);expect(h.driver.apply).not.toHaveBeenCalled();
  expect(h.driver.update).toHaveBeenLastCalledWith('paused',conflict);
 });
 it('never retries a write even if it rejects with the preview-only marker',async()=>{
  const h=harness(preview({operations:[{kind:'upload',path:'file'}]},false));
  h.driver.apply.mockRejectedValue(transient);
  h.loop.start();await vi.advanceTimersByTimeAsync(120_000);
  expect(h.driver.preview).toHaveBeenCalledTimes(1);expect(h.driver.apply).toHaveBeenCalledTimes(1);
  expect(h.driver.update).toHaveBeenLastCalledWith('paused',undefined,transient);
 });
 it('canceling a wait and starting another project cannot leave an old retry timer',async()=>{
  const h=harness();h.driver.preview.mockRejectedValueOnce(transient);
  h.loop.start();await vi.advanceTimersByTimeAsync(0);h.loop.stop();await h.loop.settled();
  await vi.advanceTimersByTimeAsync(60_000);expect(h.driver.preview).toHaveBeenCalledTimes(1);
  const next=preview({operations:[{kind:'upload',path:'other-project-file'}]},false);
  h.driver.preview.mockResolvedValueOnce(next).mockResolvedValue(preview());
  expect(h.loop.start()).toBe(true);await vi.advanceTimersByTimeAsync(0);
  expect(h.driver.apply).toHaveBeenCalledExactlyOnceWith(next);
  h.loop.stop();await vi.advanceTimersByTimeAsync(60_000);expect(h.driver.preview).toHaveBeenCalledTimes(2);
 });
 it('does not overlap a slow retry and ignores its late result after logout',async()=>{
  const h=harness();let finish!:(value:SyncPreview)=>void;
  h.driver.preview.mockRejectedValueOnce(transient).mockImplementationOnce(()=>new Promise(resolve=>{finish=resolve;}));
  h.loop.start();await vi.advanceTimersByTimeAsync(1000);await vi.advanceTimersByTimeAsync(60_000);
  expect(h.driver.preview).toHaveBeenCalledTimes(2);
  h.loop.stop();expect(h.loop.start()).toBe(false);
  finish(preview({operations:[{kind:'upload',path:'old-project-file'}]},false));await h.loop.settled();
  await vi.advanceTimersByTimeAsync(60_000);
  expect(h.driver.apply).not.toHaveBeenCalled();expect(h.driver.preview).toHaveBeenCalledTimes(2);
 });
 it('does not arm another timer when stop is requested by the retry status callback',async()=>{
  const h=harness();h.driver.preview.mockRejectedValue(transient);
  h.driver.update.mockImplementation(state=>{if(state==='waiting')h.loop.stop();});
  h.loop.start();await vi.advanceTimersByTimeAsync(120_000);
  expect(h.driver.preview).toHaveBeenCalledTimes(1);expect(h.driver.apply).not.toHaveBeenCalled();
 });
});
