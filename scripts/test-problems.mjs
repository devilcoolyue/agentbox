import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {formatProblem, problemMessages, responseError} from '../internal/web/static/js/problems.js';
import {createI18n, interpolate} from '../internal/web/static/js/i18n/core.js';
import en from '../internal/web/static/js/locales/en-dynamic.js';
import tw from '../internal/web/static/js/locales/zh-TW-dynamic.js';
import {ChatConnection} from '../internal/web/static/js/features/chat/connection.js';

const contract = JSON.parse(await readFile(new URL('../internal/server/testdata/problems-v1.json', import.meta.url), 'utf8'));
const id = '0123456789abcdef0123456789abcdef';

test('v1 error contract matches Go fixtures and all three languages', () => {
  assert.equal(contract.version, 1);
  assert.deepEqual(Object.keys(problemMessages).sort(), contract.errors.map(row => row.code).sort());
  for (const locale of ['zh-CN','zh-TW','en']) {
    const i18n = createI18n({catalogs:{en,'zh-TW':tw}, storage:null, systemLanguages:()=>[locale]});
    for (const row of contract.errors) {
      assert.deepEqual(problemMessages[row.code], [row.error,row.hint]);
      if (locale !== 'zh-CN') {
        const catalog = locale === 'en' ? en : tw;
        assert.ok(Object.hasOwn(catalog,row.error), `${locale}: missing ${row.code} message`);
        assert.ok(Object.hasOwn(catalog,row.hint), `${locale}: missing ${row.code} hint`);
      }
      const rendered = formatProblem({...row,error:'untrusted server detail',hint:'untrusted hint',operation_id:id},i18n.t);
      assert.ok(rendered.includes(i18n.t(row.error)));
      assert.ok(rendered.includes(i18n.t(row.hint)));
      assert.ok(rendered.includes(id));
      assert.ok(!rendered.includes('untrusted'));
    }
    i18n.dispose();
  }
});

test('old servers, future error codes and malformed responses remain readable', async () => {
  const translate = (source, params) => interpolate(source, params);
  assert.equal(formatProblem({error:'old error'},translate),'old error');
  assert.equal(formatProblem({code:'future_code',error:'future error',hint:'future hint'},translate),'future error future hint');
  assert.equal(formatProblem({code:'constructor',error:'old error'},translate),'old error');
  assert.equal(formatProblem({error:'old error',operation_id:'unsafe\nvalue'},translate),'old error');
  const error = await responseError(new Response('<html>proxy failure</html>', {status:502,statusText:'Bad Gateway',headers:{'X-Agentbox-Operation-ID':id}}),translate);
  assert.equal(error.status,502);
  assert.ok(error.message.includes('Bad Gateway'));
  assert.ok(error.message.includes(id));
  assert.ok(!error.message.includes('<html>'));
  const json = await responseError(new Response(JSON.stringify({code:'quota_exhausted',error:'private data',operation_id:id,retryable:false}),{status:403}),translate);
  assert.equal(json.problem.retryable,false);
  assert.ok(!json.message.includes('private data'));
});

test('WS connection references survive close callbacks and retries get new references', t => {
  t.mock.timers.enable({apis:['setTimeout']});
  const original = globalThis.WebSocket;
  const sockets=[];
  class Socket {
    static OPEN=1;
    constructor(url) {this.url=url;this.readyState=0;sockets.push(this);}
    close() {}
    send(value) {throw Error('test must not resend messages');}
  }
  globalThis.WebSocket=Socket;
  t.after(()=>{globalThis.WebSocket=original;});
  const states=[];
  const connection=new ChatConnection({message(){},state(...value){states.push(value);},reconnect(){}});
  t.after(()=>connection.dispose());
  connection.connect('ws://localhost/api/sessions/fixture/chat?token=synthetic');
  const first=new URL(sockets[0].url).searchParams.get('connection_id');
  assert.match(first,/^[a-f0-9]{32}$/);
  sockets[0].onclose({code:1006});
  assert.deepEqual(states.at(-1),['closed',0,first]);
  t.mock.timers.tick(1000);
  assert.equal(sockets.length,2);
  const second=new URL(sockets[1].url).searchParams.get('connection_id');
  assert.notEqual(first,second);
  // Late close from the prior generation cannot replace the visible reference.
  const count=states.length;
  sockets[0].onclose({code:1006});
  assert.equal(states.length,count);
  sockets[1].onclose({code:4004});
  t.mock.timers.tick(30000);
  assert.equal(sockets.length,2,'explicit refusal should not retry');
});

test('a chat socket that stops answering is replaced without waiting for a close', t => {
  t.mock.timers.enable({apis:['setTimeout','setInterval','Date']});
  const original = globalThis.WebSocket;
  const sockets=[];
  class Socket {
    static OPEN=1;
    constructor(url) {this.url=url;this.readyState=0;this.sent=[];this.closed=false;sockets.push(this);}
    close() {this.closed=true;}
    send(value) {this.sent.push(value);}
    open() {this.readyState=1;this.onopen();}
  }
  globalThis.WebSocket=Socket;
  t.after(()=>{globalThis.WebSocket=original;});
  const states=[];let reconnects=0;
  const connection=new ChatConnection({message(){},state(...value){states.push(value[0]);},reconnect(){reconnects++;}},{interval:20000,silence:45000,probe:4000});
  t.after(()=>connection.dispose());
  connection.connect('ws://localhost/api/sessions/fixture/chat?token=synthetic');
  sockets[0].open();
  // A server that never said it answers pings is never probed.
  connection.probe();t.mock.timers.tick(60000);
  assert.deepEqual(sockets[0].sent,[]);assert.equal(sockets.length,1);
  connection.heartbeat(true);
  sockets[0].onmessage({data:'{"type":"status"}'});
  // An answered probe keeps the socket.
  connection.probe();
  assert.deepEqual(sockets[0].sent,['{"type":"ping"}']);
  sockets[0].onmessage({data:'{"type":"pong"}'});
  t.mock.timers.tick(4000);
  assert.equal(sockets.length,1);
  // An unanswered one replaces it at once and reloads what was missed.
  connection.probe();t.mock.timers.tick(4000);
  assert.deepEqual(states.slice(-2),['closed','connecting']);assert.equal(sockets[0].closed,true);
  assert.equal(sockets.length,2);
  sockets[0].onmessage({data:'{"type":"late"}'});sockets[0].onclose({code:1006});
  assert.equal(sockets.length,2);
  sockets[1].open();
  assert.equal(reconnects,1);assert.equal(states.at(-1),'connected');
  // Without any probe, the timer notices the silence.
  t.mock.timers.tick(20000);t.mock.timers.tick(20000);
  assert.equal(sockets.length,2);
  t.mock.timers.tick(20000);t.mock.timers.tick(0);
  assert.equal(sockets.length,3);
});
