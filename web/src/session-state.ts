/* Shared status semantics; localization remains owned by each UI. */
import { t } from "./i18n.js";
import { workspaceState, type WorkspaceStatus } from "./contracts/workspace-state.js";
export type { SessionStateView } from "./contracts/workspace-state.js";
export function sessionState(sess: WorkspaceStatus) { return workspaceState(sess, t); }
