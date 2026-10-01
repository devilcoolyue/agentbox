export function sessionState(sess) {
    if (sess.status === "running")
        return { cls: "run", label: "运行中", tip: "容器运行中" };
    if (sess.stop_reason === "idle")
        return { cls: "idle", label: "休眠", tip: "空闲自动停机，发消息或打开终端会自动唤醒" };
    return { cls: "off", label: "已停止", tip: "已停止，文件与对话仍保留；发消息或打开终端会自动启动" };
}
