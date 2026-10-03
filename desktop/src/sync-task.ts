import { syncScheduler } from './sync-scheduler';
import { Channel } from '@tauri-apps/api/core';
import { bridge } from './bridge';

export interface SyncProgress {
 task:string;sequence:number;stage:string;path:string;operation:string;
 completed:number;total:number;bytes:number;total_bytes:number;
 file_bytes:number;file_total:number;scanned:number;scanned_bytes:number;
}
const transport={
 invoke:bridge.invoke,
 channel(callback:(event:SyncProgress)=>void){const channel=new Channel<SyncProgress>();channel.onmessage=callback;return channel;},
};
// Each invocation owns its callback. Old, canceled and unmounted tasks cannot
// repaint a new operation; acknowledging consumed events keeps native IPC bounded.
export class SyncTask {
 private generation=0;
 private alive=true;
 private running=false;
 private nativeRunning=false;
 constructor(private update:(value:SyncProgress|null)=>void,private native=transport){}
 async run<T>(command:string,args:Record<string,unknown>={}):Promise<T>{
  if(!this.alive)throw new Error('Sync view closed');
  if(this.running)throw new Error('Sync task already running');
  const generation=++this.generation;this.running=true;this.update(null);
  let sequence=0;let task='';
  const events=this.native.channel(event=>{
   // ACKs are scoped by the native task ID, so late ACKs cannot release a new task.
   void this.native.invoke('sync_progress_ack',{task:event.task,sequence:event.sequence}).catch(()=>{});
   if(!this.alive||generation!==this.generation||!this.running||event.sequence<=sequence)return;
   if(task&&event.task!==task)return;
   task=event.task;sequence=event.sequence;this.update(event);
  });
  try{return await syncScheduler.run(async()=>{
   if(!this.alive||generation!==this.generation)throw new Error('Sync task canceled before dispatch');
   this.nativeRunning=true;
   try{return await this.native.invoke<T>(command,{...args,events});}
   finally{this.nativeRunning=false;}
  });}
  finally{this.running=false;if(this.alive&&generation===this.generation){this.generation++;this.update(null);}}
 }
 async cancel(){
  if(!this.running)return;
  this.generation++;if(this.alive)this.update(null);
  if(this.nativeRunning)await this.native.invoke('sync_cancel');
 }
 close(){this.alive=false;this.generation++;if(this.nativeRunning)void this.native.invoke('sync_cancel').catch(()=>{});}
}
export const progressStages:Record<string,string>={authenticating:'正在验证连接',local_scan:'正在检查本地文件',remote_scan:'正在检查服务器文件',planning:'正在比较变更',lease:'正在取得同步租约',applying:'正在执行变更',committing:'正在提交核对结果',reviewing:'正在核对操作记录',history:'正在读取恢复记录',exporting:'正在导出原内容'};
export function byteLabel(bytes:number):string{
 if(bytes<1024)return `${bytes} B`;
 const units=['KiB','MiB','GiB'];let value=bytes/1024;let unit=0;
 while(value>=1024&&unit<units.length-1){value/=1024;unit++;}
 return `${value.toFixed(1)} ${units[unit]}`;
}
