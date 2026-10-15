import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Copying from programs in the terminal (term-clipboard.ts): `tmux load-buffer -w`
// hands what Claude Code copied to xterm as OSC 52. It reaches the system
// clipboard only right after the user touched the terminal, queries are never
// answered, and when the browser refuses a direct write (Safari) a bar offers
// the copy on click. `send` writes PTY output, `input()` returns what reached the PTY.
export async function terminalClipboardSmoke(page, {send, input}) {
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], {origin:new URL(page.url()).origin});
  const osc = (text, target = '') => `\x1b]52;${target};${Buffer.from(text).toString('base64')}\x07`;
  const clip = () => page.evaluate(() => navigator.clipboard.readText());
  const clipIs = text => page.waitForFunction(async t => (await navigator.clipboard.readText()) === t, text);
  const reset = () => page.evaluate(async () => (await import('/_v/{{BUILD}}/js/term-clipboard.js')).resetTermClipboard());
  const touchTerm = () => page.locator('#term-mount .xterm-screen').click({position:{x:40, y:20}});
  const bar = page.locator('#term-copy');
  await mkdir('output/playwright', {recursive:true});

  await page.evaluate(() => navigator.clipboard.writeText('before'));
  await reset();
  // Nobody touched the terminal: a program cannot replace the clipboard.
  send(osc('unasked'));
  await page.waitForTimeout(300);
  assert.equal(await clip(), 'before', 'OSC 52 without a recent interaction is ignored');

  await touchTerm();
  send(osc('你好 terminal ✓'));
  await clipIs('你好 terminal ✓');
  await page.locator('#toast.show').filter({hasText:'已复制到剪贴板（13 个字符）'}).waitFor();
  assert.equal(await bar.isVisible(), false);

  // Queries and clears do nothing, and nothing is ever sent back to the PTY.
  const sent = input().length;
  await touchTerm();
  send('\x1b]52;c;?\x07');
  send('\x1b]52;c;\x07');
  send('\x1b]52;c;not base64!\x07');
  await page.waitForTimeout(300);
  assert.equal(await clip(), '你好 terminal ✓');
  assert.ok(!input().slice(sent).join('').includes(']52'), 'clipboard queries must not be answered');

  // The browser refuses a write outside a click: the bar offers it and copies on click.
  await page.evaluate(() => {
   const original = navigator.clipboard.writeText.bind(navigator.clipboard);
   let refuse = true;
   navigator.clipboard.writeText = text => refuse ? (refuse = false, Promise.reject(new DOMException('no gesture', 'NotAllowedError'))) : original(text);
  });
  await touchTerm();
  send(osc('第二段\n  多行', 'c'));
  await bar.waitFor();
  assert.equal(await bar.locator('#term-copy-label').innerText(), '终端复制了 8 个字符');
  assert.equal(await bar.locator('#term-copy-preview').innerText(), '第二段 多行');
  assert.equal(await clip(), '你好 terminal ✓', 'nothing is written before the click');
  const box = await bar.boundingBox(), mount = await page.locator('#term-mount').boundingBox();
  assert.ok(box.x >= mount.x && box.x + box.width <= mount.x + mount.width + 0.5, 'the copy bar stays inside the terminal');
  await page.screenshot({path:'output/playwright/term-copy-bar.png', animations:'disabled'});
  await bar.locator('#term-copy-btn').click();
  await clipIs('第二段\n  多行');
  await bar.waitFor({state:'hidden'});
  await page.locator('#toast.show').filter({hasText:'已复制到剪贴板（8 个字符）'}).waitFor();
  console.log('Terminal clipboard: OSC 52 after interaction, unasked/query/clear ignored, click fallback passed');
}
