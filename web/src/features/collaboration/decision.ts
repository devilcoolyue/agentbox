import type {CollaborationBinding,CollaborationRoles} from "../../contracts/collaboration-v1.js";

import {cleanWorkflow} from "./strategy.js";
export type DecisionProvider="inherit"|"builtin"|"jev";
export type ModelMode="fixed"|"auto";
export interface DecisionIntent {version:1;provider:DecisionProvider;roles:{coordinator:{mode:ModelMode};executor:{mode:ModelMode}}}
export interface RoleDecision {binding:CollaborationBinding;source:"manual"|"builtin"|"jev"|"fallback";reason:string}
export interface DecisionView {state:string;revision:number;snapshot?:unknown;attempt?:{id?:string;run_id?:string;provider?:string;model?:string;actual_model?:string;state?:string;input_tokens?:number;output_tokens?:number;tokens_known?:boolean;cost_micro_usd?:number;reserved_micro_usd?:number;error_code?:string;can_reconcile?:boolean}|null;result?:{roles:CollaborationRoles;decisions:{coordinator:RoleDecision;executor:RoleDecision}}|null}
const object=(v:unknown):v is Record<string,unknown>=>!!v&&typeof v==="object"&&!Array.isArray(v);

/** Keep requested automatic intent separate from its concrete fallback binding. */
export function cleanDecision(value:unknown):DecisionIntent|undefined {
 if(!object(value)||value.version!==1||!["inherit","builtin","jev"].includes(String(value.provider))||!object(value.roles))return;
 const a=value.roles.coordinator,b=value.roles.executor;
 if(!object(a)||!object(b)||!["fixed","auto"].includes(String(a.mode))||!["fixed","auto"].includes(String(b.mode)))return;
 return {version:1,provider:value.provider as DecisionProvider,roles:{coordinator:{mode:a.mode as ModelMode},executor:{mode:b.mode as ModelMode}}};
}
const sorted=(value:unknown):unknown=>Array.isArray(value)?value.map(sorted):object(value)?Object.fromEntries(Object.entries(value).sort(([a],[b])=>a.localeCompare(b)).map(([k,v])=>[k,sorted(v)])):value;
const equal=(a:unknown,b:unknown)=>JSON.stringify(sorted(a))===JSON.stringify(sorted(b));
const inputKeys=["scope","source_session_id","repository","text","baseline","roles","context","auto_execute","max_repairs","max_steps","budget_micro_usd","verification_command","step_timeout_seconds","total_runtime_seconds","wait_timeout_seconds","decision","workflow"];
export async function automaticRequestDigest(input:unknown):Promise<string> {
 if(!object(input)||(input.decision!==undefined&&!cleanDecision(input.decision))||(input.workflow!==undefined&&!cleanWorkflow(input.workflow))||(input.decision===undefined&&input.workflow===undefined))throw new Error("Invalid automatic intent");
 const canonical=JSON.stringify(sorted(Object.fromEntries(inputKeys.map(key=>[key,input[key]]))));
 const bytes=await crypto.subtle.digest("SHA-256",new TextEncoder().encode(canonical));
 return Array.from(new Uint8Array(bytes),n=>n.toString(16).padStart(2,"0")).join("");
}

/** Automatic receipts confirm the original intent, never the resolved model. */
export function matchesAutomaticReceipt(value:unknown,input:unknown,requestID:string):boolean {
 if(!object(input)||input.decision===undefined&&input.workflow===undefined)return true;
 const expected=cleanDecision(input.decision);
 if(input.decision!==undefined&&!expected||!object(value)||value.version!==1||value.scope!==input.scope||!object(value.run)||value.run.request_id!==requestID||!object(value.run.request))return false;
 const actual=value.run.request,decision=cleanDecision(actual.decision);
 if(!equal(expected,decision))return false;
 for(const key of inputKeys.filter(key=>key!=="decision")){
  if(!equal(input[key],actual[key]))return false;
 }
 return true;
}

export function decisionReason(value:RoleDecision):string {
 const reasons:Record<string,string>={
  fixed_model:"使用指定模型",unique_candidate:"只有一个可用候选模型",builtin_fallback:"使用已配置的回退模型",
  configuration_changed:"决策设置已变化，使用回退模型",request_limit:"决策材料超出限制，使用回退模型",provider_busy:"Jev 繁忙，使用回退模型",
  provider_error:"Jev 暂不可用，使用回退模型",provider_abstained:"建议不够确定，使用回退模型",provider_suggestion:"Jev 建议的模型",
  recovered_fallback:"已恢复为预设模型",authorization_fallback:"候选授权已变化，使用回退模型",
  fixed:"使用指定模型",manual:"使用指定模型",single_candidate:"只有一个可用候选模型",workspace_default:"使用已配置的回退模型",
  builtin_default:"内置规则选择模型",builtin:"内置规则选择模型",jev_selected:"Jev 建议的模型",jev:"Jev 建议的模型",
  provider_disabled:"Jev 已关闭，使用回退模型",plugin_disabled:"Jev 已关闭，使用回退模型",provider_unavailable:"Jev 暂不可用，使用回退模型",
  unavailable:"Jev 暂不可用，使用回退模型",timeout:"Jev 超时，使用回退模型",provider_timeout:"Jev 超时，使用回退模型",
  low_confidence:"建议不够确定，使用回退模型",abstain:"建议不够确定，使用回退模型",invalid_suggestion:"建议不可用，使用回退模型",
  budget_unavailable:"决策预算不足，使用回退模型",budget_limit:"决策预算不足，使用回退模型",not_authorized:"无权使用 Jev，使用回退模型",
 };
 const fallback={manual:"使用指定模型",builtin:"内置规则选择模型",jev:"Jev 建议的模型",fallback:"使用已配置的回退模型"};
 return reasons[value.reason]||fallback[value.source]||"使用已配置的回退模型";
}
