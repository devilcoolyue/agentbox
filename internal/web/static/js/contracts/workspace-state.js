export function workspaceState(sess, t) {
    if (sess.status === "running")
        return { cls: "run", label: t("运行中"), tip: t("容器运行中") };
    if (sess.stop_reason === "idle")
        return { cls: "idle", label: t("休眠"), tip: t("空闲自动停机，发消息或打开终端会自动唤醒") };
    return { cls: "off", label: t("已停止"), tip: t("已停止，文件与对话仍保留；发消息或打开终端会自动启动") };
}
