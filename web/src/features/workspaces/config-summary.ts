import {setText,setTextRender,t} from "../../i18n.js";
import type {CreationResources} from "../../types.js";

export function validResources(value:CreationResources|undefined):value is CreationResources {
 return !!value && [value.cpus,value.memory_mb,value.pids_limit].every(n=>typeof n==="number"&&Number.isFinite(n)) && value.cpus>0 && value.memory_mb>0;
}

export function resourceKey(value:CreationResources|undefined){
 return value?`${value.cpus}/${value.memory_mb}/${value.pids_limit}`:"";
}

export function renderCreationSummary(root:HTMLElement,account:string,model:string,resources:CreationResources|undefined){
 root.replaceChildren();
 const heading=document.createElement("strong");setText(heading,"采用的配置");
 const list=document.createElement("dl");
 for(const [label,value] of [["使用账号",account],["空间默认模型",model]]){
  const key=document.createElement("dt"),text=document.createElement("dd");setText(key,label);text.textContent=value||t("尚未确认");list.append(key,text);
 }
 const key=document.createElement("dt"),value=document.createElement("dd");setText(key,"新容器资源上限");
 setTextRender(value,()=>validResources(resources)?t("{cpu} CPU · {memory} MiB 内存 · {pids} 进程",{cpu:resources.cpus,memory:resources.memory_mb,pids:resources.pids_limit>0?resources.pids_limit:t("不限")}):t("尚未确认"));
 list.append(key,value);
 const note=document.createElement("p");note.className="field-hint";
 setText(note,"模型在创建空间时保存，可在对话中切换。资源在创建容器时按当时的系统设置生效，已有容器保留原上限。");
 root.append(heading,list,note);
}
