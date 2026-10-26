import test from 'node:test';
import assert from 'node:assert/strict';
import { dockLift, lastAtOrBefore, outlineAnswer, outlineQuestion, railLayout } from '../internal/web/static/js/features/chat/outline-text.js';

const label = kind => kind === '图片' ? '[图片]' : '[附件]';

test('question keeps the original text, with attachment markers shortened', () => {
  assert.equal(outlineQuestion('  帮我看下\n\n这个为什么呢？ ', label), '帮我看下 这个为什么呢？');
  assert.equal(outlineQuestion('看图 [图片#1 /shared/.images/a-b.png] 和 [附件#2 /shared/.file/x.log] 呢', label), '看图 [图片] 和 [附件] 呢');
  // the server cuts long messages at 200 runes, possibly inside a marker
  assert.equal(outlineQuestion('前面的话 [图片#3 /shared/.ima…', label), '前面的话 [图片]');
});

test('answer is the first sentence of the reply, without markdown', () => {
  assert.equal(outlineAnswer('已经按这个改了。\n\n进入页面时两种额度会并发全量刷新。'), '已经按这个改了。');
  assert.equal(outlineAnswer('这次其实已经走过 sentinel 了，卡在下结果图。日志里连续三次 403。'), '这次其实已经走过 sentinel 了，卡在下结果图。');
  assert.equal(outlineAnswer('## 结论\n\n**改好了**，超时调到 15 秒。'), '改好了，超时调到 15 秒。');
  assert.equal(outlineAnswer('# 只有标题'), '只有标题');
  assert.equal(outlineAnswer('```go\nfunc main() {}\n```\n\nFixed it. The test covers it.'), 'Fixed it.');
  assert.equal(outlineAnswer('Bumped to v0.1.11 and 3.5x faster. Done'), 'Bumped to v0.1.11 and 3.5x faster.');
  assert.equal(outlineAnswer('- `local.commerce` 已卸载并禁用，端口不再监听'), 'local.commerce 已卸载并禁用，端口不再监听');
  assert.equal(outlineAnswer('See [the docs](https://x.y/z) first! Then retry.'), 'See the docs first!');
  assert.equal(outlineAnswer('---\n| a | b |\n|---|---|\n结果如上表。'), '结果如上表。');
  assert.equal(outlineAnswer('```\nonly code, never closed'), '');
  assert.equal(outlineAnswer(''), '');
  assert.equal(outlineAnswer('长'.repeat(200), 120), '长'.repeat(120) + '…');
});

test('dock lift peaks under the pointer and fades to zero at the radius', () => {
  assert.equal(dockLift(0, 45), 1);
  assert.ok(dockLift(10, 45) > dockLift(20, 45));
  assert.ok(Math.abs(dockLift(22.5, 45) - 0.5) < 1e-9);
  assert.equal(dockLift(45, 45), 0);
  assert.equal(dockLift(90, 45), 0);
  assert.equal(dockLift(Infinity, 45), 0);
});

test('rail layout compresses the gap, then shows a window of ticks', () => {
  assert.deepEqual(railLayout(20, 400), { gap: 10, windowed: false, height: 200 });
  assert.deepEqual(railLayout(50, 400), { gap: 8, windowed: false, height: 400 });
  const many = railLayout(600, 400);
  assert.equal(many.gap, 6);
  assert.equal(many.windowed, true);
  assert.equal(many.height, 396);
});

test('reading position is the last loaded message above the reading line', () => {
  const tops = [0, 300, 800, 1500];
  assert.equal(lastAtOrBefore(0, 4, i => tops[i], -1), -1);
  assert.equal(lastAtOrBefore(0, 4, i => tops[i], 0), 0);
  assert.equal(lastAtOrBefore(0, 4, i => tops[i], 799), 1);
  assert.equal(lastAtOrBefore(0, 4, i => tops[i], 5000), 3);
});
