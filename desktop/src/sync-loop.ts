import type { SyncPreview } from './bridge';

export type LoopState = 'stopped'|'checking'|'waiting'|'paused';
export interface LoopDriver {
 preview:()=>Promise<SyncPreview>;
 apply:(preview:SyncPreview)=>Promise<void>;
 update:(state:LoopState,preview?:SyncPreview,error?:unknown)=>void;
}

// Opt-in, application-view lifetime. No saved enable flag, hidden system
// service, overlapping rounds or automatic retries of failed mutations.
export class SyncLoop {
 private generation=0;
 private enabled=false;
 private timer:ReturnType<typeof setTimeout>|undefined;
 private current:Promise<void>|undefined;
 private idleDelay=1000;
 constructor(private driver:LoopDriver){}
 start():boolean {
  if(this.enabled||this.current)return false;
  this.enabled=true;this.idleDelay=1000;
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
  this.timer=setTimeout(()=>{
   this.timer=undefined;
   this.current=this.round(generation).finally(()=>{this.current=undefined;});
  },delay);
 }
 private async round(generation:number){
  if(!this.active(generation))return;
  this.driver.update('checking');
  try {
   const preview=await this.driver.preview();
   if(!this.active(generation))return;
   if(preview.plan.conflicts.length||preview.plan.needs_confirmation){
    this.stop();this.driver.update('paused',preview);return;
   }
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
