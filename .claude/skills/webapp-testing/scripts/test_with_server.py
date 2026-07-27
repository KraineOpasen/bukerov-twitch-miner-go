#!/usr/bin/env python3
# bukerov-local-patch: webapp-testing-server-cleanup — local test harness, not upstream.
"""Regression tests for the hardened scripts/with_server.py.

Stdlib only, deterministic, no network beyond loopback, runtime well under 40s. Exits 0 if every
test passes, 1 if any fails. Run directly:

    python3 .claude/skills/webapp-testing/scripts/test_with_server.py

Covers the three behaviors that motivated the webapp-testing-server-cleanup patch:
  1. A server that writes a large amount of stdout must not deadlock with_server.py (regression
     test for the old shell=True + stdout=PIPE design, which could fill the OS pipe buffer).
  2. A server's grandchild process must be reaped too (regression test for the old
     process.terminate()-only cleanup, which only touched the immediate child).
  3. A --server value containing a shell metacharacter must be rejected (exit 2) before anything
     is executed (regression test for the old shell=True design, which ran server strings through
     a shell verbatim).
"""

import os
import socket
import subprocess
import sys
import tempfile
import textwrap
import time

HERE = os.path.dirname(os.path.abspath(__file__))
WITH_SERVER = os.path.join(HERE, "with_server.py")

RESULTS = []


def report(name, ok, detail=""):
    RESULTS.append((name, ok))
    status = "PASS" if ok else "FAIL"
    print("[%s] %s%s" % (status, name, (": " + detail) if detail and not ok else ""))


def free_port():
    """Grab an OS-assigned free localhost port, then release it for the helper script to bind."""
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def run_with_server(args, timeout=35):
    return subprocess.run(
        [sys.executable, WITH_SERVER] + args,
        capture_output=True, text=True, timeout=timeout,
    )


def test_noisy_server():
    """A server that writes >=200KB to stdout must not deadlock with_server.py, must exit 0, and
    its log file must exist and be >=100KB (proves output is redirected to a file, not a PIPE that
    could fill and block the child)."""
    name = "test_noisy_server"
    with tempfile.TemporaryDirectory() as tmp:
        port = free_port()
        helper = os.path.join(tmp, "noisy_server.py")
        with open(helper, "w") as f:
            f.write(textwrap.dedent(f"""
                import socket, sys, time
                s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                s.bind(("127.0.0.1", {port}))
                s.listen(1)
                sys.stdout.write("X" * 250000)
                sys.stdout.flush()
                time.sleep(30)
            """))

        # with_server.py's LOG_DIR is a module constant (/tmp/webapp-testing/), not configurable
        # via env/CLI, so this checks the real location it writes to; the log filename is keyed by
        # the free port we picked, which is unique enough not to collide with a concurrent run.
        try:
            proc = run_with_server([
                "--server", f"{sys.executable} {helper}",
                "--port", str(port),
                "--timeout", "15",
                "--", sys.executable, "-c", "import time; time.sleep(1.5)",
            ])
        except subprocess.TimeoutExpired as e:
            report(name, False, "with_server.py timed out (likely deadlocked): %r" % (e,))
            return

        if proc.returncode != 0:
            report(name, False, "exit code %d, stderr: %s" % (proc.returncode, proc.stderr[-500:]))
            return

        # Find the log file with_server.py printed the path to.
        log_path = None
        for line in proc.stdout.splitlines():
            line = line.strip()
            if line.startswith("Log: "):
                log_path = line[len("Log: "):].strip()
                break

        if not log_path or not os.path.isfile(log_path):
            report(name, False, "could not find logged server log path in output")
            return

        size = os.path.getsize(log_path)
        if size < 100_000:
            report(name, False, "log file only %d bytes, expected >=100KB" % size)
            return

        report(name, True)


def test_orphan_grandchild():
    """A server's grandchild process (spawned without its own setsid) must be reaped by
    with_server.py's process-group cleanup, not just the immediate child."""
    name = "test_orphan_grandchild"
    with tempfile.TemporaryDirectory() as tmp:
        port = free_port()
        pidfile = os.path.join(tmp, "grandchild.pid")
        helper = os.path.join(tmp, "parent_server.py")
        with open(helper, "w") as f:
            f.write(textwrap.dedent(f"""
                import socket, subprocess, sys, time

                grandchild = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
                with open({pidfile!r}, "w") as f:
                    f.write(str(grandchild.pid))

                s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                s.bind(("127.0.0.1", {port}))
                s.listen(1)
                time.sleep(60)
            """))

        try:
            proc = run_with_server([
                "--server", f"{sys.executable} {helper}",
                "--port", str(port),
                "--timeout", "15",
                "--", sys.executable, "-c", "pass",
            ])
        except subprocess.TimeoutExpired as e:
            report(name, False, "with_server.py timed out: %r" % (e,))
            return

        if proc.returncode != 0:
            report(name, False, "exit code %d, stderr: %s" % (proc.returncode, proc.stderr[-500:]))
            return

        if not os.path.isfile(pidfile):
            report(name, False, "grandchild never recorded its pid")
            return

        with open(pidfile) as f:
            grandchild_pid = int(f.read().strip())

        # with_server.py has already returned, so cleanup already ran; give the kernel a brief
        # moment to finish reaping in case of scheduling delay, then check. This is a busy sandbox
        # where PIDs get recycled quickly, so a bare os.kill(pid, 0) success is not proof the
        # *grandchild* is still alive -- it could be an unrelated process reusing the same pid.
        # Cross-check identity via /proc/<pid>/cmdline (Linux-only, matches this environment).
        def grandchild_still_alive():
            try:
                os.kill(grandchild_pid, 0)
            except ProcessLookupError:
                return False
            except PermissionError:
                return True
            try:
                with open("/proc/%d/cmdline" % grandchild_pid, "rb") as f:
                    cmdline = f.read().replace(b"\x00", b" ")
            except (FileNotFoundError, ProcessLookupError):
                return False
            return b"time.sleep(60)" in cmdline

        gone = False
        for _ in range(20):
            if not grandchild_still_alive():
                gone = True
                break
            time.sleep(0.1)

        report(name, gone, "grandchild pid %d still alive after cleanup" % grandchild_pid)


def test_metachar_rejected():
    """A --server value containing a shell metacharacter must be rejected (exit 2), and nothing
    must be executed."""
    name = "test_metachar_rejected"
    port = free_port()
    try:
        proc = run_with_server([
            "--server", "echo a && echo b",
            "--port", str(port),
            "--timeout", "3",
            "--", sys.executable, "-c", "pass",
        ], timeout=10)
    except subprocess.TimeoutExpired as e:
        report(name, False, "with_server.py timed out instead of rejecting immediately: %r" % (e,))
        return

    if proc.returncode != 2:
        report(name, False, "exit code %d (expected 2); stdout=%r stderr=%r" % (
            proc.returncode, proc.stdout[-300:], proc.stderr[-300:]))
        return

    # Nothing should have been executed: with_server.py only prints "Starting <label>" once it
    # begins spawning a server, which must not happen when the metacharacter check rejects first.
    if "Starting Server" in proc.stdout or "Running:" in proc.stdout:
        report(name, False, "with_server.py appears to have started executing despite rejection: %r"
               % (proc.stdout[-300:],))
        return

    report(name, True)


def main():
    if not os.path.isfile(WITH_SERVER):
        print("[FAIL] setup: with_server.py not found at %s" % WITH_SERVER)
        return 1

    test_metachar_rejected()
    test_noisy_server()
    test_orphan_grandchild()

    failed = [n for n, ok in RESULTS if not ok]
    print("\n%d/%d tests passed" % (len(RESULTS) - len(failed), len(RESULTS)))
    if failed:
        print("Failed: %s" % ", ".join(failed))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
