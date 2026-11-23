
const reduce=matchMedia('(prefers-reduced-motion: reduce)').matches;
const c=document.getElementById('fx'),x=c.getContext('2d');
const bw=58,bh=34,tags=['pid','uts','mnt','ipc','cg2','ovl'];
let W,H,cols,stack,boxes,maxRows,t=0,fade=0;
function size(){W=c.width=innerWidth;H=c.height=innerHeight;cols=Math.floor(W/bw);maxRows=Math.floor(H*0.42/bh);stack=new Array(cols).fill(0);boxes=[];fade=0}
size();addEventListener('resize',size);
function spawn(){boxes.push({col:Math.floor(Math.random()*cols),y:-bh,v:2+Math.random()*3,tag:tags[Math.floor(Math.random()*tags.length)],done:false})}
function frame(){
x.clearRect(0,0,W,H);
if(!fade&&t++%12===0)spawn();
x.globalAlpha=fade?Math.max(0,1-fade):1;
x.font='10px ui-monospace,monospace';x.textAlign='center';
const off=(W-cols*bw)/2;
for(const b of boxes){
if(!b.done){const floor=H-(stack[b.col]+1)*bh;b.y+=b.v;b.v+=0.18;if(b.y>=floor){b.y=floor;b.done=true;b.row=stack[b.col]++}}
const px=off+b.col*bw+3,w=bw-6;
x.fillStyle='rgba(255,122,24,0.10)';x.fillRect(px,b.y,w,bh-4);
x.strokeStyle=b.done?'rgba(255,122,24,0.55)':'rgba(255,179,107,0.95)';x.lineWidth=1.5;x.strokeRect(px,b.y,w,bh-4);
x.fillStyle='rgba(255,179,107,0.8)';x.fillText(b.tag,px+w/2,b.y+bh/2+1);
}
x.globalAlpha=1;
if(!fade&&stack.some(h=>h>=maxRows))fade=0.001;
if(fade){fade+=0.018;if(fade>=1)size()}
if(!reduce)requestAnimationFrame(frame)}
frame();
