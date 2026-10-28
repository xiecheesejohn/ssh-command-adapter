# ssh-command-adapter

把本地交互命令变成 SSH 终端会话，让 tssh / OpenSSH 通过 `ProxyCommand`
调用它。无需修改或 fork tssh，主机仍显示在原有选择列表里。

```text
tssh → ProxyCommand → 本地 SSH 适配器 → PTY → 本地登录命令 → 目标机器
```

适配器不监听端口，也不需要后台服务。每次连接按需启动，通过标准输入输出
提供 SSH 协议，处理初始终端大小、后续窗口缩放、终端数据和退出状态。
它不依赖特定厂商或跳板机。

## 使用

需要 Go 1.26+，支持 macOS 和 Linux：

```sh
git clone https://github.com/xiecheesejohn/ssh-command-adapter.git
cd ssh-command-adapter
sh scripts/install.sh
ssh-command-adapter init --no-client-auth
```

安装脚本默认把程序放到 `~/.local/bin`，不修改 shell 或 SSH 配置。
如果该目录不在 PATH 中，在 shell 启动文件里加上这一行，再打开新终端：

```sh
export PATH="$HOME/.local/bin:$PATH"
```

`INSTALL_DIR=/其他目录 sh scripts/install.sh` 可自定义安装位置。
重复运行安装脚本即可更新程序，不覆盖私有配置。已有配置不必再次执行 init。
也可以用 `make install` 安装；`make build` 仅用于在项目 bin 目录内构建。

初始化在仓库外创建私有配置目录：macOS 为
`~/Library/Application Support/ssh-command-adapter`，Linux 通常为
`~/.config/ssh-command-adapter`。目录包含主机密钥、固定主机公钥、
`config.json` 和 `ssh_config`。私有管道连接无需客户端密钥；若需要客户端密钥认证，
初始化时省略 `--no-client-auth`。不会覆盖已有目录。
也可以用 `init --dir /绝对路径` 指定新目录，父目录必须存在。

编辑私有的 `config.json`，把目标定义为一个命令参数数组，例如：

```json
{
  "host_key": "host_key",
  "targets": {
    "work": {
      "command": ["/usr/bin/ssh", "-tt", "user@server.example.com"]
    }
  }
}
```

也可以把 `command` 换成你已验证正常的本地跳板机登录命令。
参数直接传给程序，不经过 shell，不自动展开 `~`，不要将密码写进参数。
密钥路径相对于该配置文件。

在生成的 `ssh_config` 中，把 `Host command-shell` 改为 `Host work`，
把 `--target shell` 改为 `--target work`。免客户端认证时，`--target` 是固定的
本地命令配置名，独立于 SSH 的 `User`；`User` 可以表示远端登录账号。
固定目标的真实用户名写在命令数组里；客户端密钥模式另要求 User 与目标名称一致。
保留生成的主机校验和其他设置。程序在 PATH 中时，默认位置的配置会生成短命令：

```sshconfig
    ProxyCommand ssh-command-adapter --stdio --no-client-auth --target work
```

无需填写项目的长路径，也无需指定默认的 config.json 路径。若初始化时 PATH
指向其他同名程序，生成器会使用当前程序的绝对路径。图形客户端也需继承正确的 PATH。

可直接参考：[精简命令配置](examples/config.json)、
[macOS SSH 配置](examples/ssh_config.macos)、[Linux SSH 配置](examples/ssh_config.linux)。
把真实命令写进私有 JSON，再把对应平台的 Host 条目放进 `~/.ssh/config` 的通配条目之前。
自定义配置目录时，使用 init 生成的路径。

先独立验证：

```sh
tssh -F /私有目录/ssh_config work
```

验证后在 `~/.ssh/config` 的通配 `Host *` **之前**加入：

```sshconfig
Include "/私有目录/ssh_config"
```

移除或重命名原来的同名条目，避免“先出现的配置值优先”造成旧的代理、
Expect 或文件传输配置继续生效。现在执行 `tssh` 即可在原有列表里选择这个主机。

如果跳板机 CLI 只进入跳板机 shell，没有可靠地自动进入最终机器，可以让本地
命令只负责登录跳板机，再由 tssh 等到明确的提示符后发送第二跳命令。
在私有 SSH 条目中把 `ExpectCount 0` 替换为：

```sshconfig
    #!! ExpectCount 1
    #!! ExpectTimeout 300
    #!! ExpectPattern1 *gateway$*
    #!! ExpectSendText1 ssh user@server.example.com\r
```

把提示符换成跳板机实际的就绪提示符，账号和地址只写在私有配置里。
不要同时启用 CLI 自己的自动二跳，否则可能重复执行。这是登录时的一次交互，
后续缩放仍由适配器处理，文件传输仍交给跳板机 CLI。该自动交互仅适用于 tssh；
普通 OpenSSH 会忽略 `#!!` 配置，需手动执行第二跳或使用本地包装命令。

同一个跳板机也可供 `Host *.example.com` 通配条目使用：不要设置
`HostName localhost`，让 `%h` 保留实际目标域名；自动二跳命令写成
`ExpectSendText1 ssh %r@%h\r`。把 `User` 设成默认远端用户名，本地命令配置
固定为 `--target gateway`，该配置只负责打开跳板机。免客户端认证时，所有域名和
远端账号都复用它；`tssh root@server.example.com` 不需要再创建 root 配置。
不要将目标写成 `--target %r`，因为 `%r` 是远端账号，不是本地配置名。
通配条目放在 `Host *` 前面；短别名需单独匹配，不能依赖其 HostName
再次命中通配规则。

部分 tssh 版本不使用 HostKeyAlias 校验，因此需在适配器专用 known_hosts
现有公钥的名称字段加入通配域名，例如 `ssh-command-adapter,localhost,*.example.com`。
不修改公钥，不关闭严格校验。这里校验的是本机适配器，远端身份仍由跳板机 CLI
处理。见[通配示例](examples/ssh_config.wildcard.macos)。普通 OpenSSH 不执行 tssh
的自动二跳指令，该配置也不适用于 SSH exec、端口转发等非交互用途。

## 可选：省略本机客户端密钥认证

`--stdio` 通过客户端与它启动的适配器之间的私有管道通信，没有监听网络端口。
因此可在 `ProxyCommand` 中加入 `--no-client-auth`，并删除 `IdentityFile`、
`IdentitiesOnly`、`IdentityAgent`、`PreferredAuthentications` 四项。
该参数必须与 `--stdio` 一起使用，本地 --target 与远端 SSH 用户名独立，也不改变
本机进程的系统用户权限；只会执行本地指定的固定命令。此时 JSON 中的
`authorized_keys` 可以省略。默认模式仍使用客户端密钥认证。

保留 `HostKeyAlias`、`UserKnownHostsFile` 和 `StrictHostKeyChecking yes`，
用于校验适配器的主机身份。SSH 协议仍需要服务端主机密钥；省略本机客户端认证
不会取消堡垒机或开发机自己的认证。

整个 `Host` 条目也可以直接写进 `~/.ssh/config`，不必使用额外的 Include 文件。
放在通配 Host 条目之前，并移除重复 Include 或同名条目。命令的 JSON 配置
仍放在原私有目录中。

## rz / sz

适配器透明传输终端字节，本身不实现文件传输。
如果本地登录命令已经内置 ZMODEM / trzsz，继续由它处理文件选择和传输。
生成的配置会关闭 tssh 的另一层传输检测：

```sshconfig
    #!! EnableTrzsz no
    #!! EnableZmodem no
    #!! EnableDragFile no
    #!! EnableOSC52 no
    #!! ExpectCount 0
```

不要再套一层 `trzsz -z`。本地和目标机是否需要安装 `lrzsz`，依照所用命令的要求。
若使用不带文件传输处理的普通 SSH 命令，则可改为由 tssh 启用文件传输；
确保只有一层处理协议即可。

## 支持范围

- 支持交互式 PTY、窗口缩放、输入输出、退出状态和断线后本地进程清理。
- 不支持远程 exec、SFTP/SCP、端口转发、agent 转发或 X11，不能代替完整 SSH
  服务供 VS Code Remote SSH 等使用。
- 每个目标只执行私有配置中固定的命令，拒绝远端传来的任意命令。
- 通过独立密钥认证和固定主机公钥校验，不开放网络端口。
- PTY 使用 raw 模式，避免重复回显、换行转换和二进制损坏，主要用于包装终端客户端。
- 断线后先向本地命令的进程组发送 SIGTERM，最多两秒后发送 SIGKILL；
  这不保证结束远端持久化会话。
- 本地配置、密钥、真实主机和账号必须留在仓库外。程序不记录终端内容或命令参数。

完整说明和测试范围见 [README](README.md)。
