# tokscale-bin 维护说明

通用流程见仓库根目录 [AGENTS.md](../../AGENTS.md)。本文件只记录此包的打包方式；当前版本、架构、依赖以 [PKGBUILD](PKGBUILD) 和 [.SRCINFO](.SRCINFO) 为准，启用状态与上游配置以 [packages.json](../../packages.json) 为准。

## 打包方式与约束

- 版本检测来自上游 GitHub Release，实际安装的 x86_64 预编译 CLI 来自 npm registry 的 `@tokscale/cli-linux-x64-gnu` 包。升级前确认对应 npm 版本已发布，不能仅凭 GitHub 标签判断二进制可用。
- 归档解包后从 `package/bin/tokscale` 安装到 `/usr/bin/tokscale`，不运行 npm 构建。
- `tokscale-LICENSE` 从对应上游 `v$pkgver` 标签下载，使用 `b2sums` 校验，安装到 `/usr/share/licenses/$pkgname/LICENSE`。
- `tokscale-LICENSE` 列入 `aur_extra_files`，由候选准备阶段下载、验证并送到 AUR；不要求预先提交到 GitHub 包目录。
- 保留对 `tokscale` 的 provides，以及与 `tokscale`、`tokscale-git` 的 conflicts。

## 验证重点

核对 npm 产物版本、归档内的可执行文件路径及许可证来源。构建后运行 CLI 的帮助或版本入口；改动影响 TUI 时，还需实际验证交互界面。
