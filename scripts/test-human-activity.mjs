import test from 'node:test';
import assert from 'node:assert/strict';
import { watchHumanActivity } from '../internal/web/static/js/human-activity.js';

// Throttling of the human-action report behind the admin "last active" time. The
// browser side (only real events count, agent output doesn't) is in
// scripts/test-user-activity.mjs.
const human = { isTrusted: true }, scripted = { isTrusted: false };
function harness(t, connected = () => true) {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'], now: 1_000_000 });
  const listeners = new Map(), sent = [];
  watchHumanActivity({ addEventListener(type, fn, options) { listeners.set(type, { fn, options }); } }, () => {
    if (!connected()) return false;
    sent.push(Date.now());
    return true;
  });
  return { listeners, sent, act: (event = human) => listeners.get('keydown').fn(event) };
}

test('clicks, keys, IME and paste are watched before xterm can stop them', t => {
  const { listeners } = harness(t);
  assert.deepEqual([...listeners.keys()].sort(), ['drop', 'input', 'keydown', 'paste', 'pointerdown']);
  for (const { options } of listeners.values()) assert.equal(options.capture, true);
});

test('the first action reports at once; later ones collapse into one trailing report', t => {
  const h = harness(t);
  h.act();
  assert.deepEqual(h.sent, [1_000_000]);
  t.mock.timers.tick(5_000);
  h.act();
  t.mock.timers.tick(1_000);
  h.act();
  assert.equal(h.sent.length, 1, 'inside the window nothing is sent yet');
  t.mock.timers.tick(9_000);
  assert.deepEqual(h.sent, [1_000_000, 1_015_000], 'the window end carries the actions inside it');
});

test('a window with no action sends nothing, and the next action reports at once', t => {
  const h = harness(t);
  h.act();
  t.mock.timers.tick(60_000);
  assert.equal(h.sent.length, 1, 'no action, no report: time passing alone is not activity');
  h.act();
  assert.deepEqual(h.sent, [1_000_000, 1_060_000]);
});

test('scripted events never count', t => {
  const h = harness(t);
  h.act(scripted);
  t.mock.timers.tick(60_000);
  assert.deepEqual(h.sent, []);
});

test('an action while signed out is not reported later, and does not start a window', t => {
  let up = false;
  const h = harness(t, () => up);
  h.act();
  t.mock.timers.tick(60_000);
  assert.deepEqual(h.sent, []);
  up = true;
  t.mock.timers.tick(60_000);
  assert.deepEqual(h.sent, [], 'signing in alone is not activity');
  h.act();
  assert.equal(h.sent.length, 1);
});
