#!/usr/bin/env python3
"""
Start one or more servers, wait for them to be ready, run a command, then clean up.

Usage:
    # Single server
    python scripts/with_server.py --server "npm run dev" --port 5173 -- python automation.py
    python scripts/with_server.py --server "npm start" --port 3000 -- python test.py

    # Multiple servers
    python scripts/with_server.py \
      --server "python server.py" --cwd backend --port 3000 \
      --server "npm run dev" --cwd frontend --port 5173 \
      -- python test.py

# bukerov-local-patch: webapp-testing-server-cleanup — server commands run with shell=False (each
# --server value is tokenized with shlex.split and exec'd directly, no shell interpreter involved).
# Shell metacharacters are rejected outright before anything is executed. Use the repeatable --cwd
# option instead of the old `cd x && ...` idiom to set a server's working directory.
"""

import argparse
import os
import shlex
import signal
import socket
import subprocess
import sys
import time

# bukerov-local-patch: webapp-testing-server-cleanup — forbidden shell metacharacters; presence of
# any of these in a --server value causes an exit(2) refusal instead of execution.
SHELL_METACHARACTERS = ("&&", "||", ";", "|", "&", ">", "<", "`", "$(")

LOG_DIR = "/tmp/webapp-testing"


def find_shell_metacharacter(cmd):
    """Return the first forbidden shell metacharacter found in cmd, or None."""
    for token in SHELL_METACHARACTERS:
        if token in cmd:
            return token
    return None


def is_server_ready(port, timeout=30):
    """Wait for server to be ready by polling the port."""
    start_time = time.time()
    while time.time() - start_time < timeout:
        try:
            with socket.create_connection(('localhost', port), timeout=1):
                return True
        except (socket.error, ConnectionRefusedError):
            time.sleep(0.5)
    return False


def stop_process_group(process, label):
    """bukerov-local-patch: webapp-testing-server-cleanup — deterministic process-group cleanup.

    SIGTERM the whole process group (the server was started with start_new_session=True, so this
    also reaches any children it spawned), wait up to 5s for it to exit, then SIGKILL the group if
    it's still alive. ProcessLookupError (already exited) is handled at every step.
    """
    try:
        pgid = os.getpgid(process.pid)
    except ProcessLookupError:
        print(f"{label} already gone")
        return

    try:
        os.killpg(pgid, signal.SIGTERM)
    except ProcessLookupError:
        print(f"{label} already gone")
        return

    try:
        process.wait(timeout=5)
        print(f"{label} stopped")
        return
    except subprocess.TimeoutExpired:
        pass

    try:
        os.killpg(pgid, signal.SIGKILL)
    except ProcessLookupError:
        print(f"{label} stopped")
        return

    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        pass
    print(f"{label} killed (did not stop within 5s)")


def main():
    parser = argparse.ArgumentParser(description='Run command with one or more servers')
    parser.add_argument('--server', action='append', dest='servers', required=True, help='Server command (can be repeated)')
    parser.add_argument('--port', action='append', dest='ports', type=int, required=True, help='Port for each server (must match --server count)')
    parser.add_argument('--cwd', action='append', dest='cwds', default=None,
                         help='Working directory for a server, in order (repeatable; count must match '
                              '--server if provided at all; omit entirely to use the current directory '
                              'for every server)')
    parser.add_argument('--timeout', type=int, default=30, help='Timeout in seconds per server (default: 30)')
    parser.add_argument('command', nargs=argparse.REMAINDER, help='Command to run after server(s) ready')

    args = parser.parse_args()

    # Remove the '--' separator if present
    if args.command and args.command[0] == '--':
        args.command = args.command[1:]

    if not args.command:
        print("Error: No command specified to run")
        sys.exit(1)

    # Parse server configurations
    if len(args.servers) != len(args.ports):
        print("Error: Number of --server and --port arguments must match")
        sys.exit(1)

    if args.cwds is not None and len(args.cwds) != len(args.servers):
        print("Error: Number of --cwd arguments must match --server (omit --cwd entirely to run "
              "every server from the current directory)")
        sys.exit(1)

    cwds = args.cwds if args.cwds is not None else [None] * len(args.servers)

    # bukerov-local-patch: webapp-testing-server-cleanup — reject shell metacharacters and tokenize
    # with shlex.split before anything is executed; commands are run with shell=False below.
    argvs = []
    for cmd in args.servers:
        bad = find_shell_metacharacter(cmd)
        if bad:
            print(f"Error: --server value contains shell metacharacter {bad!r}: {cmd!r}")
            print("This script never runs server commands through a shell (shell=False). Use --cwd "
                  "for a 'cd x && ...' equivalent, and invoke a single program directly instead of "
                  "piping or chaining commands.")
            sys.exit(2)
        argvs.append(shlex.split(cmd))

    servers = []
    for argv, port, cwd in zip(argvs, args.ports, cwds):
        servers.append({'argv': argv, 'port': port, 'cwd': cwd})

    os.makedirs(LOG_DIR, exist_ok=True)

    server_processes = []  # list of (process, log_path, label)

    try:
        # Start all servers
        for i, server in enumerate(servers):
            log_path = os.path.join(LOG_DIR, f"server-{i}-port{server['port']}.log")
            label = f"Server {i+1} (port {server['port']})"
            print(f"Starting {label}: {' '.join(server['argv'])} (cwd={server['cwd'] or '.'})")
            print(f"  Log: {log_path}")

            logfile = open(log_path, "wb")
            # bukerov-local-patch: webapp-testing-server-cleanup — shell=False (argv list, no shell
            # metacharacter interpretation); start_new_session=True puts the server in its own
            # process group so stop_process_group() can reliably clean up it and any children.
            process = subprocess.Popen(
                server['argv'],
                cwd=server['cwd'],
                shell=False,
                start_new_session=True,
                stdout=logfile,
                stderr=subprocess.STDOUT,
            )
            server_processes.append((process, log_path, label))

            # Wait for this server to be ready
            print(f"Waiting for server on port {server['port']}...")
            if not is_server_ready(server['port'], timeout=args.timeout):
                raise RuntimeError(
                    f"Server failed to start on port {server['port']} within {args.timeout}s "
                    f"(see {log_path})"
                )

            print(f"Server ready on port {server['port']}")

        print(f"\nAll {len(servers)} server(s) ready")

        # Run the command (already an argv list -- no shell here either)
        print(f"Running: {' '.join(args.command)}\n")
        result = subprocess.run(args.command)
        sys.exit(result.returncode)

    finally:
        # bukerov-local-patch: webapp-testing-server-cleanup — deterministic cleanup for every
        # server that was started, regardless of how we got here (success, exception, or timeout).
        print(f"\nStopping {len(server_processes)} server(s)...")
        for process, log_path, label in server_processes:
            stop_process_group(process, label)
        print("All servers stopped")


if __name__ == '__main__':
    main()
