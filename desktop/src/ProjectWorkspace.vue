<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue';
import { bridge, errorMessage, type Project, type ProjectTerminal, type Session } from './bridge';
import TerminalPane from './TerminalPane.vue';
import UiIcon from './UiIcon.vue';

const props = defineProps<{ session: Session; fontSize: number; light: boolean }>();
const emit = defineEmits<{ (event: 'connection-change', session: string): void }>();
const projects = ref<Project[]>([]);
const terminals = ref<ProjectTerminal[]>([]);
const tabs = ref<ProjectTerminal[]>([]);
const active = ref('');
const error = ref('');
const busy = ref(false);
const refreshing = ref(false);
const locked = computed(() => busy.value || refreshing.value);
const editing = ref(false);
const editID = ref<string|null>(null);
const name = ref('');
const path = ref('.');
const argumentsText = ref('');
const advancedOpen = ref(false);
const revision = ref(0);
const expandedProjects = ref<Set<string>>(new Set());
const confirmAction = ref<{ type: 'project'|'terminal'; id: string; name: string; revision?: number }|null>(null);
const modalOpen = computed(() => editing.value || !!confirmAction.value);
const modal = ref<HTMLElement>();
const nameInput = ref<HTMLInputElement>();
const menuElement = ref<HTMLElement>();
const menu = ref<({type: 'project'; item: Project}|{type: 'terminal'; item: ProjectTerminal}) & {x: number; y: number}>();
let menuTrigger: HTMLElement|undefined;
let returnFocus: HTMLElement|undefined;
let menuGeneration = 0;
let generation = 0;
let disposed = false;
const titleID = `project-dialog-${props.session.id}`;
const descriptionID = `${titleID}-description`;

async function refresh() {
  if (refreshing.value || disposed) return;
  const current = ++generation;
  refreshing.value = true;
  try {
    const [ps, ts] = await Promise.all([
      bridge.invoke<Project[]>('list_projects', {session: props.session.id}),
      bridge.invoke<ProjectTerminal[]>('list_terminals', {session: props.session.id}),
    ]);
    if (disposed || current !== generation) return;
    projects.value = ps;
    terminals.value = ts;
    tabs.value = tabs.value.filter(tab => ts.some(terminal => terminal.id === tab.id && terminal.state === 'open'));
    if (!tabs.value.some(tab => tab.id === active.value)) active.value = tabs.value[0]?.id || '';
  } catch (err) {
    if (!disposed && current === generation) error.value = errorMessage(err);
  } finally {
    if (!disposed && current === generation) refreshing.value = false;
  }
}

function closeMenu(restore = false) {
  menuGeneration++;
  menu.value = undefined;
  if (restore && menuTrigger?.isConnected) menuTrigger.focus();
}
async function openMenu(event: MouseEvent, target: {type: 'project'; item: Project}|{type: 'terminal'; item: ProjectTerminal}) {
  if (locked.value || modalOpen.value) return;
  if (menu.value?.type === target.type && menu.value.item.id === target.item.id) { closeMenu(true); return; }
  const current = ++menuGeneration;
  menuTrigger = event.currentTarget as HTMLElement;
  const bounds = menuTrigger.getBoundingClientRect();
  menu.value = {...target, x: bounds.right - 180, y: bounds.bottom + 6};
  await nextTick();
  if (disposed || current !== menuGeneration || !menu.value || !menuElement.value) return;
  const size = menuElement.value.getBoundingClientRect();
  menu.value.x = Math.max(8, Math.min(bounds.right - size.width, innerWidth - size.width - 8));
  menu.value.y = Math.max(8, bounds.bottom + size.height + 6 > innerHeight - 8 ? bounds.top - size.height - 6 : bounds.bottom + 6);
  menuElement.value.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
}
function menuKey(event: KeyboardEvent) {
  const buttons = Array.from(menuElement.value?.querySelectorAll<HTMLButtonElement>('button:not(:disabled)') || []);
  if (!buttons.length) return;
  const current = buttons.indexOf(document.activeElement as HTMLButtonElement);
  const index = event.key === 'ArrowDown' ? (current + 1) % buttons.length
    : event.key === 'ArrowUp' ? (current - 1 + buttons.length) % buttons.length
      : event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1 : -1;
  if (index >= 0) { event.preventDefault(); buttons[index].focus(); }
}
function outsideMenu(event: PointerEvent) {
  const target = event.target as Node;
  if (!menuElement.value?.contains(target) && !menuTrigger?.contains(target)) closeMenu();
}
function viewportChanged() { closeMenu(); }
function toggleTerminals(id: string) {
  const expanded = new Set(expandedProjects.value);
  if (expanded.has(id)) expanded.delete(id); else expanded.add(id);
  expandedProjects.value = expanded;
}
function projectTerminals(id: string) { return terminals.value.filter(terminal => terminal.project_id === id); }
function prepareDialog() {
  returnFocus = menu.value ? menuTrigger : document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
  closeMenu(); error.value = '';
}
async function focusDialog() {
  await nextTick();
  if (disposed || !modalOpen.value) return;
  (editing.value ? nameInput.value : modal.value?.querySelector<HTMLElement>('[data-dialog-cancel]'))?.focus();
}
function edit(project?: Project) {
  if (locked.value) return;
  prepareDialog();
  confirmAction.value = null;
  editing.value = true;
  editID.value = project?.id || null;
  name.value = project?.name || '';
  path.value = project?.path || '.';
  argumentsText.value = project?.arguments.join('\n') || '';
  advancedOpen.value = !!argumentsText.value;
  revision.value = project?.revision || 0;
  void focusDialog();
}
function confirmProject(project: Project) {
  if (locked.value) return;
  prepareDialog(); editing.value = false;
  confirmAction.value = {type: 'project', id: project.id, name: project.name, revision: project.revision};
  void focusDialog();
}
function confirmTerminal(terminal: ProjectTerminal) {
  if (locked.value) return;
  prepareDialog(); editing.value = false;
  confirmAction.value = {type: 'terminal', id: terminal.id, name: title(terminal)};
  void focusDialog();
}
async function closeDialog(force = false) {
  if (busy.value && !force) return;
  editing.value = false; confirmAction.value = null;
  await nextTick();
  if (!disposed && returnFocus?.isConnected && returnFocus.getClientRects().length) returnFocus.focus();
}
function keyboard(event: KeyboardEvent) {
  if (event.isComposing || event.keyCode === 229) return;
  if (modalOpen.value) {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); void closeDialog(); }
    if (event.key !== 'Tab') return;
    const fields = Array.from(modal.value?.querySelectorAll<HTMLElement>('button:not(:disabled),input:not(:disabled),textarea:not(:disabled),summary,[tabindex="0"]') || []).filter(element => element.getClientRects().length);
    const first = fields[0]; const last = fields[fields.length - 1];
    if (!first) { event.preventDefault(); return; }
    if (!modal.value?.contains(document.activeElement) || event.shiftKey && document.activeElement === first) {
      event.preventDefault(); (event.shiftKey ? last : first)?.focus();
    } else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  } else if (menu.value && (event.key === 'Escape' || event.key === 'Tab')) {
    event.preventDefault(); event.stopPropagation(); closeMenu(true);
  }
}

async function save() {
  if (locked.value || disposed) return;
  busy.value = true; error.value = '';
  try {
    await bridge.invoke('save_project', {session: props.session.id, id: editID.value, edit: {
      name: name.value, path: path.value, arguments: argumentsText.value ? argumentsText.value.split('\n') : [], revision: revision.value,
    }});
    if (disposed) return;
    await closeDialog(true); await refresh();
  } catch (err) { if (!disposed) error.value = errorMessage(err); }
  finally { if (!disposed) busy.value = false; }
}
function attach(terminal: ProjectTerminal) {
  if (terminal.state !== 'open' || disposed) return;
  closeMenu();
  if (!tabs.value.some(tab => tab.id === terminal.id)) tabs.value.push(terminal);
  active.value = terminal.id;
}
function detach(id: string) {
  tabs.value = tabs.value.filter(tab => tab.id !== id);
  if (active.value === id) active.value = tabs.value[tabs.value.length - 1]?.id || '';
}
async function create(project: Project, kind: 'agent'|'shell') {
  if (locked.value || disposed) return;
  busy.value = true; error.value = ''; closeMenu();
  try {
    const terminal = await bridge.invoke<ProjectTerminal>('create_terminal', {session: props.session.id, project: project.id, kind});
    if (disposed) return;
    await refresh();
    if (disposed) return;
    expandedProjects.value = new Set([...expandedProjects.value, project.id]);
    attach(terminal);
  } catch (err) { if (!disposed) error.value = errorMessage(err); }
  finally { if (!disposed) busy.value = false; }
}
async function remove() {
  const action = confirmAction.value;
  if (!action || locked.value || disposed) return;
  busy.value = true; error.value = '';
  try {
    if (action.type === 'project') await bridge.invoke('delete_project', {session: props.session.id, id: action.id, revision: action.revision});
    else {
      await bridge.invoke('end_terminal', {session: props.session.id, id: action.id});
      if (!disposed) detach(action.id);
    }
    if (disposed) return;
    await closeDialog(true); await refresh();
  } catch (err) {
    if (!disposed) { error.value = errorMessage(err); await refresh(); }
  } finally { if (!disposed) busy.value = false; }
}
function title(terminal: ProjectTerminal) {
  return `${projects.value.find(project => project.id === terminal.project_id)?.name || '项目'} · ${terminal.kind === 'agent' ? props.session.agent : 'Shell'} · ${terminal.id.slice(-4)}`;
}
onMounted(() => {
  void refresh();
  window.addEventListener('pointerdown', outsideMenu, true);
  window.addEventListener('keydown', keyboard, true);
  window.addEventListener('resize', viewportChanged);
  window.addEventListener('scroll', viewportChanged, true);
});
onBeforeUnmount(() => {
  disposed = true; generation++; menuGeneration++;
  window.removeEventListener('pointerdown', outsideMenu, true);
  window.removeEventListener('keydown', keyboard, true);
  window.removeEventListener('resize', viewportChanged);
  window.removeEventListener('scroll', viewportChanged, true);
});
</script>

<template>
  <section class="project-workspace">
    <div class="project-toolbar">
      <div class="project-workspace-title"><UiIcon name="folder" :size="18" /><h2>项目与终端</h2><span>{{ projects.length }} 个项目</span></div>
      <div class="project-toolbar-actions"><button class="project-icon-button" :disabled="locked" title="刷新项目与终端" aria-label="刷新项目与终端" @click="refresh"><UiIcon name="refresh" :size="16" :class="{'is-refreshing': refreshing}" /></button><button class="project-add" :disabled="locked" @click="edit()"><UiIcon name="plus" :size="15" />添加项目</button></div>
    </div>
    <p v-if="error && !modalOpen" class="error project-error" role="alert">{{ error }}</p>
    <details class="project-list" :open="!tabs.length">
      <summary><UiIcon name="chevron" :size="14" /><span>项目列表</span><span class="project-count">{{ projects.length }}</span></summary>
      <p v-if="refreshing && !projects.length" class="project-loading" role="status">正在读取项目…</p>
      <div v-else-if="!projects.length" class="project-empty"><UiIcon name="folder" :size="24" /><strong>添加一个项目目录</strong><p>选择服务器上的已有目录，打开 AI 或 Shell 终端。</p><button :disabled="locked" @click="edit()"><UiIcon name="plus" :size="14" />添加项目</button></div>
      <article v-for="project in projects" :key="project.id" class="project-row">
        <div class="project-heading">
          <UiIcon class="project-folder" name="folder" :size="17" />
          <div class="project-identity"><strong>{{ project.name }}</strong><code :title="project.path">{{ project.path }}</code></div>
          <button v-if="projectTerminals(project.id).length" class="terminal-count" :aria-expanded="expandedProjects.has(project.id)" :aria-label="`${projectTerminals(project.id).length} 个终端，展开或收起`" @click="toggleTerminals(project.id)">{{ projectTerminals(project.id).length }} 个终端<UiIcon name="chevron" :size="12" /></button>
          <div class="project-row-actions"><button class="project-create-agent" :disabled="locked" :title="`在 ${project.name} 中新建 ${session.agent} 终端`" @click="create(project, 'agent')"><UiIcon name="plus" :size="14" />新建 AI 终端</button><button :disabled="locked" @click="create(project, 'shell')"><UiIcon name="terminal" :size="14" />新建 Shell</button><button class="project-icon-button" :disabled="locked" title="项目操作" :aria-label="`${project.name} 的更多操作`" aria-haspopup="menu" :aria-expanded="menu?.type === 'project' && menu.item.id === project.id" @click="openMenu($event, {type: 'project', item: project})"><UiIcon name="more" :size="17" /></button></div>
        </div>
        <div v-if="expandedProjects.has(project.id)" class="project-terminal-list">
          <div v-for="terminal in projectTerminals(project.id)" :key="terminal.id" class="terminal-row">
            <UiIcon name="terminal" :size="14" /><span class="terminal-state" :class="{closing: terminal.state === 'closing'}"></span><span class="terminal-row-title" :title="title(terminal)">{{ terminal.kind === 'agent' ? session.agent : 'Shell' }}<code>{{ terminal.id.slice(-4) }}</code></span><span v-if="terminal.state === 'closing'" class="terminal-closing">关闭未完成</span><span v-else-if="tabs.some(tab => tab.id === terminal.id)" class="terminal-opened">已打开</span><button :disabled="locked || terminal.state !== 'open'" @click="attach(terminal)">{{ tabs.some(tab => tab.id === terminal.id) ? '切换' : '连接' }}</button><button class="project-icon-button" :disabled="locked" title="终端操作" :aria-label="`${title(terminal)} 的更多操作`" aria-haspopup="menu" :aria-expanded="menu?.type === 'terminal' && menu.item.id === terminal.id" @click="openMenu($event, {type: 'terminal', item: terminal})"><UiIcon name="more" :size="16" /></button>
          </div>
        </div>
      </article>
    </details>
    <nav v-if="tabs.length" class="terminal-tabs" aria-label="独立终端"><div v-for="tab in tabs" :key="tab.id" :class="{active: active === tab.id}"><button class="terminal-tab-title" :aria-current="active === tab.id ? 'page' : undefined" :title="title(tab)" @click="active = tab.id"><UiIcon name="terminal" :size="14" /><span>{{ title(tab) }}</span></button><button class="terminal-tab-close" title="断开标签，远端任务继续运行" aria-label="断开标签" @click="detach(tab.id)"><UiIcon name="close" :size="13" /></button></div></nav>
    <TerminalPane v-for="tab in tabs" v-show="active === tab.id" :key="tab.id" :session="session.id" :terminal="tab.id" :font-size="fontSize" :light="light" @connection-change="emit('connection-change', $event)" />
    <div v-if="!tabs.length && projects.length" class="project-hint"><UiIcon name="terminal" :size="30" /><h3>打开一个终端</h3><p>新建 AI 终端开始工作，或展开项目连接已有终端。</p><span>关闭标签只断开连接，远端任务会继续运行。</span></div>

    <div v-if="menu" ref="menuElement" class="project-menu" role="menu" aria-label="更多操作" :style="{left: menu.x+'px', top: menu.y+'px'}" @keydown="menuKey">
      <template v-if="menu.type === 'project'"><button role="menuitem" @click="edit(menu.item)"><UiIcon name="settings" :size="15" />项目设置</button><button class="danger" role="menuitem" @click="confirmProject(menu.item)"><UiIcon name="trash" :size="15" />移除项目映射</button></template>
      <button v-else class="danger" role="menuitem" @click="confirmTerminal(menu.item)"><UiIcon name="trash" :size="15" />{{ menu.item.state === 'closing' ? '重试结束终端' : '结束远程终端' }}</button>
    </div>
    <div v-if="modalOpen" class="project-overlay" @pointerdown.self="closeDialog()">
      <section ref="modal" class="project-dialog" :class="{'project-confirm': !!confirmAction}" :role="confirmAction ? 'alertdialog' : 'dialog'" aria-modal="true" :aria-labelledby="titleID" :aria-describedby="descriptionID" tabindex="-1">
        <div class="project-dialog-head"><div><span class="project-dialog-context">{{ session.name }}</span><h3 :id="titleID">{{ editing ? (editID ? '项目设置' : '添加项目') : confirmAction?.type === 'project' ? '移除项目映射' : '结束远程终端' }}</h3></div><button class="project-icon-button" :disabled="busy" aria-label="关闭弹窗" @click="closeDialog()"><UiIcon name="close" :size="17" /></button></div>
        <form v-if="editing" class="project-edit" @submit.prevent="save">
          <p :id="descriptionID" class="project-dialog-description">将服务器上的已有目录设为项目。</p>
          <label>项目名称<input ref="nameInput" v-model="name" required maxlength="128" placeholder="例如：网站项目" :disabled="busy"></label>
          <label>服务器目录<input v-model="path" required placeholder=". 或 project-a" :disabled="busy"><span>工作空间内的相对路径。“.” 表示根目录。</span></label>
          <details class="project-advanced" :open="advancedOpen" @toggle="advancedOpen = ($event.target as HTMLDetailsElement).open"><summary><UiIcon name="chevron" :size="13" />AI 终端选项</summary><label>启动参数<textarea v-model="argumentsText" rows="4" placeholder="每行一个参数，例如：&#10;--model&#10;模型名称" :disabled="busy"></textarea><span>仅影响新建的 AI 终端。</span></label></details>
          <p v-if="editID" class="project-dialog-note">修改目录前，请先结束此项目的所有远程终端。</p>
          <p v-if="error" class="error" role="alert">{{ error }}</p>
          <div class="project-dialog-actions"><button type="button" :disabled="busy" data-dialog-cancel @click="closeDialog()">取消</button><button class="primary" :disabled="busy">{{ busy ? '正在保存…' : '保存' }}</button></div>
        </form>
        <div v-else-if="confirmAction" class="project-confirm-content"><p class="project-confirm-name">{{ confirmAction.name }}</p><p :id="descriptionID">{{ confirmAction.type === 'project' ? '只移除项目映射，目录内的文件会保留。' : '该终端中的任务将停止。仅断开连接时，请关闭终端标签。' }}</p><p v-if="error" class="error" role="alert">{{ error }}</p><div class="project-dialog-actions"><button :disabled="busy" data-dialog-cancel @click="closeDialog()">取消</button><button class="project-danger" :disabled="busy" @click="remove">{{ busy ? '正在处理…' : confirmAction.type === 'project' ? '确认移除' : '确认结束' }}</button></div></div>
      </section>
    </div>
  </section>
</template>

<style scoped>
.project-workspace{display:flex;flex-direction:column;flex:1;min-height:0;min-width:0}
.project-toolbar{display:flex;align-items:center;justify-content:space-between;gap:16px;flex:none;padding:15px 22px;border-bottom:1px solid var(--line)}
.project-workspace-title,.project-toolbar-actions,.project-row-actions{display:flex;align-items:center;gap:8px;min-width:0}
.project-workspace-title>svg{flex:none;color:var(--muted)}
.project-workspace-title h2{margin:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:15px;font-weight:600}
.project-workspace-title>span{margin:0 0 0 4px;font:11px var(--mono);color:var(--muted)}
.project-toolbar-actions{flex:none}
.project-workspace button{display:inline-flex;align-items:center;justify-content:center;gap:6px;min-height:32px;padding:5px 10px;border-radius:6px;font-size:12px;line-height:20px;transition:background 140ms ease,color 140ms ease,border-color 140ms ease}
.project-workspace button>svg{flex:none}
.project-workspace button:disabled{cursor:not-allowed}
.project-workspace .project-icon-button{width:30px;min-width:30px;min-height:30px;padding:0;border-color:transparent;background:transparent;color:var(--muted)}
.project-workspace .project-icon-button:hover:not(:disabled){color:var(--text);background:var(--panel-2);border-color:transparent}
.project-workspace .project-add{color:var(--amber);border-color:var(--line);background:transparent}
.project-error{flex:none;margin:0;padding:10px 22px;border-bottom:1px solid var(--line-soft);font-size:12px}
.project-list{flex:none;max-height:38vh;margin:0;padding:0;overflow:auto;border-bottom:1px solid var(--line-soft)}
.project-list>summary{display:flex;align-items:center;gap:7px;padding:10px 22px;list-style:none;cursor:pointer;color:var(--muted);font-size:11px;user-select:none}
.project-list>summary::-webkit-details-marker,.project-advanced>summary::-webkit-details-marker{display:none}
.project-list>summary>svg,.project-advanced>summary>svg,.terminal-count>svg{transform:rotate(-90deg);transition:transform 140ms ease}
.project-list[open]>summary>svg,.project-advanced[open]>summary>svg,.terminal-count[aria-expanded=true]>svg{transform:rotate(0deg)}
.project-count{font:10px var(--mono);margin-left:3px;opacity:.8}
.project-row{margin:0;padding:0;border-bottom:1px solid var(--line-soft)}
.project-row:last-child{border-bottom:0}
.project-heading{display:flex;align-items:center;flex-wrap:nowrap;gap:10px;min-height:61px;padding:10px 22px}
.project-folder{flex:none;color:var(--muted)}
.project-identity{display:flex;flex:1;min-width:80px;flex-direction:column;gap:2px}
.project-identity strong{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:13px;font-weight:550}
.project-identity code{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--muted);font:11px var(--mono);margin:0}
.project-row-actions{flex:none;gap:5px}
.project-workspace .project-create-agent{color:var(--amber);background:transparent;border-color:var(--line)}
.project-workspace .project-create-agent:hover:not(:disabled){border-color:var(--amber)}
.project-workspace .terminal-count{min-height:28px;gap:4px;padding:3px 5px;border-color:transparent;background:transparent;color:var(--muted);font-size:11px}
.project-workspace .terminal-count:hover{color:var(--text);background:var(--panel-2)}
.project-terminal-list{padding:0 22px 8px 49px}
.terminal-row{display:flex;align-items:center;gap:8px;min-height:35px;padding:3px 0;border:0;font-size:12px}
.terminal-row>svg{flex:none;color:var(--muted)}
.terminal-state{width:5px;height:5px;border-radius:50%;background:var(--green);flex:none}
.terminal-state.closing{background:var(--red)}
.terminal-row-title{display:flex;align-items:center;gap:8px;flex:1;min-width:0;color:var(--text)}
.terminal-row-title code{font:10px var(--mono);color:var(--muted)}
.terminal-closing{color:var(--red);font-size:11px}.terminal-opened{color:var(--muted);font-size:11px}
.terminal-tabs{display:flex;align-items:stretch;flex:none;overflow-x:auto;border-bottom:1px solid var(--line);background:var(--bg)}
.terminal-tabs>div{display:flex;align-items:center;flex:none;max-width:310px;border:0;border-right:1px solid var(--line-soft);border-bottom:2px solid transparent}
.terminal-tabs>div.active{background:var(--panel);border-bottom-color:var(--amber)}
.terminal-tabs .terminal-tab-title{justify-content:flex-start;min-width:0;min-height:39px;gap:7px;padding:8px 5px 8px 15px;border:0;background:transparent;color:var(--muted);font-size:12px}
.terminal-tab-title span{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.terminal-tabs>div.active .terminal-tab-title{color:var(--text)}
.terminal-tabs .terminal-tab-close{width:27px;min-width:27px;min-height:27px;padding:0;margin:0 6px;border:0;background:transparent;color:var(--muted)}
.terminal-tabs .terminal-tab-close:hover{color:var(--text);background:var(--panel-2)}
.project-empty{display:flex;align-items:flex-start;flex-direction:column;gap:8px;padding:18px 22px 24px;font-size:12px}
.project-empty>svg{color:var(--muted);margin-bottom:2px}.project-empty strong{font-size:13px;font-weight:500}.project-empty p{margin:0;color:var(--muted);line-height:1.6}.project-empty button{margin-top:5px}
.project-loading{margin:0;padding:16px 22px;color:var(--muted);font-size:12px}.is-refreshing{animation:project-refresh .9s linear infinite}
.project-hint{display:flex;flex:1;flex-direction:column;align-items:center;justify-content:center;gap:10px;min-height:150px;padding:26px;text-align:center;color:var(--muted)}
.project-hint>svg{opacity:.6;margin-bottom:3px}.project-hint h3{margin:0;color:var(--text);font-size:14px;font-weight:500}.project-hint p,.project-hint span{margin:0;font-size:12px;line-height:1.7}.project-hint span{font-size:11px;opacity:.8}
.project-menu{position:fixed;z-index:170;min-width:180px;max-width:calc(100vw - 16px);padding:5px;border:1px solid var(--line);border-radius:8px;background:var(--panel);box-shadow:var(--shadow-lg,0 12px 32px #0004)}
.project-menu button{justify-content:flex-start;gap:9px;width:100%;border:0;background:transparent;padding:8px 10px;text-align:left;font-size:12px}
.project-menu button:hover{background:var(--panel-2)}.project-menu button.danger{color:var(--red)}
.project-overlay{position:fixed;inset:0;z-index:220;display:flex;align-items:center;justify-content:center;padding:24px;background:var(--backdrop,rgba(0,0,0,.48))}
.project-dialog{display:flex;flex-direction:column;width:min(480px,100%);max-height:calc(100vh - 48px);overflow:auto;padding:0;border:1px solid var(--line);border-radius:10px;background:var(--panel);box-shadow:var(--shadow-lg,0 16px 44px #0005);animation:project-dialog-enter 140ms ease-out}
.project-dialog-head{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;padding:22px 24px 17px;border-bottom:1px solid var(--line-soft)}
.project-dialog-head>div{min-width:0}.project-dialog-context{display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;margin-bottom:5px;color:var(--muted);font-size:11px}.project-dialog h3{margin:0;font-size:16px;font-weight:600;line-height:1.5}
.project-edit{display:flex;flex-direction:column;gap:16px;max-height:none;overflow:visible;max-width:none;padding:19px 24px 0;background:transparent}
.project-dialog-description{margin:0;color:var(--muted);font-size:12px;line-height:1.7}
.project-edit label{display:flex;flex-direction:column;gap:7px;color:var(--text);font-size:12px}
.project-edit label>span,.project-dialog-note{margin:0;color:var(--muted);font-size:11px;line-height:1.7}
.project-edit input,.project-edit textarea{width:100%;min-width:0;margin:0;padding:9px 11px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--text);font-size:13px}
.project-edit textarea{font:12px/1.6 var(--mono);resize:vertical;min-height:96px}
.project-advanced{border-top:1px solid var(--line-soft);padding-top:13px}.project-advanced summary{display:flex;align-items:center;gap:6px;list-style:none;color:var(--muted);cursor:pointer;font-size:12px}.project-advanced label{margin-top:14px}
.project-dialog-actions{display:flex;align-items:center;justify-content:flex-end;gap:8px;margin:2px -24px 0;padding:15px 24px;border-top:1px solid var(--line-soft)}
.project-dialog-actions button{min-width:70px;min-height:35px}.project-dialog-actions button+button{margin:0}
.project-confirm{font-size:13px}.project-confirm-content{padding:20px 24px 0}.project-confirm-content>p{margin:0 0 15px;font-size:12px;line-height:1.8;color:var(--muted)}.project-confirm-content>p.project-confirm-name{font-size:14px;font-weight:500;color:var(--text);overflow-wrap:anywhere}.project-confirm-content>p.error{color:var(--red)}
.project-workspace .project-danger{color:var(--red);border-color:var(--red);background:transparent}
@keyframes project-dialog-enter{from{opacity:0;transform:translateY(5px)}to{opacity:1;transform:translateY(0)}}
@keyframes project-refresh{to{transform:rotate(360deg)}}
@media(max-width:900px){.project-workspace-title>span{display:none}.project-heading{gap:8px;padding:10px 16px}.project-terminal-list{padding-left:40px;padding-right:16px}.project-workspace .terminal-count{font-size:10px}}
@media(max-width:620px){.project-toolbar{padding:12px 14px}.project-heading{flex-wrap:wrap}.project-row-actions{margin-left:auto}.project-identity{min-width:100px}.project-list>summary{padding-inline:16px}.project-overlay{padding:12px}.project-dialog{max-height:calc(100vh - 24px)}.project-dialog-head{padding:18px 18px 14px}.project-edit{padding:16px 18px 0}.project-confirm-content{padding:18px 18px 0}.project-dialog-actions{margin-inline:-18px;padding:14px 18px}}
@media(max-width:560px){
  .project-toolbar{flex-wrap:wrap;gap:8px;padding:8px 12px}
  .project-workspace-title{flex:1 1 120px;min-width:0}
  .project-workspace-title h2{font-size:13px}
  .project-toolbar-actions{flex-wrap:wrap;justify-content:flex-end;max-width:100%;margin-left:auto}
  .project-heading{gap:7px;padding:8px 12px}
  .project-identity{flex:1 1 110px;min-width:0}
  .project-row-actions{flex:0 1 auto;flex-wrap:wrap;justify-content:flex-end;max-width:100%}
  .project-terminal-list{padding-left:12px;padding-right:12px}
  .terminal-row{flex-wrap:wrap;gap:6px}
  .terminal-row-title{flex:1 1 70px;overflow-wrap:anywhere}
  .terminal-tabs>div{max-width:min(260px,100%)}
  .project-dialog-actions{flex-wrap:wrap}
}
@media(max-width:560px) and (max-height:400px){
  .project-list>summary{padding:6px 12px}
  .terminal-tabs .terminal-tab-title{min-height:31px;padding-top:4px;padding-bottom:4px}
  .project-hint{min-height:0;overflow:auto;justify-content:flex-start;gap:6px;padding:12px}
}
@media(prefers-reduced-motion:reduce){.project-dialog,.is-refreshing{animation:none}.project-workspace button,.project-list>summary>svg,.project-advanced>summary>svg,.terminal-count>svg{transition:none}}
</style>
