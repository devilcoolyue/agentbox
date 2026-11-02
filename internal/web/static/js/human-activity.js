/* 人为操作检测：管理员「登录与活跃」面板的最后活跃时间就来自这里。全站页面的点击和输入都算。
 *
 * 只认浏览器发出的真实事件（isTrusted），不看任何业务信号：Agent 在对话页流式输出、在
 * 终端里跑命令都不算。终端尤其不能用 xterm 的 onData 判断——程序查询光标位置、设备属性
 * 时 xterm 也从 onData 自动应答，焦点和鼠标上报同样走这条路。
 *
 * 限流：第一次操作立刻上报，之后最多每 INTERVAL 一次，窗口内的操作在窗口结束时补报，
 * 所以记录的时间最多比实际晚一个窗口。没登录（send 返回 false）就不补，下一次操作再试。
 * 这个模块不碰 DOM 以外的状态，登录与发送都交给 send。 */
const EVENTS = ["pointerdown", "keydown", "input", "paste", "drop"];
const INTERVAL = 15_000;
export function watchHumanActivity(root, send) {
    let last = -Infinity;
    let timer = null;
    const flush = () => {
        timer = null;
        if (send())
            last = Date.now();
    };
    const onEvent = (event) => {
        if (!event.isTrusted || timer)
            return;
        const wait = last + INTERVAL - Date.now();
        if (wait <= 0)
            flush();
        else
            timer = setTimeout(flush, wait);
    };
    // 捕获阶段：xterm 等组件处理按键时会阻止冒泡。
    for (const type of EVENTS)
        root.addEventListener(type, onEvent, { capture: true, passive: true });
}
