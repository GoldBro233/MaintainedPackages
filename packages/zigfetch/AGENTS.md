# zigfetch 维护说明

通用流程见仓库根目录 [AGENTS.md](../../AGENTS.md)。本文件只记录此包的打包方式；当前版本、架构、依赖以 [PKGBUILD](PKGBUILD) 和 [.SRCINFO](.SRCINFO) 为准，启用状态与上游配置以 [packages.json](../../packages.json) 为准。

## 打包方式与约束

- 从上游 `v$pkgver` 标签的源码归档构建，当前配方面向 x86_64。
- 构建依赖为 Zig，运行依赖包含 `pciutils`。
- `build()` 使用 `zig build -Doptimize=ReleaseSafe`，`check()` 使用 `zig build test`，保留这两个阶段。
- 构建与检查阶段都将 `ZIG_GLOBAL_CACHE_DIR` 和 `ZIG_LOCAL_CACHE_DIR` 放在 `$srcdir` 下，避免依赖用户全局缓存。
- 安装可执行文件之外，还要保留许可证、README 和示例 `config.json` 的安装。

## 验证重点

升级时核对 Arch 所提供 Zig 版本与上游源码的兼容性，完成源码构建及上游测试，并实际运行构建出的 zigfetch 检查系统信息输出。
