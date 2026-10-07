import { afterEach, describe, expect, it } from 'vitest';
import { errorMessage, errorNotice, retryable } from './bridge';
import { language, t, msg, messageRef } from './i18n';
import { workspaceState } from '../../web/src/contracts/workspace-state';
import nativeProblems from './native-problems.json';
import contract from '../../internal/server/testdata/problems-v1.json';
import en from './locales/en';
import traditional from './locales/zh-TW';

const previous = language.value;
afterEach(() => { language.value = previous; });
const id = '0123456789abcdef0123456789abcdef';

describe('web and desktop contract meanings', () => {
  it('renders every structured error and hint in all three languages with the operation ID', () => {
    for (const locale of ['zh-CN', 'zh-TW', 'en'] as const) {
      language.value = locale;
      for (const row of contract.errors) {
        if (locale !== 'zh-CN') {
          const catalog = locale === 'en' ? en : traditional;
          expect(Object.hasOwn(catalog, row.error), row.code).toBe(true);
          expect(Object.hasOwn(catalog, row.hint), row.code).toBe(true);
        }
        const rendered = errorMessage({kind:'server', message:'HTTP fallback', code:row.code, operation_id:id});
        expect(rendered).toContain(t(row.error));
        expect(rendered).toContain(t(row.hint));
        expect(rendered).toContain(id);
        expect(rendered).not.toContain('HTTP fallback');
      }
      expect(workspaceState({status:'running',stop_reason:'idle'}, t).label).toBe(t('运行中'));
      expect(workspaceState({status:'stopped',stop_reason:'idle'}, t).label).toBe(t('休眠'));
      expect(workspaceState({status:'stopped'}, t).label).toBe(t('已停止'));
    }
  });
  it('preserves safe native fallbacks for old/unknown errors without trusting server strings', () => {
    expect(errorMessage({message:'native fallback',code:'future',error:'private',hint:'private',operation_id:id})).toContain('native fallback');
    expect(errorMessage({message:'native fallback',code:'constructor',operation_id:'invalid\nid'})).toBe('native fallback');
    expect(errorMessage({message:'unchanged native error'})).toBe('unchanged native error');
    expect(retryable({kind:'server',retryable:false})).toBe(false);
    expect(retryable({kind:'server'})).toBe(true);
    expect(retryable({kind:'forbidden',retryable:true})).toBe(false);
  });
  it('updates visible structured errors and nested recovery notices without changing captured context', () => {
    language.value='zh-CN';
    const original={code:'quota_exhausted',operation_id:id,message:'native fallback'};
    const notice=messageRef(errorNotice(original));
    const nested=messageRef(msg('{p1}；请刷新目录检查实际结果后再重试。',{p1:errorNotice(original)}));
    const literal=messageRef(errorNotice({message:'文件 /项目/{p1}.txt: unknown detail'}));
    original.code='storage_full';original.operation_id='f'.repeat(32);
    for(const locale of ['en','zh-TW','zh-CN'] as const){
      language.value=locale;
      expect(notice.value).toContain(t('额度已用完'));
      expect(notice.value).toContain(id);
      expect(notice.value).not.toContain(t('数据盘可用空间不足'));
      expect(nested.value).toContain(notice.value);
      expect(nested.value).not.toContain('[object Object]');
      expect(literal.value).toBe('文件 /项目/{p1}.txt: unknown detail');
    }
  });

  it('localizes only known native fallback pairs and keeps arbitrary details literal', () => {
    language.value='zh-CN';
    for(const [kind,source] of Object.entries(nativeProblems)){
      const notice=messageRef(errorNotice({kind,message:source,operation_id:id}));
      for(const locale of ['en','zh-TW','zh-CN'] as const){
        language.value=locale;
        expect(Object.hasOwn(en,source)).toBe(true);expect(Object.hasOwn(traditional,source)).toBe(true);
        expect(notice.value).toContain(t(source));expect(notice.value).toContain(id);
      }
    }
    language.value='en';
    expect(errorMessage({kind:'server',message:'unexpected file /工作空间'})).toBe('unexpected file /工作空间');
    expect(errorMessage({kind:'foreign',message:nativeProblems.server})).toBe(nativeProblems.server);
  });

});
