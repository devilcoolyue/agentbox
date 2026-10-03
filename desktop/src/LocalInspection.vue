<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue';
import { bridge,errorMessage } from './bridge';

interface Inspection {
  files:number;directories:number;bytes:number;windows_issues:number;
  capabilities:{executable:boolean;name_policy:{windows:boolean;case_sensitive:boolean;normalization_sensitive:boolean}};
}
const busy=ref(false);
const result=ref<Inspection|null>(null);
const error=ref('');
let alive=true;
async function inspect(){
  if(busy.value)return;busy.value=true;error.value='';result.value=null;
  try{const value=await bridge.invoke<Inspection|null>('inspect_local');if(alive)result.value=value;}
  catch(err){if(alive)error.value=errorMessage(err);}finally{if(alive)busy.value=false;}
}
async function cancel(){try{await bridge.invoke('cancel_inspection');}catch(err){if(alive)error.value=errorMessage(err);}}
onBeforeUnmount(()=>{alive=false;if(busy.value)void cancel();});
</script>

<template>
  <details class="local-inspection">
    <summary>检查本地目录</summary>
    <p>扫描前检查目录兼容性和文件大小；不会上传、下载或启动自动同步。按项目的 .agentboxignore 及默认规则排除依赖和构建目录。</p>
    <button :disabled="busy" @click="inspect">{{ busy?'正在检查…':'选择目录并检查' }}</button><button v-if="busy" @click="cancel">取消检查</button>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <div v-if="result" role="status">
      <p>{{ result.files }} 个文件 · {{ result.directories }} 个目录 · {{ (result.bytes/1024/1024).toFixed(1) }} MiB</p>
      <p>此目录{{ result.capabilities.name_policy.case_sensitive?'区分':'不区分' }}大小写；{{ result.capabilities.name_policy.normalization_sensitive?'区分':'合并' }} Unicode 等价名称。</p>
      <p v-if="result.windows_issues" class="error">发现 {{ result.windows_issues }} 项 Windows 名称兼容问题，跨平台同步前需要处理。</p>
      <p v-else>本次清单未发现 Windows 保留名、大小写或 Unicode 名称碰撞。</p>
    </div>
  </details>
</template>
