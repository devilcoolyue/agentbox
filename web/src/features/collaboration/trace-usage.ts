import type {CollaborationStepUsage} from "../../contracts/collaboration-v1.js";
import type {TraceEntry} from "./trace-model.js";

export const tokens=(value:number)=>value>=1000000?`${(value/1000000).toFixed(2)}M`:value>=1000?`${(value/1000).toFixed(1)}k`:String(value);
export const dollars=(micro:number)=>`$${(micro/1000000).toFixed(micro>0&&micro<100?6:4)}`;

/** A turn is charged once. Dispatch, output and handoff rows are not charges. */
export function traceStepUsage(entry:TraceEntry,entries:readonly TraceEntry[],usage:readonly CollaborationStepUsage[]=[],replay=false):CollaborationStepUsage|undefined {
 if(replay||!entry.stepID)return;
 const matching=entries.filter(e=>e.stepID===entry.stepID);
 const anchor=matching.find(e=>e.id.includes(`step:${entry.stepID}:end:`))||matching.find(e=>e.id.endsWith(`step:${entry.stepID}:start`))||matching.find(e=>e.id.endsWith(`step:${entry.stepID}:dispatch`));
 if(anchor?.id!==entry.id)return;
 const result=usage.find(s=>s.step_id===entry.stepID);
 // A newer view can arrive before its event page. Do not attach the final
 // settlement to an earlier start just because its end has not loaded yet.
 if(result&&!["accepted","starting","running","finalizing"].includes(result.state)&&!matching.some(e=>e.id.includes(`step:${entry.stepID}:end:`)))return;
 return result;
}
export function traceTokenTotal(usage:readonly CollaborationStepUsage[]=[]):{value:number|null;partial:boolean}{
 const known=usage.filter(s=>s.total_tokens!==null);
 const total=known.reduce((sum,s)=>sum+s.total_tokens!,0);
 return{value:known.length&&Number.isSafeInteger(total)?total:null,partial:usage.some(s=>s.total_tokens===null||s.status!=="settled")};
}
