import {setSelectValue} from "../../select.js";
import {setText,t} from "../../i18n.js";
import type {DecisionSettings,DecisionSettingsPatch} from "../../types.js";
import type {SaveGroup} from "./savebar.js";

const input=(id:string)=>document.getElementById(id) as HTMLInputElement;
const select=(id:string)=>document.getElementById(id) as HTMLSelectElement;
const ids=["set-decision-revision","set-decision-enabled","set-decision-default","set-decision-gateway","set-decision-key-gateway","set-decision-key-configured","set-decision-key-action","set-decision-key","set-decision-model","set-decision-timeout","set-decision-bytes","set-decision-concurrency","set-decision-access","set-decision-users"];

export function refreshDecisionSettings(){
 setText(document.getElementById("set-decision-gateway-hint")!,select("set-decision-gateway").value==="mindshub"
  ?"Mindshub 需要其对应的 API Key。建议将决策超时设为 15 秒，当前设置不会自动更改。"
  :"使用 TypeSafe 官方 API Key。");
 document.getElementById("set-decision-mindshub-pricing")!.hidden=select("set-decision-gateway").value!=="mindshub";
 document.getElementById("set-decision-gateway-key-note")!.hidden=input("set-decision-key-configured").value!=="true"||input("set-decision-key-gateway").value===select("set-decision-gateway").value;
}

export function fillDecisionSettings(value:DecisionSettings|undefined){
 const root=document.getElementById("decision-settings-fields") as HTMLFieldSetElement;
 root.disabled=!value;
 document.getElementById("decision-settings-unavailable")!.hidden=!!value;
 input("set-decision-revision").value=String(value?.revision??0);
 input("set-decision-enabled").checked=!!value?.plugins.jev.enabled;
 setSelectValue(select("set-decision-default"),value?.default_provider||"builtin");
 setSelectValue(select("set-decision-gateway"),value?.plugins.jev.gateway||"typesafe");
 input("set-decision-key-gateway").value=value?.plugins.jev.gateway||"typesafe";
 input("set-decision-key-configured").value=String(!!value?.plugins.jev.key_configured);refreshDecisionSettings();
 setSelectValue(select("set-decision-key-action"),"keep");input("set-decision-key").value="";
 setText(document.getElementById("set-decision-key-state")!,value?.plugins.jev.key_configured?"API Key 已配置":"尚未配置 API Key");
 input("set-decision-model").value=value?.plugins.jev.model||"jev-1.13.0";
 input("set-decision-timeout").value=String(value?.plugins.jev.timeout_ms??1500);
 input("set-decision-bytes").value=String(value?.plugins.jev.max_request_bytes??32768);
 input("set-decision-concurrency").value=String(value?.plugins.jev.max_concurrency??2);
 setSelectValue(select("set-decision-access"),value?.plugins.jev.access.mode||"admin");
 input("set-decision-users").value=(value?.plugins.jev.access.users||[]).join(", ");
}

export function decisionSettingsGroup():SaveGroup {
 return {id:"decision",get label(){return t("智能决策");},fields:ids,patch:()=>{
  if((document.getElementById("decision-settings-fields") as HTMLFieldSetElement).disabled)return {};
  const action=select("set-decision-key-action").value as "keep"|"set"|"clear";
  const key=input("set-decision-key"),changedGateway=input("set-decision-key-configured").value==="true"&&input("set-decision-key-gateway").value!==select("set-decision-gateway").value;
  key.setCustomValidity(action==="set"&&!key.value.trim()?t("请输入新的 API Key"):action==="keep"&&changedGateway?t("切换接入服务时，请设置新密钥或清除已有密钥。"):"");
  const access=select("set-decision-access").value as "all"|"admin"|"users";
  const users=[...new Set(input("set-decision-users").value.split(/[,，\s]+/).filter(Boolean))];
  input("set-decision-users").setCustomValidity(access==="users"&&!users.length?t("请填写至少一个用户名"):"");
  for(const id of ids){const field=input(id);if(!field.reportValidity())return null;}
  const decision:DecisionSettingsPatch={expected_revision:Number(input("set-decision-revision").value),default_provider:select("set-decision-default").value as "builtin"|"jev",plugins:{jev:{
   enabled:input("set-decision-enabled").checked,gateway:select("set-decision-gateway").value as "typesafe"|"mindshub",model:input("set-decision-model").value.trim(),
   timeout_ms:Number(input("set-decision-timeout").value),max_request_bytes:Number(input("set-decision-bytes").value),max_concurrency:Number(input("set-decision-concurrency").value),
   access:{mode:access,...(access==="users"?{users}:{})},api_key:{action,...(action==="set"?{value:key.value.trim()}:{})},
  }}};
  return {decision};
 }};
}

export function initDecisionSettings(signal:AbortSignal){
 const key=input("set-decision-key"),action=select("set-decision-key-action");
 select("set-decision-gateway").addEventListener("change",()=>{key.setCustomValidity("");refreshDecisionSettings();},{signal});
 key.addEventListener("input",()=>{key.setCustomValidity("");if(key.value)setSelectValue(action,"set");},{signal});
 action.addEventListener("change",()=>{key.setCustomValidity("");if(action.value!=="set")key.value="";},{signal});
 input("set-decision-users").addEventListener("input",()=>input("set-decision-users").setCustomValidity(""),{signal});
 signal.addEventListener("abort",()=>{key.value="";setSelectValue(action,"keep");},{once:true});
}
