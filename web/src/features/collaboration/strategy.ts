import type {CollaborationWorkflow} from "../../contracts/collaboration-v1.js";

export function cleanWorkflow(value:unknown):CollaborationWorkflow|undefined {
 if(!value||typeof value!=="object"||Array.isArray(value))return;
 const v=value as Record<string,unknown>;
 if(v.version!==1||!['direct','reviewed'].includes(String(v.mode))||Object.keys(v).some(key=>key!=="version"&&key!=="mode"))return;
 return {version:1,mode:v.mode as CollaborationWorkflow['mode']};
}
type WorkflowView={configuration?:{workflow?:CollaborationWorkflow};run?:{request?:{workflow?:CollaborationWorkflow}}};
/** Absent workflow retains the original reviewed pipeline, including old drafts. */
export const isDirect=(view:WorkflowView)=> (view.configuration?.workflow||view.run?.request?.workflow)?.mode==="direct";
export const activeRoles=(view:WorkflowView):("coordinator"|"executor")[]=>isDirect(view)?["executor"]:["coordinator","executor"];
export const completionLabel=(view:WorkflowView)=>isDirect(view)?"执行完成，结果待应用":"评审通过，结果待应用";
