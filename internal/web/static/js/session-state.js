/* Shared status semantics; localization remains owned by each UI. */
import { t } from "./i18n.js";
import { workspaceState } from "./contracts/workspace-state.js";
export function sessionState(sess) { return workspaceState(sess, t); }
