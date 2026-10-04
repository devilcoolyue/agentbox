<script setup lang="ts">
import UiIcon from './UiIcon.vue';
import { SyncLoop, type LoopState } from './sync-loop';
import { SyncTask, progressStages, byteLabel, type SyncProgress } from './sync-task';
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { bridge, errorMessage, type Project, type Session, type SyncBinding, type SyncPreview, type SyncReview, type RecoveryPage, type AbandonReview, type RemoteCleanupReview } from './bridge';

const props=defineProps<{session:Session}>();
const emit=defineEmits<{(event:'sync-status',value:string):void}>();
const projects=ref<Project[]>([]);
const bindings=ref<SyncBinding[]>([]);
const selected=ref('');
const historical=computed(()=>bindings.value.filter(item=>item.archived||item.server_changed||!projects.value.some(project=>project.id===item.binding.project)));
const binding=computed(()=>bindings.value.find(item=>selected.value==='binding:'+item.id||(!item.archived&&!item.server_changed&&item.binding.project===selected.value))||null);
const archiving=ref<SyncBinding|null>(null);
const preview=ref<SyncPreview|null>(null);
const direction=ref('automatic');
const choices=ref<Record<string,'local'|'remote'|''>>({});
let choiceBasis='';
const review=ref<SyncReview|null>(null);
const history=ref<RecoveryPage|null>(null);
const historyCursors=ref<string[]>([]);
const cleaning=ref<{bindingId:string;batchId:string;revision:number}|null>(null);
const discarding=ref<{bindingId:string;batchId:string;operationId:string;revision:number;path:string}|null>(null);
const remoteCleaning=ref<RemoteCleanupReview|null>(null);
const remoteCleanupBytes=computed(()=>remoteCleaning.value?.items.reduce((total,item)=>total+(item.state==='retired'?0:item.bytes),0)||0);
const resolving=ref<'finish'|'replan'|null>(null);
const abandoning=ref<AbandonReview|null>(null);
const abandonPhrase=ref('');
const busy=ref(false);
const message=ref('');
let alive=true;
const progress=ref<SyncProgress|null>(null);
const task=new SyncTask(value=>{progress.value=value;});
const loopState=ref<LoopState>('stopped');
const loopActive=computed(()=>loopState.value==='checking'||loopState.value==='waiting');
const loop=new SyncLoop({
 preview:async()=>{
  if(!binding.value)throw new Error('Binding unavailable');
  const result=await task.run<{preview:SyncPreview}>('sync_preview',{bindingId:binding.value.id,direction:'automatic'});
  return result.preview;
 },
 apply:async(value)=>{
  if(!binding.value)throw new Error('Binding unavailable');
  await task.run('sync_apply',{bindingId:binding.value.id,preview:value,confirmation:value.plan.digest});
 },
 update:(state,value,error,retry)=>{
  if(!alive)return;
  loopState.value=state;emit('sync-status',state==='paused'?'同步待处理':state==='checking'?'同步中':'持续同步');
  if(state==='checking')message.value='';
  if(retry)message.value=`连接暂时不可用，${retry.delayMs/1000} 秒后重新检查（重试 ${retry.attempt}/${retry.limit}）。`;
  if(state==='paused'){
   if(value){preview.value=value;choiceBasis=value.plan.digest;}
   message.value=error?errorMessage(error):'持续同步已暂停，请手动核对冲突或需要确认的变更。';
   void load().catch(()=>{}).finally(()=>{if(alive)busy.value=false;});
  }
 },
});
function startLoop(){
 if(busy.value||!binding.value||binding.value.pending||binding.value.archived||binding.value.server_changed)return;
 clearDialogs();preview.value=null;choices.value={};history.value=null;message.value='';direction.value='automatic';busy.value=true;
 if(loop.start()){loopState.value='checking';emit('sync-status','持续同步');}else busy.value=false;
}
async function stopLoop(){
 loop.stop();loopState.value='stopped';emit('sync-status','');
 try{await task.cancel();await loop.settled();if(alive){message.value='持续同步已停止。中断的批次需先核对再继续。';await load();}}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive)busy.value=false;}
}
onBeforeUnmount(()=>{alive=false;loop.stop();task.close();});
async function load(){
 const ps=await bridge.invoke<Project[]>('list_projects',{session:props.session.id});
 if(!alive)return;
 const result=await task.run<{bindings?:SyncBinding[]}>('sync_list');
 if(!alive)return;
 projects.value=ps;bindings.value=(result.bindings||[]).filter(item=>item.binding.workspace===props.session.id);
 if(!selected.value)selected.value=ps[0]?.id||(bindings.value[0]?'binding:'+bindings.value[0].id:'');
}
async function refresh(){clearDialogs();busy.value=true;message.value='';review.value=null;preview.value=null;history.value=null;try{await load();}catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive)busy.value=false;}}
async function bind(){
 const project=projects.value.find(item=>item.id===selected.value);if(!project)return;
 busy.value=true;message.value='';
 try{const result=await task.run<{binding:SyncBinding}|null>('sync_bind',{session:props.session.id,project:project.id,projectPath:project.path});if(alive&&result){bindings.value.push(result.binding);preview.value=null;}}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive)busy.value=false;}
}
async function check(){
 if(!binding.value)return;clearDialogs();busy.value=true;message.value='';preview.value=null;choices.value={};
 try{const result=await task.run<{preview:SyncPreview}>('sync_preview',{bindingId:binding.value.id,direction:direction.value});if(alive){preview.value=result.preview;choiceBasis=result.preview.plan.digest;}}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive)busy.value=false;}
}
async function previewChoices(){
 if(!binding.value||!preview.value)return;
 const selectedChoices=Object.fromEntries(Object.entries(choices.value).filter(([,choice])=>choice));
 if(!Object.keys(selectedChoices).length)return;
 busy.value=true;message.value='';
 try{const result=await task.run<{preview:SyncPreview}>('sync_preview',{bindingId:binding.value.id,direction:'automatic',choices:selectedChoices,basisDigest:choiceBasis});if(alive){preview.value=result.preview;}}
 catch(err){if(alive){preview.value=null;choices.value={};message.value=errorMessage(err);}}finally{if(alive)busy.value=false;}
}
async function apply(){
 if(!binding.value||!preview.value)return;busy.value=true;message.value='';
 try{await task.run('sync_apply',{bindingId:binding.value.id,preview:preview.value,confirmation:preview.value.plan.digest});if(alive)message.value='同步完成';}
 catch(err){if(alive)message.value=errorMessage(err);}
 finally{if(alive){preview.value=null;try{await load();}catch{/* Keep the outcome and reload on explicit refresh. */}busy.value=false;}}
}
async function reviewAbandon(){
 if(!binding.value)return;clearDialogs();busy.value=true;message.value='';
 try{const result=await task.run<{abandon:AbandonReview}>('sync_abandon_review',{bindingId:binding.value.id});if(alive)abandoning.value=result.abandon;}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive)busy.value=false;}
}
async function abandonPending(){
 const shown=abandoning.value;if(!shown||abandonPhrase.value!=='归档未核验批次')return;
 busy.value=true;message.value='';
 try{await task.run('sync_abandon',{bindingId:shown.binding_id,confirmation:shown.digest});if(alive){resetSelection();await load();selected.value='binding:'+shown.binding_id;message.value='绑定和未核验批次已归档，结果未知，文件与恢复引用均保留。重新绑定后必须重新确认来源。';}}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive){busy.value=false;abandoning.value=null;abandonPhrase.value='';}}
}
async function inspectPending(){
 if(!binding.value)return;clearDialogs();busy.value=true;message.value='';review.value=null;
 try{const result=await task.run<{review:SyncReview}>('sync_review',{bindingId:binding.value.id});if(alive)review.value=result.review;}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive)busy.value=false;}
}
async function resolvePending(){
 if(!binding.value||!review.value||!resolving.value)return;
 const action=resolving.value;busy.value=true;message.value='';
 try{await task.run('sync_resolve',{bindingId:binding.value.id,confirmation:review.value.digest,action});if(alive){message.value=action==='finish'?'核对完成，同步基线已提交':'旧批次已结束，原基线和恢复记录已保留。请重新检查变更。';review.value=null;await load();}}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive){busy.value=false;resolving.value=null;}}
}
async function listRecovery(cursor='',previous=false){
 if(!binding.value)return false;clearDialogs();busy.value=true;message.value='';
 try{const result=await task.run<{page:RecoveryPage}>('sync_history',{bindingId:binding.value.id,cursor});if(alive){
  history.value=result.page;
  if(previous)historyCursors.value.pop();else if(!cursor)historyCursors.value=[''];else historyCursors.value.push(cursor);
  return true;
 }return false;}
 catch(err){if(alive)message.value=errorMessage(err);return false;}finally{if(alive)busy.value=false;}
}
async function cleanupHistory(){
 const shown=cleaning.value;if(!shown)return;
 busy.value=true;message.value='';
 try{await task.run('sync_history_cleanup',shown);if(alive){cleaning.value=null;await load();await listRecovery();message.value='已清理这条本机历史，仅释放本机元数据和历史条目容量。服务器永久执行收据保留。';}}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive){busy.value=false;cleaning.value=null;}}
}
async function discardRecovery(){
 const shown=discarding.value;if(!shown)return;
 busy.value=true;message.value='';
 let failure='';
 try{await task.run('sync_recovery_discard',{bindingId:shown.bindingId,batchId:shown.batchId,operationId:shown.operationId,revision:shown.revision});}
 catch(err){failure=errorMessage(err);}
 finally{if(alive){
  discarding.value=null;
  // Disposal intent can commit before an error/cancel. Reload its revision
  // and resumable state rather than offering a stale confirmation again.
  try{await load();if(!await listRecovery())failure ||= '无法重新加载恢复历史';}catch(err){failure ||= errorMessage(err);}
  message.value=failure||'本地恢复副本已清理，历史引用保留。当前项目文件和同步基线未修改。';busy.value=false;
 }}
}
async function reviewRemoteCleanup(batchId:string){
 if(!binding.value||!history.value||history.value.remote_storage_status!=='available')return;
 const bindingId=binding.value.id,revision=history.value.revision;
 clearDialogs();busy.value=true;message.value='';
 try{
  const result=await task.run<{cleanup:RemoteCleanupReview}>('sync_remote_cleanup_review',{bindingId,batchId,revision});
  if(alive)remoteCleaning.value=result.cleanup;
 }catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive)busy.value=false;}
}
async function cleanupRemote(){
 const shown=remoteCleaning.value;if(!shown)return;
 busy.value=true;message.value='';
 let failure='';
 try{await task.run('sync_remote_cleanup',{bindingId:shown.binding_id,batchId:shown.batch_id,revision:shown.revision,confirmation:shown.digest});}
 catch(err){failure=errorMessage(err);}
 finally{if(alive){
  // Server operations may already be retired when a response is interrupted.
  // Never reuse the old confirmation; reload and preview remaining work.
  remoteCleaning.value=null;history.value=null;
  try{await load();if(!await listRecovery())failure ||= '无法重新加载恢复历史';}catch(err){failure ||= errorMessage(err);}
  message.value=failure?`${failure}。清理可能已部分完成；请重新加载历史并预览服务器清理后续做。`:'服务器清理已完成，原内容已永久删除。执行收据保留以防重复执行，当前文件和同步基线保留。';
  busy.value=false;
 }}
}
async function exportRecovery(batchId:string,operationId:string){
 if(!binding.value)return;clearDialogs();busy.value=true;message.value='';
 try{const result=await task.run<{filename:string}|null>('sync_export',{bindingId:binding.value.id,batchId,operationId});if(alive&&result)message.value=`副本已导出：${result.filename}`;}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive)busy.value=false;}
}
function clearDialogs(){cleaning.value=null;discarding.value=null;remoteCleaning.value=null;abandoning.value=null;abandonPhrase.value='';archiving.value=null;resolving.value=null;}
function openHistoryCleanup(batchId:string){
 if(!binding.value||!history.value)return;clearDialogs();
 cleaning.value={bindingId:binding.value.id,batchId,revision:history.value.revision};
}
function openDiscardRecovery(batchId:string,operationId:string,path:string){
 if(!binding.value||!history.value)return;clearDialogs();
 discarding.value={bindingId:binding.value.id,batchId,operationId,revision:history.value.revision,path};
}
function openArchive(){if(!binding.value)return;clearDialogs();archiving.value={...binding.value};}
function openResolve(action:'finish'|'replan'){clearDialogs();resolving.value=action;}
function resetSelection(){loop.stop();emit('sync-status','');clearDialogs();loopState.value='stopped';preview.value=null;review.value=null;history.value=null;message.value='';}
async function archiveBinding(){
 const shown=archiving.value;if(!shown)return;
 busy.value=true;message.value='';
 try{await task.run('sync_archive',{bindingId:shown.id,revision:shown.revision});if(alive){resetSelection();await load();selected.value='binding:'+shown.id;message.value='已解除绑定，文件、旧基线和恢复记录均已保留。';}}
 catch(err){if(alive)message.value=errorMessage(err);}finally{if(alive){busy.value=false;archiving.value=null;}}
}
function chooseRebind(){
 const project=binding.value?.binding.project;if(!project)return;
 resetSelection();selected.value=project;
}
const observations:Record<string,string>={before:'目标仍是操作前的内容',after:'目标符合计划结果',diverged:'目标出现其他变化'};
const receipts:Record<string,string>={local:'本地操作',not_attempted:'尚未发送',missing:'服务器无此操作记录',applied:'服务器已记录成功',uncertain:'服务器结果未确定',retiring:'服务器副本清理中',retired:'服务器副本已清理'};
async function cancel(){if(loopState.value==='checking'||loopState.value==='waiting'){await stopLoop();return;}try{await task.cancel();}catch(err){if(alive)message.value=errorMessage(err);}}
const operations:Record<string,string>={replace:'写入服务器文件',delete:'删除服务器文件',mkdir:'新建服务器目录',rmdir:'移除服务器空目录',upload:'上传',download:'下载',delete_local:'删除本地文件',delete_remote:'删除服务器文件',mkdir_local:'新建本地目录',mkdir_remote:'新建服务器目录',rmdir_local:'移除本地空目录',rmdir_remote:'移除服务器空目录'};
const conflicts:Record<string,string>={both_sides_changed:'两端都有修改',initial_source_required:'首次同步需选择来源',file_directory_type_change:'文件和目录类型不同',directory_contains_retained_changes:'目录内仍有需保留的变更',baseline_not_converged:'两端基线尚未一致'};
onMounted(refresh);
</script>
<template>
 <section class="sync-workspace">
  <div class="sync-heading"><strong><UiIcon name="folder" :size="18" />同步项目</strong><button :disabled="busy" @click="refresh"><UiIcon name="refresh" />刷新</button></div>
  <label class="sync-project-selector"><span>服务器项目</span><select v-model="selected" :disabled="busy" @change="resetSelection" aria-label="同步项目"><option value="" disabled>选择服务器项目</option><option v-for="project in projects" :key="project.id" :value="project.id">{{ project.name }} · {{ project.path }}</option><option v-for="item in historical" :key="item.id" :value="'binding:'+item.id">{{ item.archived?'已归档':item.server_changed?'服务器身份已变化':'已移除项目' }} · {{ item.binding.project_path }} · {{ item.id.slice(-6) }}</option></select></label>
  <p v-if="!binding&&bindings.some(item=>!item.archived&&item.server_changed&&item.binding.project===selected)" role="alert">此项目有旧服务器的绑定。请从项目列表选择“服务器身份已变化”记录，核对并解除旧绑定后再继续。</p>
  <button v-if="!binding" :disabled="busy||!selected" @click="bind"><UiIcon name="folder-plus" />选择本地目录并绑定</button>
  <template v-else>
   <div class="sync-directory"><UiIcon name="folder" :size="20" /><div><span class="sync-directory-label">本地目录</span><code class="sync-path">{{ binding.directory }}</code></div></div>
   <p v-if="binding.server_changed&&binding.pending" role="alert">旧实例仍有未核对批次，当前不能解绑或重新同步。可以恢复旧实例与目录完成核对，或显式归档整个绑定并保留未知结果。</p>
   <p v-if="binding.server_changed" role="alert">此地址的服务器身份已变化。旧基线不会用于新实例；服务器上的旧副本不能从新实例读取。</p>
   <p v-if="binding.archived" role="status">此绑定已归档，可以查看、导出恢复记录，并清理符合条件的历史。</p>
   <div class="sync-binding-actions">
    <button v-if="!binding.archived" :disabled="busy||binding.pending" @click="openArchive"><UiIcon name="unlink" />解除绑定</button>
    <button v-if="binding.archived&&projects.some(p=>p.id===binding?.binding.project)" :disabled="busy" @click="chooseRebind"><UiIcon name="link" />重新绑定此项目</button>
   </div>
   <div v-if="archiving" class="sync-confirm" role="alertdialog" aria-label="确认解除同步绑定">
    <p>解除 {{ archiving.directory }} 的同步绑定？当前文件、旧基线和恢复记录会保留。重新绑定需选择目录并重新预览；不会自动沿用旧基线。</p>
    <button :disabled="busy" @click="archiveBinding"><UiIcon name="unlink" />确认解除绑定</button><button :disabled="busy" @click="archiving=null"><UiIcon name="arrow-left" />返回</button>
   </div>
   <p v-if="!binding.archived&&!binding.pending" class="muted">更换目录、项目路径或忽略规则时，请先解除绑定，再重新选择目录并核对新的同步预览。</p>
   <div v-if="binding.pending" class="sync-pending">
    <p role="status">上次同步未完成核对。请检查当前文件与操作记录，再决定如何继续。</p>
    <button :disabled="busy||binding.server_changed" @click="inspectPending"><UiIcon name="shield" />核对未完成批次</button>
    <button :disabled="busy" @click="reviewAbandon"><UiIcon name="history" />无法核对：归档此绑定</button>
    <div v-if="abandoning" class="sync-confirm" role="alertdialog" aria-label="归档未核验批次">
     <p>此操作不会核验文件、回滚修改或认定同步成功。将同时停用绑定，保留这批的未知结果；新绑定不继承旧基线。</p>
     <p>批次 {{ abandoning.batch_id.slice(-8) }}：未发送 {{ abandoning.prepared }} 项，已开始 {{ abandoning.started }} 项，历史上已核验 {{ abandoning.verified }} 项。</p>
     <p v-if="abandoning.server_changed">服务器身份已变化，仅保留本机记录和本地副本引用，不向新实例查询旧操作。</p>
     <p>请输入“归档未核验批次”确认。</p>
     <input v-model="abandonPhrase" :disabled="busy" aria-label="归档确认文字" />
     <button :disabled="busy||abandonPhrase!=='归档未核验批次'" @click="abandonPending"><UiIcon name="history" />确认归档未知结果</button>
     <button :disabled="busy" @click="abandoning=null"><UiIcon name="arrow-left" />返回</button>
    </div>
    <template v-if="review">
     <p v-if="review.can_finish">完整目录与计划结果一致，服务器写入均有成功记录，可以确认完成。</p>
     <p v-else>当前结果还不能确认为整批完成。可以保留当前文件，结束旧批次后重新预览。</p>
     <p v-if="review.rules_changed" role="alert">忽略规则已变化。结束批次后仍需重新确认同步范围，不能沿用旧规则传播删除。</p>
     <ul class="sync-changes"><li v-for="item in review.items" :key="item.id"><UiIcon name="file" /><div class="sync-row-content"><code>{{ item.path }}</code><span>{{ observations[item.current] }} · {{ receipts[item.receipt] }}</span></div></li></ul>
     <button v-if="review.can_finish" :disabled="busy" @click="openResolve('finish')"><UiIcon name="check" />确认这批已完成</button>
     <button :disabled="busy" @click="openResolve('replan')"><UiIcon name="history" />保留文件并结束旧批次</button>
     <div v-if="resolving" class="sync-confirm" role="alertdialog" aria-label="确认处理未完成同步">
      <p>{{ resolving==='finish'?'将再次核对当前目录，确认结果仍一致后提交基线。':'将保留当前文件和旧基线，归档此批次及恢复记录；不会撤销已发生的修改。下一次同步需要重新预览。' }}</p>
      <button :disabled="busy" @click="resolvePending"><UiIcon name="check" />确认</button><button :disabled="busy" @click="resolving=null"><UiIcon name="arrow-left" />返回</button>
     </div>
    </template>
   </div>
   <template v-else-if="!binding.archived&&!binding.server_changed">
    <div class="sync-controls">
     <label><span>同步方式</span><select v-model="direction" :disabled="busy" @change="preview=null" aria-label="同步方向"><option value="automatic">双向比较</option><option value="local">以本地内容为准</option><option value="remote">以服务器内容为准</option></select></label>
     <button class="primary" :disabled="busy" @click="check"><UiIcon name="refresh" />检查变更</button>
     <button :disabled="busy" @click="startLoop"><UiIcon name="play" />开启持续同步</button>
    </div>
    <p v-if="direction!=='automatic'" class="sync-notice" role="note"><UiIcon name="alert" />此方向可能覆盖或删除另一端内容，请仔细核对预览。</p>
    <p class="muted">切换工作空间后持续同步仍在后台运行，各项目依次使用同步通道。退出登录或关闭应用会停止。只读检查的瞬时网络故障最多重试 4 次；冲突、首次同步、大批量删除和写入错误会暂停。</p>
   </template>
   <button :disabled="busy" @click="listRecovery()"><UiIcon name="history" />查看恢复副本</button>
   <section v-if="history" class="sync-history-section">
    <h3 class="sync-section-title"><UiIcon name="history" :size="18" />恢复副本与历史</h3>
    <p>副本会导出到另选目录。请核对内容后自行恢复，当前项目文件不会被覆盖。服务器副本下载时会检查是否存在。</p>
    <details class="sync-capacity"><summary><UiIcon name="chevron-right" />容量与清理说明</summary><div class="sync-capacity-content">
    <p class="muted">本机全部绑定 {{ history.metadata.bindings }} / {{ history.metadata.binding_limit }}；记录占用 {{ byteLabel(history.metadata.bytes) }} / {{ byteLabel(history.metadata.byte_limit) }}（当前项目 {{ byteLabel(history.metadata.binding_bytes) }}）。本绑定历史 {{ history.metadata.history_batches }} / {{ history.metadata.history_limit }} 批。</p>
    <p v-if="history.local_recovery" class="muted">本目录恢复副本 {{ history.local_recovery.files }} / {{ history.local_recovery.file_limit }} 个，{{ byteLabel(history.local_recovery.bytes) }} / {{ byteLabel(history.local_recovery.byte_limit) }}。不含旧版子目录副本、导出文件和服务器占用。</p>
    <p v-else class="muted">本地恢复目录不可核验，占用未知；历史记录仍可查看。</p>
    <template v-if="history.remote_storage_status==='available'&&history.remote_storage">
     <p class="muted">服务器当前空间：活动操作 {{ history.remote_storage.active_operations }} / {{ history.remote_storage.operation_limit }}，恢复副本 {{ byteLabel(history.remote_storage.recovery_bytes) }} / {{ byteLabel(history.remote_storage.recovery_byte_limit) }}；操作记录 {{ byteLabel(history.remote_storage.metadata_bytes) }}。</p>
     <p class="muted">永久执行收据 {{ history.remote_storage.retained_receipts }} / {{ history.remote_storage.receipt_limit }}。清理服务器历史会释放活动操作与旧内容容量，保留收据以防重复执行；收据达到上限后无法继续回收，活动操作容量用满时停止新增写入。</p>
    </template>
    <p v-else-if="history.remote_storage_status==='unsupported'" class="muted">此服务端不支持服务器容量查询与同步历史清理；仍可查看本地历史。</p>
    <p v-else-if="history.remote_storage_status==='server_changed'" class="muted">服务器身份已变化，旧实例占用未知；不能从当前实例清理旧记录。</p>
    <p v-else class="muted">服务器存储暂不可核验，占用未知。请重新加载历史后再尝试清理。</p>
    <p class="muted">记录大小按逻辑内容计算，并不等同于实际磁盘占用。达到上限会停止新增写入，恢复记录不会自动删除。</p>
    </div></details>
    <div class="sync-recovery">
    <p v-if="history.pending" role="status">存在未完成批次，其恢复记录排在第一页。</p>
    <p v-if="!history.history.some(batch=>batch.files.length)">本页暂无可导出的覆盖/删除记录。</p>
    <details v-for="batch in history.history" :key="batch.id" class="sync-batch">
     <summary><UiIcon name="chevron-right" />{{ batch.pending?'未完成批次':batch.action==='abandoned'?'未核验归档（结果未知）':batch.action==='replan'?'已结束批次':'完成批次' }} · {{ batch.id.slice(-8) }} · {{ batch.total_files }} 项<span v-if="batch.total_files">（显示 {{ batch.file_offset+1 }}～{{ batch.file_offset+batch.files.length }}）</span></summary>
     <p v-if="batch.remote_operations" class="muted">服务器操作 {{ batch.remote_operations }} 项，已回收 {{ batch.remote_retired }} 项。</p>
     <button v-if="batch.cleanable" :disabled="busy||history.pending" @click="openHistoryCleanup(batch.id)"><UiIcon name="trash" />清理无副本历史</button>
     <button v-if="batch.remote_cleanable&&history.remote_storage_status==='available'" :disabled="busy||history.pending||loopActive" @click="reviewRemoteCleanup(batch.id)"><UiIcon name="list-check" />预览服务器清理</button>
     <ul class="sync-changes"><li v-for="file in batch.files" :key="file.id">
      <UiIcon name="file" /><code>{{ file.path }}</code><span class="sync-source">{{ file.side==='local'?'本地':'服务器' }}</span>
      <span v-if="file.state==='discarded'||file.state==='retired'">原内容已清理（保留历史引用）</span>
      <span v-else-if="file.state==='retiring'">服务器清理未完成，请重新预览后继续；此副本已停止导出</span>
      <template v-else>
       <span v-if="file.state==='discarding'">清理未完成，请重新核对后继续</span>
       <button :disabled="busy" @click="exportRecovery(batch.id,file.id)"><UiIcon name="download" />导出原内容</button>
       <button v-if="file.disposable" :disabled="busy||history.pending||loopActive" @click="openDiscardRecovery(batch.id,file.id,file.path)"><UiIcon name="trash" />{{ file.state==='discarding'?'继续清理本地副本':'清理本地副本' }}</button>
      </template>
     </li></ul>
    </details>
    <div v-if="cleaning" class="sync-confirm" role="alertdialog" aria-label="清理无副本历史">
     <p>删除批次 {{ cleaning.batchId.slice(-8) }} 的本机历史记录？该批次已完成，恢复副本和远端操作均已回收或原本不存在。只释放本机元数据和历史条目容量，保留当前文件、同步基线及服务器永久执行收据。</p>
     <button :disabled="busy" @click="cleanupHistory"><UiIcon name="trash" />确认清理历史</button><button :disabled="busy" @click="cleaning=null"><UiIcon name="arrow-left" />返回</button>
    </div>
    <div v-if="discarding" class="sync-confirm danger-confirm" role="alertdialog" aria-label="清理本地恢复副本">
     <p>永久删除 {{ discarding.path }} 的这一份本地原内容？请先按需导出。只处理已完成批次，历史引用、当前项目文件和同步基线保留。清理后不能再从这份副本恢复。</p>
     <button :disabled="busy||loopActive" @click="discardRecovery"><UiIcon name="trash" />确认永久清理此副本</button><button :disabled="busy" @click="discarding=null"><UiIcon name="arrow-left" />返回</button>
    </div>
    <div v-if="remoteCleaning" class="sync-confirm danger-confirm" role="alertdialog" aria-label="清理服务器同步历史">
     <p>永久清理批次 {{ remoteCleaning.batch_id.slice(-8) }} 中以下已完成的服务器操作及原文件内容？请先按需导出恢复副本。清理后不能从这些副本恢复。</p>
     <p>{{ remoteCleaning.items.length }} 项操作，已回收 {{ remoteCleaning.items.filter(item=>item.state==='retired').length }} 项；本次剩余原内容 {{ byteLabel(remoteCleanupBytes) }}。</p>
     <ul class="sync-changes"><li v-for="item in remoteCleaning.items" :key="item.id"><code>{{ item.path }}</code> · {{ operations[item.kind]||item.kind }} · {{ item.state==='retired'?'已回收':item.state==='retiring'?'清理未完成，继续回收':'待回收' }} · {{ byteLabel(item.state==='retired'?0:item.bytes) }}</li></ul>
     <p>服务器会保留永久执行收据，阻止这些操作被重复执行。当前项目文件、本地恢复副本和同步基线保留。</p>
     <button :disabled="busy||loopActive" @click="cleanupRemote"><UiIcon name="trash" />确认永久清理服务器历史</button><button :disabled="busy" @click="remoteCleaning=null"><UiIcon name="arrow-left" />返回</button>
    </div>
    <nav aria-label="恢复历史分页">
     <button :disabled="busy||historyCursors.length<2" @click="listRecovery(historyCursors[historyCursors.length-2],true)"><UiIcon name="chevron-left" />上一页</button>
     <span>第 {{ historyCursors.length }} 页 · 共 {{ history.total_batches }} 批</span>
     <button :disabled="busy||!history.next_cursor" @click="listRecovery(history.next_cursor)"><UiIcon name="chevron-right" />下一页</button>
     <button :disabled="busy" @click="listRecovery()"><UiIcon name="refresh" />重新加载历史</button>
    </nav>
    </div>
   </section>
  </template>
  <section v-if="preview" class="sync-preview-panel" aria-label="同步变更预览">
   <h3 class="sync-section-title"><UiIcon name="list-check" :size="18" />变更预览</h3>
   <p class="sync-preview">{{ preview.plan.operations.length }} 项变更，{{ preview.plan.conflicts.length }} 个冲突</p>
   <template v-if="preview.plan.conflicts.length">
    <p class="muted">逐项选择要保留的版本，再生成预览。选择已删除的一端会删除另一端文件；未解决的冲突仍会暂停整个项目。目录结构冲突需先手动整理。</p>
    <ul class="sync-changes"><li v-for="conflict in preview.plan.conflicts" :key="conflict.path+conflict.reason"><UiIcon name="alert" /><div class="sync-row-content"><code>{{ conflict.path }}</code><span>{{ conflicts[conflict.reason]||'需要手动核对' }}</span></div>
     <select v-if="direction==='automatic'&&['both_sides_changed','initial_source_required','baseline_not_converged'].includes(conflict.reason)" v-model="choices[conflict.path]" :disabled="busy" :aria-label="'冲突来源 '+conflict.path">
      <option :value="undefined">暂不选择</option><option value="local">保留本地版本（含删除）</option><option value="remote">保留服务器版本（含删除）</option>
     </select>
    </li></ul>
    <button :disabled="busy||!Object.values(choices).some(Boolean)" @click="previewChoices"><UiIcon name="list-check" />根据逐项选择生成预览</button>
   </template>
   <ul v-else class="sync-changes"><li v-for="operation in preview.plan.operations" :key="operation.kind+operation.path"><UiIcon :name="operation.kind.includes('delete')?'trash':operation.kind==='upload'?'upload':operation.kind==='download'?'download':'folder'" /><code>{{ operation.path }}</code><span class="sync-source">{{ operations[operation.kind] }}</span></li></ul>
   <button v-if="!preview.plan.conflicts.length" class="primary" :disabled="busy" @click="apply"><UiIcon name="check" />确认执行以上变更</button>
  </section>
  <div v-if="busy&&progress" class="sync-progress" role="status" aria-live="polite">
   <strong>{{ progressStages[progress.stage]||'正在处理' }}</strong>
   <code v-if="progress.path">{{ progress.operation ? operations[progress.operation]+' · ' : '' }}{{ progress.path }}</code>
   <span v-if="progress.stage==='local_scan'">已检查 {{ progress.scanned }} 个文件 · {{ byteLabel(progress.scanned_bytes) }}</span>
   <span v-if="progress.path&&progress.file_total">当前文件 {{ byteLabel(progress.file_bytes) }} / {{ byteLabel(progress.file_total) }}</span>
   <template v-if="progress.total">
    <span>已核验 {{ progress.completed }} / {{ progress.total }} 项操作</span>
    <progress :value="progress.completed" :max="progress.total" aria-label="已核验操作" />
   </template>
   <template v-if="progress.total_bytes">
    <span>已读取传输内容 {{ byteLabel(progress.bytes) }} / {{ byteLabel(progress.total_bytes) }}</span>
    <progress :value="progress.bytes" :max="progress.total_bytes" aria-label="已读取传输字节" />
    <small>传输后还需校验文件并提交结果。</small>
   </template>
  </div>
  <p v-if="loopState==='checking'||loopState==='waiting'" class="sync-loop-status" role="status"><UiIcon :name="loopState==='waiting'?'clock':'refresh'" />持续同步：{{ loopState==='checking'?'正在核对变更':'等待下一轮检查' }} <button @click="stopLoop"><UiIcon name="stop" />停止持续同步</button></p>
  <p v-if="busy&&loopState!=='waiting'" class="sync-busy-status" role="status"><UiIcon name="refresh" class="sync-spinning" />正在处理… <button @click="cancel"><UiIcon name="close" />取消</button></p>
  <p v-if="message" class="sync-feedback" role="status"><UiIcon name="info" />{{ message }}</p>
 </section>
</template>

<style scoped>
.sync-workspace { display: grid; align-content: start; gap: 16px; margin: 0; padding: 0; border: 0; color: var(--text); font-size: 13px; }
.sync-workspace p { margin: 0; line-height: 1.65; }
.sync-workspace button { display: inline-flex; align-items: center; justify-content: center; gap: 7px; justify-self: start; width: fit-content; min-height: 34px; transition: border-color 140ms ease, background-color 140ms ease; }
.sync-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.sync-heading strong { display: flex; align-items: center; gap: 9px; margin: 0; font-size: 14px; font-weight: 600; }
.sync-heading strong > .ui-icon { color: var(--amber, var(--accent)); }
.sync-project-selector, .sync-controls label { display: grid; gap: 7px; min-width: 0; }
.sync-project-selector > span, .sync-controls label > span { font-size: 12px; color: var(--muted); }
.sync-project-selector { width: min(600px, 100%); }
.sync-workspace select { min-height: 36px; min-width: 0; max-width: 100%; padding: 7px 10px; border: 1px solid var(--line); border-radius: 6px; color: var(--text); background: var(--panel); font: inherit; }
.sync-directory { display: flex; align-items: center; gap: 12px; padding: 16px 0; border-top: 1px solid var(--line); border-bottom: 1px solid var(--line); }
.sync-directory > .ui-icon { color: var(--amber, var(--accent)); }
.sync-directory > div { display: grid; gap: 5px; min-width: 0; }
.sync-directory-label { font-size: 11px; color: var(--muted); }
.sync-path { overflow-wrap: anywhere; font-family: var(--mono, monospace); font-size: 12px; color: var(--text); }
.sync-binding-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.sync-binding-actions button { min-height: 30px; padding: 5px 9px; background: transparent; font-size: 12px; }
.sync-controls { display: flex; align-items: flex-end; flex-wrap: wrap; gap: 10px; padding-top: 6px; }
.sync-controls label { min-width: 180px; flex: 0 1 250px; }
.sync-notice, .sync-feedback { display: flex; align-items: start; gap: 9px; padding: 12px 14px; border-radius: 6px; background: var(--panel-2, var(--panel)); }
.sync-notice > .ui-icon, .sync-feedback > .ui-icon { margin-top: 3px; color: var(--amber, var(--accent)); }
.sync-workspace > p[role="alert"] { padding: 12px 14px; border-left: 3px solid var(--amber, var(--accent)); background: var(--panel-2, var(--panel)); }
.sync-pending { display: grid; gap: 13px; padding: 18px 20px; border-left: 3px solid var(--amber, var(--accent)); border-radius: 0 6px 6px 0; background: var(--panel-2, var(--panel)); }
.sync-confirm { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; padding: 16px; border: 1px solid var(--line); border-left: 3px solid var(--amber, var(--accent)); border-radius: 6px; background: var(--panel); }
.sync-confirm > p, .sync-confirm > ul { flex-basis: 100%; }
.sync-confirm input { width: min(360px, 100%); }
.sync-confirm.danger-confirm { border-left-color: var(--red, #c96859); }
.danger-confirm > button:first-of-type { color: var(--red, #c96859); border-color: var(--red-line, var(--line)); background: transparent; }
.sync-section-title { display: flex; align-items: center; gap: 9px; margin: 0; font-size: 14px; font-weight: 600; }
.sync-section-title > .ui-icon { color: var(--muted); }
.sync-history-section, .sync-preview-panel { display: grid; gap: 14px; padding-top: 22px; margin-top: 4px; border-top: 1px solid var(--line); }
.sync-capacity { font-size: 12px; color: var(--muted); }
.sync-capacity > summary { display: flex; align-items: center; gap: 6px; width: fit-content; padding: 5px 0; list-style: none; cursor: pointer; }
.sync-capacity-content { display: grid; gap: 10px; padding: 12px 0 4px; }
.sync-capacity > summary::-webkit-details-marker, .sync-batch > summary::-webkit-details-marker { display: none; }
.sync-capacity > summary > .ui-icon, .sync-batch > summary > .ui-icon { transition: transform 140ms ease; }
.sync-capacity[open] > summary > .ui-icon, .sync-batch[open] > summary > .ui-icon { transform: rotate(90deg); }
.sync-recovery { display: grid; gap: 12px; }
.sync-batch { padding: 14px 0; border-top: 1px solid var(--line); }
.sync-batch > summary { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; list-style: none; cursor: pointer; font-size: 12px; font-weight: 500; }
.sync-batch > summary > .ui-icon { color: var(--muted); }
.sync-batch > p { margin-top: 12px; font-size: 12px; }
.sync-batch > button { margin: 10px 8px 0 0; padding: 5px 9px; min-height: 30px; font-size: 12px; }
.sync-changes { margin: 0; padding: 0; list-style: none; max-height: 320px; overflow-y: auto; overflow-wrap: anywhere; }
.sync-changes li { display: flex; align-items: center; flex-wrap: wrap; gap: 9px; padding: 12px 0; border-bottom: 1px solid var(--line); }
.sync-changes li > .ui-icon { color: var(--muted); }
.sync-changes code { flex: 1; min-width: 100px; font-family: var(--mono, monospace); font-size: 12px; }
.sync-changes li > button { min-height: 29px; padding: 5px 8px; font-size: 11px; }
.sync-source { color: var(--muted); font-size: 11px; }
.sync-row-content { display: grid; flex: 1; min-width: 160px; gap: 5px; }
.sync-row-content > span { color: var(--muted); font-size: 12px; }
.sync-preview { color: var(--text); font-size: 13px; font-variant-numeric: tabular-nums; }
.sync-recovery nav { display: flex; align-items: center; flex-wrap: wrap; justify-content: flex-end; gap: 10px; padding-top: 12px; border-top: 1px solid var(--line); }
.sync-recovery nav span { color: var(--muted); font-size: 12px; }
.sync-progress { display: flex; flex-direction: column; gap: 9px; padding: 16px 0; border-top: 1px solid var(--line); font-size: 12px; }
.sync-progress code { overflow-wrap: anywhere; white-space: pre-wrap; font-family: var(--mono, monospace); }
.sync-progress progress { width: 100%; height: 6px; accent-color: var(--amber, var(--accent)); }
.sync-progress small { color: var(--muted); }
.sync-loop-status, .sync-busy-status { display: flex; align-items: center; flex-wrap: wrap; gap: 9px; color: var(--muted); }
.sync-loop-status > .ui-icon { color: var(--amber, var(--accent)); }
.sync-spinning { animation: sync-turn 1.4s linear infinite; }
@keyframes sync-turn { to { transform: rotate(360deg); } }
@media (max-width: 680px) { .sync-controls label { flex-basis: 100%; }.sync-pending { padding: 14px; }.sync-recovery nav { justify-content: flex-start; }.sync-confirm { padding: 12px; }.sync-changes li > select { width: 100%; }.sync-project-selector { width: 100%; } }
@media (prefers-reduced-motion: reduce) { .sync-workspace button, .sync-capacity > summary > .ui-icon, .sync-batch > summary > .ui-icon { transition: none; }.sync-spinning { animation: none; } }
</style>
