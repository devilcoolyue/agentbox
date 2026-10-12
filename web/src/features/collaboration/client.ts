import {api} from "../../api.js";
import {validView} from "./protocol.js";
import {t} from "../../i18n.js";
import type {CollaborationConfiguration as Configuration,CollaborationStepUsage,CollaborationWorkflow} from "../../contracts/collaboration-v1.js";
import type {Choice} from "./roles.js";
import type {DecisionIntent,DecisionView} from "./decision.js";
export type {Configuration};
export interface Context {thread_id:string;anchor:number;include:boolean;sha256:string;text:string;truncated:boolean}
export interface Run {id:string;request_id:string;state:string;phase:string;wait_reason?:string;revision:number;config_version:number;plan_version:number;pending_question?:string;steps_used?:number;request?:{text:string;source_session_id:string;repository:string;context?:Context;decision?:DecisionIntent;workflow?:CollaborationWorkflow}}
export interface View {version:1;scope:string;run:Run;configuration:Configuration;step:{id:string;revision:number;state:string;role:string}|null;step_usage?:CollaborationStepUsage[];cost:{cost_micro_usd:number;partial:boolean;decision_cost_micro_usd?:number;decision_reserved_micro_usd?:number;decision_calls?:number;decision_partial?:boolean};decision?:DecisionView|null}
export interface Event {seq:number;type:string;created_at:string;body:unknown}
export interface Options {version:1;scope:string;thread_mode?:number;workflow_version?:number;automatic_intent_version?:number;decision?:{jev_available:boolean;default_provider:"builtin"|"jev"};accounts:Choice[]}
export const requestID=()=>crypto.randomUUID().replaceAll("-","");
export const terminal=(state:string)=>["completed","cancelled","failed","deleted"].includes(state);
export const stateLabels:Record<string,string>={accepted:"已接收",preparing:"准备项目",planning:"规划中",awaiting_plan:"等待确认计划",executing:"执行中",waiting_answer:"正在答疑",waiting_user:"等待你的答复",reviewing:"评审中",paused:"已暂停",cancelling:"正在取消",cancelled:"已取消",completed:"评审通过，结果待应用",failed:"任务失败",uncertain:"结果待核对",deleted:"已删除"};
export function messageText(value:unknown):string {
 if(!value||typeof value!=="object")return "";const b=value as Record<string,unknown>,lines:string[]=[];
 for(const key of ["summary","text"])if(typeof b[key]==="string")lines.push(b[key] as string);
 for(const key of ["items","constraints","completed","remaining","tests","changes","options"])if(Array.isArray(b[key]))for(const item of b[key]){if(typeof item==="string")lines.push("• "+item);else if(item&&typeof item==="object"&&typeof item.text==="string")lines.push("• "+item.text);}
 if(Array.isArray(b.code_map))for(const entry of b.code_map){if(!entry||typeof entry!=="object"||typeof entry.path!=="string"||typeof entry.purpose!=="string")continue;lines.push("\n"+entry.path+(Array.isArray(entry.symbols)?" · "+entry.symbols.filter((s:unknown)=>typeof s==="string").join(", "):""),entry.purpose);if(Array.isArray(entry.constraints))for(const constraint of entry.constraints)if(typeof constraint==="string")lines.push("• "+constraint);if(typeof entry.excerpt==="string")lines.push(entry.excerpt);}
 return lines.join("\n");
}
export function collaborationClient(scope:string,signal:AbortSignal){
 const check=<T extends {version:number;scope:string}>(v:T)=>{if(v.version!==1||v.scope!==scope)throw new Error(t("登录状态已变化"));return v;};
 return {
  async list(session:string,thread:string){return check(await api<{version:number;scope:string;runs:Run[]}>(`/collaboration/runs?session=${encodeURIComponent(session)}&thread=${encodeURIComponent(thread)}`,{signal})).runs;},
  async view(id:string){const v=await api<View>(`/collaboration/runs/${id}`,{signal});if(!validView(v,id,scope))throw new Error(t("无法确认任务状态"));return v;},
  async events(id:string,after:number){return check(await api<{version:number;scope:string;events:Event[]}>(`/collaboration/runs/${id}/events?after=${after}`,{signal})).events;},
  async context(session:string,thread:string,include:boolean){return check(await api<{version:number;scope:string;context:Context}>(`/sessions/${session}/collaboration/context?thread=${encodeURIComponent(thread)}&include=${include?1:0}`,{signal})).context;},
  async preflight(source_session_id:string,repository:string,configuration:Configuration){return check(await api<{version:number;scope:string;baseline:string}>("/collaboration/preflight",{method:"POST",body:JSON.stringify({scope,source_session_id,repository,roles:configuration.roles,...(configuration.workflow?{workflow:configuration.workflow}:{})}),signal})).baseline;},
  put(path:string,input:unknown){return api(path,{method:"PUT",body:JSON.stringify(input),signal});},
  query(path:string){return api(path,{signal});},
 };
}
