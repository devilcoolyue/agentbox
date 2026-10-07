import type {ChatRequestInput} from "../../types.js";
import type {Draft} from "./draft-store.js";

export interface SendSnapshot { owner: string; content: string; input: ChatRequestInput; draft: Draft }
interface Hooks {
 read: () => SendSnapshot | undefined;
 allowed: () => boolean;
 durable: () => boolean;
 connected: () => boolean;
 connect: () => void;
 validate: () => Promise<boolean>;
 deliver: (snapshot: SendSnapshot, current: () => boolean) => Promise<void>;
 changed: () => void;
 edited: () => void;
 waking: () => void;
 awake: () => void;
 timeout: () => void;
}

/** One explicit send owns its frozen input, validation and optional legacy wake.
 * Cancel invalidates in-flight completions; it never replays a delivery. */
export class ChatSender {
 private generation = 0;
 private pending?: {timer: ReturnType<typeof setTimeout>; resolve: () => void};
 busy = false;
 constructor(private hooks: Hooks, private wakeLimit = 60_000) {}
 cancel() {
  ++this.generation; this.busy = false;
  if(this.pending) {clearTimeout(this.pending.timer);this.pending.resolve();this.pending=undefined;}
 }
 async send() {
  if(this.busy || !this.hooks.allowed()) return;
  const snapshot=this.hooks.read();if(!snapshot) return;
  const generation=++this.generation;
  const owns=()=>generation===this.generation && this.hooks.read()?.owner===snapshot.owner;
  const current=()=>owns() && this.hooks.read()?.content===snapshot.content;
  this.busy=true;this.hooks.changed();
  try {
   if(!this.hooks.durable() && !this.hooks.connected()) {
    this.hooks.waking();this.hooks.connect();
    const deadline=Date.now()+this.wakeLimit;
    while(owns()&&!this.hooks.connected()&&Date.now()<deadline) {
     await new Promise<void>(resolve=>{this.pending={resolve,timer:setTimeout(()=>{this.pending=undefined;resolve();},400)};});
    }
    if(!owns())return;
    if(!this.hooks.connected()){this.hooks.timeout();return;}
    this.hooks.awake();
   }
   if(!await this.hooks.validate() || !owns() || !this.hooks.allowed()) return;
   if(!current()){this.hooks.edited();return;}
   await this.hooks.deliver(snapshot,current);
  } finally {
   if(generation===this.generation){this.busy=false;this.hooks.changed();}
  }
 }
}
