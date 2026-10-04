import { t } from './i18n';
// All project views share one native sidecar slot. Queueing belongs to the
// renderer; Rust still enforces its own exclusive lock. A canceled queued job
// checks its owner before dispatch, so it never cancels another project's IPC.
export class SyncScheduler {
 private active=false;
 private queue:(()=>void)[]=[];
 run<T>(work:()=>Promise<T>):Promise<T>{
  if(this.queue.length>=64)return Promise.reject(new Error(t('等待同步的任务过多，请稍后重试')));
  return new Promise<T>((resolve,reject)=>{
   const start=()=>{
    this.active=true;
    let result:Promise<T>;
    try{result=work();}catch(error){result=Promise.reject(error);}
    result.then(resolve,reject).finally(()=>{
     const next=this.queue.shift();
     if(next)next();else this.active=false;
    });
   };
   if(this.active)this.queue.push(start);else start();
  });
 }
}
export const syncScheduler=new SyncScheduler();
