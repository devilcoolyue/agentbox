import type { SyncPreview } from './bridge';

export type LoopState = 'stopped'|'checking'|'waiting'|'paused';
export interface PreviewRetry { attempt: number; limit: number; delayMs: number }
export interface LoopDriver {
 preview:()=>Promise<SyncPreview>;
 apply:(preview:SyncPreview)=>Promise<void>;
 update:(state:LoopState,preview?:SyncPreview,error?:unknown,retry?:PreviewRetry)=>void;
}

const previewRetryDelays = [1000, 2000, 4000, 8000];
function retryablePreview(error: unknown): boolean {
 // This kind is emitted only by the native sync_preview path after the Go
 // transport classifies a transient connection/read/HTTP failure. A generic
 // network error may describe TLS, IPC or a different phase and is not enough.
 return typeof error === 'object' && error !== null && 'kind' in error && error.kind === 'sync_preview_retryable';
}

// Opt-in, application-view lifetime. Retrying a read always builds a fresh
// preview; an apply failure is never replayed, even if its error looks transient.
export class SyncLoop {
 private generation=0;
 private enabled=false;
 private timer:ReturnType<typeof setTimeout>|undefined;
 private current:Promise<void>|undefined;
 private idleDelay=1000;
 private previewFailures=0;
 constructor(private driver:LoopDriver){}
 start():boolean {
  if(this.enabled||this.current)return false;
  this.enabled=true;this.idleDelay=1000;this.previewFailures=0;
  this.schedule(++this.generation,0);
  return true;
 }
 stop(){
  this.enabled=false;this.generation++;
  clearTimeout(this.timer);this.timer=undefined;
 }
 async settled(){await this.current;}
 private active(generation:number){return this.enabled&&this.generation===generation;}
 private schedule(generation:number,delay:number){
  if(!this.active(generation))return;
  this.timer=setTimeout(()=>{
   this.timer=undefined;
   this.current=this.round(generation).finally(()=>{this.current=undefined;});
  },delay);
 }
 private async round(generation:number){
  if(!this.active(generation))return;
  this.driver.update('checking');
  let preview:SyncPreview;
  try {
   preview=await this.driver.preview();
  }catch(error){
   if(!this.active(generation))return;
   if(retryablePreview(error)&&this.previewFailures<previewRetryDelays.length){
    const delayMs=previewRetryDelays[this.previewFailures++];
    this.driver.update('waiting',undefined,error,{attempt:this.previewFailures,limit:previewRetryDelays.length,delayMs});
    this.schedule(generation,delayMs);
   }else{
    this.stop();this.driver.update('paused',undefined,error);
   }
   return;
  }
  if(!this.active(generation))return;
  this.previewFailures=0;
  if(preview.plan.conflicts.length||preview.plan.needs_confirmation){
   this.stop();this.driver.update('paused',preview);return;
  }
  try {
   const unchanged=preview.plan.operations.length===0&&preview.baseline_current;
   if(!unchanged)await this.driver.apply(preview);
   if(!this.active(generation))return;
   this.idleDelay=unchanged?Math.min(this.idleDelay*2,30_000):1000;
   this.driver.update('waiting');
   this.schedule(generation,this.idleDelay);
  }catch(error){
   if(!this.active(generation))return;
   this.stop();this.driver.update('paused',undefined,error);
  }
 }
}
