# ssh-command-adapter

Use a local interactive command as an SSH terminal session, without modifying
your SSH client. Designed for terminal clients such as `tssh` and OpenSSH that
support `ProxyCommand`.

[中文说明](README.zh-CN.md)

```text
tssh / OpenSSH
  └─ ProxyCommand: ssh-command-adapter --stdio
       └─ SSH server over private stdin/stdout pipes
            └─ raw PTY: configured local command
                 └─ destination session
```

The adapter translates SSH `pty-req` and `window-change` requests into PTY
dimensions, relays terminal bytes in both directions, and returns the local
command's exit status. It does not open a listening port or require a daemon.
Each connection starts its own adapter and child command. A target can be any
trusted local terminal client; no particular vendor or gateway is required.

## Build

Requires Go 1.26 or newer. Supported adapter platforms: macOS and Linux.

```sh
go build -trimpath -o bin/ssh-command-adapter .
```

Keep the executable at a stable absolute path before initializing it: the
generated SSH configuration refers to that path.

## Quick start

```sh
./bin/ssh-command-adapter init
```

Initialization creates a **new private directory outside the repository**:

- macOS: `~/Library/Application Support/ssh-command-adapter`
- Linux: `$XDG_CONFIG_HOME/ssh-command-adapter`, usually `~/.config/ssh-command-adapter`

The directory contains an Ed25519 SSH host key, a dedicated client key,
`authorized_keys`, pinned `known_hosts`, `config.json`, and an `ssh_config`
snippet. It has mode `0700`, files have mode `0600`. Existing state is never
overwritten. Use `init --dir /absolute/private/path` for a different location;
its parent must already exist.

The printed command opens a local Bash session for an initial smoke test. Then
edit the private `config.json` to define your actual local terminal command:

```json
{
  "host_key": "host_key",
  "authorized_keys": "authorized_keys",
  "targets": {
    "remote": {
      "command": ["/usr/bin/ssh", "-tt", "user@server.example.com"]
    }
  }
}
```

Command arguments are an array, passed directly to the executable. There is no
implicit shell, string interpolation, or automatic expansion of `~` in command
arguments. Use absolute paths when possible. Key paths are resolved relative to
the configuration file. To validate without connecting:

```sh
./bin/ssh-command-adapter --config /absolute/private/path/config.json --target remote --check
```

In the generated `ssh_config`, rename `Host command-shell` to your desired alias,
change `User shell` to `User remote`, and change `--target shell` to
`--target remote`. The SSH username selects the configured local target; the
destination's real username belongs in the command array. Keep the generated
identity and host-verification settings. Then:

```sh
tssh -F /absolute/private/path/ssh_config your-alias
# or
ssh -F /absolute/private/path/ssh_config your-alias
```

To include the alias in your normal `tssh` selector, add this **before broad
`Host *` blocks** in your normal SSH config:

```sshconfig
Include "/absolute/private/path/ssh_config"
```

Remove or rename an older alias with the same name. OpenSSH and tssh generally
take the first value encountered for an option; a prior wildcard or duplicate
entry can override the generated transport, transfer, or expect settings.
The generated entry disables connection multiplexing to isolate each child
process. The original remote `ProxyCommand` and login-time `stty` workaround
are not used for this alias.

### Gateways that require a second login command

If a gateway CLI opens its own shell but does not reliably enter the destination,
configure the target command to open only the gateway. Let tssh send the second
login command after the gateway prompt appears. Override `ExpectCount 0` in the
private generated SSH entry, for example:

```sshconfig
    #!! ExpectCount 1
    #!! ExpectTimeout 300
    #!! ExpectPattern1 *gateway$*
    #!! ExpectSendText1 ssh user@server.example.com\r
```

Use the gateway's actual, distinctive ready prompt. This is a one-time login
interaction, not a resize workaround: subsequent resizes continue through the
adapter's `window-change` handling. File transfer remains owned by the gateway
CLI. Do not combine this with the CLI's own automatic second login or it may
send the command twice. This automation is a tssh feature; ordinary OpenSSH
ignores the `#!!` directives and requires manual second login or a local wrapper.

### Optional: omit local client key authentication

Since `--stdio` uses private pipes between the SSH client and its own adapter
child, client key authentication is optional. Add `--no-client-auth` to this
entry's `ProxyCommand` and remove `IdentityFile`, `IdentitiesOnly`,
`IdentityAgent`, and `PreferredAuthentications`. The flag requires `--stdio`;
there is no unauthenticated network listener. The requested SSH username must
still match the configured target. `authorized_keys` may be omitted from the
JSON configuration in this mode. Key authentication remains the default.

Keep `HostKeyAlias`, `UserKnownHostsFile`, and `StrictHostKeyChecking yes` to
verify the adapter's stable host identity. The SSH protocol still needs a host
key; skipping client key authentication does not remove host verification or
the gateway/destination's own authentication.

The `Host` entry may be pasted directly into `~/.ssh/config` instead of using
an `Include`. Put it before broad wildcard blocks and remove the redundant
include or duplicate alias. The JSON command configuration stays in its private
directory.

## File transfers

The adapter provides binary-safe terminal transport, **not a file-transfer
implementation**. If the configured local command already handles ZMODEM or
trzsz on this computer, let that command own transfer detection. The generated
configuration disables tssh's second transfer interceptor:

```sshconfig
    #!! EnableTrzsz no
    #!! EnableZmodem no
    #!! EnableDragFile no
    #!! EnableOSC52 no
    #!! ExpectCount 0
```

For example, a gateway CLI with its own ZMODEM handler can continue to process
remote `rz` / `sz` requests and local file-selection dialogs. Follow that CLI's
requirements for local and destination `lrzsz` installations.

If the command is a plain SSH client **without its own file-transfer handler**,
you may instead enable tssh's transfer features in this entry. Exactly one
layer should handle the transfer protocol. Actual end-to-end transfers depend
on the selected command and gateway; automated tests verify byte transparency,
not every third-party file-transfer implementation.

## Scope and security

- Interactive PTY sessions only. Remote `exec`, SFTP/SCP subsystems, TCP
  forwarding, agent forwarding, and X11 are not implemented. Tools such as VS
  Code Remote SSH cannot use this adapter as a general SSH server.
- It is an SSH endpoint, not a TCP tunnel to the destination's SSH daemon.
  Authentication to the destination remains the configured command's job.
- Only a configured command can run. The authenticated SSH username must match
  `--target`; remote command strings are rejected.
- Uses a dedicated client key and a pinned host key even though transport is
  over local pipes. It never listens on a network interface.
- Configuration and command executables are trusted local code, running as the
  current OS user. Protect them like your SSH configuration. Do not add secrets
  to command arguments or publish private runtime state.
- Input/output, command arguments, environment, and destination credentials are
  not logged. Only `LANG`, `LC_ALL`, and `LC_CTYPE` SSH environment requests are
  accepted; the child inherits the local environment, except stale SSH/tmux
  session identifiers and `TERM` (set from the PTY request).
- The child gets a fresh controlling PTY. Terminal modes requested by the SSH
  client are intentionally replaced with raw mode to avoid double echo, CRLF
  conversion, and binary corruption. This is primarily for terminal clients.
- Disconnect or adapter termination sends SIGTERM to the child process group,
  then SIGKILL after two seconds if needed. This cleans up the local client; it
  does not promise to terminate a persistent session on a destination server.

## Tests

```sh
go test -race -timeout 90s ./...
go vet ./...
go build -trimpath -o bin/ssh-command-adapter .
python3 scripts/smoke_clients.py
```

Integration tests use local Unix socket pairs and synthetic terminal processes.
They cover initial dimensions, repeated resize notifications, all 256 byte
values, exit/output draining, disconnect cleanup, key/target authentication,
unsupported request rejection, and non-destructive initialization. No real
hosts, credentials, vendor binaries, or destination accounts are needed.
The optional Python smoke test checks installed OpenSSH/tssh clients against a
synthetic local terminal, including configuration paths with spaces. Some tssh
versions return process status zero even when the adapter sends a nonzero SSH
exit status; OpenSSH preserves it. The adapter's exit-status forwarding is
independently asserted by the Go integration tests.
