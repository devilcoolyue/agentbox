#!/usr/bin/env -S python3 -I
"""Agentbox scoped Git helper. Contains no provider credentials."""
import json
import pathlib
import sys
import urllib.error
import urllib.request
import uuid


def main():
    allowed = {"status", "fetch", "pull", "push-preview", "push"}
    if len(sys.argv) != 2 or sys.argv[1] not in allowed:
        print("用法: abox-git status|fetch|pull|push-preview|push")
        return 2
    with open(pathlib.Path(__file__).resolve().parent / "grant.json", encoding="utf-8") as handle:
        config = json.load(handle)
    # Account proxy environment must never receive the short-lived capability.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    current_id = None

    def call(action, payload=None, request_id=None):
        nonlocal current_id
        current_id = request_id or uuid.uuid4().hex
        body = dict(payload or {}, request_id=current_id)
        req = urllib.request.Request(config["url"] + "/" + action,
            data=json.dumps(body).encode(), method="POST",
            headers={"Authorization": "Bearer " + config["token"], "Content-Type": "application/json"})
        try:
            with client.open(req, timeout=180) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            try:
                message = json.load(error).get("error", "Git 请求失败")
            except (ValueError, AttributeError):
                message = "Git 请求失败（HTTP %s）" % error.code
            raise RuntimeError(message) from None
        except (urllib.error.URLError, TimeoutError):
            raise RuntimeError("无法连接授权网桥；授权可能已失效。写操作结果未知，请在网页核实，勿自动重试。") from None

    try:
        action = sys.argv[1]
        if action == "push":
            preview = call("push-preview")
            print("仓库: %s\n远程: %s\n分支: %s\n连接: %s" % (
                preview["repo"] or "/workspace", preview["url"], preview["ref"], preview["connection"]))
            print("待推送提交（最多 20 条）:\n" + preview["commits"])
            if input("输入 push 确认推送: ").strip() != "push":
                print("已取消，提交保留在本地。")
                return 0
            fields = {k: preview[k] for k in ("ref", "expected_head", "expected_remote_head")}
            result = call("push", fields)
        else:
            result = call(action)
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return 0
    except (KeyboardInterrupt, EOFError):
        if current_id:
            try:
                call("cancel", request_id=current_id)
            except Exception:
                pass
        print("\n已请求取消；远程可能已处理，请在网页核实结果。", file=sys.stderr)
        return 130
    except RuntimeError as error:
        print(str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
