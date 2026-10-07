import assert from 'node:assert/strict';

export async function featureLifetimeSmoke(page){
 const result=await page.evaluate(async()=>{
  const {createChatStream}=await import('/_v/{{BUILD}}/js/features/chat/stream.js');
  const original=requestAnimationFrame,cancel=cancelAnimationFrame,callbacks=new Map();let sequence=0,updates=0;
  const root=document.createElement('div'),footer=document.createElement('div');document.body.append(root);
  window.requestAnimationFrame=fn=>{callbacks.set(++sequence,fn);return sequence;};
  window.cancelAnimationFrame=id=>callbacks.delete(id);
  const stream=createChatStream({append:node=>root.append(node),working(){},state:()=> 'running',footer:()=>({el:footer,update(){updates++;}}),log:()=>root,nearBottom:()=>false});
  const start=text=>{stream.event({type:'stream_event',event:{type:'content_block_start',content_block:{type:'text'}}});stream.event({type:'stream_event',event:{type:'content_block_delta',delta:{type:'text_delta',text}}});};
  try{
   start('old thread');const stale=[...callbacks.values()][0];stream.reset();root.replaceChildren();
   start('new thread');const before=root.textContent;stale(performance.now()+1000);
   const protectedView=root.textContent===before&&updates===0;
   for(const [id,frame] of [...callbacks]){callbacks.delete(id);frame(performance.now()+1000);}
   const current=root.textContent.includes('new thread')&&!root.textContent.includes('old thread');
   stream.reset();const stopped=callbacks.size===0;
   return {protectedView,current,stopped};
  }finally{stream.reset();root.remove();window.requestAnimationFrame=original;window.cancelAnimationFrame=cancel;}
 });
 assert.deepEqual(result,{protectedView:true,current:true,stopped:true});
 console.log('Feature lifetimes: stale real-stream frame cannot touch a new thread; reset cancels frames');
}

export async function thinkingStreamSmoke(page){
 const result=await page.evaluate(async()=>{
  const {createChatStream}=await import('/_v/{{BUILD}}/js/features/chat/stream.js');
  const root=document.createElement('div');document.body.append(root);
  const labels=[];
  const stream=createChatStream({append:node=>root.append(node),working(text){labels.push(typeof text==='function'?text():text);},state:()=> 'running',footer:()=>null,log:()=>root,nearBottom:()=>false});
  const ev=event=>stream.event({type:'stream_event',event});
  const boxes=()=>root.querySelectorAll('details.think').length;
  try{
   // Claude without a requested summary: empty deltas and a signature only.
   ev({type:'content_block_start',content_block:{type:'thinking'}});
   ev({type:'content_block_delta',delta:{type:'thinking_delta',thinking:''}});
   ev({type:'content_block_delta',delta:{type:'signature_delta',signature:'sig'}});
   const omittedWhileThinking=boxes();
   ev({type:'content_block_stop'});
   stream.event({type:'assistant',message:{content:[{type:'thinking',thinking:'',signature:'sig'}]}});
   const omitted=boxes();
   // Codex reasoning item without a summary: start/stop with nothing between.
   ev({type:'content_block_start',content_block:{type:'thinking'}});ev({type:'content_block_stop'});
   const emptyReasoning=boxes();
   // A real summary appears on its first non-empty delta and is replaced by the final render.
   ev({type:'content_block_start',content_block:{type:'thinking'}});
   const beforeText=boxes();
   ev({type:'content_block_delta',delta:{type:'thinking_delta',thinking:'Weighing options'}});
   const streaming=boxes();
   ev({type:'content_block_stop'});
   stream.event({type:'assistant',message:{content:[{type:'thinking',thinking:'Weighing options',signature:'sig'}]}});
   const final=[...root.querySelectorAll('details.think')].map(d=>d.textContent);
   return {omittedWhileThinking,omitted,emptyReasoning,beforeText,streaming,final,label:labels.includes('思考中…')};
  }finally{stream.reset();root.remove();}
 });
 assert.deepEqual(result,{omittedWhileThinking:0,omitted:0,emptyReasoning:0,beforeText:0,streaming:1,final:['思考过程Weighing options'],label:true});
 console.log('Thinking stream: empty thinking blocks never render; summaries appear on first text');
}
