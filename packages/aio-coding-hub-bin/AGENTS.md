# aio-coding-hub-bin 维护说明

通用流程见仓库根目录 [AGENTS.md](../../AGENTS.md)。本文件只记录此包的打包方式；当前版本、架构、依赖以 [PKGBUILD](PKGBUILD) 和 [.SRCINFO](.SRCINFO) 为准，启用状态与上游配置以 [packages.json](../../packages.json) 为准。

## 打包方式与约束

- 重打包上游 x86_64 `.deb`，不在本仓库编译应用源码。
- 上游标签使用 `aio-coding-hub-v` 前缀，下载 URL 中的标签与应用名需要保持一致。
- `.deb` 下载文件名包含版本，上游产物名为 `aio-coding-hub-linux-amd64.deb`；保留本地文件别名，避免缓存混淆。
- 使用 `bsdtar` 先解包 deb，再将其中的 `data.tar.*` 解入 `$pkgdir`；保留 `--no-same-owner`。
- 上游许可证按对应版本标签单独下载并校验，安装到 `/usr/share/licenses/$pkgname/LICENSE`。
- 保留配方对 Debian 元数据、lintian 和上游文档目录的清理，以及 `!strip`、`!debug` 选项。

## 验证重点

更新后核对上游 deb 的内容布局与架构，检查包内许可证及桌面集成文件，并在具备 GTK/WebKit 运行依赖的环境中实际启动应用。
