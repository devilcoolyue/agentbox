<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue';
import { bridge,errorMessage } from './bridge';
interface UpdateInfo {configured:boolean;current:string;version:string|null;notes:string|null}
const info=ref<UpdateInfo|null>(null);const busy=ref(false);const message=ref('');const confirm=ref(false);
const automatic=ref(localStorage.getItem('agentbox.checkDesktopUpdates')==='true');let alive=true;
async function check(){if(busy.value)return;busy.value=true;message.value='';confirm.value=false;try{const result=await bridge.invoke<UpdateInfo>('desktop_update_check');if(alive){info.value=result;message.value=!result.configured?'当前开发包未配置更新签名公钥':result.version?'':'当前桌面客户端已是最新版本';}}catch(error){if(alive)message.value=errorMessage(error);}finally{if(alive)busy.value=false;}}
async function install(){if(!info.value?.version||busy.value)return;busy.value=true;message.value='正在下载并验证签名，完成后安装并重启…';try{await bridge.invoke('desktop_update_install',{version:info.value.version});}catch(error){if(alive){message.value=errorMessage(error);busy.value=false;confirm.value=false;}}}
function preference(){localStorage.setItem('agentbox.checkDesktopUpdates',String(automatic.value));}
onMounted(()=>{if(automatic.value)void check();});onBeforeUnmount(()=>{alive=false;});
</script>
<template>
 <details class="desktop-update"><summary>桌面版本与更新</summary><button :disabled="busy" @click="check">检查桌面更新</button>
  <label><input v-model="automatic" type="checkbox" :disabled="busy" @change="preference">启动时检查更新</label>
  <p v-if="info">当前桌面版本 {{ info.current }}<span v-if="info.version"> · 可更新至 {{ info.version }}</span></p>
  <pre v-if="info?.notes">{{ info.notes }}</pre><p v-if="message" role="status">{{ message }}</p>
  <button v-if="info?.version&&!confirm" :disabled="busy" @click="confirm=true">安装此桌面更新…</button>
  <div v-if="confirm" role="alertdialog" aria-label="确认安装桌面更新"><p>安装 {{ info?.version }} 并重启客户端？现有终端连接会断开，远端任务继续运行。登录凭证和设置保留，请先停止同步和文件传输。</p><button :disabled="busy" @click="install">确认安装并重启</button><button :disabled="busy" @click="confirm=false">返回</button></div>
 </details>
</template>
<style scoped>
.desktop-update{padding:10px 18px;font-size:12px}.desktop-update label{display:inline-flex;align-items:center;gap:5px;margin:8px}.desktop-update pre{white-space:pre-wrap;max-height:200px;overflow:auto}.desktop-update p{margin:8px 0}
</style>
