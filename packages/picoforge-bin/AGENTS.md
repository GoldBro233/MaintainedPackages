# picoforge-bin 维护说明

通用流程见仓库根目录 [AGENTS.md](../../AGENTS.md)。本文件只记录此包的打包方式；当前版本、架构、依赖以 [PKGBUILD](PKGBUILD) 和 [.SRCINFO](.SRCINFO) 为准，启用状态与上游配置以 [packages.json](../../packages.json) 为准。

## 打包方式与布局

- 使用上游 x86_64 / aarch64 AppImage；构建依赖 `7zip` 提供 `7z`，在 `prepare()` 中解包，安装后运行不需要 FUSE。
- 上游产物名包含版本、`glibc-2.28` 和架构；x86_64 对应上游文件名中的 `x86-64`。两种架构分别维护来源和 SHA-256 校验和。
- 保留 `/opt/picoforge/usr/bin`、`usr/lib` 与 ELF `$ORIGIN/../lib` 的相对布局，通过 `/usr/bin/picoforge` 符号链接暴露程序入口。
- 保留桌面文件、图标及 metainfo 安装，以及 `!strip`、`!debug` 选项。

## 兼容性与依赖

- 删除 AppImage 自带的 `libxkbcommon.so*` 和 `libxkbcommon-x11.so*`，让程序使用 Arch 系统库及匹配的 Compose 数据，避免旧库无法解析 `dead_hamza` 等新 keysyms。两套库需要一起移除，避免混用版本。
- 保留其余上游私有库。namcap 在临时目录逐个分析 ELF 时可能丢失相对库布局；仅凭 `/opt` 路径或 unused-link 警告，不能判定私有库可删除。
- `ccid` 提供 PC/SC 驱动，Wayland 和 Vulkan 通过 `dlopen()` 使用；这些需求不一定出现在主程序 ELF 的 `DT_NEEDED` 中，不能仅凭静态依赖分析删除。

## 附加文件与安装提示

- 本地 [LICENSE](LICENSE) 和 SPDX 头覆盖打包代码；下载的 `picoforge-LICENSE` 是上游应用许可证，两者用途不同。
- [picoforge-bin.install](picoforge-bin.install) 必须随候选提供，生成元数据时也需要可用。它提示用户执行 `sudo systemctl enable --now pcscd.socket`，不会自动启用服务；同时提供未知 USB VID/PID 的上游排障入口。
- `LICENSE`、`picoforge-LICENSE` 和 `picoforge-bin.install` 均列入 `aur_extra_files`；下载型上游许可证由准备阶段获取，不要求预先存在于 GitHub 包目录。

## 验证重点

修改私有库、依赖或布局后，实际启动 GUI，检查键盘/Compose 兼容性及受影响的显示后端；涉及安全密钥访问时，验证 PC/SC 和设备访问。x86_64 / aarch64 需分别构建和运行验证；当前 CI 仅覆盖 x86_64，不能据此宣称 aarch64 已验证。
