/** A login owns requests and cleanup. Explicit writes are serialized; cancelling
 * the client never claims that an already submitted server write rolled back. */
export class SettingsRequests {
 private lifetime=new AbortController();
 private reading?:AbortController;
 private revision=0;
 private writes:Promise<unknown>=Promise.resolve();
 private cleanups=new Set<()=>void>();
 constructor(private send:<T>(path:string,options?:RequestInit)=>Promise<T>,private owns:()=>boolean){}
 current(){return !this.lifetime.signal.aborted&&this.owns();}
 cleanup(fn:()=>void){this.cleanups.add(fn);return ()=>this.cleanups.delete(fn);}
 dispose(){
  this.lifetime.abort();this.reading?.abort();++this.revision;
  for(const cleanup of this.cleanups)cleanup();this.cleanups.clear();
 }
 async request<T>(path:string,options:RequestInit={}):Promise<T|undefined>{
  if(!this.current())return;
  try{
   const value=await this.send<T>(path,{...options,signal:options.signal?AbortSignal.any([options.signal,this.lifetime.signal]):this.lifetime.signal});
   return this.current()?value:undefined;
  }catch(error){if(this.current()&&!options.signal?.aborted)throw error;}
 }
 async read<T>():Promise<T|undefined>{
  this.reading?.abort();const controller=new AbortController();this.reading=controller;
  const revision=++this.revision;
  const value=await this.request<T>('/settings',{signal:controller.signal});
  return revision===this.revision?value:undefined;
 }
 save<T>(body:string):Promise<T|undefined>{
  ++this.revision;this.reading?.abort();
  const operation=this.writes.catch(()=>{}).then(async()=>{
   ++this.revision;this.reading?.abort();
   try{return await this.request<T>('/settings',{method:'PUT',body});}
   finally{++this.revision;this.reading?.abort();}
  });
  this.writes=operation;return operation;
 }
}
