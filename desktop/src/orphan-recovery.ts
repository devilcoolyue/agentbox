export interface RecoveryEntry { kind: 'file'|'directory'; size: number; hash?: string; executable?: boolean }
export interface RemoteRecoveryStatus {
  operation: {operation_id: string; status: 'applied'|'uncertain'; recovery: boolean; replayed: boolean};
  path: string; kind: string; before: RecoveryEntry|null; after: RecoveryEntry|null;
  digest: string; device: string; project: string; retirement?: ''|'retiring'|'retired';
}
export interface OrphanPage {
  server_id: string; workspace: string; device: string;
  items: {id: string; status?: RemoteRecoveryStatus; issue?: 'missing_record'|'invalid_record'}[];
  next_cursor: string;
}
export interface OrphanReview {
  server_id: string; workspace: string; status: RemoteRecoveryStatus;
  comparison: 'matches_after'|'matches_before'|'changed'|'project_missing'|'project_changed'|'unavailable';
  current?: RecoveryEntry; can_retire: boolean; digest: string;
  recovery_state: 'available'|'missing'|'corrupt'|'none'|'retiring'|'retired';
  lease_active: boolean; local_pending: boolean;
}
export const comparisonLabels: Record<OrphanReview['comparison'], string> = {
  matches_after: '当前文件符合该次操作之后的状态',
  matches_before: '当前文件符合该次操作之前的状态',
  changed: '当前文件已不同于该次操作前后的状态',
  project_missing: '原项目已不存在，当前文件无法核对',
  project_changed: '原项目目录已改变，当前文件无法核对',
  unavailable: '当前文件无法核对',
};
export function recoveryStatusLabel(status: RemoteRecoveryStatus): string {
  if (status.retirement === 'retired') return '原内容已清理';
  if (status.retirement === 'retiring') return '清理待续做';
  return status.operation.status === 'applied' ? '操作已执行' : '执行结果不确定';
}
