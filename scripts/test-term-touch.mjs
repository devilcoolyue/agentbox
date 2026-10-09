import assert from 'node:assert/strict';

// Touch scrolling in the terminal (term-touch.ts): one finger drags history instead of the page.
// Real touch input via CDP; `send` writes to the terminal, `input()` returns what reached the PTY.
export async function terminalTouchSmoke(page, {send, input}) {
  const cdp = await page.context().newCDPSession(page);
  const touch = (type, x, y) => cdp.send('Input.dispatchTouchEvent', {type, touchPoints: type === 'touchEnd' ? [] : [{x, y}]});
  // Slow drag that stops before lifting: no fling, so the count follows the distance exactly.
  const drag = async (y0, y1, hold = 200, steps = 20) => {
    await touch('touchStart', 200, y0);
    for (let i = 1; i <= steps; i++) { await touch('touchMove', 200, y0 + (y1 - y0) * i / steps); if (hold) await page.waitForTimeout(16); }
    if (hold) await page.waitForTimeout(hold);
    await touch('touchEnd');
  };
  const term = () => page.evaluate(async () => {
    const {S} = await import('/_v/{{BUILD}}/js/state.js');
    return {viewportY:S.term.buffer.active.viewportY, rowHeight:document.querySelector('.xterm-screen').clientHeight / S.term.rows,
      focused:document.activeElement === S.term.textarea, pageY:Math.round(document.querySelector('#app').getBoundingClientRect().top + scrollY)};
  });
  const sentSince = async from => { await page.waitForTimeout(150); return input().slice(from).join(''); };
  const reports = (text, button) => (text.match(new RegExp(`\\x1b\\[<${button};\\d+;\\d+M`, 'g')) || []).length;

  send(Array.from({length:300}, (_, i) => `history ${i}`).join('\r\n') + '\r\n$ ');
  await page.waitForFunction(async () => (await import('/_v/{{BUILD}}/js/state.js')).S.term.buffer.active.baseY > 100);
  await page.locator('.xterm-helper-textarea').evaluate(el => el.blur());
  const before = await term();
  // Plain shell (no tmux): xterm's own scrollback follows the finger row for row.
  await drag(300, 500);
  const plain = await term();
  assert.equal(before.viewportY - plain.viewportY, Math.floor(200 / before.rowHeight), 'finger drag scrolls xterm scrollback');
  assert.equal(plain.focused, false, 'a drag must not focus the terminal or raise the keyboard');
  assert.equal(plain.pageY, before.pageY, 'the page must not move under the terminal');

  // tmux: alternate screen + SGR mouse reports. Swipes become the same wheel reports as a desktop wheel.
  send('\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[H\x1b[2Jtmux');
  await page.waitForFunction(async () => (await import('/_v/{{BUILD}}/js/state.js')).S.term.modes.mouseTrackingMode !== 'none');
  const step = 3 * before.rowHeight;
  let from = input().length;
  await drag(300, 500);
  let sent = await sentSince(from);
  assert.equal(reports(sent, 64), Math.floor(200 / step), 'drag down sends wheel-up reports');
  assert.equal(sent.replace(/\x1b\[<64;\d+;\d+M/g, ''), '', 'a drag sends no click');
  from = input().length;
  await drag(500, 300);
  assert.equal(reports(await sentSince(from), 65), Math.floor(200 / step), 'drag up sends wheel-down reports');
  // A quick flick keeps scrolling after the finger lifts. Momentum is motion, so it needs
  // motion enabled (the launcher emulates reduced motion, under which a flick stops at once).
  await page.emulateMedia({reducedMotion:'no-preference'});
  from = input().length;
  await drag(600, 450, 0, 4);
  await page.waitForTimeout(2000);
  const flicked = reports(await sentSince(from), 65);
  await page.emulateMedia({reducedMotion:'reduce'});
  assert.ok(flicked > Math.floor(150 / step), `flick keeps scrolling (${flicked} reports)`);

  // Two fingers stay with the browser (pinch zoom); one finger never pans the page.
  const prevented = await page.locator('.xterm-screen').evaluate(screen => {
    const r = screen.getBoundingClientRect();
    const at = (id, dy) => new Touch({identifier:id, target:screen, clientX:r.left + 40 + id * 60, clientY:r.top + 80 + dy});
    const fire = (type, touches) => { const e = new TouchEvent(type, {touches, changedTouches:touches, bubbles:true, cancelable:true}); screen.dispatchEvent(e); return e.defaultPrevented; };
    fire('touchstart', [at(1, 0), at(2, 0)]);
    const pinch = fire('touchmove', [at(1, -20), at(2, 20)]);
    fire('touchend', []);
    fire('touchstart', [at(1, 0)]);
    const single = fire('touchmove', [at(1, 3)]);
    fire('touchend', []);
    return {pinch, single};
  });
  assert.deepEqual(prevented, {pinch:false, single:true});

  // A plain tap still reaches the terminal and raises the keyboard.
  from = input().length;
  await touch('touchStart', 200, 400); await touch('touchEnd');
  await page.waitForTimeout(150);
  assert.equal((await term()).focused, true, 'tap focuses the terminal');
  assert.match(input().slice(from).join(''), /\x1b\[<0;\d+;\d+M/, 'tap is still reported to tmux');
  // Long press opens a paste menu above the finger: no click reaches tmux and focus stays put.
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], {origin:new URL(page.url()).origin});
  const pasteItem = page.locator('.menu-pop button', {hasText:'粘贴'});
  const longPress = async () => {
    from = input().length;
    await touch('touchStart', 200, 400); await page.waitForTimeout(650); await touch('touchEnd');
    await pasteItem.waitFor();
    assert.equal(input().slice(from).join(''), '', 'a long press sends nothing to tmux');
    assert.ok((await pasteItem.boundingBox()).y + 30 < 400, 'paste menu floats above the finger');
  };
  const tapPaste = async () => {
    const box = await pasteItem.boundingBox();
    await touch('touchStart', box.x + box.width / 2, box.y + box.height / 2); await touch('touchEnd');
    await pasteItem.waitFor({state:'detached'});
  };
  send('\x1b[?2004h'); // Claude Code turns on bracketed paste.
  await page.evaluate(() => navigator.clipboard.writeText('第一行\n第二行'));
  await longPress();
  assert.equal((await term()).focused, true, 'opening the menu keeps the keyboard up');
  from = input().length;
  await tapPaste();
  assert.equal(await sentSince(from), '\x1b[200~第一行\r第二行\x1b[201~', 'pasted text arrives as one bracketed paste');
  assert.equal((await term()).focused, true, 'pasting keeps the keyboard up');
  // Images take the same upload path as a desktop paste and type the container path.
  let uploads = 0;
  await page.route('**/api/sessions/*/images', route => { uploads++; return route.fulfill({json:{path:'/shared/.images/fixture.png', name:'fixture.png'}}); });
  await page.evaluate(async () => {
    const canvas = Object.assign(document.createElement('canvas'), {width:4, height:4});
    const png = await new Promise(r => canvas.toBlob(r, 'image/png'));
    await navigator.clipboard.write([new ClipboardItem({'image/png':png})]);
  });
  await longPress();
  from = input().length;
  await tapPaste();
  await page.waitForTimeout(300);
  assert.equal(uploads, 1, 'clipboard image is uploaded');
  assert.equal(input().slice(from).join(''), '/shared/.images/fixture.png ', 'uploaded image path is typed');
  await page.unroute('**/api/sessions/*/images');
  // Tapping the terminal only dismisses the menu.
  await longPress();
  from = input().length;
  await touch('touchStart', 200, 600); await touch('touchEnd');
  await pasteItem.waitFor({state:'detached'});
  assert.equal(await sentSince(from), '', 'the dismissing tap is not a click');
  // Android also fires contextmenu on a long press; xterm would treat it as a right click.
  assert.equal(await page.locator('.xterm-screen').evaluate(el => {
    const e = new PointerEvent('contextmenu', {pointerType:'touch', bubbles:true, cancelable:true});
    el.dispatchEvent(e);
    return e.defaultPrevented;
  }), true);
  send('\x1b[?2004l');
  send('\x1b[?1000l\x1b[?1006l\x1b[?1049l');
  await page.waitForFunction(async () => (await import('/_v/{{BUILD}}/js/state.js')).S.term.modes.mouseTrackingMode === 'none');
}
