import {api} from "./api.js";
import { skelBar } from "./skeleton.js";
import {S,bus,emit} from "./state.js";
import {setText,setTextRender,t} from "./i18n.js";
import {actionButton} from "./icons.js";
import {openDiagnostics} from "./diagnostics.js";
import type {OnboardingSnapshot,DiagnosticReport,BrowserDiagnosticCheck} from "./types.js";

export function openSetupSettings(section:"accounts"|"models") {
  if(S.role!=="admin")return;
  // The default model for new workspaces is on the system model list tab.
  S.sec=section;if(section==="models")S.modelTab="system";emit("open-settings");
}

/** One authenticated lifetime owns the guide, requests and observations. */
export function initOnboarding() {
  const lifetime=new AbortController();
  const owner=S.token;
  const details=document.getElementById("home-setup") as HTMLDetailsElement;
  const status=document.getElementById("setup-status")!;
  const steps=document.getElementById("setup-steps")!;
  const refresh=document.getElementById("setup-refresh") as HTMLButtonElement;
  let snapshot:OnboardingSnapshot|undefined;
  let request:AbortController|undefined;
  let observed:{report:DiagnosticReport;browser:BrowserDiagnosticCheck}|undefined;
  let rendered=new AbortController();
  let queued=false;
  let initialized=false;
  let loadedAt=0;
  let accountKey="";
  const visible=()=>S.view==="work" && !S.current && !document.getElementById("empty")!.classList.contains("hidden");
  const current=()=>!lifetime.signal.aborted && S.token===owner;
  const newButton=document.getElementById("empty-new") as HTMLButtonElement;
  newButton.disabled=true;
  steps.replaceChildren();setText(status,"正在读取准备状态…");

  const button=(label:string,icon:string,run:()=>void)=>{
    const node=document.createElement("button");node.type="button";node.className="btn btn-sm";
    actionButton(node,()=>t(label),icon);node.addEventListener("click",run,{signal:rendered.signal});return node;
  };
  const row=(title:string,text:()=>string,action?:HTMLButtonElement)=>{
    const item=document.createElement("li");
    const body=document.createElement("div");const heading=document.createElement("strong");setText(heading,title);
    const note=document.createElement("p");note.className="note";setTextRender(note,text);body.append(heading,note);item.append(body);
    if(action)item.append(action);steps.append(item);return item;
  };

  const render=()=>{
    if(!current()||!snapshot)return;
    const view=snapshot;
    rendered.abort();rendered=new AbortController();
    steps.replaceChildren();
    newButton.disabled=!view.can_create;
    setText(status,view.can_configure?"按下面的顺序准备实例，再创建第一个工作空间。":"使用已授权账号开始；需要更改账号或默认模型时请联系管理员。");
    if(view.can_configure){
      const environment=observed;
      const relevant=environment?.report.checks.filter(c=>!['account_configuration','model','websocket'].includes(c.id));
      row("检查运行环境",()=>{
        if(!environment)return t("尚未检查；请先确认 Docker、镜像、目录和浏览器连接。");
        if(relevant!.some(c=>c.state==="failed")||environment.browser.state==="failed")return t("上次环境检查发现问题，请按诊断建议处理后重新检查。");
        if(relevant!.some(c=>c.state==="not_checked")||environment.browser.state!=="passed")return t("部分环境项目未检查，请查看诊断结果。");
        return t("上次环境检查通过；模型调用仍未验证，可随时重新检查。");
      },button("检查环境","activity",()=>openDiagnostics(undefined,(report,browser)=>{
        if(!current())return;observed={report,browser};render();
      })));
    }
    const configured=view.accounts.filter(a=>a.credentials_present).length;
    const accountRow=row(view.can_configure?"接入账号":"确认可用账号",()=>{
      if(!view.accounts.length)return t(view.can_configure?"还没有账号，请先在账号池接入 Claude 或 Codex。":"当前没有授权账号，请联系管理员配置账号或调整使用范围。");
      if(configured<view.accounts.length)return t(view.can_configure?"部分账号尚未检测到凭证，请在账号池完成接入。":"部分账号尚未检测到凭证，开始任务前请联系管理员确认。");
      return t("已检测到凭证配置；登录有效性和模型权限仍需实际调用验证。");
    },view.can_configure?button("配置账号","settings",()=>openSetupSettings("accounts")):undefined);
    if(view.accounts.length){
      const names=document.createElement("p");names.className="setup-account-names";
      names.textContent=view.accounts.map(a=>a.label||a.id).join(" · ");accountRow.firstElementChild!.append(names);
    }
    row("确认默认模型",()=>{
      const models=Object.entries(view.default_models).filter(([agent])=>view.can_configure||view.accounts.some(a=>a.type===agent));
      return models.length?models.map(([agent,model])=>`${agent==="claude"?"Claude Code":"Codex CLI"} · ${model}`).join(" / "):t("获得账号授权后显示新空间的默认模型。");
    },view.can_configure?button("配置默认模型","settings",()=>openSetupSettings("models")):undefined);
    const create=button("创建工作空间","plus",()=>document.getElementById("btn-new")!.click());
    create.disabled=!view.can_create;
    row("开始第一个项目",()=>t("创建后可上传项目或使用 Git，再发送任务并查看改动。"),create);
  };

  const load=async(force=false)=>{
    if(!current()||!visible())return;
    if(request){queued ||= force;return;}
    if(!force && Date.now()-loadedAt<30_000)return;
    const controller=request=new AbortController();
    const timer=setTimeout(()=>controller.abort(),10000);
    refresh.disabled=true;newButton.disabled=true;setText(status,"正在读取准备状态…");
    // 第一次读取时步骤列表是空的：先铺四条与步骤同形的骨架（标题、说明、右侧按钮）
    if(!steps.children.length)steps.replaceChildren(...Array.from({length:4},(_,i)=>{
      const item=document.createElement("li");item.className="skeleton-row";item.setAttribute("aria-hidden","true");
      const body=document.createElement("div");body.append(skelBar(18+i*6,"text"),skelBar(45+(i*17)%35,"text note"));
      item.append(body,skelBar("7em","btn-like"));return item;
    }));
    try{
      const view=await api<OnboardingSnapshot>("/onboarding",{signal:controller.signal});
      if(!current()||controller.signal.aborted)return;
      if(view.version!==1 || !Array.isArray(view.accounts) || typeof view.can_create!=="boolean" || typeof view.can_configure!=="boolean" || typeof view.has_workspaces!=="boolean" || !view.default_models || typeof view.default_models!=="object" || Array.isArray(view.default_models)
        || view.accounts.some(a=>!a || typeof a.id!=="string" || !["claude","codex"].includes(a.type) || typeof a.label!=="string" || typeof a.credentials_present!=="boolean")
        || Object.values(view.default_models).some(model=>typeof model!=="string") || view.can_create!==(view.accounts.length>0))throw new Error(t("无法读取准备状态，请刷新重试。"));
      const hadWorkspaces=snapshot?.has_workspaces;
      snapshot=view;loadedAt=Date.now();
      if(!initialized || hadWorkspaces===false && view.has_workspaces){details.open=!view.has_workspaces;initialized=true;}
      render();
    }catch(error){
      if(!current())return;
      snapshot=undefined;rendered.abort();steps.replaceChildren();newButton.disabled=true;
      const message=controller.signal.aborted?t("读取准备状态超时，请重试。"):String((error as Error).message);
      setTextRender(status,()=>t("无法读取准备状态，请刷新重试。")+" "+message);
    }finally{clearTimeout(timer);if(request===controller)request=undefined;if(current()){refresh.disabled=false;if(queued){queued=false;void load(true);}}}
  };
  refresh.addEventListener("click",()=>void load(true),{signal:lifetime.signal});
  for(const event of ["app-ready","view-changed","navigation-changed"]){bus.addEventListener(event,()=>{if(visible())void load(true);},{signal:lifetime.signal});}
  bus.addEventListener("data-updated",()=>{
    const key=JSON.stringify(S.accounts.map(a=>[a.id,a.type,a.cred_status]));
    const changed=key!==accountKey;accountKey=key;
    if(visible())void load(changed);
  },{signal:lifetime.signal});
  void load();
  return ()=>{lifetime.abort();rendered.abort();request?.abort();snapshot=undefined;observed=undefined;steps.replaceChildren();status.textContent="";newButton.disabled=true;};
}
