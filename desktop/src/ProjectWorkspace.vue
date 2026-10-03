<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue';
import { bridge, errorMessage, type Project, type ProjectTerminal, type Session } from './bridge';
import TerminalPane from './TerminalPane.vue';

const props=defineProps<{ session: Session; fontSize: number; light: boolean }>();
const projects=ref<Project[]>([]);
const terminals=ref<ProjectTerminal[]>([]);
const tabs=ref<ProjectTerminal[]>([]);
const active=ref('');
const error=ref('');
const busy=ref(false);
const editing=ref(false);
const editID=ref<string|null>(null);
const name=ref('');
const path=ref('.');
const argumentsText=ref('');
const revision=ref(0);
const confirmAction=ref<{ type:'project'|'terminal';id:string;name:string;revision?:number }|null>(null);
let generation=0;
let disposed=false;

async function refresh() {
  const current=++generation;
  try {
    const [ps,ts]=await Promise.all([bridge.invoke<Project[]>('list_projects',{session:props.session.id}),bridge.invoke<ProjectTerminal[]>('list_terminals',{session:props.session.id})]);
    if(disposed||current!==generation)return;
    projects.value=ps;terminals.value=ts;
    tabs.value=tabs.value.filter(tab=>ts.some(t=>t.id===tab.id&&t.state==='open'));
    if(!tabs.value.some(tab=>tab.id===active.value))active.value=tabs.value[0]?.id||'';
  } catch(err){if(!disposed&&current===generation)error.value=errorMessage(err);}
}
function edit(project?:Project) {
  editing.value=true;editID.value=project?.id||null;name.value=project?.name||'';path.value=project?.path||'.';
  argumentsText.value=project?.arguments.join('\n')||'';revision.value=project?.revision||0;
}
async function save() {
  busy.value=true;error.value='';
  try {
    await bridge.invoke('save_project',{session:props.session.id,id:editID.value,edit:{name:name.value,path:path.value,arguments:argumentsText.value?argumentsText.value.split('\n'):[],revision:revision.value}});
    if(disposed)return;editing.value=false;await refresh();
  }catch(err){error.value=errorMessage(err);}finally{busy.value=false;}
}
function attach(terminal:ProjectTerminal){
  if(terminal.state!=='open')return;
  if(!tabs.value.some(t=>t.id===terminal.id))tabs.value.push(terminal);
  active.value=terminal.id;
}
function detach(id:string){tabs.value=tabs.value.filter(t=>t.id!==id);if(active.value===id)active.value=tabs.value[tabs.value.length-1]?.id||'';}
async function create(project:Project,kind:'agent'|'shell'){
  busy.value=true;error.value='';
  try{
    const terminal=await bridge.invoke<ProjectTerminal>('create_terminal',{session:props.session.id,project:project.id,kind});
    if(disposed)return;await refresh();attach(terminal);
  }catch(err){error.value=errorMessage(err);}finally{busy.value=false;}
}
async function remove(){
  const action=confirmAction.value;if(!action)return;
  busy.value=true;error.value='';
  try{
    if(action.type==='project')await bridge.invoke('delete_project',{session:props.session.id,id:action.id,revision:action.revision});
    else{await bridge.invoke('end_terminal',{session:props.session.id,id:action.id});detach(action.id);}
    confirmAction.value=null;await refresh();
  }catch(err){error.value=errorMessage(err);await refresh();}finally{busy.value=false;}
}
function title(terminal:ProjectTerminal){return `${projects.value.find(p=>p.id===terminal.project_id)?.name||'项目'} · ${terminal.kind==='agent'?props.session.agent:'Shell'} · ${terminal.id.slice(-4)}`;}
onMounted(refresh);
onBeforeUnmount(()=>{disposed=true;generation++;});
</script>

<template>
  <section class="project-workspace">
    <div class="project-toolbar"><h2>{{ session.name }}</h2><span class="muted">{{ session.agent }} · 项目独立终端</span><button :disabled="busy" @click="refresh">刷新</button><button :disabled="busy" @click="edit()">添加项目</button></div>
    <p v-if="error" class="error project-error" role="alert">{{ error }}</p>
    <form v-if="editing" class="project-edit" @submit.prevent="save">
      <h3>{{ editID?'编辑项目':'映射已有目录' }}</h3>
      <label>项目名称<input v-model="name" required maxlength="128" :disabled="busy"></label>
      <label>工作空间内的相对目录<input v-model="path" required placeholder=". 或 project-a" :disabled="busy"></label>
      <label>AI 启动参数（每行一个，原样传入）<textarea v-model="argumentsText" rows="3" placeholder="--model&#10;模型名称" :disabled="busy"></textarea></label>
      <p class="muted">目录必须已经存在；“.” 表示工作空间根目录。修改参数只影响之后新建的终端，修改目录前需先结束该项目的所有终端。</p>
      <div><button class="primary" :disabled="busy">保存</button><button type="button" :disabled="busy" @click="editing=false">取消</button></div>
    </form>
    <div v-if="confirmAction" class="project-confirm" role="alertdialog" aria-label="确认操作">
      <p>{{ confirmAction.type==='project'?`移除项目「${confirmAction.name}」的映射？已有文件会保留。`:`结束「${confirmAction.name}」？该终端中的任务将被停止。` }}</p>
      <button :disabled="busy" @click="remove">确认{{ confirmAction.type==='project'?'移除':'结束' }}</button><button :disabled="busy" @click="confirmAction=null">取消</button>
    </div>
    <details class="project-list" :open="!tabs.length">
      <summary>项目与终端 · {{ projects.length }} 个项目</summary>
      <p v-if="!projects.length" class="muted">添加一个目录映射，然后打开 AI 或 Shell 终端。原有文件不会移动。</p>
      <article v-for="project in projects" :key="project.id" class="project-row">
        <div class="project-heading"><strong>{{ project.name }}</strong><code>{{ project.path }}</code><button :disabled="busy" @click="create(project,'agent')">新建 {{ session.agent }} 终端</button><button :disabled="busy" @click="create(project,'shell')">新建 Shell</button><button :disabled="busy" @click="edit(project)">设置</button><button :disabled="busy" @click="confirmAction={type:'project',id:project.id,name:project.name,revision:project.revision}">移除</button></div>
        <div v-for="terminal in terminals.filter(t=>t.project_id===project.id)" :key="terminal.id" class="terminal-row"><span>{{ title(terminal) }}</span><span v-if="terminal.state==='closing'" class="error">关闭未完成，可重试结束</span><button :disabled="busy||terminal.state!=='open'" @click="attach(terminal)">连接</button><button :disabled="busy" @click="confirmAction={type:'terminal',id:terminal.id,name:title(terminal)}">结束</button></div>
      </article>
    </details>
    <nav v-if="tabs.length" class="terminal-tabs" aria-label="独立终端"><div v-for="tab in tabs" :key="tab.id" :class="{active:active===tab.id}"><button @click="active=tab.id">{{ title(tab) }}</button><button title="断开标签，远端任务继续运行" aria-label="断开标签" @click="detach(tab.id)">×</button></div></nav>
    <TerminalPane v-for="tab in tabs" v-show="active===tab.id" :key="tab.id" :session="session.id" :terminal="tab.id" :font-size="fontSize" :light="light" />
    <p v-if="!tabs.length" class="project-hint muted">终端按项目独立运行。关闭标签只断开连接；使用“结束”停止远端任务。</p>
  </section>
</template>
