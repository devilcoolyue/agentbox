import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Shared synthetic browser suite: the composer's "@" file mentions, fed by a
// synthetic /files/search. Ranking is the server's (Go tests); here only what
// the page does with the answer. No provider calls.
/** Share of the text's pixels that clearly differ between two same-sized screenshots (canvas in the page, no CSP). */
async function pixelDrift(page, a, b) {
 return page.evaluate(async ([a, b]) => {
  const load = src => new Promise((resolve, reject) => { const img = new Image(); img.onload = () => resolve(img); img.onerror = reject; img.src = 'data:image/png;base64,' + src; });
  const pixels = img => { const c = document.createElement('canvas'); c.width = img.width; c.height = img.height; const x = c.getContext('2d'); x.drawImage(img, 0, 0); return x.getImageData(0, 0, img.width, img.height).data; };
  const [ia, ib] = await Promise.all([load(a), load(b)]);
  if (ia.width !== ib.width || ia.height !== ib.height) return 1;
  const pa = pixels(ia), pb = pixels(ib);
  const near = (p, i, q, j, limit) => Math.abs(p[i] - q[j]) <= limit && Math.abs(p[i + 1] - q[j + 1]) <= limit && Math.abs(p[i + 2] - q[j + 2]) <= limit;
  // Ink: pixels away from the background (the top-left corner) in either shot.
  let ink = 0, off = 0;
  for (let i = 0; i < pa.length; i += 4) {
   if (near(pa, i, pa, 0, 40) && near(pb, i, pb, 0, 40)) continue;
   ink++;
   if (!near(pa, i, pb, i, 100)) off++;
  }
  return ink ? off / ink : 0;
 }, [a.toString('base64'), b.toString('base64')]);
}

export async function chatMentionsSmoke(page, { setHistory, messages, socket, setAgent }) {
 const files = [
  {path:'src',dir:true}, {path:'docs',dir:true}, {path:'README.md'},
  {path:'src/demo.py'}, {path:'src/main.go'}, {path:'src/lib',dir:true}, {path:'docs/demo notes.md'},
 ];
 const queries = [];
 let failing = false, slow = 0;
 const search = async route => {
  const q = new URL(route.request().url()).searchParams.get('q');
  queries.push(q);
  if (failing) { await route.fulfill({status:500,json:{error:'boom'}}).catch(() => {}); return; }
  if (slow) await new Promise(resolve => setTimeout(resolve, slow));
  const lower = q.toLowerCase();
  const out = lower === '' ? files.filter(f => !f.path.includes('/'))
   : lower.endsWith('/') ? files.filter(f => f.path.startsWith(lower) && !f.path.slice(lower.length).includes('/'))
   : files.filter(f => f.path.toLowerCase().includes(lower));
  // Superseded keystrokes are aborted by the page; their late answers go nowhere.
  await route.fulfill({json:{files:out, truncated: lower === 'readme'}}).catch(() => {});
 };
 await page.route('**/api/sessions/*/files/search*', search);
 const input = page.locator('#chat-input'), menu = page.locator('#chat-mentions');
 const rows = () => menu.locator('.mention-row').evaluateAll(list => list.map(row =>
  [row.querySelector('.mention-name').textContent, row.querySelector('.mention-dir')?.textContent || ''].join(' ').trim()));
 const shows = async expected => {
  for (let i = 0; ; i++) {
   const got = await rows();
   if (JSON.stringify(got) === JSON.stringify(expected)) return;
   assert.ok(i < 100, `mention rows ${JSON.stringify(got)}, want ${JSON.stringify(expected)}`);
   await page.waitForTimeout(20);
  }
 };
 const reload = async () => {
  const before = socket();
  await page.reload();
  await page.locator('#chat-log .turn, #chat-hero:not(.hidden)').first().waitFor();
  for (let i = 0; socket() === before; i++) { assert.ok(i < 100, 'chat socket did not reconnect'); await page.waitForTimeout(20); }
 };
 const marks = () => page.locator('#chat-input-marks span').evaluateAll(list => list.map(s => [s.className, s.textContent]));
 setAgent('claude');
 setHistory([]);
 await reload();

 // "/command" and "@path" turn blue: a mirror behind the textarea draws the text, an address's "@" stays plain.
 await input.click();
 await input.fill('/compact 保留 @src/demo.py 和 @"docs/demo notes.md" 的接口，me@x.com 不算');
 assert.deepEqual(await marks(), [['mark-command','/compact'],['mark-mention','@src/demo.py'],['mark-mention','@"docs/demo notes.md"']]);
 assert.equal(await page.locator('#chat-input-marks').textContent(), await input.inputValue());
 assert.equal(await input.evaluate(el => el.classList.contains('marked')), true);
 assert.equal(await input.evaluate(el => getComputedStyle(el).color), 'rgba(0, 0, 0, 0)');
 await mkdir('output/playwright', {recursive:true});
 await page.locator('.composer').screenshot({path:'output/playwright/chat-input-marks.png', animations:'disabled'});
 // The mirror must lay out exactly like the textarea, wrapped, after a trailing newline and scrolled.
 const aligned = async (text, scroll) => {
  await input.fill(text);
  await input.evaluate((el, top) => { el.blur(); el.scrollTop = top; }, scroll);
  await page.evaluate(() => new Promise(requestAnimationFrame));
  const style = await page.addStyleTag({content:'.composer-marks span{color:inherit!important;background:none!important}'});
  const painted = await page.locator('.composer-field').screenshot();
  await page.evaluate(() => {
   document.getElementById('chat-input').classList.remove('marked');
   document.getElementById('chat-input-marks').replaceChildren();
  });
  const plain = await page.locator('.composer-field').screenshot();
  await style.evaluate(el => el.remove());
  // Span boundaries may move glyphs by a fraction of a pixel: softer antialiasing, or a
  // whole pixel for a few glyphs of a bitmap fallback font (WenQuanYi on CI). A misplaced
  // line, a different wrap or a one-pixel shift of the layer changes most of the text.
  const drift = await pixelDrift(page, painted, plain);
  assert.ok(drift < 0.1, `mirror text drifted from the textarea: ${(drift * 100).toFixed(1)}% of its pixels differ (${JSON.stringify(text.slice(0, 30))}, scroll ${scroll})`);
  await input.fill('');
 };
 await aligned('/review 帮我看看 @src/main.go 里 handleFiles 的边界条件，'.repeat(3) + 'averyveryverylongwordwithoutanyspacesaverylongwordwithoutanyspacesaverylongword\n\t缩进 @README.md\n', 0);
 await aligned(Array.from({length:18}, (_, i) => `第 ${i + 1} 行 @src/demo.py`).join('\n'), 120);
 await input.fill('普通的一句话');
 assert.deepEqual(await marks(), []);
 assert.equal(await input.evaluate(el => el.classList.contains('marked')), false, 'plain text is drawn by the textarea itself');
 await input.fill('');

 // "@" after a space lists the top level, folders first; an address's "@" doesn't.
 await input.click();
 const searched = queries.length;
 await input.pressSequentially('mail me@x.com ');
 await page.waitForTimeout(150);
 assert.equal(await menu.isVisible(), false);
 assert.equal(queries.length, searched);
 await input.fill('');
 await input.pressSequentially('看看 @');
 await shows(['src/', 'docs/', 'README.md']);
 assert.equal(await input.getAttribute('aria-activedescendant'), 'mention-opt-0');

 // Typing narrows; name first, its folder muted beside it, the match in bold.
 await input.pressSequentially('demo');
 await shows(['demo.py src', 'demo notes.md docs']);
 assert.equal(await menu.locator('.mention-row').first().locator('.mention-name b').innerText(), 'demo');
 assert.equal(await menu.locator('.mention-row').first().locator('svg').getAttribute('data-icon-name'), 'file-code');

 // Enter fills (never sends) and quotes a path with spaces the way the CLI reads it.
 const sent = messages().length;
 await input.press('ArrowDown');
 await input.press('Enter');
 assert.equal(await input.inputValue(), '看看 @"docs/demo notes.md" ');
 assert.deepEqual(await marks(), [['mark-mention','@"docs/demo notes.md"']]);
 assert.equal(await menu.isVisible(), false);
 assert.equal(messages().length, sent);

 // A folder fills "@dir/" and lists inside it; then a file inside.
 await input.pressSequentially('和 @sr');
 await shows(['src/', 'demo.py src', 'main.go src', 'lib/ src']);
 await input.press('Tab');
 assert.equal(await input.inputValue(), '看看 @"docs/demo notes.md" 和 @src/');
 await shows(['demo.py src', 'main.go src', 'lib/ src']);
 await menu.locator('.mention-row').filter({hasText:'main.go'}).click();
 assert.equal(await input.inputValue(), '看看 @"docs/demo notes.md" 和 @src/main.go ');

 // Editing inside a mention searches up to the caret and replaces the whole word.
 await input.evaluate(el => { const at = el.value.indexOf('main.go'); el.setSelectionRange(at, at); });
 await input.press('Backspace');
 await shows(['src/', 'demo.py src', 'main.go src', 'lib/ src']);
 await input.press('ArrowDown');
 await input.press('Enter');
 assert.equal(await input.inputValue(), '看看 @"docs/demo notes.md" 和 @src/demo.py ');

 // Esc closes until the text changes.
 await input.press('End');
 await input.pressSequentially('@d');
 await menu.locator('.mention-row').first().waitFor();
 await input.press('Escape');
 assert.equal(await menu.isVisible(), false);
 await input.press('ArrowLeft');
 await page.waitForTimeout(150);
 assert.equal(await menu.isVisible(), false, 'stays closed while only the caret moves');
 await input.press('End');
 await input.pressSequentially('o');
 await menu.locator('.mention-row').first().waitFor();

 // The message goes out with the paths as typed: the CLI inlines them.
 await input.fill('看看 @src/demo.py');
 await input.press('Escape');
 await input.press('Enter');
 for (let i = 0; messages().length === sent; i++) { assert.ok(i < 100, 'message not sent'); await page.waitForTimeout(20); }
 assert.equal(messages().at(-1).text, '看看 @src/demo.py');
 assert.equal(await menu.isVisible(), false);

 // Enter before this query's results arrive takes this query's first result, not the last list's.
 slow = 300;
 await input.fill('');
 await input.pressSequentially('@READ');
 await input.press('Enter');
 for (let i = 0; await input.inputValue() !== '@README.md '; i++) { assert.ok(i < 100, `waited for results: ${await input.inputValue()}`); await page.waitForTimeout(20); }
 slow = 0;

 // No match, a cut-short index and a failed read each say so instead of vanishing.
 await input.fill('');
 await input.pressSequentially('@zzz');
 await menu.locator('.cmd-note', {hasText:'没有匹配的文件'}).waitFor();
 await input.fill('');
 await input.pressSequentially('@readme');
 await menu.locator('.cmd-note', {hasText:'只搜索了靠上的一部分'}).waitFor();
 failing = true;
 await input.fill('');
 await input.pressSequentially('@err');
 await menu.locator('.cmd-note', {hasText:'暂时读不到工作区文件'}).waitFor();
 failing = false;

 // An attachment placeholder is blue too, the caret lands after a space, and the delete
 // keys take it (and its attachment) in one piece; undo brings both back.
 let uploaded = 0;
 const upload = async route => { uploaded++; await route.fulfill({json:{path:`/shared/.images/shot-${uploaded}.png`,name:`shot-${uploaded}.png`,orig:'shot.png'}}).catch(() => {}); };
 await page.route('**/api/sessions/*/images', upload);
 const thumbs = page.locator('#chat-attach .attach-thumb');
 const attach = async () => {
  const before = uploaded;
  await page.locator('#attach-input').setInputFiles({name:'shot.png',mimeType:'image/png',buffer:Buffer.from('synthetic image')});
  for (let i = 0; uploaded === before || await page.locator('#chat-attach .up').count(); i++) { assert.ok(i < 100, 'upload did not finish'); await page.waitForTimeout(20); }
 };
 const caret = () => input.evaluate(el => [el.selectionStart, el.selectionEnd]);
 await input.fill('看这张');
 await input.press('Escape');
 await attach();
 assert.equal(await input.inputValue(), '看这张[Image #1] ');
 assert.deepEqual(await caret(), [14, 14]);
 assert.deepEqual(await marks(), [['mark-attachment','[Image #1]']]);
 await page.locator('.composer').screenshot({path:'output/playwright/chat-input-attachment.png', animations:'disabled'});
 await input.pressSequentially('哪里不对');
 assert.equal(await input.inputValue(), '看这张[Image #1] 哪里不对');
 await input.evaluate(el => el.setSelectionRange(14, 14));
 await input.press('Backspace');
 assert.equal(await input.inputValue(), '看这张[Image #1]哪里不对', 'the space before the caret goes on its own');
 await input.press('Backspace');
 assert.equal(await input.inputValue(), '看这张哪里不对');
 assert.equal(await thumbs.count(), 0, 'the attachment goes with its placeholder');
 assert.deepEqual(await marks(), []);
 await input.press('ControlOrMeta+z');
 assert.equal(await input.inputValue(), '看这张[Image #1]哪里不对');
 assert.equal(await thumbs.count(), 1, 'undo brings the attachment back');
 assert.deepEqual(await marks(), [['mark-attachment','[Image #1]']]);
 // Delete from in front, a caret inside, a selection over part of it: always the whole placeholder.
 await input.evaluate(el => el.setSelectionRange(3, 3));
 await input.press('Delete');
 assert.equal(await input.inputValue(), '看这张哪里不对');
 await input.press('ControlOrMeta+z');
 await input.evaluate(el => el.setSelectionRange(8, 8));
 await input.press('Backspace');
 assert.equal(await input.inputValue(), '看这张哪里不对');
 await input.press('ControlOrMeta+z');
 await input.evaluate(el => el.setSelectionRange(10, 15));
 await input.press('Backspace');
 assert.equal(await input.inputValue(), '看这张不对');
 assert.equal(await thumbs.count(), 0);
 await input.fill('看这张 ');
 await input.press('End');
 await attach();
 assert.equal(await input.inputValue(), '看这张 [Image #2] ');
 // Removing the attachment takes its placeholder and the space after it.
 await thumbs.filter({hasText:'#2'}).locator('.attach-remove').click();
 assert.equal(await input.inputValue(), '看这张 ');
 while (await page.locator('#chat-attach .attach-remove').count()) await page.locator('#chat-attach .attach-remove').first().click();
 await page.unroute('**/api/sessions/*/images', upload);

 // A finished @path deletes in one piece too; one still being typed goes letter by letter.
 await input.fill('');
 await input.pressSequentially('看 @READ');
 await menu.locator('.mention-row').first().waitFor();
 await input.press('Backspace');
 assert.equal(await input.inputValue(), '看 @REA');
 await input.press('Enter');
 for (let i = 0; await input.inputValue() !== '看 @README.md '; i++) { assert.ok(i < 100, `waited for the mention: ${await input.inputValue()}`); await page.waitForTimeout(20); }
 await input.press('Backspace');
 assert.equal(await input.inputValue(), '看 @README.md');
 await input.press('Backspace');
 assert.equal(await input.inputValue(), '看 ');
 await input.fill('@src/demo.py 看看');
 await input.evaluate(el => el.setSelectionRange(0, 0));
 await input.press('Delete');
 assert.equal(await input.inputValue(), ' 看看');
 await input.fill('');

 // Narrow screens keep the list on screen.
 await page.setViewportSize({width:390,height:844});
 await input.fill('');
 await input.pressSequentially('@demo');
 await shows(['demo.py src', 'demo notes.md docs']);
 const box = await menu.boundingBox();
 assert.ok(box.x >= 0 && box.x + box.width <= 390 && box.y >= 0, 'menu off screen');
 assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
 await mkdir('output/playwright', {recursive:true});
 await page.screenshot({path:'output/playwright/chat-mentions-mobile.png', animations:'disabled'});
 await page.setViewportSize({width:1280,height:900});
 await input.fill('');
 await input.pressSequentially('解释一下 @src/');
 await shows(['demo.py src', 'main.go src', 'lib/ src']);
 await page.screenshot({path:'output/playwright/chat-mentions.png', animations:'disabled'});

 // Sent: a CLI command reads as a command, @paths as file chips; copy keeps the original text.
 setHistory([
  {kind:'user',ts:'2026-10-14T08:00:00Z',text:'/compact 保留 @src/demo.py 的接口说明',turn:{id:'m1',model:'claude-opus-5'}},
  {kind:'event',ts:'2026-10-14T08:00:01Z',event:{type:'result',subtype:'success',num_turns:0,result:'Compacted'}},
  {kind:'user',ts:'2026-10-14T08:01:00Z',text:'解释一下 @src/main.go 和 @"docs/demo notes.md"，再看看 @src/ 目录，发邮件给 me@x.com',turn:{id:'m2',model:'claude-opus-5'}},
  {kind:'event',ts:'2026-10-14T08:01:01Z',event:{type:'assistant',message:{id:'a2',model:'claude-opus-5',content:[{type:'text',text:'好的'}]}}},
  {kind:'user',ts:'2026-10-14T08:02:00Z',text:'/etc/hosts 是什么文件',turn:{id:'m3',model:'claude-opus-5'}},
 ]);
 await reload();
 const bubbles = page.locator('#chat-log .msg.user');
 await bubbles.nth(2).waitFor();
 assert.deepEqual(await bubbles.evaluateAll(list => list.map(b => b.classList.contains('cmd'))), [true, false, false]);
 assert.equal(await bubbles.first().locator('.msg-cmd').innerText(), '/compact');
 assert.equal(await bubbles.first().locator('.msg-cmd-icon').count(), 1);
 assert.deepEqual(await bubbles.nth(1).locator('.msg-mention').allInnerTexts(), ['src/main.go', 'docs/demo notes.md', 'src/']);
 assert.equal(await bubbles.nth(1).locator('.msg-mention').nth(2).locator('svg').getAttribute('data-icon-name'), 'folder');
 assert.equal(await bubbles.nth(2).locator('.msg-cmd, .msg-mention').count(), 0, 'a path at the start is not a command');
 await page.locator('#chat-log').evaluate(el => { el.scrollTop = 0; });
 await page.screenshot({path:'output/playwright/chat-syntax-bubbles.png', animations:'disabled'});
 await page.emulateMedia({colorScheme:'dark'});
 await page.screenshot({path:'output/playwright/chat-syntax-bubbles-dark.png', animations:'disabled'});
 await page.emulateMedia({colorScheme:'light'});
 await page.setViewportSize({width:390,height:844});
 await page.screenshot({path:'output/playwright/chat-syntax-bubbles-mobile.png', animations:'disabled'});
 assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
 await page.setViewportSize({width:1280,height:900});
 setHistory([]);

 // Codex has no @ expansion: the path goes in plain, as in Codex's own picker.
 setAgent('codex');
 await reload();
 // Only the commands the web page handles itself mean anything there.
 await input.fill('/compact @src/demo.py');
 assert.deepEqual(await marks(), []);
 await input.fill('/clear');
 assert.deepEqual(await marks(), [['mark-command','/clear']]);
 await input.fill('');
 await input.click();
 await input.pressSequentially('read @demo');
 await shows(['demo.py src', 'demo notes.md docs']);
 await input.press('Enter');
 assert.equal(await input.inputValue(), 'read src/demo.py ');
 await input.pressSequentially('and @notes');
 await shows(['demo notes.md docs']);
 await input.press('Tab');
 assert.equal(await input.inputValue(), 'read src/demo.py and "docs/demo notes.md" ');
 await input.fill('');
 await page.unroute('**/api/sessions/*/files/search*', search);
 setHistory([]);
 console.log('Chat mentions: blue /command and @path in the input with a pixel-aligned mirror (wrapped, trailing newline, scrolled), command bubbles and file chips once sent, "@" lists the top level, narrows with bold matches, Enter/Tab/click fill without sending, quoted spaced paths, folders open in place, mid-word edits, Esc, pending Enter, empty/truncated/error notes, mobile, Codex plain paths passed');
}
