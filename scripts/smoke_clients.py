#!/usr/bin/env python3
"""Check a built adapter with real SSH clients, using only a local PTY helper."""

import argparse
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time


HELPER = '''import os,sys,signal,fcntl,termios,struct
def report(*args):
    rows,cols,_,_=struct.unpack("HHHH",fcntl.ioctl(0,termios.TIOCGWINSZ,b"\\0"*8))
    print(f"SIZE={rows}x{cols}",flush=True)
signal.signal(signal.SIGWINCH,report)
report()
while True:
    data=os.read(0,1024)
    if b"finish" in data:
        sys.exit(23)
'''


def check_client(name, args, state):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
    proc = subprocess.Popen(
        args + ["-F", str(state / "ssh_config"), "command-shell"],
        stdin=slave, stdout=slave, stderr=slave, start_new_session=True,
    )
    os.close(slave)
    output = bytearray()

    def expect(needle):
        deadline = time.monotonic() + 10
        while needle not in output and time.monotonic() < deadline:
            if select.select([master], [], [], 0.2)[0]:
                try:
                    output.extend(os.read(master, 65536))
                except OSError:
                    break
        if needle not in output:
            raise RuntimeError(f"{name}: missing {needle!r}; output={output[-2000:]!r}")

    try:
        expect(b"SIZE=24x80")
        for rows, cols in [(40, 120), (17, 63)]:
            fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
            os.kill(proc.pid, signal.SIGWINCH)
            expect(f"SIZE={rows}x{cols}".encode())
        os.write(master, b"finish\n")
        code = proc.wait(timeout=10)
        # Some tssh versions normalize the process return code to zero, despite
        # receiving the SSH exit-status. OpenSSH must preserve the child status.
        allowed = (0, 23) if name == "tssh" else (23,)
        if code not in allowed:
            raise RuntimeError(f"{name}: unexpected exit code {code}")
        print(f"{name}: pinned host key, key auth and repeated resize PASS; client exit={code}")
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait()
        os.close(master)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--adapter", default="bin/ssh-command-adapter")
    args = parser.parse_args()
    adapter = Path(args.adapter).resolve()
    with tempfile.TemporaryDirectory(prefix="adapter-client-smoke-") as tmp:
        root = Path(tmp)
        # Include spaces to exercise quoting in both client configuration parsers.
        state = root / "private state"
        subprocess.run([str(adapter), "init", "--dir", str(state)], check=True, capture_output=True)
        helper = root / "terminal.py"
        helper.write_text(HELPER, encoding="utf-8")
        config_path = state / "config.json"
        config = json.loads(config_path.read_text())
        config["targets"]["shell"]["command"] = [sys.executable, str(helper)]
        config_path.write_text(json.dumps(config), encoding="utf-8")
        clients = [("OpenSSH", shutil.which("ssh"), ["-tt"]), ("tssh", shutil.which("tssh"), [])]
        for name, command, extra in clients:
            if command:
                check_client(name, [command] + extra, state)
            else:
                print(f"{name}: skipped (not installed)")


if __name__ == "__main__":
    main()
