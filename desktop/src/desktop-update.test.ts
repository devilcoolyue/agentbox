import {afterEach,describe,it,expect,vi} from 'vitest';
import {createSSRApp} from 'vue';
import {renderToString} from 'vue/server-renderer';
import DesktopUpdate from './DesktopUpdate.vue';
import {language} from './i18n';
import {bridge,type Connection} from './bridge';

const previous=language.value;
afterEach(()=>{language.value=previous;vi.unstubAllGlobals();vi.restoreAllMocks();});

describe('desktop update scope and observed connection capabilities',()=>{
 it('distinguishes disconnected, legacy, supported and unknown protocols without running an updater',async()=>{
  vi.stubGlobal('localStorage',{getItem:()=>null});
  const invoke=vi.spyOn(bridge,'invoke');language.value='en';
  const connection:Connection={server:'https://private.example.invalid',user:'private-user',role:'user',capabilities:null};
  const render=(value:Connection|null)=>renderToString(createSSRApp(DesktopUpdate,{connection:value}));
  expect(await render(null)).toContain('Not connected to a server');
  const legacy=await render(connection);expect(legacy).toContain('Basic connection mode');
  expect(legacy).toContain('Desktop app version');expect(legacy).not.toContain('private-user');
  connection.capabilities={protocol_version:1,features:{pairing:1,project_terminals:1,sync:0}};
  expect(await render(connection)).toContain('Directory sync is disabled');
  connection.capabilities.features.sync=1;
  expect(await render(connection)).toContain('Directory sync is enabled');
  connection.capabilities.protocol_version=2;
  expect(await render(connection)).toContain('Unknown desktop protocol');
  expect(invoke).not.toHaveBeenCalled();
 });
 it('renders the scope in all three languages',async()=>{
  vi.stubGlobal('localStorage',{getItem:()=>null});
  for(const [locale,label] of [['zh-CN','桌面应用版本'],['zh-TW','桌面應用程式版本'],['en','Desktop app version']] as const){
   language.value=locale;
   const html=await renderToString(createSSRApp(DesktopUpdate,{connection:null}));
   expect(html).toContain(label);expect(html).toContain('Agent');
  }
 });
});
