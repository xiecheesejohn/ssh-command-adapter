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
if sys.argv[1:2] == ["gateway"]:
    sys.stdout.write("gateway$ ")
    sys.stdout.flush()
    command=b""
    while b"\\r" not in command:
        command+=os.read(0,1024)
    expected=(sys.argv[2] if len(sys.argv)>2 else "user").encode()
    if command != b"ssh "+expected+b"@server.example.com\\r":
        sys.exit(92)
    print("AUTOLOGIN_OK",flush=True)
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


def check_client(name, args, state, auto_login=False, no_client_auth=False, destination="command-shell"):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
    proc = subprocess.Popen(
        args + ["-F", str(state / "ssh_config"), destination],
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
        if auto_login:
            expect(b"AUTOLOGIN_OK")
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
        label = name + (" automated gateway login" if auto_login else "")
        auth = "no client key" if no_client_auth else "key auth"
        print(f"{label}: pinned host key, {auth} and repeated resize PASS; client exit={code}")
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait()
        os.close(master)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--adapter", default="bin/ssh-command-adapter")
    parser.add_argument("--install", action="store_true", help="also test source installation into a temporary PATH directory")
    args = parser.parse_args()
    adapter = Path(args.adapter).resolve()
    with tempfile.TemporaryDirectory(prefix="adapter-client-smoke-") as tmp:
        root = Path(tmp)
        if args.install:
            installer = Path(__file__).resolve().parent / "install.sh"
            install_dir = root / "installed bin"
            subprocess.run(["sh", str(installer)], check=True, env={**os.environ, "INSTALL_DIR": str(install_dir)})
            adapter = install_dir / "ssh-command-adapter"
            os.environ["PATH"] = str(install_dir) + os.pathsep + os.environ.get("PATH", "")
            fresh = root / "no-client-key state"
            subprocess.run([str(adapter), "init", "--no-client-auth", "--dir", str(fresh)], check=True, capture_output=True)
            generated = (fresh / "ssh_config").read_text()
            if "ProxyCommand 'ssh-command-adapter' --stdio --no-client-auth" not in generated:
                raise RuntimeError("installed binary did not generate a short ProxyCommand")
            if (fresh / "client_key").exists():
                raise RuntimeError("client-key-free initialization created a client key")
            print("Installer and short-name initialization PASS")
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
        ssh_config = state / "ssh_config"
        text = ssh_config.read_text().replace("--stdio", "--stdio --no-client-auth")
        text = "\n".join(line for line in text.splitlines() if not line.strip().startswith(
            ("IdentityFile ", "IdentitiesOnly ", "IdentityAgent ", "PreferredAuthentications ")
        )) + "\n"
        ssh_config.write_text(text, encoding="utf-8")
        for name, command, extra in clients:
            if command:
                check_client(name, [command] + extra, state, no_client_auth=True)
        tssh = shutil.which("tssh")
        if tssh:
            config["targets"]["shell"]["command"].append("gateway")
            config_path.write_text(json.dumps(config), encoding="utf-8")
            ssh_config = state / "ssh_config"
            ssh_config.write_text(ssh_config.read_text().replace(
                "#!! ExpectCount 0", "#!! ExpectCount 1\n"
                "    #!! ExpectTimeout 10\n"
                "    #!! ExpectPattern1 *gateway$*\n"
                r"    #!! ExpectSendText1 ssh user@server.example.com\r"
            ), encoding="utf-8")
            check_client("tssh", [tssh], state, auto_login=True, no_client_auth=True)
            # A wildcard entry must preserve the input hostname for %h, while
            # the dedicated known_hosts file pins the local adapter's key.
            text = ssh_config.read_text().replace("Host command-shell", "Host *.example.com")
            text = text.replace("    HostName localhost\n", "")
            text = text.replace("ssh user@server.example.com", "ssh %r@%h")
            text = text.replace("--target %r", "--target shell")
            text = text.replace("User shell", "User user")
            ssh_config.write_text(text, encoding="utf-8")
            known = state / "known_hosts"
            known.write_text(known.read_text().replace("ssh-command-adapter,localhost ", "ssh-command-adapter,localhost,*.example.com "), encoding="utf-8")
            check_client("tssh", [tssh], state, auto_login=True, no_client_auth=True, destination="server.example.com")
            # Override the SSH username without adding another local profile.
            config["targets"]["shell"]["command"].append("root")
            config_path.write_text(json.dumps(config), encoding="utf-8")
            check_client("tssh", [tssh], state, auto_login=True, no_client_auth=True, destination="root@server.example.com")
            print("Explicit destination username root with shared local profile PASS")
            print("Wildcard hostname expansion and pinned local host key PASS")


if __name__ == "__main__":
    main()
