import {setAttr,setText} from "../../i18n.js";
/** History supplies a stable SQLite-backed anchor, not a forged provider event. */
export function collaborationAnchor(id:string,text:string){
 const section=document.createElement("section");section.className="collaboration-thread";section.dataset.collaborationRun=id;
 const request=document.createElement("div");request.className="collaboration-request";
 const label=document.createElement("div");label.className="collaboration-request-label";const author=document.createElement("strong"),kind=document.createElement("span");setText(author,"你");setText(kind,"任务需求");label.append(author,kind);
 const demand=document.createElement("div");demand.className="msg user collaboration-request-body";demand.textContent=text;
 request.append(label,demand);
 const body=document.createElement("div");body.className="collaboration-thread-body";body.setAttribute("role","group");setAttr(body,"aria-label","协作回复");
 const identity=document.createElement("strong");identity.className="collaboration-reply-loading-label";setText(identity,"协作回复");const pending=document.createElement("p");setText(pending,"正在加载协作进度…");body.append(identity,pending);
 section.append(request,body);return section;
}
