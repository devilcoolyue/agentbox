#!/usr/bin/env python3
"""Regression for abox-remote (internal/agent/builtin/.local/bin/abox-remote).

SSH is replaced by a local `sh -c`, so the real sh and PowerShell runtimes run
against a temporary root without sshd or a tunnel. The PowerShell runtime runs
when pwsh is on PATH (or AGENTBOX_PWSH points at it); the Windows-only parts
(CreateProcess breakaway, taskkill) still need a real Windows machine.
"""
import contextlib
import importlib.machinery
import importlib.util
import io
import os
import shutil
import sys
import tarfile
import tempfile
import time

# The tool sits inside the tree go:embed packs into the server, and a bytecode cache
# written beside it would ship to every workspace.
sys.dont_write_bytecode = True

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TOOL = os.path.join(REPO, "internal", "agent", "builtin", ".local", "bin", "abox-remote")


def load(state):
    os.environ["ABOX_REMOTE_HOME"] = state
    loader = importlib.machinery.SourceFileLoader("abox_remote", TOOL)
    spec = importlib.util.spec_from_loader("abox_remote", loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    mod.ssh_base = lambda h: ["sh", "-c"]  # the remote command string runs locally
    return mod


def run(mod, *argv):
    """Invoke the CLI in-process; returns (exit code, stdout)."""
    out = io.StringIO()
    sys.argv = ["abox-remote", *argv]
    code = 0
    with contextlib.redirect_stdout(out):
        try:
            mod.main()
        except SystemExit as e:
            code = e.code if isinstance(e.code, int) else (0 if e.code is None else 1)
    return code, out.getvalue()


def expect(cond, msg):
    if not cond:
        raise AssertionError(msg)


def scenario(mod, host, work):
    H = ["-H", host]
    code, out = run(mod, *H, "exec", "--", "echo hello")
    expect(code == 0 and out == "", f"exec output goes straight to the terminal, got {code} {out!r}")

    code, out = run(mod, *H, "run", "ok", "--", "echo step1" if host == "posix" else "'step1'")
    jid = out.splitlines()[0] if out else ""
    expect(code == 0 and jid.endswith("-ok"), f"run: {code} {out!r}")
    code, out = run(mod, *H, "wait", "--timeout", "30")
    expect(code == 0 and "succeeded" in out and "step1" in out, f"wait ok: {code} {out!r}")

    fail = "echo before; exit 7" if host == "posix" else '"before"; sh -c "exit 7"; "never"'
    run(mod, *H, "run", "fail", "--", fail)
    code, out = run(mod, *H, "wait", "--timeout", "30")
    expect(code == 7 and "failed" in out and "never" not in out.splitlines(), f"wait fail: {code} {out!r}")

    run(mod, *H, "run", "long", "--", "sleep 300" if host == "posix" else "Start-Sleep 300")
    time.sleep(1)
    code, out = run(mod, *H, "cancel")
    expect(code == 0 and "canceled" in out, f"cancel: {code} {out!r}")
    code, _ = run(mod, *H, "wait", "--timeout", "10")
    expect(code == mod.EXIT_CANCELED, f"wait canceled: {code}")

    run(mod, *H, "run", "victim", "--", "sleep 300" if host == "posix" else "Start-Sleep 300")
    root = mod.load_hosts()["hosts"][host]["root"]
    victim = sorted(d for d in os.listdir(os.path.join(root, ".abox-remote", "jobs")) if d.endswith("-victim"))[-1]
    pid = int(open(os.path.join(root, ".abox-remote", "jobs", victim, "pid")).read())
    os.killpg(pid, 9)
    time.sleep(0.5)
    code, out = run(mod, *H, "wait", "--timeout", "10")
    expect(code == mod.EXIT_LOST and "lost" in out, f"wait lost: {code} {out!r}")

    code, out = run(mod, *H, "list")
    expect(code == 0 and len(out.strip().splitlines()) == 4, f"list: {out!r}")

    src = os.path.join(work, "src")
    os.makedirs(os.path.join(src, "sub"))
    open(os.path.join(src, "a.txt"), "w").write("hi\n")
    open(os.path.join(src, "sub", "中文 名.txt"), "w", encoding="utf-8").write("内容\n")
    big = os.path.join(work, "big.bin")
    with open(big, "wb") as f:
        f.write(os.urandom(mod.BIG_FILE + 12345))
    expect(run(mod, *H, "push", src, "--to", "proj")[0] == 0, "push dir")
    expect(open(os.path.join(root, "proj", "src", "sub", "中文 名.txt"), encoding="utf-8").read() == "内容\n", "pushed content")
    # A partial upload is resumed from where it stopped.
    os.makedirs(os.path.join(root, "proj"), exist_ok=True)
    with open(big, "rb") as f, open(os.path.join(root, "proj", "big.bin.abox-part"), "wb") as part:
        part.write(f.read(1 << 20))
    expect(run(mod, *H, "push", big, "--to", "proj")[0] == 0, "push big")
    expect(mod.sha256_file(os.path.join(root, "proj", "big.bin")) == mod.sha256_file(big), "big push checksum")
    expect(not os.path.exists(os.path.join(root, "proj", "big.bin.abox-part")), "part file removed")

    back = os.path.join(work, "back")
    expect(run(mod, *H, "pull", "proj/src", back)[0] == 0, "pull dir")
    expect(open(os.path.join(back, "src", "sub", "中文 名.txt"), encoding="utf-8").read() == "内容\n", "pulled content")
    expect(run(mod, *H, "pull", "proj/big.bin", os.path.join(back, "big.bin"))[0] == 0, "pull big")
    expect(mod.sha256_file(os.path.join(back, "big.bin")) == mod.sha256_file(big), "big pull checksum")
    expect(run(mod, *H, "pull", "proj/src/a.txt", os.path.join(back, "renamed.txt"))[0] == 0, "pull file renamed")
    expect(open(os.path.join(back, "renamed.txt")).read() == "hi\n", "renamed content")

    for bad in (["push", src, "--to", "../out"], ["pull", "/etc/passwd"], ["pull", "proj/../../x"]):
        expect(run(mod, *H, *bad)[0] != 0, f"escape accepted: {bad}")

    audit = open(os.path.join(root, ".abox-remote", "audit.log"), encoding="utf-8").read().splitlines()
    expect(any(" exec " in l and "echo hello" in l for l in audit) and any(" start " in l and "-ok " in l for l in audit)
           and any(" cancel " in l for l in audit), f"audit log: {audit}")


def unsafe_archive(mod, work):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        info = tarfile.TarInfo("../evil")
        info.size = 1
        tf.addfile(info, io.BytesIO(b"x"))
    buf.seek(0)
    with tarfile.open(fileobj=buf, mode="r:gz") as tf:
        try:
            list(mod.safe_members(tf, work))
        except SystemExit:
            return
    raise AssertionError("archive entry outside the destination was accepted")


def setup_guidance(mod):
    # Both came from a real macOS setup: the two sharing switches look alike, and a separate
    # account could still read the owner's home through the staff group.
    guide = mod.GUIDE.format(name="m", pub="ssh-ed25519 AAAA", user="agentbox", host="self.abox-link", port=22, flag="")
    expect("Remote Login (not Remote Management" in guide, "guide must name the right macOS switch")
    expect("checkmember -m agentbox com.apple.access_ssh" in guide, "guide must show how to check the allow list")
    expect("chmod 700 ~" in guide, "guide must say how to hide the owner's home")
    # A dropped login is not a key problem; a refused key still is.
    closed = mod.login_hint("Connection closed by 198.18.0.1 port 22")
    expect("Remote Login" in closed and "public key" not in closed, closed)
    denied = mod.login_hint("agentbox@self.abox-link: Permission denied (publickey,password).")
    expect("public key" in denied, denied)


def main():
    base = tempfile.mkdtemp(prefix="abox-remote-test-")
    try:
        mod = load(os.path.join(base, "state"))
        hosts = {"default": "posix", "hosts": {
            "posix": {"host": "local", "port": 22, "user": "u", "root": os.path.join(base, "root-sh"), "os": "posix", "ps": ""}}}
        pwsh = os.environ.get("AGENTBOX_PWSH") or shutil.which("pwsh")
        if pwsh:
            hosts["hosts"]["pwsh"] = {"host": "local", "port": 22, "user": "u", "root": os.path.join(base, "root-ps"), "os": "windows", "ps": pwsh}
        os.makedirs(os.path.join(base, "state"))
        mod.save_hosts(hosts)
        unsafe_archive(mod, base)
        setup_guidance(mod)
        for name in hosts["hosts"]:
            work = os.path.join(base, "work-" + name)
            os.makedirs(work)
            scenario(mod, name, work)
            print(f"abox-remote {name} runtime: exec, jobs (ok/fail/cancel/lost), list, push/pull with resume, path escapes passed")
        if not pwsh:
            print("abox-remote PowerShell runtime skipped: pwsh not found")
    finally:
        shutil.rmtree(base, ignore_errors=True)


if __name__ == "__main__":
    main()
