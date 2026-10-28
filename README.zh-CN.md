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
go build -trimpath -o bin/ssh-command-adapter .
./bin/ssh-command-adapter init
```

先把编译后的程序放在固定位置再初始化，因为生成的 SSH 配置会引用它的绝对路径。

初始化在仓库外创建私有配置目录：macOS 为
`~/Library/Application Support/ssh-command-adapter`，Linux 通常为
`~/.config/ssh-command-adapter`。目录包含独立的主机密钥、客户端密钥、固定
主机公钥、`config.json` 和 `ssh_config`。不会覆盖已有目录。
也可以用 `init --dir /绝对路径` 指定新目录，父目录必须存在。

编辑私有的 `config.json`，把目标定义为一个命令参数数组，例如：

```json
{
  "host_key": "host_key",
  "authorized_keys": "authorized_keys",
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
把 `User shell` 改为 `User work`，把 `--target shell` 改为 `--target work`。
这里 `User` 是本地目标名称，真正的远端用户名写在命令数组里。
保留生成的独立身份密钥、主机校验和其他设置。

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
