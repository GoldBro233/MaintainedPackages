# wlan-auto-login-bin 维护说明

通用流程见仓库根目录 [AGENTS.md](../../AGENTS.md)。本文件只记录此包的打包方式；当前版本、架构、依赖以 [PKGBUILD](PKGBUILD) 和 [.SRCINFO](.SRCINFO) 为准，启用状态与上游配置以 [packages.json](../../packages.json) 为准。

## 打包方式与约束

- 版本来自 `SEUCXCS/wlan-auto-login` 的 GitHub Release，标签前缀为 `v`。安装上游预编译程序，因此包名使用 `-bin`，实际命令为 `wlanlogin`。
- 上游 `packaging/archlinux/PKGBUILD` 的源码包名为 `wlanlogin`；本包提供并冲突于 `wlanlogin`，两种安装方式不能同时拥有 `/usr/bin/wlanlogin`。沿用其运行依赖和安装路径，不引入 Rust、pkgconf 或 Cargo 构建步骤。
- Linux Release 当前仅提供 `wlanlogin-linux-x86_64.tar.gz`；不要根据 macOS 的 aarch64 产物声明 Linux aarch64 支持。
- 下载文件使用包含版本的本地名称，避免上游固定附件名导致缓存串版；使用 `sha256sums` 校验，并与 Release 的 `SHA256SUMS` 核对。
- 归档根目录包含 `wlanlogin`、`LICENSE` 和 `README.md`。分别安装到 `/usr/bin`、包许可证目录和包文档目录；无需 Rust 构建工具链。
- 二进制动态链接 D-Bus、GCC 运行库和 glibc；Wi-Fi 识别使用 NetworkManager，服务管理和系统凭据使用 systemd。用户凭据需要已启动并解锁的 Secret Service，GNOME Keyring 和 KWallet 作为可选依赖。
- 软件包不自动安装、启动或删除后台服务。用户按上游说明执行 `wlanlogin --user service install`，或使用 `sudo wlanlogin --system service install`。程序为服务保存独立的可执行文件副本，升级 `/usr/bin/wlanlogin` 不会自动更新该副本；升级后需按上游说明重新执行对应范围的 `service install`。
- [wlan-auto-login-bin.install](wlan-auto-login-bin.install) 提供 `post_install` 和 `post_upgrade` 提示，说明服务安装、升级独立后台副本、Secret Service 解锁、自定义桌面的 `session-ready` / `session-end`，以及移除软件包前的手动服务卸载。`--purge` 仅用于明确要删除配置和凭据的情况；钩子只打印说明，不执行上述操作。
- `wlan-auto-login-bin.install` 必须提交并列入 `aur_extra_files`，以便候选准备、元数据生成、构建和 AUR 发布均能取得该文件。

## 验证重点

在仓库外验证来源并实际构建；检查包内可执行文件、MIT 许可证和 README，运行打包后程序的 `--version`、`--help` 和 `service --help`。依赖或上游产物变化时检查 ELF 动态链接。

校园网认证需要 `Student_CX` 网络及有效账号；未具备这些条件时不能声称已验证登录、凭据保存或自动重连。服务安装会修改主机状态，常规打包验证只运行帮助和只读查询。
