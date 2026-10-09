// Shared Playwright launcher for the core browser regressions. Chromium stays the
// default; AGENTBOX_BROWSER_ENGINE=webkit|firefox runs the same synthetic scenario
// in another engine. Channels (chrome, msedge) only exist for Chromium builds.
import {pathToFileURL} from 'node:url';

export const ENGINES=['chromium','webkit','firefox'];

export function browserEngine(env=process.env){
 const engine=env.AGENTBOX_BROWSER_ENGINE||'chromium';
 if(!ENGINES.includes(engine))throw new Error(`unsupported AGENTBOX_BROWSER_ENGINE: ${engine}`);
 return engine;
}

export async function launchBrowser(options={}){
 const playwright=await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE?pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href:'playwright');
 const engine=browserEngine();
 const channel=engine==='chromium'&&process.env.AGENTBOX_BROWSER_CHANNEL?{channel:process.env.AGENTBOX_BROWSER_CHANNEL}:{};
 return reduceMotion(await playwright[engine].launch({headless:true,...channel,...options}));
}

// Functional regressions run with prefers-reduced-motion: reduce, which the console honours by
// zeroing its motion tokens. Otherwise exit animations keep a closed dialog, popover or menu on
// screen for one transition, and assertions that something is hidden right after closing it
// race the animation. Animation behaviour itself is checked with motion enabled in
// test-motion.mjs. A context or page can still opt in with reducedMotion:'no-preference'.
export function reduceMotion(browser){
 const newContext=browser.newContext.bind(browser),newPage=browser.newPage.bind(browser);
 browser.newContext=(options={})=>newContext({reducedMotion:'reduce',...options});
 browser.newPage=(options={})=>newPage({reducedMotion:'reduce',...options});
 return browser;
}

// Playwright grants clipboard-write only in Chromium; WebKit writes without it and
// reads with clipboard-read.
export function clipboardPermissions(){
 return {chromium:['clipboard-read','clipboard-write'],webkit:['clipboard-read'],firefox:[]}[browserEngine()];
}

// WebKit logs a fetch cancelled by reload or navigation as an access-control console error,
// which Playwright reports as a page error even when the page handles the rejection; a
// rejection the page does not handle still arrives separately as "Unhandled Promise Rejection".
// Playwright 1.58 splits that text at its first colon (name "…cannot load http", message
// "/host/path …"), so the original line is rebuilt before matching.
const CANCELLED_FETCH=/^Fetch API cannot load \S+ due to access control checks\.?$/;
export function cancelledFetchError(error){
 if(browserEngine()!=='webkit')return false;
 const rejoined=/^Fetch API cannot load https?$/.test(error.name)?error.name+':/'+error.message:'';
 return [error.message,rejoined].some(text=>CANCELLED_FETCH.test(String(text).split('\n')[0]));
}

// WebKit 26 (Playwright 1.58) refuses navigator.clipboard.readText() even with
// clipboard-read granted; a real paste into a scratch textarea reads the same pasteboard.
export async function readClipboard(page){
 const read=await page.evaluate(()=>navigator.clipboard.readText().then(text=>({text}),error=>({error:error.name})));
 if(!read.error)return read.text;
 if(browserEngine()==='chromium'||read.error!=='NotAllowedError')throw new Error('clipboard read failed: '+read.error);
 const scratch=await page.evaluateHandle(()=>{const t=document.createElement('textarea');t.style.cssText='position:fixed;left:0;top:0;opacity:0';document.body.append(t);t.focus();return t;});
 try{
  await page.keyboard.press('ControlOrMeta+V');
  await page.waitForFunction(t=>t.value!=='',scratch,{timeout:2000}).catch(()=>{});
  return await scratch.evaluate(t=>t.value);
 }finally{await scratch.evaluate(t=>t.remove());await scratch.dispose();}
}
