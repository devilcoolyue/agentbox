import {openDiagnostics} from "../../diagnostics.js";
import {$,toast,fmtTime,fmtBytes,askConfirm} from "../../util.js";
import {t,setTextRender} from "../../i18n.js";
import type {SettingsRequests} from "./requests.js";

/** Administration actions share the settings login lifetime, including modal
 * confirmations and downloads. A late response may not act for the next user. */
export function initSettingsMaintenance(signal:AbortSignal,scope:SettingsRequests){
 $("btn-environment-check").addEventListener("click",()=>openDiagnostics(),{signal});
 $("btn-diagnostics").addEventListener("click",async()=>{
  try{
   const data=await scope.request('/diagnostics');if(!scope.current())return;
   const url=URL.createObjectURL(new Blob([JSON.stringify(data,null,2)],{type:'application/json'}));
   const link=document.createElement('a');link.href=url;link.download='agentbox-diagnostics.json';link.click();
   setTimeout(()=>URL.revokeObjectURL(url),1000);
  }catch(error){if(scope.current())toast((error as Error).message,true);}
 },{signal});
 $("btn-storage").addEventListener("click",async()=>{
  try{
   const report=await scope.request<{data:{bytes:number;partial:boolean;scanned_at:number;users:Record<string,number>};disk_available:number}>('/storage');
   if(!report)return;
   setTextRender($("storage-report"),()=>report.data.scanned_at?t("数据文件 {p0} · 磁盘可用 {p1} · {p2}{p3}\n",{p0:String(fmtBytes(report.data.bytes)),p1:String(fmtBytes(report.disk_available)),p2:String(fmtTime(report.data.scanned_at)),p3:report.data.partial?t("（统计未完整）"):""})+Object.entries(report.data.users||{}).map(([name,bytes])=>`${name}: ${fmtBytes(bytes)}`).join(' · '):t("统计中，请稍后刷新"));
  }catch(error){if(scope.current())toast((error as Error).message,true);}
 },{signal});
 $("btn-clear-cache").addEventListener("click",async()=>{
  if(!await askConfirm(()=>t("清理官方市场的下载缓存？下次打开市场会重新下载。"),{icon:'trash',get okLabel(){return t("清理");}})||!scope.current())return;
  try{await scope.request('/cache/marketplace',{method:'DELETE'});if(scope.current())toast(t("缓存已清理"));}
  catch(error){if(scope.current())toast((error as Error).message,true);}
 },{signal});
}
