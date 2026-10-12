import assert from 'node:assert/strict';

// Inspect actual SVG geometry and browser animation effects. This helper only
// changes viewport/zoom, pointer position and an existing animation's clock;
// it never calls a task controller, adds trace events or submits API work.
export async function verifyTraceGeometry(page,trace,{widths=[1440,1000,820],zooms=[1,.8,1.25],height=900}={}){
 const viewport=page.viewportSize(),originalZoom=await trace.evaluate(e=>e.style.zoom);
 const report=[];
 try{
  for(const width of widths)for(const zoom of zooms){
   await page.setViewportSize({width,height});await trace.evaluate((e,zoom)=>e.style.zoom=String(zoom),zoom);
   // ResizeObserver runs after layout, then the trace updates SVG user units.
   await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
   const candidates=await trace.locator('.collaboration-trace-row').evaluateAll(rows=>{
    const types=new Set(),out=[];
    for(const row of rows){
     const path=row.querySelector('.collaboration-trace-path');if(!path)continue;
     const length=path.getTotalLength(),first=path.getPointAtLength(0),last=path.getPointAtLength(length);
     const type=Math.abs(last.x-first.x)<1&&Math.abs(last.y-first.y)>1?'self':last.x>first.x?'forward':'reverse';
     if(types.has(type))continue;types.add(type);out.push({id:row.dataset.entry,type});
    }
    return out;
   });
   assert.deepEqual(candidates.map(r=>r.type).sort(),['forward','reverse','self'],'trace lacks one of the real arrow geometries');
   const shapes=[];
   for(const candidate of candidates){
    const selected=trace.locator(`.collaboration-trace-row[data-entry="${candidate.id}"]`);
    await page.mouse.move(1,1);await selected.hover({position:{x:8,y:8}});
    await selected.locator('.collaboration-trace-packet').evaluate(async packet=>{
     for(let n=0;n<12&&!packet.getAnimations().some(a=>a.effect?.target===packet);n++)await new Promise(requestAnimationFrame);
    });
    const measured=await selected.evaluate(async (row,type)=>{
     const wire=row.querySelector('.collaboration-trace-wire'),path=row.querySelector('.collaboration-trace-path'),packet=row.querySelector('.collaboration-trace-packet');
     const dot=packet.querySelector('.collaboration-trace-dot')||[...packet.querySelectorAll('circle')].find(e=>!e.classList.contains('collaboration-trace-aura'));
     const origin=[...wire.children].find(e=>e.tagName.toLowerCase()==='circle');
     const animation=packet.getAnimations().find(a=>a.effect?.target===packet);
     if(!animation)return{error:'packet has no browser animation',type};
     const timing=animation.effect.getTiming(),duration=Number(timing.duration),prior={time:animation.currentTime,state:animation.playState};
     animation.pause();await animation.ready;
     const length=path.getTotalLength(),matrix=path.getScreenCTM();
     const screen=p=>({x:matrix.a*p.x+matrix.c*p.y+matrix.e,y:matrix.b*p.x+matrix.d*p.y+matrix.f});
     const line=Array.from({length:513},(_,i)=>({...screen(path.getPointAtLength(length*i/512)),progress:i/512}));
     function nearest(point){
      let best={distance:Infinity,progress:0};
      for(let i=1;i<line.length;i++){
       const a=line[i-1],b=line[i],dx=b.x-a.x,dy=b.y-a.y,square=dx*dx+dy*dy;
       const t=square?Math.max(0,Math.min(1,((point.x-a.x)*dx+(point.y-a.y)*dy)/square)):0;
       const distance=Math.hypot(point.x-a.x-t*dx,point.y-a.y-t*dy);
       if(distance<best.distance)best={distance,progress:a.progress+t*(b.progress-a.progress)};
      }
      return best;
     }
     const circle=e=>{const r=e.getBoundingClientRect();return{x:r.x+r.width/2,y:r.y+r.height/2,width:r.width,height:r.height,ratio:r.width/r.height};};
     const samples=[];
     for(let i=0;i<=40;i++){
      const fraction=Math.min(i/40,.999999);animation.currentTime=Number(timing.delay)+duration*fraction;
      const point=circle(dot),near=nearest(point);samples.push({fraction,...point,...near});
     }
     const start=line[0],end=line.at(-1),farX=line.reduce((far,p)=>Math.abs(p.x-start.x)>Math.abs(far-start.x)?p.x:far,start.x);
     const epsilon=Math.max(.6,Math.abs(end.y-start.y)*.08);
     const legs=type==='self'?{
      outgoing:samples.some(p=>Math.abs(p.y-start.y)<epsilon&&Math.abs(p.x-start.x)>2&&Math.abs(p.x-farX)>2),
      turning:samples.some(p=>Math.abs(p.x-farX)<epsilon&&p.y>Math.min(start.y,end.y)+1&&p.y<Math.max(start.y,end.y)-1),
      returning:samples.some(p=>Math.abs(p.y-end.y)<epsilon&&Math.abs(p.x-start.x)>2&&Math.abs(p.x-farX)>2),
     }:undefined;
     const result={type,svg:{width:wire.getBoundingClientRect().width,height:wire.getBoundingClientRect().height,viewBox:wire.getAttribute('viewBox'),preserveAspectRatio:wire.getAttribute('preserveAspectRatio')},origin:circle(origin),duration,start,end,pathLength:length,legs,samples};
     animation.currentTime=prior.time;if(prior.state==='running')animation.play();
     return result;
    },candidate.type);
    const context=JSON.stringify({width,zoom,type:candidate.type,...measured});
    assert.equal(measured.error,undefined,context);
    assert.ok(measured.duration>=900&&measured.duration<=12000,'packet cadence is too fast or stalls: '+context);
    assert.ok(Math.abs(measured.origin.ratio-1)<.02,'origin node is stretched: '+context);
    assert.ok(measured.samples.every(s=>s.width>0&&s.height>0&&Math.abs(s.ratio-1)<.02),'moving dot is stretched: '+context);
    assert.ok(measured.samples.every(s=>s.distance<.75),'packet cuts across or escapes its visible path: '+context);
    assert.ok(measured.samples[0].progress<.03&&measured.samples.at(-1).progress>.97,'packet misses a path endpoint: '+context);
    assert.ok(measured.samples.every((s,i,a)=>!i||s.progress+.005>=a[i-1].progress),'packet travels backwards within an arrow: '+context);
    if(candidate.type==='self')assert.ok(Object.values(measured.legs).every(Boolean),'self-loop skips one of its three segments: '+context);
    shapes.push({...measured,sampleCount:measured.samples.length,maxPathError:Math.max(...measured.samples.map(s=>s.distance)),maxCircleRatioError:Math.max(Math.abs(measured.origin.ratio-1),...measured.samples.map(s=>Math.abs(s.ratio-1))),samples:measured.samples.filter((_,i)=>i%5===0)});
   }
   report.push({width,zoom,shapes});
  }
 }finally{
  await page.mouse.move(1,1);await trace.evaluate((e,zoom)=>e.style.zoom=zoom,originalZoom);if(viewport)await page.setViewportSize(viewport);
 }
 return report;
}
