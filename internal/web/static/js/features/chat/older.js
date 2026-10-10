import { setTextRender, t as i18nText } from "../../i18n.js";
import { spinEl } from "../../util.js";
import { ChatHistory } from "./history.js";
import { joinTurn } from "./turns.js";
/* 向上加载更早的对话。首屏只拿最新一页（服务端按整回合切页），还有更早内容时
 * 对话流顶部挂一条加载条：滚到附近自动拉上一页插到上方，保持阅读位置不跳；
 * 失败时显示重试。切空间/线程、整体重载与退出都会作废在途请求。 */
export function createOlderHistory(hooks) {
    const loads = new ChatHistory(hooks.read, hooks.session);
    let cursor = null;
    let busy = 0, seq = 0;
    let observer;
    const bar = document.createElement("div");
    bar.className = "chat-older";
    const spin = spinEl();
    const text = document.createElement("span");
    text.setAttribute("role", "status");
    const button = document.createElement("button");
    button.type = "button";
    button.className = "btn btn-sm";
    bar.append(spin, text, button);
    button.addEventListener("click", () => void load());
    function show(state) {
        spin.hidden = state !== "loading";
        text.hidden = state === "idle";
        button.hidden = state === "loading";
        bar.classList.toggle("error", state === "error");
        setTextRender(text, () => state === "error" ? i18nText("更早的对话加载失败") : i18nText("正在加载更早的对话…"));
        setTextRender(button, () => state === "error" ? i18nText("重试") : i18nText("加载更早的对话"));
    }
    // 重新 observe 会立刻按当前位置回报一次：插入一页后加载条仍在视口附近就接着拉
    function watch() {
        observer ??= new IntersectionObserver(items => {
            if (items.some(item => item.isIntersecting))
                void load();
        }, { root: hooks.log(), rootMargin: "800px 0px 0px 0px" });
        observer.unobserve(bar);
        observer.observe(bar);
    }
    async function load() {
        const c = cursor;
        if (!c || !c.more || busy)
            return;
        const mine = busy = ++seq;
        show("loading");
        try {
            const result = await loads.load(c.session, `/sessions/${c.session}/history?thread=${encodeURIComponent(c.thread)}&before=${c.start}`, async () => { }, value => {
                const start = typeof value.start === "number" ? value.start : -1;
                if (value.thread_id !== c.thread || start < 0 || start >= c.start)
                    throw new Error("history page mismatch");
                const log = hooks.log(), fromBottom = log.scrollHeight - log.scrollTop;
                const box = hooks.render(value);
                joinTurn(box.lastElementChild, bar.nextElementSibling);
                bar.after(...box.children);
                c.start = start;
                c.more = !!value.has_more && start > 0;
                if (!c.more)
                    bar.remove();
                // 按距底部的距离还原，上方插入或移走多少内容视口都停在原处（不依赖 overflow-anchor，Safari 没有）
                log.scrollTop = log.scrollHeight - fromBottom;
            });
            if (result.state === "stale" || cursor !== c)
                return;
            if (result.state === "failed") {
                show("error");
                return;
            }
            if (c.more) {
                show("idle");
                watch();
            }
            hooks.changed();
        }
        finally {
            if (busy === mine)
                busy = 0;
        }
    }
    return {
        /** 整体重建对话流之后调用：记下这批内容的起点，有更早的内容时在顶部挂加载条 */
        reset(next) {
            loads.cancel();
            busy = 0;
            const session = hooks.session();
            cursor = next && session ? { ...next, session } : null;
            if (!cursor?.more) {
                bar.remove();
                return;
            }
            show("idle");
            hooks.log().prepend(bar);
            watch();
        },
        /** 整体重拉开始前：作废在途的上一页请求，内容与加载条原样保留到重拉完成 */
        pause() {
            loads.cancel();
            busy = 0;
            if (cursor?.more)
                show("idle");
        },
        cancel() {
            loads.cancel();
            busy = 0;
            cursor = null;
            bar.remove();
            observer?.disconnect();
            observer = undefined;
        },
        /** 当前已显示内容的起点：重连补拉时只刷新这一段，保留已加载的较早内容 */
        cursor() {
            return cursor && cursor.session === hooks.session() ? { thread: cursor.thread, start: cursor.start } : null;
        },
    };
}
