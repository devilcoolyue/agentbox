"""Real controlling-terminal acceptance, launched only with Go test fixtures."""
import errno
import fcntl
import os
import pty
import select
import signal
import subprocess
import sys
import termios
import time

binary, config, scenario = sys.argv[1:]
command = [binary, "-test.run=^TestAdminPasswordTerminalHelper$", "--",
           "admin-reset-password", "--config", config, "--user", "boxadmin"]
secret = b"synthetic-terminal-password"

if scenario == "no-tty":
    result = subprocess.run(command, input=secret + b"\n", capture_output=True,
                            start_new_session=True, timeout=10)
    assert result.returncode != 0
    assert b"interactive terminal is required" in result.stderr
    assert secret not in result.stdout + result.stderr
    sys.exit(0)

master, slave = pty.openpty()
pid = os.fork()
if pid == 0:
    os.close(master)
    os.setsid()
    fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
    for fd in (0, 1, 2):
        os.dup2(slave, fd)
    if slave > 2:
        os.close(slave)
    child = subprocess.Popen(command)
    signal.signal(signal.SIGINT, lambda sig, frame: child.send_signal(sig))
    signal.signal(signal.SIGTERM, lambda sig, frame: child.send_signal(sig))
    code = child.wait()
    # Keep the controlling session alive while verifying shell-facing state.
    attrs = termios.tcgetattr(0)
    assert attrs[3] & termios.ECHO, "terminal echo not restored"
    raw = list(attrs)
    raw[3] &= ~termios.ICANON
    termios.tcsetattr(0, termios.TCSANOW, raw)
    os.set_blocking(0, False)
    try:
        assert not os.read(0, 8192), "password remained in terminal input"
    except BlockingIOError:
        pass
    termios.tcsetattr(0, termios.TCSANOW, attrs)
    os.write(1, b"\nterminal-cleanup-verified\n")
    os._exit(code if code >= 0 else 128 - code)

transcript = bytearray()
reaped = False


def read_until(needle):
    deadline = time.monotonic() + 8
    while needle not in transcript:
        assert time.monotonic() < deadline, "terminal prompt timed out"
        if select.select([master], [], [], 0.1)[0]:
            chunk = os.read(master, 8192)
            assert chunk, "terminal closed before prompt"
            transcript.extend(chunk)


try:
    read_until(b"New password: ")
    assert not termios.tcgetattr(master)[3] & termios.ECHO, "password would echo"
    if scenario in ("sigint", "sigterm"):
        os.write(master, b"synthetic-unfinished-secret")
        os.kill(pid, signal.SIGINT if scenario == "sigint" else signal.SIGTERM)
    else:
        os.write(master, secret + b"\n")
        read_until(b"Repeat new password: ")
        assert not termios.tcgetattr(master)[3] & termios.ECHO, "confirmation would echo"
        os.write(master, (secret if scenario == "success" else b"different-password") + b"\n")

    deadline = time.monotonic() + 8
    status = None
    while status is None:
        assert time.monotonic() < deadline, "recovery did not exit"
        if select.select([master], [], [], 0.1)[0]:
            try:
                transcript.extend(os.read(master, 8192))
            except OSError as exc:
                if exc.errno != errno.EIO:
                    raise
        finished, result = os.waitpid(pid, os.WNOHANG)
        if finished:
            reaped, status = True, result
    # Drain any final diagnostic written just before exit.
    while select.select([master], [], [], 0)[0]:
        try:
            chunk = os.read(master, 8192)
        except OSError as exc:
            if exc.errno == errno.EIO:
                break
            raise
        if not chunk:
            break
        transcript.extend(chunk)
    assert b"terminal-cleanup-verified" in transcript, "terminal cleanup was not verified"
    assert termios.tcgetattr(master)[3] & termios.ECHO, "terminal echo not restored"
    assert secret not in transcript and b"different-password" not in transcript, "password leaked"
    assert b"synthetic-unfinished-secret" not in transcript, "unfinished password leaked"
    if scenario == "success":
        assert os.waitstatus_to_exitcode(status) == 0
        assert b'"password_reset":true' in transcript
    else:
        assert os.waitstatus_to_exitcode(status) != 0
        assert b'"password_reset":true' not in transcript
        if scenario == "mismatch":
            assert b"passwords do not match" in transcript
finally:
    if not reaped:
        os.kill(pid, signal.SIGKILL)
        os.waitpid(pid, 0)
    os.close(master)
    os.close(slave)
