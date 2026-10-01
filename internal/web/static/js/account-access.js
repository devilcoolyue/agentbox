import { api } from "./api.js";
import { refreshAll } from "./data.js";
import { enhanceSelects, setSelectValue } from "./select.js";
import { toast } from "./util.js";
import { decorateIcons } from "./icons.js";
const labels = { all: "全体用户", users: "指定用户", admin: "仅管理员" };
export const accessLabel = (account) => labels[account.access?.mode || "all"];
export async function editAccountAccess(account) {
    try {
        // Refresh before editing: the settings list may predate another admin's edit.
        const accounts = await api("/accounts");
        const current = accounts.find(a => a.id === account.id);
        if (!current)
            throw new Error("账号已不存在");
        openAccess(current);
    }
    catch (e) {
        toast(e.message, true);
    }
}
function openAccess(account) {
    const dialog = document.createElement("dialog");
    // Static markup only. Account labels and usernames are assigned as text/value.
    dialog.innerHTML = `<form>
    <h2></h2>
    <label>使用范围<select name="mode">
      <option value="all">全体用户</option>
      <option value="users">指定用户（管理员始终可用）</option>
      <option value="admin">仅管理员</option>
    </select></label>
    <label data-users>用户名<textarea name="users" rows="4" placeholder="每行一个用户名，也可用逗号分隔"></textarea></label>
    <p class="muted">保存后阻止未授权用户发起新操作，并停止向其空间同步凭证。已发起的操作和容器进程可能继续运行，已交付的凭证无法收回；如需彻底撤销，请停止相关容器并在上游轮换凭证。</p>
    <p role="alert"></p>
    <div class="dlg-actions"><button type="button" class="btn" data-cancel data-icon="close">取消</button><button type="submit" class="btn btn-primary" data-icon="save">保存</button></div>
  </form>`;
    dialog.querySelector("h2").textContent = account.label + " · 使用范围";
    const form = dialog.querySelector("form");
    const mode = form.elements.namedItem("mode");
    const users = form.elements.namedItem("users");
    const userLabel = dialog.querySelector("[data-users]");
    const save = dialog.querySelector('[type="submit"]');
    const cancel = dialog.querySelector("[data-cancel]");
    const error = dialog.querySelector('[role="alert"]');
    setSelectValue(mode, account.access?.mode || "all");
    users.value = (account.access?.users || []).join("\n");
    const sync = () => {
        userLabel.classList.toggle("hidden", mode.value !== "users");
        users.disabled = mode.value !== "users";
    };
    mode.addEventListener("change", sync);
    sync();
    let saving = false;
    cancel.addEventListener("click", () => dialog.close());
    dialog.addEventListener("cancel", e => { if (saving)
        e.preventDefault(); });
    dialog.addEventListener("close", () => dialog.remove(), { once: true });
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        if (saving)
            return;
        const access = { mode: mode.value };
        if (access.mode === "users") {
            access.users = [...new Set(users.value.split(/[\s,，]+/).filter(Boolean))];
        }
        saving = true;
        save.disabled = cancel.disabled = true;
        mode.disabled = users.disabled = true;
        error.textContent = "";
        try {
            await api("/accounts/" + account.id, { method: "PATCH", body: JSON.stringify({ access }) });
            dialog.close();
            toast("账号使用范围已更新");
            await refreshAll();
        }
        catch (e) {
            error.textContent = e.message;
            if (!dialog.open)
                toast("刷新账号列表失败：" + e.message, true);
        }
        finally {
            saving = false;
            save.disabled = cancel.disabled = mode.disabled = false;
            sync();
        }
    });
    document.body.append(dialog);
    decorateIcons(dialog);
    enhanceSelects(dialog);
    dialog.showModal();
}
