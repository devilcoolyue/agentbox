<script setup lang="ts">
import { newTaskId } from './task-id';
import { onBeforeUnmount, ref } from 'vue';
import { Channel } from '@tauri-apps/api/core';
import { bridge, errorMessage } from './bridge';
import { byteLabel } from './sync-task';
const props=defineProps<{session:string}>();
interface Entry{name:string;is_dir:boolean;size:number;mode:string;mtime:string}
const scope=ref('workspace');const path=ref('');const entries=ref<Entry[]>([]);
interface SelectedFile{ticket:string;name:string;size:number}
interface UploadPreview{confirmation:string;session:string;path:string;scope:string;name:string;size:number;replaces:boolean;archive:boolean}
const uploadPreview=ref<UploadPreview|null>(null);const uploading=ref(false);const completedUpload=ref('');
const busy=ref(false);const message=ref('');const page=ref(0);const loaded=ref(false);
const bytes=ref(0);const total=ref<number|null>(null);const downloading=ref(false);
const task=newTaskId();let generation=0;let alive=true;
async function discardPreview(){const shown=uploadPreview.value;uploadPreview.value=null;if(shown)await bridge.invoke('discard_file_upload',{confirmation:shown.confirmation}).catch(()=>{});}
async function load(next=path.value){
 if(busy.value)return;busy.value=true;message.value='';await discardPreview();if(!alive)return;const request=++generation;
 try{const result=await bridge.invoke<Entry[]>('list_files',{session:props.session,path:next,scope:scope.value});if(alive&&generation===request){entries.value=result;path.value=next;page.value=0;loaded.value=true;}}
 catch(error){if(alive&&generation===request)message.value=errorMessage(error);}finally{if(alive&&generation===request)busy.value=false;}
}
function join(name:string){return path.value?`${path.value}/${name}`:name;}
async function download(entry:Entry){
 if(busy.value)return;busy.value=true;downloading.value=true;message.value='';bytes.value=0;total.value=null;const request=++generation;
 const events=new Channel<{bytes:number;total:number|null}>();events.onmessage=value=>{if(alive&&generation===request){bytes.value=value.bytes;total.value=value.total;}};
 try{const saved=await bridge.invoke<boolean>('download_file',{task,session:props.session,path:join(entry.name),scope:scope.value,events});if(alive&&request===generation)message.value=saved?'文件已保存':'已取消保存选择';}
 catch(error){if(alive&&request===generation)message.value=errorMessage(error);}finally{if(alive&&request===generation){busy.value=false;downloading.value=false;}}
}
async function chooseUpload(){
 if(busy.value||!loaded.value)return;busy.value=true;message.value='';await discardPreview();if(!alive)return;const request=++generation;
 let files:SelectedFile[]=[];
 try{
  files=await bridge.invoke<SelectedFile[]>('choose_attachments');
  if(!alive||request!==generation||files.length===0)return;
  if(files.length!==1){message.value='目录上传每次请选择一个文件或代码压缩包，以便逐项核对覆盖范围。';return;}
  const result=await bridge.invoke<UploadPreview>('preview_file_upload',{request:{session:props.session,path:path.value,scope:scope.value,file:files[0]}});
  if(alive&&request===generation)uploadPreview.value=result;else await bridge.invoke('discard_file_upload',{confirmation:result.confirmation});
 }catch(error){if(alive&&request===generation)message.value=errorMessage(error);}
 finally{if(files.length)await bridge.invoke('release_attachments',{tickets:files.map(f=>f.ticket)}).catch(()=>{});if(alive&&request===generation)busy.value=false;}
}
async function upload(){
 const shown=uploadPreview.value;if(!shown||busy.value)return;
 busy.value=true;uploading.value=true;message.value='';completedUpload.value='';bytes.value=0;total.value=shown.size;const request=++generation;
 const events=new Channel<{bytes:number;total:number|null}>();events.onmessage=value=>{if(alive&&request===generation){bytes.value=value.bytes;total.value=value.total;}};
 try{const result=await bridge.invoke<{mode:string;files:number}>('apply_file_upload',{request:{task,confirmation:shown.confirmation},events});if(alive&&request===generation)completedUpload.value=result.mode==='archive'?`压缩包已合并到目标目录（${result.files} 个文件）`:'文件已上传到目标目录';}
 catch(error){if(alive&&request===generation)message.value=errorMessage(error)+'；请刷新目录检查实际结果后再重试。';}
 finally{if(alive&&request===generation){uploadPreview.value=null;uploading.value=false;busy.value=false;}}
}
async function cancel(){await bridge.invoke('cancel_attachment',{task}).catch(()=>{});}
onBeforeUnmount(()=>{alive=false;generation++;void discardPreview();if(downloading.value||uploading.value)void cancel();});
</script>
<template>
 <details class="file-browser" @toggle="!loaded&&($event.target as HTMLDetailsElement).open&&load()">
  <summary>服务器文件</summary>
  <div class="file-toolbar"><select v-model="scope" :disabled="busy" aria-label="文件范围" @change="entries=[];loaded=false;path='';load('')"><option value="workspace">工作空间</option><option value="shared">共享目录</option></select><button :disabled="busy" @click="load()">刷新文件</button><button :disabled="busy||!loaded" @click="chooseUpload">上传到此目录…</button><button :disabled="busy||!path" @click="load(path.split('/').slice(0,-1).join('/'))">上一级</button></div>
  <code>/{{ scope==='shared'?'shared':'workspace' }}/{{ path }}</code>
  <div v-if="uploadPreview" role="alertdialog" aria-label="确认目录上传">
   <p>将 {{ uploadPreview.name }}（{{ byteLabel(uploadPreview.size) }}）上传到 /{{ uploadPreview.scope==='shared'?'shared':'workspace' }}/{{ uploadPreview.path }}。</p>
   <p v-if="uploadPreview.archive">服务器会解压并合并此压缩包。同名文件可能被覆盖；目录内原有的其他文件保留。合并失败或取消时可能已完成部分修改。</p>
   <p v-else-if="uploadPreview.replaces">目标已有同名项，上传可能覆盖该文件，请先下载备份。</p>
   <p v-else>预览时目标没有同名项。执行前会再次检查目录。</p>
   <p>旧上传接口无法锁住外部编辑，最终检查后仍可能出现并发修改；此操作不生成同步恢复副本。</p>
   <button :disabled="busy" @click="upload">确认上传{{ uploadPreview.archive?'并合并':uploadPreview.replaces?'并覆盖':'' }}</button><button :disabled="busy" @click="discardPreview">返回</button>
  </div>
  <p v-if="completedUpload" role="status">{{ completedUpload }} <button :disabled="busy" @click="load()">刷新目录</button></p>
  <ul class="file-list"><li v-for="entry in entries.slice(page*50,(page+1)*50)" :key="entry.name"><button v-if="entry.is_dir" :disabled="busy" @click="load(join(entry.name))">📁 {{ entry.name }}</button><template v-else><span>{{ entry.name }}</span><small>{{ byteLabel(entry.size) }}</small><button :disabled="busy||entry.size>64*1024*1024" @click="download(entry)">下载</button></template></li></ul>
  <p v-if="loaded&&!entries.length&&!busy">此目录为空。</p>
  <div v-if="entries.length>50"><button :disabled="busy||page===0" @click="page--">上一页</button> {{ page+1 }} / {{ Math.ceil(entries.length/50) }} <button :disabled="busy||(page+1)*50>=entries.length" @click="page++">下一页</button></div>
  <p v-if="busy" role="status">{{ downloading?'正在下载':uploading?'正在上传':'正在读取目录或准备文件' }}<template v-if="downloading||uploading"> · {{ byteLabel(bytes) }}<span v-if="total!==null"> / {{ byteLabel(total) }}</span> <button @click="cancel">取消传输</button></template></p>
  <p v-if="message" role="status">{{ message }}</p>
 </details>
</template>
<style scoped>
.file-browser{padding:12px;border-top:1px solid var(--border);font-size:12px}.file-toolbar{display:flex;flex-wrap:wrap;gap:4px;margin:10px 0}.file-browser code{overflow-wrap:anywhere}.file-list{list-style:none;padding:0;max-height:300px;overflow:auto}.file-list li{display:flex;align-items:center;gap:6px;padding:5px 0}.file-list li>span{flex:1;overflow-wrap:anywhere}.file-list button{text-align:left}.file-list small{color:var(--muted)}
</style>
