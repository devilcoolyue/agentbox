import type {DraftStorage} from "./draft-store.js";

const key="agentbox.chat-identity.v1",scope=/^[a-f0-9]{64}$/;

/** Only opaque actor scopes, never a token or prompt. The page replacing the
 * browser login clears the prior identity before initializing the new editor.
 * Suspended peer pages must not repeat that clear after the new editor writes. */
export function rememberChatIdentity(storage:DraftStorage,draftScope:string,chatScope:string){
 try{storage.setItem(key,JSON.stringify({draft:scope.test(draftScope)?draftScope:"",chat:scope.test(chatScope)?chatScope:""}));}catch{}
}
export function clearPreviousChatIdentity(storage:DraftStorage,hints:DraftStorage){
 try{
  const raw=storage.getItem(key);if(!raw||raw.length>1024)return;
  const value=JSON.parse(raw) as {draft?:string;chat?:string};
  const prefixes=[scope.test(value.draft||"")?`agentbox.chat-draft.v1.${value.draft}.`:"",scope.test(value.chat||"")?`agentbox.chat-outbox.v1.${value.chat}.`:""].filter(Boolean);
  for(let i=storage.length-1;i>=0;i--){const name=storage.key(i);if(name&&!name.endsWith('.enabled')&&prefixes.some(p=>name.startsWith(p)))storage.removeItem(name);}
  if(scope.test(value.draft||"")){const prefix=`agentbox.chat-hint.v1.${value.draft}.`;for(let i=hints.length-1;i>=0;i--){const name=hints.key(i);if(name?.startsWith(prefix))hints.removeItem(name);}}
  // Keep the opaque previous identity marker so a suspended peer that receives
  // the sign-out event can clear a late local write from the same identity.
  // A subsequent login replaces it with the new scope; it contains no token or
  // user content.
 }catch{}
}
