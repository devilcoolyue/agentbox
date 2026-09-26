# Invoked with python3 -I: no workspace modules, environment hooks or site imports.
# EOF on the Docker exec stdin is a cancellation signal. The child gets its own
# session/process group, so Git helpers and grandchildren are terminated too.
import os
import select
import signal
import subprocess
import sys
import time

child = subprocess.Popen(sys.argv[1:], stdin=subprocess.DEVNULL, start_new_session=True)
cancelled = False
try:
    while child.poll() is None:
        ready, _, _ = select.select([sys.stdin.buffer], [], [], 0.1)
        if ready:
            os.read(sys.stdin.fileno(), 1)
            cancelled = True
            break
    if cancelled:
        try:
            os.killpg(child.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        deadline = time.monotonic() + 2
        while child.poll() is None and time.monotonic() < deadline:
            time.sleep(0.05)
        # The parent may have exited while a helper ignored TERM. Still kill the
        # whole dedicated group before returning from the supervisor.
        try:
            os.killpg(child.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    code = child.wait()
    sys.exit(130 if cancelled else (128 - code if code < 0 else code))
finally:
    if child.poll() is None:
        try:
            os.killpg(child.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        child.wait()
