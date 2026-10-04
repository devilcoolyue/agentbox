import { t } from './i18n';
import { Channel, invoke } from '@tauri-apps/api/core';

export interface Connection {
  server: string;
  user: string;
  role: string;
  capabilities: null | { protocol_version: number; server_id?: string; features: { pairing: number; project_terminals: number; sync: number; sync_recovery_inspect?: number; sync_recovery_gc?: number } };
}
export interface Session { id: string; name: string; agent: string; status: string; account_label: string }
export interface Project { id: string; session_id: string; name: string; path: string; arguments: string[]; revision: number; created_at: string }
export interface ProjectTerminal { id: string; session_id: string; project_id: string; kind: 'shell' | 'agent'; state: 'open' | 'closing'; arguments: string[]; created_at: string }
export interface SyncBinding { archived:boolean;server_changed:boolean;id: string; revision: number; binding: { project: string; project_path: string; server_id: string; user: string; workspace: string }; directory: string; pending: boolean }
export interface SyncPreview { baseline_current: boolean; plan: { digest: string; operations: { kind: string; path: string }[]; conflicts: { path: string; reason: string }[]; needs_confirmation: boolean; reasons: string[] }; state_revision: number; project_revision: number }
export interface BridgeError { kind: string; message: string }
export type TerminalEvent = { type: 'data'; sequence: number; bytes: number[] } | { type: 'closed'; code: number; message: string };

export function errorMessage(error: unknown): string {
  return typeof error === 'object' && error !== null && 'message' in error ? String(error.message) : t("操作失败，请重试");
}
export function retryable(error: unknown): boolean {
  return typeof error === 'object' && error !== null && 'kind' in error && ['network', 'server'].includes(String(error.kind));
}

export const bridge = {
  invoke,
  channel(callback: (event: TerminalEvent) => void): Channel<TerminalEvent> {
    const channel = new Channel<TerminalEvent>();
    channel.onmessage = callback;
    return channel;
  },
};

export interface SyncReview {
 binding_id: string; batch_id: string; state_revision: number; project_revision: number;
 digest: string; final_matches: boolean; can_finish: boolean; rules_changed: boolean;
 items: { id: string; path: string; kind: string; recorded: string; current: 'before'|'after'|'diverged'; receipt: string; recovery: boolean }[];
}
export interface RecoveryBatch {
 id: string; pending: boolean; action: string; cleanable:boolean; total_files: number; file_offset: number;
 remote_operations: number; remote_retired: number; remote_cleanable: boolean;
 files: { id: string; path: string; side: 'local'|'remote'; disposable: boolean; state?: 'discarding'|'discarded'|'retiring'|'retired' }[];
}

export interface RecoveryPage {
 history: RecoveryBatch[]; next_cursor: string; revision: number; total_batches: number; pending: boolean;
 metadata: {bindings: number; binding_limit: number; bytes: number; byte_limit: number; history_batches: number; history_limit: number; binding_bytes: number};
 local_recovery: null | {files: number; file_limit: number; bytes: number; byte_limit: number};
 local_recovery_status: 'available'|'unavailable';
 remote_storage: null | {active_operations: number; operation_limit: number; retained_receipts: number; receipt_limit: number; recovery_bytes: number; recovery_byte_limit: number; metadata_bytes: number};
 remote_storage_status: 'available'|'unsupported'|'unavailable'|'server_changed';
}

export interface RemoteCleanupReview {
 binding_id: string; batch_id: string; revision: number; digest: string;
 items: {id: string; path: string; kind: string; bytes: number; state: ''|'retiring'|'retired'}[];
}

export interface AbandonReview {
 binding_id:string;batch_id:string;revision:number;digest:string;server_changed:boolean;
 prepared:number;started:number;verified:number;
}
