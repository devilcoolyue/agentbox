"""agentbox delegation MCP server (stdio).

Started by the main agent's Claude Code for one turn. It only relays tool calls
to the agentbox server over the per-turn bridge; routing, authorization,
execution and accounting all stay on the server. The server endpoint, ticket
and tool list arrive through the environment written into --mcp-config.
"""
import json
import os
import sys
import threading
import urllib.error
import urllib.request

URL = os.environ.get("AGENTBOX_DELEGATE_URL", "")
TOKEN = os.environ.get("AGENTBOX_DELEGATE_TOKEN", "")
TOOLS = json.loads(os.environ.get("AGENTBOX_DELEGATE_TOOLS", "[]"))
# Account proxy variables are inherited from the CLI; the bridge is local.
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
OUT = threading.Lock()
CALLS = {}


def send(message):
    data = json.dumps(message, ensure_ascii=False)
    with OUT:
        sys.stdout.write(data + "\n")
        sys.stdout.flush()


def post(path, body, timeout):
    request = urllib.request.Request(
        URL + path,
        data=json.dumps(body).encode("utf-8"),
        method="POST",
        headers={"Authorization": "Bearer " + TOKEN, "Content-Type": "application/json"},
    )
    try:
        with OPENER.open(request, timeout=timeout) as response:
            return response.status, response.read(1 << 20).decode("utf-8", "replace")
    except urllib.error.HTTPError as error:
        return error.code, error.read(1 << 16).decode("utf-8", "replace")


def call(request_id, name, arguments):
    path = {"delegate": "/delegate", "delegation_feedback": "/feedback"}.get(name)
    if path is None:
        send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32602, "message": "unknown tool"}})
        return
    body = dict(arguments or {})
    body["call_id"] = str(request_id)
    try:
        status, text = post(path, body, 3 * 3600)
    except Exception as error:  # the bridge closes when the turn ends
        status, text = 0, json.dumps({"status": "failed", "error": "agentbox bridge unavailable: " + type(error).__name__})
    finally:
        CALLS.pop(str(request_id), None)
    send({"jsonrpc": "2.0", "id": request_id, "result": {"content": [{"type": "text", "text": text}], "isError": status != 200}})


def main():
    for raw in sys.stdin:
        try:
            message = json.loads(raw)
        except ValueError:
            continue
        method, request_id = message.get("method"), message.get("id")
        params = message.get("params") or {}
        if method == "initialize":
            send({"jsonrpc": "2.0", "id": request_id, "result": {
                "protocolVersion": params.get("protocolVersion", "2025-06-18"),
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "agentbox", "version": "1"},
            }})
        elif method == "tools/list":
            send({"jsonrpc": "2.0", "id": request_id, "result": {"tools": TOOLS}})
        elif method == "tools/call":
            worker = threading.Thread(target=call, args=(request_id, params.get("name"), params.get("arguments")), daemon=True)
            CALLS[str(request_id)] = worker
            worker.start()
        elif method == "notifications/cancelled":
            target = str(params.get("requestId", ""))
            if target in CALLS:
                try:
                    post("/cancel", {"call_id": target}, 30)
                except Exception:
                    pass
        elif method == "ping":
            send({"jsonrpc": "2.0", "id": request_id, "result": {}})
        elif request_id is not None and method is not None:
            send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": "method not found"}})


if __name__ == "__main__":
    main()
