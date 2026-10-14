import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Shared synthetic browser suite: the composer's "/" command menu, fed by a
// synthetic /commands listing. No provider calls.
export async function chatCommandsSmoke(page, { setHistory, messages, socket, setAgent }) {
 const commands = [
  {name:'clear',description:'Start a new session',argument_hint:'[name]',aliases:['reset','new'],builtin:true},
  {name:'compact',description:'Free up context by summarizing the conversation so far',argument_hint:'<optional custom summarization instructions>',builtin:true},
  {name:'context',description:'Show current context usage',builtin:true},
  {name:'computer-use',description:'Read this skill before the first step of any request to do something in an app'},
  {name:'doctor',description:'terminal only',builtin:true},
  {name:'__remote-workflow',description:'internal',builtin:true},
  {name:'review-pr',description:'Project command: review the open pull request <b>now</b>'},
 ];
 let lists = 0, threads = 0;
 const listing = async route => { lists++; await route.fulfill({json:{state:'available',commands}}); };
 const newThread = async route => { threads++; await route.fulfill({json:{created:false}}); };
 await page.route('**/api/sessions/*/commands', listing);
 await page.route('**/api/sessions/*/chat/threads', newThread);
 const input = page.locator('#chat-input'), menu = page.locator('#chat-commands');
 const names = () => menu.locator('.cmd-row .cmd-name').allInnerTexts();
 const selected = () => menu.locator('.cmd-row[aria-selected="true"] .cmd-name').innerText();
 const reload = async () => {
  const before = socket();
  await page.reload();
  await page.locator('#chat-log .turn, #chat-hero:not(.hidden)').first().waitFor();
  for (let i = 0; socket() === before; i++) { assert.ok(i < 100, 'chat socket did not reconnect'); await page.waitForTimeout(20); }
 };
 setAgent('claude');
 setHistory([
  {kind:'user',ts:'2026-10-14T08:00:00Z',text:'hi',turn:{id:'c1',model:'claude-opus-5'}},
  {kind:'event',ts:'2026-10-14T08:00:01Z',event:{type:'system',subtype:'init',slash_commands:['context'],terminal_slash_commands:['focus','review-pr']}},
  {kind:'event',ts:'2026-10-14T08:00:02Z',event:{type:'assistant',message:{id:'m',model:'claude-opus-5',content:[{type:'text',text:'hello'}],usage:{input_tokens:10}}}},
 ]);
 await reload();

 // Focusing the composer reads the list once; "/" shows it with the web's own commands first.
 await input.click();
 await input.pressSequentially('/');
 await menu.locator('.cmd-row').first().waitFor();
 await page.waitForFunction(() => document.querySelectorAll('#chat-commands .cmd-row').length > 3);
 assert.equal(lists, 1);
 assert.deepEqual(await names(), ['/clear', '/model', '/effort', '/compact', '/context', '/computer-use']);
 assert.equal(await menu.locator('.cmd-row').first().locator('.cmd-tag').innerText(), '网页');
 assert.match(await menu.locator('.cmd-row').nth(3).locator('.cmd-desc').innerText(), /压缩上下文/);
 assert.equal(await input.getAttribute('aria-activedescendant'), 'cmd-opt-0');

 // Prefix before substring, the typed part highlighted; arrows move, Tab fills with a space.
 await input.pressSequentially('comp');
 assert.deepEqual(await names(), ['/compact', '/computer-use']);
 assert.equal(await menu.locator('.cmd-row').first().locator('b').innerText(), 'comp');
 await input.press('ArrowDown');
 assert.equal(await selected(), '/computer-use');
 await input.press('ArrowDown');
 assert.equal(await selected(), '/compact');
 await input.press('Tab');
 assert.equal(await input.inputValue(), '/compact ');
 assert.equal(await menu.isVisible(), false);

 // Descriptions match too; user text stays text.
 await input.fill('');
 await input.pressSequentially('/review');
 assert.equal(await menu.isVisible(), false, 'the CLI listed review-pr as terminal-only');
 await input.fill('');
 await input.pressSequentially('/summar');
 assert.deepEqual(await names(), ['/compact']);

 // Esc closes until the input changes.
 await input.press('Escape');
 assert.equal(await menu.isVisible(), false);
 await input.press('Backspace');
 await menu.locator('.cmd-row').first().waitFor();

 // Enter fills and sends a CLI command as the message itself.
 await input.fill('');
 await input.pressSequentially('/cont');
 const before = messages().length;
 await input.press('Enter');
 for (let i = 0; messages().length === before; i++) { assert.ok(i < 100, 'command not sent'); await page.waitForTimeout(20); }
 assert.equal(messages().at(-1).type, 'user_message');
 assert.equal(messages().at(-1).text, '/context');
 assert.equal(await input.inputValue(), '');

 // /clear and its aliases start a new conversation in the web; nothing goes to the CLI.
 const sent = messages().length;
 for (const typed of ['/clear', '/new']) {
  await input.fill(typed);
  await input.press('Escape');
  await input.press('Enter');
 }
 for (let i = 0; threads < 2; i++) { assert.ok(i < 100, 'new conversation not requested'); await page.waitForTimeout(20); }
 assert.equal(messages().length, sent);
 assert.equal(await input.inputValue(), '');
 // /model opens the model picker instead of changing a one-turn CLI setting.
 await input.fill('/model');
 await input.press('Escape');
 await input.press('Enter');
 await page.locator('#pick-menu:not(.hidden)').waitFor();
 assert.equal(messages().length, sent);
 await page.keyboard.press('Escape');
 await page.mouse.click(5, 5);

 // Clicking a row fills it without sending; narrow screens keep the list on screen.
 await page.setViewportSize({width:390,height:844});
 await input.click();
 await input.fill('');
 await input.pressSequentially('/co');
 await menu.locator('.cmd-row').first().waitFor();
 const box = await menu.boundingBox();
 assert.ok(box.x >= 0 && box.x + box.width <= 390 && box.y >= 0, 'menu off screen');
 assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
 await mkdir('output/playwright', {recursive:true});
 await page.screenshot({path:'output/playwright/chat-commands-mobile.png', animations:'disabled'});
 await menu.locator('.cmd-row').filter({hasText:'/context'}).click();
 assert.equal(await input.inputValue(), '/context ');
 assert.equal(messages().length, sent);
 await page.setViewportSize({width:1280,height:900});
 await input.fill('');
 await input.pressSequentially('/comp');
 await page.screenshot({path:'output/playwright/chat-commands.png', animations:'disabled'});
 await input.fill('');

 // Codex: only the web's commands, no CLI listing.
 setAgent('codex');
 await reload();
 const listed = lists;
 await input.click();
 await input.pressSequentially('/');
 await menu.locator('.cmd-row').first().waitFor();
 assert.deepEqual(await names(), ['/clear', '/model', '/effort']);
 assert.equal(lists, listed);
 await input.fill('');
 await page.unroute('**/api/sessions/*/commands', listing);
 await page.unroute('**/api/sessions/*/chat/threads', newThread);
 setHistory([]);
 console.log('Chat commands: CLI listing with web commands first, terminal-only filtered, prefix/substring/description match, arrows/Tab/Enter/Esc, /clear /new /model handled by the web, click fills, mobile, Codex passed');
}
