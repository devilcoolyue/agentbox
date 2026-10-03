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
});
