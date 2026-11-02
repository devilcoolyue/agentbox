import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { resolve } from 'node:path';

// Settings → 登录与活跃 on the synthetic API: an administrator sees each user's
// last activity and the login records, can filter and page them, and text a
// client controls (the User-Agent) stays text. The ordinary-user half lives in
// test-browser.mjs next to the other permission checks.
export async function userActivitySmoke(page) {
  const now = Date.now();
  const evil = '<img src=x onerror="window.__uaInjected=1">';
  const chrome = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36';
  const event = (id, user, method, ua) => ({ id, user, at: now - id * 60_000, method, ip: `203.0.113.${id}`, user_agent: ua });
  const reads = [];
  const handlers = {
    '**/api/user-activity': route => route.fulfill({ json: { users: [
      { name: 'alice', role: 'user', last_active_at: now - 120_000, last_login_at: now - 3_600_000, last_login_ip: '203.0.113.1' },
      { name: 'fixture', role: 'admin', last_active_at: 0, last_login_at: 0, last_login_ip: '' },
    ] } }),
    '**/api/login-events?*': route => {
      const q = new URL(route.request().url()).searchParams;
      reads.push(Object.fromEntries(q));
      if (q.get('before')) return route.fulfill({ json: { events: [event(3, 'alice', 'abox-link', 'Go-http-client/2.0')], has_more: false } });
      if (q.get('user')) return route.fulfill({ json: { events: [event(1, 'alice', 'password', chrome)], has_more: false } });
      return route.fulfill({ json: { events: [event(1, 'alice', 'password', chrome), event(2, 'fixture', 'desktop', evil)], has_more: true } });
    },
  };
  for (const [pattern, handler] of Object.entries(handlers)) await page.route(pattern, handler);
  const users = page.locator('#ua-users-tbody'), rows = page.locator('#ua-events-tbody tr');
  try {
    await page.evaluate(() => { location.hash = '#/settings/logins'; });
    await page.locator('#sec-logins').waitFor();
    assert.equal(await page.locator('#set-nav [data-sec="logins"]').getAttribute('class').then(c => c.includes('active')), true);

    await users.filter({ hasText: 'alice' }).waitFor();
    assert.match(await users.locator('tr').nth(0).innerText(), /alice\s+普通用户\s+\d{4}-.*2 分钟前.*203\.0\.113\.1/s);
    assert.match(await users.locator('tr').nth(1).innerText(), /fixture\s+管理员\s+暂无记录\s+暂无记录/);

    await rows.nth(1).waitFor();
    assert.match(await rows.nth(0).innerText(), /alice\s+密码\s+203\.0\.113\.1\s+Chrome · macOS/);
    assert.equal(await rows.nth(0).locator('td').last().getAttribute('title'), chrome, 'the full User-Agent stays reachable');
    // A client chooses its own User-Agent; it must reach the page as text.
    assert.equal(await rows.nth(1).locator('td').last().innerText(), evil);
    assert.equal(await page.locator('#sec-logins img').count(), 0);
    assert.equal(await page.evaluate(() => window.__uaInjected), undefined);

    await mkdir(resolve('output/playwright'), { recursive: true });
    await page.screenshot({ path: resolve('output/playwright/user-activity.png'), animations: 'disabled' });

    // The next page continues from the last row, and double clicks don't repeat it.
    await page.locator('#ua-more').dblclick();
    await rows.nth(2).waitFor();
    assert.equal(reads.filter(r => r.before).length, 1, 'a double click reads the next page once');
    assert.equal(reads.find(r => r.before).before, '2');
    assert.equal(await rows.count(), 3);
    assert.equal(await page.locator('#ua-more').isHidden(), true, 'nothing more to load');

    // Filtering starts over from the top for that user.
    await page.evaluate(() => {
      const select = document.getElementById('ua-user');
      select.value = 'alice';
      select.dispatchEvent(new Event('change', { bubbles: true }));
    });
    await page.waitForFunction(() => document.querySelectorAll('#ua-events-tbody tr').length === 1);
    assert.deepEqual(reads.at(-1), { limit: '50', user: 'alice' });

    // Refreshing keeps the filter.
    const readsBefore = reads.length;
    await page.locator('#ua-refresh').click();
    for (let i = 0; reads.length === readsBefore && i < 250; i++) await new Promise(r => setTimeout(r, 20));
    assert.equal(reads.length, readsBefore + 1, 'refresh reads the records again');
    assert.equal(reads.at(-1).user, 'alice');
    console.log('User activity: last active and last login per user, login records with method and client, User-Agent stays text, paging without repeats, filter and refresh passed');
  } finally {
    for (const [pattern, handler] of Object.entries(handlers)) await page.unroute(pattern, handler);
  }
}

// What counts as a user being active, watched at the terminal page because that
// is where a running program most resembles someone typing. Expects it with the
// main tab connected. Earlier scenarios click around for real and a late report
// opens a new window (15s, human-activity.ts), so wait for a whole window with no
// report: then nothing is pending and the next action reports at once.
export async function humanActivitySmoke(page, socket, input) {
  let reports = 0;
  const count = route => { reports++; return route.fulfill({ status: 204 }); };
  const until = async (done, timeout) => {
    const deadline = Date.now() + timeout;
    while (!done() && Date.now() < deadline) await new Promise(r => setTimeout(r, 20));
  };
  await page.route('**/api/me/activity', count);
  try {
    await page.locator('#term-mount .xterm').waitFor();
    let seen;
    do {
      seen = reports;
      await page.waitForTimeout(15_100);
    } while (reports !== seen);
    reports = 0;

    // Output from a program running by itself makes xterm answer through the same
    // input path a keystroke takes: that is exactly what must not count.
    input.length = 0;
    socket().send(Buffer.from('agent output\r\n\x1b[6n\x1b[c'));
    await until(() => input.length, 5_000);
    assert.ok(input.some(data => data.includes('\x1b[')), 'xterm answered the terminal queries over the input channel');
    await page.waitForTimeout(100);
    assert.equal(reports, 0, 'agent output and xterm auto-replies are not human activity');

    // Scripted events don't count either.
    await page.evaluate(() => {
      const target = document.querySelector('#term-mount textarea') ?? document.getElementById('term-mount');
      target.dispatchEvent(new KeyboardEvent('keydown', { key: 'a', bubbles: true }));
      target.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
    });
    await page.waitForTimeout(100);
    assert.equal(reports, 0, 'scripted events are not human activity');

    // A real keystroke does, even though xterm stops its propagation.
    await page.locator('.xterm-helper-textarea').focus();
    await page.keyboard.press('x');
    await until(() => reports, 5_000);
    assert.equal(reports, 1, 'a real keystroke reports activity');

    // So does a real click outside the terminal, held until the window ends.
    await page.locator('.tab[data-tab="term"]').click();
    await page.waitForTimeout(100);
    assert.equal(reports, 1, 'reports are throttled');
    await until(() => reports > 1, 16_000);
    assert.equal(reports, 2, 'the click outside the terminal is reported when the window ends');
    console.log('Human activity: agent output, xterm auto-replies and scripted events not counted; real keystroke reported, click elsewhere reported after the throttle window passed');
  } finally {
    await page.unroute('**/api/me/activity', count);
  }
}
