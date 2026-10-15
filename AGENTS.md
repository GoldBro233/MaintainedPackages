# Agent 工作指南

本仓库维护 Arch Linux / AUR 包配方及 Go 维护工具源码，以 `packages.json` 驱动 CLI `maintpkg` 和 GitHub Actions。上游应用的源码或发布产物由各包的 PKGBUILD 获取。GitHub 仓库为 `GoldBro233/MaintainedPackages`，发布目标为 GitHub `main` 和各包 AUR 仓库的 `master`。

## 进入仓库时先获取状态

在仓库根目录核对状态，并将维护 CLI 编译到仓库外的临时目录；以实际输出为准，不把本文当成版本或 CI 状态快照：

```bash
git status --short --branch
git remote -v
git log -5 --oneline
jq -r '.packages[] | [.name, .enabled, .upstream_repo, .tag_prefix] | @tsv' packages.json
repo_root="$PWD"
cli_bin_dir="$(mktemp -d /tmp/maintpkg-cli.XXXXXX)"
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$cli_bin_dir/maintpkg" ./cmd/maintpkg
"$cli_bin_dir/maintpkg" --repo "$repo_root" matrix
```

- 工作区修改可能属于用户，先理解现有改动，再继续工作。
- 当前配方与版本读取 `packages/<name>/PKGBUILD` 和 `.SRCINFO`；需要区分未提交修改时，再与 `git show HEAD:packages/<name>/PKGBUILD` 比较。
- `matrix` 验证配置以及所有登记包（包括禁用包）的 `PKGBUILD`、`.SRCINFO`，仅输出启用包。`matrix <name>` 可检查单包选择；未知或禁用包会失败。
- 本地文件不能证明 AUR 已同步，也不能证明远端 CI 成功。需要远端状态时，用已认证的 GitHub CLI：

```bash
gh run list --repo GoldBro233/MaintainedPackages --limit 10
# 将 RUN_ID 替换为实际运行 ID；失败时查看失败步骤。
gh run view RUN_ID --repo GoldBro233/MaintainedPackages
gh run view RUN_ID --repo GoldBro233/MaintainedPackages --log-failed
```

需要核对 AUR 时，读取 `https://aur.archlinux.org/packages/<name>`，或将 `https://aur.archlinux.org/<name>.git` 克隆到仓库外检查。GitHub 上的旧版本、AUR 上的新版本可能是发布后半段失败，先看日志。

## 文件与工具

| 入口 | 用途 |
| --- | --- |
| [packages.json](packages.json) | 包登记、启用状态、上游 GitHub 仓库、标签前缀和 AUR 附加文件白名单 |
| `packages/<name>/PKGBUILD` | 版本、来源、校验和、依赖及打包实现 |
| `packages/<name>/.SRCINFO` | 由 `makepkg --printsrcinfo` 生成、提交给 AUR 的元数据 |
| `packages/<name>/AGENTS.md` | 按需读取的单包打包方式、特殊约束与验证重点 |
| [cmd/maintpkg/main.go](cmd/maintpkg/main.go) | 进程入口：建立信号取消 context，调用 CLI 并返回退出码 |
| [internal/cli/cli.go](internal/cli/cli.go) | 五个子命令的参数、环境变量、标准流和退出码；调用 maintenance Service |
| [service.go](internal/maintenance/service.go)、[process.go](internal/maintenance/process.go) | Service 依赖、操作参数、矩阵选择；Runner/ExecRunner 和逐命令执行环境 |
| [config.go](internal/maintenance/config.go)、[paths.go](internal/maintenance/paths.go) | 包登记校验、受管文件列表、路径边界、普通文件与 SHA-256 |
| [srcinfo.go](internal/maintenance/srcinfo.go)、[candidate.go](internal/maintenance/candidate.go) | .SRCINFO 与依赖解析、vercmp 比较；manifest 和候选精确文件集合校验 |
| [prepare.go](internal/maintenance/prepare.go)、[build.go](internal/maintenance/build.go) | 上游查询、可信配方准备、来源验证、依赖安装及独立构建 |
| [git.go](internal/maintenance/git.go)、[publish.go](internal/maintenance/publish.go) | Git blob/树、AUR 基线、受信任输入、两端同步、竞争重试和部分失败状态 |
| [go.mod](go.mod) | 单一 Go 模块，最低 Go 1.27.0；仅使用标准库 |
| [.github/workflows/maintain-packages.yml](.github/workflows/maintain-packages.yml) | 定时及手动维护入口，按包生成矩阵 |
| [.github/workflows/release-package.yml](.github/workflows/release-package.yml) | 可复用的候选准备、构建、发布流水线 |
| [.github/workflows/check-maintenance.yml](.github/workflows/check-maintenance.yml) | push / pull request 的 Go 行为测试 |
| [.github/aur_known_hosts](.github/aur_known_hosts) | 发布时固定使用的 AUR SSH 主机公钥 |
| `internal/cli/*_test.go`、`internal/maintenance/*_test.go` | 测试与职责文件相邻；无 integration 标签的测试默认运行 |
| `internal/maintenance/integration*_test.go` | integration 标签下的真实工具验收：主流程、附加文件与信任检查、发布竞争和恢复 |
| [integration_helpers_test.go](internal/maintenance/integration_helpers_test.go)、[testdata/hook/main.go](internal/maintenance/testdata/hook/main.go) | 隔离夹具及编译后用于模拟远端拒绝的 Go pre-receive hook |
| `temp/` | 已忽略的本地试验和历史构建产物；不代表当前已发布状态 |

主要环境是 Arch Linux。CI 使用 `archlinux:base-devel`，安装 `go git curl jq nodejs pacman-contrib`，发布和行为测试还使用 `openssh`。`nodejs` 用于 Actions 执行环境，不是所有包的构建依赖。Go 版本须满足 go.mod；系统工具链不足时，CI 使用 `actions/setup-go` 提供 1.27.x。

- Go：编译维护 CLI，以标准库处理 JSON、HTTP 和子进程；Git：读取可信输入、管理远端及发布提交。Bash 仍由 makepkg 执行 PKGBUILD，curl 仍可用于来源下载，jq 用于工作流矩阵计数和人工查询。
- `makepkg`、`vercmp`、`pacman`：生成元数据、构建、比较版本、安装依赖；来自 Arch 的打包工具链。
- `updpkgsums`：来自 `pacman-contrib`，更新配方现有的校验和数组，包括架构专用数组。
- 解包、编译和应用专用工具：按目标包的 PKGBUILD 和包目录说明确定，不作为所有包的共同依赖。
- `namcap`：本地可选的包及依赖检查，不是当前 CI 的必经步骤。其结论需结合运行方式判断。
- `gh`：可选的远端状态查询和工作流触发工具；维护 CLI 本身不依赖它。

`makepkg` 和行为测试应以普通用户执行。依赖安装由有权限的步骤完成；CI 使用 root 安装依赖，再以 `builder` 用户构建。需要补齐维护工具时，参考上述 CI 安装列表，在任务所用的 Arch 环境中安装。

## 修改 Go 维护工具

- Go 维护逻辑位于同一个 `internal/maintenance` 包内，按上表中的职责文件定位；包专属的来源、依赖和安装行为继续放在 PKGBUILD 及包目录中。
- `internal/cli.Run` 负责解析参数和环境，再通过 `PrepareOptions`、`PublishOptions` 等值传给 Service。Service 保存仓库、已验证配置、Runner、HTTP client 和输出流；包、版本、候选及发布进度属于单次操作的局部状态。
- 外部命令统一通过 Runner 执行，传入 context、Dir、Env 和输出流。ExecRunner 按合并后的 PATH 查找程序；不要用 `os.Chdir`、`os.Setenv` 或 shell 命令字符串建立隐式进程状态。CLI 已从普通子进程环境中移除发布凭据，凭据只在对应发布边界使用。
- 配置和候选格式的兼容性由 `config.go`、`candidate.go` 及对应测试共同约束。候选校验只读取数据；publish 不执行候选 PKGBUILD。修改命令契约时，同步 CLI、调用它的工作流、行为测试和本指南中的示例。
- Git 推送的 porcelain 状态只从 stdout 解析，stderr 与之分开；保留外部命令的底层错误。AUR 完成后的失败通过 `PublishError.AURSynced` 和 Stage 表达，恢复规则见下文。
- 测试放在相应 Go 包中。新增真实工具用例使用 integration build tag，名称以 `TestIntegration` 或 `TestMakepkg` 开头，确保 CI 的筛选表达式会运行它们。复用隔离夹具和 Runner/HTTP client 边界，以候选内容、包内文件、Git refs/tree 和错误状态验证行为。
- 集成夹具将完整 `cmd/`、`internal/` 和受信任配置复制到临时仓库，远端重写到本地 bare 仓库并限制 `GIT_ALLOW_PROTOCOL=file`；上游响应由 loopback HTTP 提供。拒绝 hook 使用当前 Go 工具链的绝对路径编译，不依赖业务子进程 PATH 中的 Go。保持这些隔离边界，避免测试触及生产远端或安装宿主依赖。
- 修改 Go 源码后格式化受影响文件，并按文末执行验证；重新编译临时目录中的 maintpkg，避免继续使用修改前的二进制。


## 按需读取包文档

根文档只维护仓库级工具和通用流程，不列举具体包及其实现。包清单、启用状态、上游与标签前缀以 `packages.json` 为准；版本、依赖和来源以目标包的 PKGBUILD 与 .SRCINFO 为准。

只有任务涉及某个包时，才读取该包的 `packages/<name>/AGENTS.md`，再按需查看配方、安装钩子及相关文件。包目录说明补充该包的打包方式、兼容性处理和验证重点；不要在进入仓库时预读所有包文档。修改多个包时，分别读取受影响包的说明。

包目录的 AGENTS.md 是仓库维护文档，不需要加入 `aur_extra_files`。新增包时在其目录建立说明，维护该包时同步更新相关约束；根文档无需新增包清单或逐包链接。

## 修改配方与本地验证

1. 先读取目标包的 `packages/<name>/AGENTS.md`，再阅读 PKGBUILD、.SRCINFO、安装钩子以及 `packages.json` 记录，确认上游标签、产物命名、架构和许可证。
2. 上游版本升级时更新 `pkgver` 并将 `pkgrel` 重置为 `1`；同一上游版本的配方修复递增 `pkgrel`。自动维护也遵循此规则，不会因较旧的上游标签降级本地版本。
3. 来源或版本改变时运行 `updpkgsums`，再运行 `makepkg --printsrcinfo > .SRCINFO`。仅改依赖、安装逻辑或钩子时，也要重新生成 .SRCINFO。不要手改生成的 .SRCINFO，也不要用 `SKIP` 绕过校验失败。
4. 在仓库外的工作目录验证来源和实际构建。以下示例用于验证尚未提交的配方修改；先将 shell 变量 `package` 设为目标包名，并在构建环境内安装依赖：

```bash
: "${package:?请先将 package 设置为目标包名}"
work_root="$(mktemp -d /tmp/maintainedpackages-recipe.XXXXXX)"
mkdir "$work_root/package"
cp -a "packages/$package/." "$work_root/package/"
(
  cd "$work_root/package"
  # 仅在来源或版本改变时更新校验和：
  # updpkgsums
  makepkg --printsrcinfo > .SRCINFO
  makepkg --verifysource --noconfirm
  makepkg -f --noconfirm
)
```

将确认后的配方、校验和及生成的 .SRCINFO 同步回 `packages/<name>/`，同时带回有意修改的本地附加文件；不要复制下载归档、`src/`、`pkg/` 或二进制包。根目录 `.gitignore` 只忽略 `.codex` 和 `temp/`，直接在包目录构建容易产生未忽略的产物。

构建成功后，根据修改内容检查包内文件、运行 CLI 或实际打开 GUI。多架构配方需分别验证目标架构；当前 CI 的 `ubuntu-latest` 构建仅覆盖 x86_64，声明 aarch64 不等于已验证该架构。清理自己创建的临时目录，不删除用户已有试验产物。

## 统一候选流程

`prepare` 的打包输入来自当前 Git `HEAD` 中的文件，**不会使用工作区尚未提交的配方修改**。因此未提交修改先用上一节的工作副本验证；需要验证统一候选流程时，再使用包含目标修改的提交。`packages.json` 从当前检出目录读取，CLI 应从同一提交的源码构建；新候选的发布信任检查要求该提交包含 Go 实现。

各子命令参数：

```text
maintpkg [--repo <path>] matrix [name]
maintpkg [--repo <path>] prepare <name> <candidate-dir>
maintpkg [--repo <path>] install-build-deps <name> <candidate-dir>
maintpkg [--repo <path>] build <name> <candidate-dir> <build-dir>
maintpkg [--repo <path>] publish <name> <candidate-dir>
```

- `--repo` 默认 `.`，必须位于子命令之前；显式指定后可从仓库外调用。`--help` 无需有效仓库，退出 0；参数错误退出 2，运行失败退出 1，诊断写入 stderr。
- `matrix` 不调用 Git/Arch 工具或网络；无参数时按登记顺序选择启用包，零个启用包输出 `{"include":[]}`。显式空包名、未知或禁用包失败。
- 候选目录和构建目录必须为仓库外的绝对路径，初始为空或不存在；两者不能重叠，路径各级不能含符号链接。仓库内的 `temp/` 也不能作为这两个参数。
- `prepare` 访问 GitHub `/releases/latest`，严格剥离登记的标签前缀，仅接受数字点分版本；使用 `vercmp` 比较。当前不支持预发布版本字符串或 split packages，`pkgbase` / `pkgname` 必须与登记名一致。
- `prepare` 必要时更新版本、校验来源，生成 .SRCINFO，并读取 AUR 基线；AUR 不可读会失败，只有成功读取且确实无 refs 的空仓库才视为首次发布。
- 准备阶段的 makepkg/updpkgsums 将 `SRCDEST` 固定在操作自己的临时工作目录，保证已校验的下载型附加文件可进入候选；其他 makepkg 配置继续继承调用环境，构建阶段保留调用者的输出和缓存设置。
- 候选为 `manifest.json` 和 `package/` 下的白名单文件。manifest 记录 `package`、`source_commit`、`version`、`aur_commit`、`files`、`aur_files`，文件散列使用 SHA-256。
- `prepare` 成功 stdout 恰好三行：`needs_build=<true|false>`、`needs_publish=<true|false>`、`candidate_version=<version>`；工具日志进入 stderr。`FORCE_BUILD` 缺失或为空时为 false，仅接受 true/false；true 只强制构建，不会在无差异时强制发布。
- `install-build-deps`、`build`、`publish` 要求 `SOURCE_COMMIT`，且候选提交须等于当前检出的 HEAD；候选内容、文件集合和散列均需通过校验，不要手改候选。

从已提交配方进行一次强制构建验证（不会发布）；先将 shell 变量 `package` 设为目标包名：

```bash
set -euo pipefail
: "${package:?请先将 package 设置为目标包名}"
: "${cli_bin_dir:?请先按上文编译 maintpkg}"
repo_root="$PWD"
export SOURCE_COMMIT="$(git rev-parse HEAD)"
candidate_dir="$(mktemp -d /tmp/maintainedpackages-candidate.XXXXXX)"
build_dir="$(mktemp -d /tmp/maintainedpackages-build.XXXXXX)"
FORCE_BUILD=true "$cli_bin_dir/maintpkg" --repo "$repo_root" prepare "$package" "$candidate_dir"
# 如需安装候选声明的依赖，以下步骤会修改当前 Arch 环境的软件包：
sudo env SOURCE_COMMIT="$SOURCE_COMMIT" \
  "$cli_bin_dir/maintpkg" --repo "$repo_root" install-build-deps "$package" "$candidate_dir"
"$cli_bin_dir/maintpkg" --repo "$repo_root" build "$package" "$candidate_dir" "$build_dir"
```

候选准备和构建均以普通用户运行。依赖安装从候选 .SRCINFO 提取 `depends`、`makedepends`、`checkdepends` 及当前架构条目，交给 pacman；不自动从 AUR 安装依赖。`CARCH` 为空时使用 `uname -m`。构建会再次由 makepkg 检查依赖。CLI 不调用 sudo；上述示例由调用者使用 sudo，在容器内可由独立 root 步骤执行依赖安装。

## CI、发布与恢复

- `Check Maintenance`：push / pull request 以普通用户运行 Go 离线测试与隔离集成测试，不执行生产发布。
- `Maintain Packages`：cron 为 `0 */6 * * *`（UTC），计划任务启用发布；手动输入 `package` 留空时选择全部启用包，`publish` 和 `force_build` 默认均为 `false`。
- `Release Package`：按包执行 `prepare → build → publish`。候选通过不可覆盖的 Actions artifact 传递，并包含隐藏文件 .SRCINFO。构建 job 不接收发布凭据；发布要求构建成功、`needs_publish=true` 且调用方启用发布。
- 每包使用 `package-maintenance-<name>` 并发组，运行不会互相取消。不同包可以并行维护。
- 工作流限制从默认分支发布；CLI 进一步要求 `GITHUB_TARGET_BRANCH=main`。迁移默认分支时两处都要处理。各阶段从同一 github.sha 独立编译 CLI，发布凭据只注入 publish 步骤。

需要远端构建验证时，先将 shell 变量 `package` 设为目标包名，再手动触发（会创建 Actions 运行）：

```bash
: "${package:?请先将 package 设置为目标包名}"
gh workflow run maintain-packages.yml \
  --repo GoldBro233/MaintainedPackages --ref main \
  -f "package=$package" -f publish=false -f force_build=true
```

需要发布时在任务授权范围内使用相同工作流，将 `publish` 设为 `true`。推送到 main 本身只触发检查，发布由维护工作流处理；计划任务也会自动发布。

维护工具或工作流上线时，先在实现分支以 `publish=false`、`force_build=true` 验证真实包构建，再合入 main 并执行单包生产运行。需要控制首次发布范围时，在上线变更中临时暂停 schedule，待该次运行结束后恢复；不要在候选发布途中修改受信任输入。`needs_publish=false` 时 publish job 正常跳过，构建成功不代表已验证真实 SSH 推送或 GitHub 写入。

`publish` 需要 `SOURCE_COMMIT`、`GITHUB_TARGET_BRANCH`；有 AUR 写入时需要 `AUR_SSH_PRIVATE_KEY`，有 GitHub 回写时需要 `GITHUB_TOKEN`。CI 分别从同名 secret 和 `github.token` 提供凭据。AUR 使用固定 known_hosts、公钥校验及隔离 SSH 配置，不关闭主机校验，不将凭据写入仓库或文档。

发布顺序与恢复规则：

1. 先检查 GitHub 目标仍包含来源提交，维护配置和目标包没有发生冲突；再重新读取并核对 AUR。受保护输入包括 packages.json、固定主机公钥、维护/发布工作流、go.mod、可选 go.sum，以及完整的已跟踪 `cmd/`、`internal/` 树；新增、删除文件也会使旧候选失效。
2. 先向 AUR `master` 提交 `PKGBUILD`、`.SRCINFO` 和 `aur_extra_files`，再向 GitHub `main` 回写该包的 **PKGBUILD 和 .SRCINFO 两个文件**。附加文件不会自动回写 GitHub；需在仓库保留的本地文件应随人工修改提交。
3. AUR 同版本但 PKGBUILD 不同会失败并要求增加 `pkgrel`；AUR 版本领先也会失败。不要强推覆盖，先查明两边状态。
4. 仅 non-fast-forward / fetch-first 推送竞争会重试，最多三次；认证、网络和受管文件冲突直接报错。非受管的 AUR 文件保留。
5. 出现 `AUR synchronized; GitHub synchronization failed` 表示 AUR 已完成，GitHub 未完成。修复日志指出的问题后重新运行维护流程；两边状态仍允许时可补齐 GitHub，同一候选内容已在 AUR 时不会额外制造 AUR 提交。
6. 若准备后配置或同包文件已改变，重新准备并构建候选；不要修改 manifest 或绕过提交/散列检查。

## 新增或停用包

- 新增 `packages/<name>/PKGBUILD`、生成的 `.SRCINFO` 和需要跟踪的本地文件；在同目录的 `AGENTS.md` 记录包专属打包方式、约束和验证重点，不复制根文档的通用流程。
- 在 `packages.json` 添加 `name`、布尔 `enabled`、`upstream_repo`（`owner/repo`）和 `tag_prefix`；额外字段仅支持 `aur_extra_files`，包名不得重复。
- 需要送到 AUR 的安装钩子、许可证或其他文件必须列入 `aur_extra_files`。PKGBUILD / .SRCINFO 自动管理，不要重复列入。附加文件使用包目录下的相对路径，不能包含 `..`、`.git`、`src`、`pkg` 路径组件或符号链接。
- 本地附加文件必须被提交，才能进入从 HEAD 准备的候选。下载型附加文件可由 `source=("文件名::URL")` 配合校验和在准备阶段生成，不必预先存在于 GitHub 包目录。
- 停用时设置 `enabled=false`，保留有效配方和元数据；不需要复制一份新的单包工作流。
- 先运行 `matrix` 和配置测试，再完成目标包的来源验证与实际构建。新增非数字版本、特殊标签或 split packages 前，需明确修改维护 CLI 当前的契约及对应行为测试。

## 验证与交付

按修改范围选择验证：Go 逻辑和配置契约变更运行下列测试；涉及真实工具、候选或发布行为时完成 integration 套件。工作流改动运行 `actionlint` 并核对受影响的 Actions 阶段。仅修改文档时，检查链接、路径与涉及的命令示例即可，无需重复完整集成套件。

在仓库根目录执行；integration 套件必须使用普通用户：

```bash
go test ./...
go test -tags=integration ./... -run '^(TestIntegration|TestMakepkg)' -count=1 -timeout=20m
```

默认测试仅使用标准库，不需要 Git/Arch 工具或网络。集成测试使用临时目录、本地 bare Git 仓库、测试 SSH 密钥和 loopback HTTP，调用真实 makepkg、updpkgsums、vercmp、pacman -T、Git、ssh-keygen 与 bsdtar；依赖安装边界不会实际执行宿主 pacman -S。缺少工具或以 root 运行会失败，不会静默跳过。测试会检查归档 payload 与 .INSTALL、候选不变性和远端 refs/tree；这不验证真实 SSH 握手或 GitHub 凭据。

CLI 变更后，先重新执行本文开头的构建和 matrix 命令，确认输出与当前配置中的启用列表一致；再用同一二进制检查 help 和仓库外调用：

```bash
: "${cli_bin_dir:?请先按上文编译 maintpkg}"
: "${repo_root:?请先设置仓库根目录}"
"$cli_bin_dir/maintpkg" --help
(cd /tmp && "$cli_bin_dir/maintpkg" --repo "$repo_root" matrix)
```

配方变更还须构建实际目标包并验证受影响的运行路径，不能用 fixture 测试代替。交付时说明修改包及版本、执行的检查、构建/运行结果、是否进行了远端发布和确切失败点；未验证的架构或 GUI 路径如实注明。维护流程改变时同步更新本文件。
