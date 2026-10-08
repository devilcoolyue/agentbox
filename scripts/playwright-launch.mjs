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
 return playwright[engine].launch({headless:true,...channel,...options});
}

// Playwright grants clipboard-write only in Chromium; WebKit writes without it and
// reads with clipboard-read.
export function clipboardPermissions(){
 return {chromium:['clipboard-read','clipboard-write'],webkit:['clipboard-read'],firefox:[]}[browserEngine()];
}
