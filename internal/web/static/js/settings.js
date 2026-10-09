import { createSettingsController } from "./features/settings/controller.js";
export { MODEL_TABS, SET_SECS } from "./features/settings/controller.js";
let active;
export function initSettings() {
    active?.dispose();
    const controller = createSettingsController();
    active = controller;
    return () => { controller.dispose(); if (active === controller)
        active = undefined; };
}
export async function openSettingsView() { await active?.openSettingsView(); }
export function renderSettingsAccounts() { active?.renderSettingsAccounts(); }
export function openAuthDlg(account) { active?.openAuthDlg(account); }
export async function putSettings(patch, button, message, keepDirty = true) { return await active?.putSettings(patch, button, message, keepDirty) ?? false; }
