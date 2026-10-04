<script setup lang="ts">
import { t, locale, messageRef, msg } from './i18n';
import { newTaskId } from './task-id';
import { computed, onBeforeUnmount, ref, watch } from 'vue';
import UiIcon from './UiIcon.vue';
import UiDialog from './UiDialog.vue';
import { Channel } from '@tauri-apps/api/core';
import { bridge, errorMessage } from './bridge';
import { byteLabel } from './sync-task';
const props=withDefaults(defineProps<{session:string;visible?:boolean}>(),{visible:true});
interface Entry{name:string;is_dir:boolean;size:number;mode:string;mtime:string}
const scope=ref('workspace');const path=ref('');const entries=ref<Entry[]>([]);
interface SelectedFile{ticket:string;name:string;size:number}
interface UploadPreview{confirmation:string;session:string;path:string;scope:string;name:string;size:number;replaces:boolean;archive:boolean}
const uploadPreview=ref<UploadPreview|null>(null);const uploading=ref(false);const completedUpload=messageRef('');
const busy=ref(false);const message=messageRef('');const page=ref(0);const loaded=ref(false);
const bytes=ref(0);const total=ref<number|null>(null);const downloading=ref(false);
const task=newTaskId();let generation=0;let alive=true;
const sortedEntries=computed(()=>[...entries.value].sort((a,b)=>Number(b.is_dir)-Number(a.is_dir)||a.name.localeCompare(b.name, locale.value)));
const breadcrumbs=computed(()=>path.value.split('/').filter(Boolean).map((name,index,all)=>({name,path:all.slice(0,index+1).join('/')})));
function fileIcon(entry:Entry){if(entry.is_dir)return 'folder';if(/\.(png|jpe?g|gif|webp|svg)$/i.test(entry.name))return 'file-image';if(/\.(zip|gz|tar|7z)$/i.test(entry.name))return 'file-archive';if(/\.(ts|js|vue|go|rs|py|json|html|css|yml|yaml)$/i.test(entry.name))return 'file-code';return 'file';}
function modified(value:string){const date=new Date(value);return Number.isNaN(date.getTime())?'—':date.toLocaleDateString(locale.value,{month:'2-digit',day:'2-digit'})+' '+date.toLocaleTimeString(locale.value,{hour:'2-digit',minute:'2-digit',hour12:false});}
watch(()=>props.visible,visible=>{if(visible&&!loaded.value)void load();else if(!visible&&!uploading.value)void discardPreview();},{immediate:true});
async function discardPreview(){const shown=uploadPreview.value;uploadPreview.value=null;if(shown)await bridge.invoke('discard_file_upload',{confirmation:shown.confirmation}).catch(()=>{});}
async function load(next=path.value){
 if(busy.value)return;if(next!==path.value)completedUpload.value='';busy.value=true;message.value='';await discardPreview();if(!alive)return;const request=++generation;
 try{const result=await bridge.invoke<Entry[]>('list_files',{session:props.session,path:next,scope:scope.value});if(alive&&generation===request){entries.value=result;path.value=next;page.value=0;loaded.value=true;}}
 catch(error){if(alive&&generation===request)message.value=errorMessage(error);}finally{if(alive&&generation===request)busy.value=false;}
}
function join(name:string){return path.value?`${path.value}/${name}`:name;}
async function download(entry:Entry){
 if(busy.value)return;busy.value=true;downloading.value=true;message.value='';bytes.value=0;total.value=null;const request=++generation;
 const events=new Channel<{bytes:number;total:number|null}>();events.onmessage=value=>{if(alive&&generation===request){bytes.value=value.bytes;total.value=value.total;}};
 try{const saved=await bridge.invoke<boolean>('download_file',{task,session:props.session,path:join(entry.name),scope:scope.value,events});if(alive&&request===generation)message.value=saved?msg("文件已保存"):msg("已取消保存选择");}
 catch(error){if(alive&&request===generation)message.value=errorMessage(error);}finally{if(alive&&request===generation){busy.value=false;downloading.value=false;}}
}
async function chooseUpload(){
 if(busy.value||!loaded.value)return;busy.value=true;message.value='';await discardPreview();if(!alive)return;const request=++generation;
 let files:SelectedFile[]=[];
 try{
  files=await bridge.invoke<SelectedFile[]>('choose_attachments');
  if(!alive||!props.visible||request!==generation||files.length===0)return;
  if(files.length!==1){message.value=msg("目录上传每次请选择一个文件或代码压缩包，以便逐项核对覆盖范围。");return;}
  const result=await bridge.invoke<UploadPreview>('preview_file_upload',{request:{session:props.session,path:path.value,scope:scope.value,file:files[0]}});
  if(alive&&props.visible&&request===generation)uploadPreview.value=result;else await bridge.invoke('discard_file_upload',{confirmation:result.confirmation});
 }catch(error){if(alive&&request===generation)message.value=errorMessage(error);}
 finally{if(files.length)await bridge.invoke('release_attachments',{tickets:files.map(f=>f.ticket)}).catch(()=>{});if(alive&&request===generation)busy.value=false;}
}
async function upload(){
 const shown=uploadPreview.value;if(!shown||busy.value)return;
 busy.value=true;uploading.value=true;message.value='';completedUpload.value='';bytes.value=0;total.value=shown.size;const request=++generation;
 const events=new Channel<{bytes:number;total:number|null}>();events.onmessage=value=>{if(alive&&request===generation){bytes.value=value.bytes;total.value=value.total;}};
 try{const result=await bridge.invoke<{mode:string;files:number}>('apply_file_upload',{request:{task,confirmation:shown.confirmation},events});if(alive&&request===generation)completedUpload.value=result.mode==='archive'?msg("压缩包已合并到目标目录（{p1} 个文件）", { p1: (result.files) }):msg("文件已上传到目标目录");}
 catch(error){if(alive&&request===generation)message.value=msg("{p1}；请刷新目录检查实际结果后再重试。", {p1: errorMessage(error)});}
 finally{if(alive&&request===generation){uploadPreview.value=null;uploading.value=false;busy.value=false;}}
}
async function cancel(){await bridge.invoke('cancel_attachment',{task}).catch(()=>{});}
onBeforeUnmount(()=>{alive=false;generation++;void discardPreview();if(downloading.value||uploading.value)void cancel();});
</script>
<template>
 <section class="file-browser" :aria-label="t('服务器文件')">
  <div class="file-toolbar"><div class="segmented file-scopes" :aria-label="t('文件范围')"><button :disabled="busy" :class="{active:scope==='workspace'}" :aria-pressed="scope==='workspace'" @click="scope='workspace';entries=[];loaded=false;path='';completedUpload='';load('')"><UiIcon name="folder" />{{ t("工作空间") }}</button><button :disabled="busy" :class="{active:scope==='shared'}" :aria-pressed="scope==='shared'" @click="scope='shared';entries=[];loaded=false;path='';completedUpload='';load('')"><UiIcon name="folder-shared" />{{ t("共享目录") }}</button></div><div class="file-actions"><button class="icon-button" :disabled="busy" :aria-label="t('刷新文件')" :title="t('刷新文件')" @click="load()"><UiIcon name="refresh" /></button><button class="primary" :disabled="busy||!loaded" @click="chooseUpload"><UiIcon name="upload" />{{ t("上传到此目录…") }}</button></div></div>
  <nav class="file-breadcrumbs" :aria-label="t('目录位置')"><button class="icon-button" :disabled="busy||!path" :title="t('上一级')" :aria-label="t('上一级')" @click="load(path.split('/').slice(0,-1).join('/'))"><UiIcon name="arrow-left" :size="15" /></button><button class="breadcrumb-root" :disabled="busy" @click="load('')">{{ scope==='shared'?'shared':'workspace' }}</button><template v-for="crumb in breadcrumbs" :key="crumb.path"><UiIcon name="chevron-right" :size="12" /><button :disabled="busy" @click="load(crumb.path)">{{ crumb.name }}</button></template></nav>
  <div class="file-table-head"><span>{{ t("名称") }}</span><span>{{ t("大小") }}</span><span class="modified-column">{{ t("修改时间") }}</span><span class="sr-only">{{ t("操作") }}</span></div>
  <ul class="file-list"><li v-for="entry in sortedEntries.slice(page*50,(page+1)*50)" :key="entry.name"><UiIcon :name="fileIcon(entry)" :size="18" :class="{'folder-icon':entry.is_dir}" /><button v-if="entry.is_dir" class="file-name ghost" :disabled="busy" @click="load(join(entry.name))">{{ entry.name }}</button><span v-else class="file-name" :title="entry.name">{{ entry.name }}</span><small class="file-size">{{ entry.is_dir?'—':byteLabel(entry.size) }}</small><small class="modified-column">{{ modified(entry.mtime) }}</small><button v-if="!entry.is_dir" class="icon-button file-download" :disabled="busy||entry.size>64*1024*1024" :aria-label="t('下载 ')+entry.name" :title="entry.size>64*1024*1024?t('文件超过下载大小限制'):t('下载文件')" @click="download(entry)"><UiIcon name="download" :size="15" /></button><UiIcon v-else name="chevron-right" class="file-chevron" :size="14" /></li></ul>
  <div v-if="loaded&&!entries.length&&!busy" class="file-empty"><UiIcon name="folder" :size="32" /><h3>{{ t("此目录为空") }}</h3><p>{{ t("上传文件或代码压缩包到当前目录。") }}</p><button :disabled="busy" @click="chooseUpload"><UiIcon name="upload" />{{ t("上传文件") }}</button></div>
  <div class="file-footer"><span>{{ loaded?entries.length+t(" 个项目"):busy?t("正在读取目录…"):t("目录未加载") }}</span><div v-if="entries.length>50" class="file-pagination"><button class="icon-button" :disabled="busy||page===0" :aria-label="t('文件上一页')" @click="page--"><UiIcon name="chevron-left" /></button><span>{{ page+1 }} / {{ Math.ceil(entries.length/50) }}</span><button class="icon-button" :disabled="busy||(page+1)*50>=entries.length" :aria-label="t('文件下一页')" @click="page++"><UiIcon name="chevron-right" /></button></div></div>
  <div v-if="completedUpload" class="inline-notice success" role="status"><UiIcon name="check" /><span>{{ completedUpload }}</span><button class="ghost" :disabled="busy" @click="load()">{{ t("刷新目录") }}</button></div>
  <div v-if="busy" class="inline-notice" role="status"><span class="spin" /><span>{{ downloading?t("正在下载"):uploading?t("正在上传"):t("正在读取目录或准备文件") }}<template v-if="downloading||uploading"> · {{ byteLabel(bytes) }}<span v-if="total!==null"> / {{ byteLabel(total) }}</span></template></span><button v-if="downloading||uploading" class="ghost" @click="cancel">{{ t("取消传输") }}</button></div>
  <p v-if="message" class="inline-notice" role="status"><UiIcon name="info" />{{ message }}</p>
  <UiDialog :open="visible&&!!uploadPreview" :title="t('确认上传')" :busy="busy" @close="discardPreview">
   <div v-if="uploadPreview" role="alertdialog" :aria-label="t('确认目录上传')" class="upload-confirmation">
    <div class="upload-file-summary"><UiIcon :name="uploadPreview.archive?'file-archive':'file'" :size="25" /><div><strong>{{ uploadPreview.name }}</strong><small>{{ byteLabel(uploadPreview.size) }}</small></div></div>
    <p>{{ t("上传到") }} <code>/{{ uploadPreview.scope==='shared'?'shared':'workspace' }}/{{ uploadPreview.path }}</code></p>
    <p v-if="uploadPreview.archive" class="inline-notice warning">{{ t("压缩包会解压并合并到目录，同名文件可能被覆盖。") }}</p><p v-else-if="uploadPreview.replaces" class="inline-notice warning">{{ t("目标已有同名项，上传将覆盖该文件。请先下载需要保留的版本。") }}</p><p v-else class="muted">{{ t("预览时目标没有同名项，执行前会再次检查目录。") }}</p>
    <p class="muted">{{ t("上传时请避免同时编辑同一文件。中断后可能已有部分内容写入，此操作不会创建同步恢复副本。") }}</p>
    <p v-if="uploading" role="status">{{ t("正在上传 · {p1} / {p2}", { p1: (byteLabel(bytes)), p2: (byteLabel(total||0)) }) }}</p>
    <div class="dialog-actions"><button v-if="uploading" @click="cancel">{{ t("取消传输") }}</button><button v-else :disabled="busy" @click="discardPreview">{{ t("返回") }}</button><button class="primary" :disabled="busy" @click="upload"><UiIcon name="upload" />{{ t("确认上传{p1}", { p1: (uploadPreview.archive?t("并合并"):uploadPreview.replaces?t("并覆盖"):'') }) }}</button></div>
   </div>
  </UiDialog>
 </section>
</template>
