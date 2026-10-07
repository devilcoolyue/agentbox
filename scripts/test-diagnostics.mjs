import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {diagnosticMessages} from '../internal/web/static/js/diagnostic-messages.js';
import en from '../internal/web/static/js/locales/en-dynamic.js';
import tw from '../internal/web/static/js/locales/zh-TW-dynamic.js';

test('diagnostic vocabulary matches Go and has both translations without missing placeholders',async()=>{
  const fixture=JSON.parse(await readFile(new URL('../internal/diagnostics/testdata/messages-v1.json',import.meta.url),'utf8'));
  assert.deepEqual(diagnosticMessages,fixture);
  for(const values of Object.values(fixture))for(const source of values){
    assert.ok(Object.hasOwn(en,source),'missing English: '+source);
    assert.ok(Object.hasOwn(tw,source),'missing Traditional: '+source);
  }
});
