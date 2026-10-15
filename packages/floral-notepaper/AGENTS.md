# floral-notepaper 维护说明

通用流程见仓库根目录 [AGENTS.md](../../AGENTS.md)。本文件只记录此包的打包方式；当前版本、架构、依赖以 [PKGBUILD](PKGBUILD) 和 [.SRCINFO](.SRCINFO) 为准，启用状态与上游配置以 [packages.json](../../packages.json) 为准。

## 维护状态

此目录保留历史源码配方，登记时已禁用自动维护。处理任务时重新读取 `packages.json` 的 `enabled`，不要仅因目录存在而构建或发布。禁用期间仍需保留有效的 PKGBUILD 和 .SRCINFO；本说明不代表当前配方已通过构建或运行验证。

## 打包方式与约束

- 从上游 `v$pkgver` 标签源码构建，使用 npm + Rust/Tauri，当前配方面向 x86_64。
- `prepare()` 调整 Tauri 的前置构建命令，并使 `package.json`、Tauri 配置和 Cargo 元数据中的版本与 `pkgver` 一致。
- `build()` 使用 `npm ci`，缓存位于 `$srcdir/npm-cache`，再调用 Tauri 生成 deb。
- `package()` 从生成的 `花笺_${pkgver}_amd64.deb` 中提取文件，重写桌面入口为 `floral-notepaper.desktop`，调用 `desktop-file-validate`，并安装上游许可证。
- 恢复维护前先核对上游项目结构、npm 锁文件、Rust/Tauri 构建工具及产物命名；依赖清单以配方为准。

## 验证重点

恢复自动维护或修改配方时，完成源码构建，核对 deb 的实际路径和数据归档格式，验证桌面入口并实际启动应用，再根据任务范围调整启用状态。
