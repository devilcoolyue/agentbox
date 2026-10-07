import {createSettingsController} from "./features/settings/controller.js";
import type {Account} from "./types.js";
import type {SettingsPatch} from "./features/settings/savebar.js";
export {SET_SECS} from "./features/settings/controller.js";
let active:ReturnType<typeof createSettingsController>|undefined;
export function initSettings(){
 active?.dispose();const controller=createSettingsController();active=controller;
 return ()=>{controller.dispose();if(active===controller)active=undefined;};
}
export async function openSettingsView(){await active?.openSettingsView();}
export function renderSettingsAccounts(){active?.renderSettingsAccounts();}
export function openAuthDlg(account:Account){active?.openAuthDlg(account);}
export async function putSettings(patch:SettingsPatch,button:HTMLButtonElement|null,message?:string,keepDirty=true){return await active?.putSettings(patch,button,message,keepDirty)??false;}
