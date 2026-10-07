import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { readChatReceipt, chatStates } from '../internal/web/static/js/contracts/chat.js';
import { workspaceState } from '../internal/web/static/js/contracts/workspace-state.js';

const schema = JSON.parse(await readFile(new URL('../contracts/chat-errors-v1.schema.json', import.meta.url)));
const examples = JSON.parse(await readFile(new URL('../contracts/chat-v1.examples.json', import.meta.url)));

test('Go/TS share every durable state, frozen envelope and tombstone shape', () => {
  assert.equal(examples.version, 1);
  assert.deepEqual(chatStates, schema.$defs.ChatRequestReceipt.properties.state.enum);
  assert.deepEqual(examples.receipts.map(c => c.state), chatStates);
  for (const c of examples.receipts) {
    assert.equal(readChatReceipt(c, c.session_id, c.request_id), c);
    assert.throws(() => readChatReceipt(c, 'foreign-space'));
    assert.throws(() => readChatReceipt(c, c.session_id, 'a'.repeat(32)));
  }
});

test('malformed/future receipts cannot acknowledge or release a saved task', () => {
  const original = examples.receipts[0];
  for (const patch of [
    {state: 'future_success'}, {state: 'unconfirmed'}, {state: 'constructor'},
    {revision: 0}, {revision: 1.5}, {revision: Number.MAX_SAFE_INTEGER + 1},
    {request_id: 'unsafe'}, {request: null}, {created_at: undefined}, {error_code: {}},
    {request: {...original.request, scope: 'foreign'}},
    {request: {...original.request, thread_id: 'other-thread'}},
    {request: {...original.request, attachments: ['/shared/.file/../../secret']}},
    {request: {...original.request, attachments: Array(65).fill('/shared/.file/fixture')}},
    {request: {...original.request, effort: 'x'.repeat(513)}},
  ]) assert.throws(() => readChatReceipt({...original, ...patch}, original.session_id), JSON.stringify(patch));
  assert.doesNotThrow(() => readChatReceipt({...original, future_additive_field: true}, original.session_id));
});

test('shared workspace status gives running precedence and distinguishes idle stop', () => {
  const t = value => value;
  assert.equal(workspaceState({status:'running', stop_reason:'idle'}, t).label, '运行中');
  assert.equal(workspaceState({status:'stopped', stop_reason:'idle'}, t).label, '休眠');
  assert.equal(workspaceState({status:'stopped'}, t).label, '已停止');
});
