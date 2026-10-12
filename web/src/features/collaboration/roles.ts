import {setText,t} from "../../i18n.js";
import {setSelectValue} from "../../select.js";
import {defaultReasoning} from "../../reasoning.js";
import type {ModelOption} from "../../types.js";
import type {CollaborationBinding as Binding,CollaborationWorkflow} from "../../contracts/collaboration-v1.js";
import type {ModelMode} from "./decision.js";
export interface Choice {id:string;type:"claude"|"codex";label:string;models:ModelOption[];default_model:string;restricted:boolean}
const id=()=>crypto.randomUUID().replaceAll("-","");
function node<K extends keyof HTMLElementTagNameMap>(tag:K,text?:string){const e=document.createElement(tag);if(text)setText(e,text);return e;}
function field(text:string,input:HTMLElement){const label=node("label");label.className="collaboration-field";label.append(node("span",text),input);return label;}
export function workflowSelector(){
 const select=node("select"),hint=node("small"),box=node("div");box.className="collaboration-strategy";select.dataset.workflowMode="";hint.className="collaboration-hint";
 for(const [value,label] of [["direct","直接执行"],["reviewed","协作复核"]]){const option=node("option",label);option.value=value;select.append(option);}
 box.append(field("执行方式",select),hint);let supported=false,legacy=false;
 const update=()=>setText(hint,select.value==="direct"?"执行者直接完成任务并运行平台验证，不做独立模型评审。":"先规划，再执行，最后由另一角色独立复核。");
 select.addEventListener("change",()=>{legacy=false;update();});update();
 return {box,select,direct:()=>supported&&select.value==="direct",read:():CollaborationWorkflow|undefined=>supported&&!legacy?{version:1,mode:select.value==="direct"?"direct":"reviewed"}:undefined,
  configure(value:CollaborationWorkflow|undefined,available:boolean,existing=false,locked=false){supported=available;legacy=existing&&!value;box.hidden=!available;select.disabled=locked;setSelectValue(select,value?.mode||(existing||!available?"reviewed":"direct"));update();}};
}
export function roleSelector(title:string,choices:Choice[],signal:AbortSignal,automatic=false){
 const box=node("fieldset"),legend=node("legend",title),kind=node("select"),account=node("select"),model=node("input"),effort=node("select"),suggestions=node("datalist");
 suggestions.id="collab-models-"+id();model.setAttribute("list",suggestions.id);model.required=true;model.maxLength=68;
 kind.append(new Option("Claude Code","claude"),new Option("Codex","codex"));kind.value=choices.some(a=>a.type==="claude")?"claude":choices[0]?.type||"claude";
 const mode=node("select"),modeField=field("选模方式",mode),modelField=field("模型",model);
 const fixed=node("option","固定模型"),auto=node("option","自动选择模型");fixed.value="fixed";auto.value="auto";mode.append(fixed,auto);mode.dataset.roleModelMode="";modeField.hidden=!automatic;
 const hint=node("small","自动选择仅使用当前账号的可用模型，CLI 和推理强度保持当前设置。");hint.className="collaboration-hint";hint.hidden=true;
 const showMode=()=>{setText(modelField.firstElementChild!,automatic&&mode.value==="auto"?"回退模型":"模型");hint.hidden=!automatic||mode.value!=="auto";};
 mode.addEventListener("change",showMode,{signal});
 box.append(legend,field("Agent",kind),field("推理强度",effort),field("账号",account),modeField,modelField,suggestions,hint);
 const selected=()=>choices.find(a=>a.id===account.value&&a.type===kind.value);
 const capability=()=>selected()?.models.find(m=>m.id===model.value)?.reasoning||defaultReasoning(kind.value);
 const levels=()=>{const c=capability();const defaultOption=node("option","默认");defaultOption.value="";effort.replaceChildren(defaultOption);const values=c.support==="supported"?c.levels||[]:[];for(const value of values)effort.append(new Option(value,value));effort.disabled=c.support!=="supported";const preferred=kind.value==="claude"?"high":"medium";setSelectValue(effort,values.includes(preferred)?preferred:values[Math.floor(values.length/2)]||"");};
 const models=()=>{const a=selected();suggestions.replaceChildren();for(const m of (a?.models||[]).filter(m=>!m.hidden)){const o=new Option(m.label||m.id,m.id);suggestions.append(o);}model.value=a?.default_model||"";levels();};
 const accounts=()=>{account.replaceChildren();for(const a of choices.filter(a=>a.type===kind.value))account.append(new Option(a.label||a.id,a.id));setSelectValue(account,account.options[0]?.value||"");models();};
 kind.addEventListener("change",accounts,{signal});account.addEventListener("change",models,{signal});model.addEventListener("change",levels,{signal});
 accounts();
 return {box,active(value:boolean){const changed=box.disabled===value;box.hidden=box.disabled=!value;if(changed)for(const select of box.querySelectorAll("select"))setSelectValue(select,select.value);},mode:():ModelMode=>automatic&&mode.value==="auto"?"auto":"fixed",setMode(value:ModelMode){setSelectValue(mode,value);showMode();},allowAutomatic(value:boolean){automatic=value;modeField.hidden=!value;if(!value)setSelectValue(mode,"fixed");showMode();},read():Binding{const a=selected(),c=capability();if(!a||!model.value.trim())throw new Error(t("请选择可用账号和模型"));if(a.restricted&&!a.models.some(m=>m.id===model.value))throw new Error(t("请选择可用账号和模型"));return {agent_type:a.type,account_id:a.id,model:model.value.trim(),effort:effort.value,effort_control:effort.value?(c.control||defaultReasoning(a.type).control||"effort") as "effort"|"budget":""};},set(b:Binding){setSelectValue(kind,b.agent_type);accounts();setSelectValue(account,b.account_id);models();model.value=b.model;levels();if(b.effort&&!Array.from(effort.options).some(o=>o.value===b.effort))effort.append(new Option(b.effort,b.effort));setSelectValue(effort,b.effort);}};
}
