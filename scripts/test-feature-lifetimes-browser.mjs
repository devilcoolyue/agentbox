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
