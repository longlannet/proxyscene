# proxyscene

[![CI](https://github.com/longlannet/proxyscene/actions/workflows/ci.yml/badge.svg)](https://github.com/longlannet/proxyscene/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/longlannet/proxyscene)](https://github.com/longlannet/proxyscene/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/longlannet/proxyscene)](go.mod)

`proxyscene` 是一个面向 Linux 服务器的 Xray 代理管理器。项目由一个安装脚本和一个 Go 编写的单二进制管理程序组成：

- `install.sh`：负责安装期工作——校验并更新固定版本 Xray、安装 `proxyscene` 管理程序、初始化 systemd。支持显式离线 bundle、固定 Release 下载和源码编译三种方式。
- `proxyscene`：负责运行期管理，包括节点管理、场景开关、Xray 配置生成、systemd 服务管理和开机恢复。

> 当前项目适合在使用 systemd 的 Linux 服务器上运行。大多数管理命令需要 root 权限，建议统一使用 `sudo` 执行。

## 功能特性

- 单二进制 Go 管理程序，安装后命令为 `proxyscene`。
- Release 的 `checksums.txt` 覆盖版本化 `install.sh`、管理程序包、离线 bundle 和固定 Xray 对应源码归档；安装器解析 `latest` 后固定到一个明确 tag，再按 SHA256 校验下载内容。
- 支持离线安装：先用 Release checksum 校验自包含 bundle，解压后显式运行 `install.sh --offline`；包内 manifest 会在复制前复核全部组件。
- GitHub Actions 从 `main` 手动发起发布，交叉编译 amd64/arm64/386/armv7，校验 SHA256 后发布为 immutable Latest。
- 支持 Xray 主服务和开机恢复服务的 systemd 管理。
- 支持三类代理场景：
  - 全局代理：写入系统 profile 和 apt 代理配置。
  - 开发代理：为目标用户设置 git/npm 代理，并在关闭时恢复。
  - Telegram 服务代理：为 Hermes systemd 服务注入专用环境，并事务化托管用户级 OpenClaw 的 Telegram 配置。
- 支持多节点管理：添加、删除、改名、列表、订阅导入、测速、自动选择。
- 支持基础节点协议解析：VLESS、VMess、Trojan、Shadowsocks。
- 支持按场景选择不同节点。
- 状态文件带进程锁，避免多个管理进程并发写入造成覆盖。
- Xray 配置写入前会进行配置测试。

## 目录结构

```text
proxyscene/
├── .github/
│   └── workflows/
│       ├── ci.yml          # 格式/测试/vet/shellcheck
│       └── release.yml     # main 手动发布多架构 + SHA256 + immutable Release
├── .gitignore
├── LICENSE
├── LICENSE-GPL-3.0             # Xray GPL 依赖所需的完整 GPL-3.0 文本
├── NOTICE                       # manager 依赖与离线 Xray 的第三方声明
├── SOURCE-Xray                  # 固定 Xray ELF 与同版对应源码资产说明
├── THIRD_PARTY_LICENSES         # manager 静态链接依赖的完整许可证文本
├── THIRD_PARTY_LICENSES-Xray    # 固定 Xray ELF 已链接依赖的许可证与 module sum
├── SECURITY.md
├── go.mod
├── go.sum
├── install.sh
├── README.md
├── scripts/
│   ├── build-bundle.sh
│   ├── build-xray-source-archive.sh
│   ├── generate-xray-third-party-licenses.sh
│   ├── install-test.sh
│   ├── systemd-integration-test.sh
│   └── verify-release-artifacts.sh
├── cmd/
│   └── proxyscene/
│       └── main.go
└── internal/
    └── manager/
        ├── app.go
        ├── dev.go
        ├── global_journal.go
        ├── node.go
        ├── openclaw.go
        ├── openclaw_json5.go
        ├── ownership.go
        ├── scenes.go
        ├── store.go
        ├── systemd.go
        ├── telegram_discovery.go
        ├── telegram_journal.go
        ├── telegram_runtime.go
        ├── types.go
        ├── user_identity.go
        ├── util.go
        └── xray.go
```

## 系统要求

- Linux。
- systemd。
- root 或 sudo 权限。
- `flock`（通常由 `util-linux` 提供）。安装器必须先取得全局锁，不能等开始改包以后再自动安装它。
- 联机安装需要可访问 HTTPS，并需要可用的软件包管理器之一：apt、dnf、yum、apk、zypper。
- 离线 bundle 安装不需要网络或 Go，但目标机仍需具备 Bash、`flock` 和基础校验/归档工具。
- Go 1.27.1 或更高版本**仅在源码编译时需要**（默认走预编译二进制，目标机无需 Go）。若需源码编译且系统没有可用 Go，安装脚本会自动准备。

## 快速开始

不要把 mutable `main` 分支脚本通过管道直接交给 root shell，也不要把节点或订阅 URL 放进命令参数。
下面的 Release bootstrap 示例要求验证机已有 `curl`、`jq` 和 `sha256sum`。

> 迁移说明：`v0.7.1` 是旧的 mutable Release，新的 SHA256 + immutable 模型从 `v0.8.0` 起生效。新安装器会有意拒绝 `immutable=false` 的 `v0.7.1`；请使用明确的 `v0.8.0` 或更高版本，不要让 `latest` 意外解析到旧版本。

### 方式一：固定 Release 离线安装

在联网机器上把 `<release-tag>` 替换为明确版本，并按目标架构选择 bundle。整个下载和校验过程在同一个严格退出的 root shell 中进行，任何一步失败都会终止本段命令。下载目录随机生成，权限为 root-only；记录成功后输出的 `STAGE` 路径。

<!-- bootstrap:offline-download -->
```bash
sudo bash -s -- '<release-tag>' amd64 <<'BOOTSTRAP'
set -euo pipefail
umask 077
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
VERSION=$1
ARCH=$2
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
case "$ARCH" in amd64|arm64|386|armv7) ;; *) exit 2 ;; esac
BASE="https://github.com/longlannet/proxyscene/releases/download/${VERSION}"
STAGE=$(mktemp -d "/root/proxyscene-bootstrap-${VERSION}-${ARCH}.XXXXXXXX")
curl -q -fsSL --proto '=https' --proto-redir '=https' -o "$STAGE/release.json" \
  "https://api.github.com/repos/longlannet/proxyscene/releases/tags/${VERSION}"
curl -q -fL --proto '=https' --proto-redir '=https' \
  -o "$STAGE/checksums.txt" "${BASE}/checksums.txt"
curl -q -fL --proto '=https' --proto-redir '=https' \
  -o "$STAGE/proxyscene_bundle_linux_${ARCH}.tar.gz" \
  "${BASE}/proxyscene_bundle_linux_${ARCH}.tar.gz"
cd -- "$STAGE"
jq -se --arg tag "$VERSION" \
  'length == 1 and (.[0] | type == "object" and .tag_name == $tag and .immutable == true)' release.json
awk -v file="proxyscene_bundle_linux_${ARCH}.tar.gz" \
  '$2 == file {count++; line=$0} END {if (count != 1) exit 1; print line}' \
  checksums.txt | sha256sum -c -
printf 'STAGE=%q\n' "$STAGE"
BOOTSTRAP
```
<!-- /bootstrap:offline-download -->

将整个 staging 目录传到目标机的 `/root` 下，保留 root 所有权、目录 `0700` 和文件 `0600` 权限，或在同一台机器继续。把下段的版本、架构和 `<staging-directory>` 替换为实际值。下段会重新核对 Release 身份和归档 SHA256，通过后才解压执行；不依赖上一次 shell 的校验结果。

<!-- bootstrap:offline-install -->
```bash
sudo bash -s -- '<release-tag>' amd64 '<staging-directory>' <<'BOOTSTRAP'
set -euo pipefail
umask 077
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
VERSION=$1
ARCH=$2
STAGE=$3
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
case "$ARCH" in amd64|arm64|386|armv7) ;; *) exit 2 ;; esac
[[ "$STAGE" == "/root/proxyscene-bootstrap-${VERSION}-${ARCH}.${STAGE##*.}" ]]
[[ "${STAGE##*.}" =~ ^[a-zA-Z0-9]{8}$ ]]
[[ -d "$STAGE" && ! -L "$STAGE" && $(stat -c '%u:%g:%a' -- "$STAGE") == 0:0:700 ]]
cd -- "$STAGE"
BUNDLE="proxyscene_bundle_linux_${ARCH}.tar.gz"
for file in release.json checksums.txt "$BUNDLE"; do
  [[ -f "$file" && ! -L "$file" && $(stat -c '%u:%g:%a' -- "$file") == 0:0:600 ]]
done
jq -se --arg tag "$VERSION" \
  'length == 1 and (.[0] | type == "object" and .tag_name == $tag and .immutable == true)' release.json
awk -v file="$BUNDLE" \
  '$2 == file {count++; line=$0} END {if (count != 1) exit 1; print line}' \
  checksums.txt | sha256sum -c -
EXTRACTED=$(mktemp -d "$STAGE/extracted.XXXXXXXX")
tar --no-same-owner --no-same-permissions -xzf "$BUNDLE" -C "$EXTRACTED"
cd -- "$EXTRACTED/proxyscene_bundle_linux_${ARCH}"
exec ./install.sh --offline
BOOTSTRAP
```
<!-- /bootstrap:offline-install -->

bundle 内含 `install.sh`、管理程序、固定版本 Xray、项目及第三方许可证、Xray 对应
源码说明和 `bundle-manifest.sha256`。安装器会在复制前复核 manifest；同目录存在二进制不会让普通联机安装自动切换到离线模式。完整的 Xray 及其链接模块对应源码另作为同一 Release 的 `xray_source_v26.9.9.tar.gz` 资产发布，并由同一 `checksums.txt` 约束。

当前固定 Xray 为官方 `v26.9.9`（上游标记为预发布版），以获得更新的 Go 工具链和依赖；构建流程固定其提交、四架构归档 SHA256 和对应源码，并检查配置兼容性与已知漏洞。

升级兼容性：此版本移除了 Shadowsocks `none/plain`，且 VLESS/Trojan 明文传输仅允许上游内建的私有/保留地址与本地域名。新导入和启用会明确拒绝不兼容节点；旧节点记录仍可读取、删除和替换。若当前正在使用上述模式，请在升级前切换到 TLS/REALITY 或支持的 AEAD 节点。

### 方式二：固定 Release 联机安装

从同一个明确 tag 下载安装器和 checksum。下载、校验、展示和执行使用 root-only staging 中的同一个文件，并在同一个严格退出的 shell 中进行；校验失败时不会继续执行安装器。

<!-- bootstrap:online -->
```bash
sudo bash -s -- '<release-tag>' <<'BOOTSTRAP'
set -euo pipefail
umask 077
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
VERSION=$1
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
BASE="https://github.com/longlannet/proxyscene/releases/download/${VERSION}"
STAGE=$(mktemp -d "/root/proxyscene-bootstrap-${VERSION}.XXXXXXXX")
curl -q -fsSL --proto '=https' --proto-redir '=https' -o "$STAGE/release.json" \
  "https://api.github.com/repos/longlannet/proxyscene/releases/tags/${VERSION}"
curl -q -fL --proto '=https' --proto-redir '=https' \
  -o "$STAGE/checksums.txt" "${BASE}/checksums.txt"
curl -q -fL --proto '=https' --proto-redir '=https' \
  -o "$STAGE/install.sh" "${BASE}/install.sh"
cd -- "$STAGE"
jq -se --arg tag "$VERSION" \
  'length == 1 and (.[0] | type == "object" and .tag_name == $tag and .immutable == true)' release.json
awk '$2 == "install.sh" {count++; line=$0} END {if (count != 1) exit 1; print line}' \
  checksums.txt | sha256sum -c -
cat -- install.sh
exec env PROXYSCENE_VERSION="$VERSION" bash "$STAGE/install.sh"
BOOTSTRAP
```
<!-- /bootstrap:online -->

从 `v0.8.0` 起，安装器内部也可使用 `latest`；它会先通过 GitHub API 把 `latest` 解析为明确 tag，随后只从该 tag 下载。上面的 bootstrap 仍要求显式版本，便于人工确认目标。自定义管理程序镜像必须设置明确 `PROXYSCENE_VERSION`，并让 `PROXYSCENE_BASE_URL` 直接指向该 tag 的资产目录；镜像只提供管理程序归档，`checksums.txt` 始终来自 `PROXYSCENE_REPO` 对应固定 tag 的 GitHub Release，因此镜像文件必须与该 Release 完全一致。

### 方式三：从源码安装

进入可信源码 checkout 后强制源码编译：

```bash
sudo PROXYSCENE_BUILD_FROM_SOURCE=1 bash ./install.sh
```

安装器默认不导入节点。安装完成后再通过交互输入，避免秘密进入 shell 历史和进程参数：

```bash
sudo proxyscene install
```

SHA256 能发现损坏或资产不一致，但不是独立发布者签名：若 GitHub 仓库控制面在 immutable 发布前被攻陷，攻击者可能同时替换文件和 checksum。官方仓库只需在 Settings 启用 immutable releases；发布工作流会在发布后从 API 验证 immutable 状态、Latest、全部资产 digest 和下载字节。高威胁环境还应通过独立可信渠道取得已知 checksum。

默认安装完成后，管理程序会安装为：

```text
/usr/local/bin/proxyscene
```

因此后续可以在任意目录运行；如果安装时设置了 `PROXYSCENE_SWITCH_BIN`，则使用对应的自定义绝对路径：

```bash
sudo proxyscene
```

### 安装后：导入订阅或添加节点

导入订阅链接：

```bash
sudo proxyscene node import --stdin
```

添加单个节点：

```bash
sudo proxyscene node add --stdin '我的节点'
```

查看节点：

```bash
sudo proxyscene node list
```

### 安装后：开启代理场景

开启全局代理：

```bash
sudo proxyscene global on
```

开启开发代理：

```bash
sudo proxyscene dev on
```

开启 Telegram 服务代理：

```bash
sudo proxyscene tg on
```

查看当前状态：

```bash
sudo proxyscene status
```

## 安装脚本说明

`install.sh` 支持三种用法（见上文「快速开始」）：显式离线 bundle、固定 Release 联机安装、可信源码目录编译。下面以联机安装为例说明各步骤；离线安装会跳过下载与编译。

脚本会执行以下步骤：

1. 在任何文件创建前固定 `umask 077`，检查 root、布尔/路径/URL 参数，并取得 `/run/proxyscene-install.lock`；进入文件事务前再取得 `/run/proxyscene-host-ownership.lock`。已有 CoreDir 时安装器继续取得 `.state.lock`，并把三把已持有的 flock FD 交给新管理程序校验、复用；fresh CoreDir 不会被安装器提前放入锁文件，而是交接 install/host 两把外层锁，由 manager 在验证新目录 marker 后创建并取得 state lock。整个过程保持 `install -> host -> state` 顺序，直到初始化完成才逆序释放，因此旧版（只认识 state lock）和新版命令都不能插入文件替换与初始化之间。
2. 安装缺少的基础依赖：curl、ca-certificates、tar、unzip、coreutils、jq。
3. 从固定版本下载 Xray，用仓库内按架构固定的 SHA256 校验，再暂存到核心目录。
4. 安装管理程序 `proxyscene` 到 `PROXYSCENE_SWITCH_BIN`（默认 `/usr/local/bin/proxyscene`）：
   - 默认先通过 GitHub REST 元数据确认 tag 精确匹配且 `immutable=true`，再下载对应架构的预编译归档（`proxyscene_linux_<arch>.tar.gz`）。归档可来自自定义镜像，但 SHA256 必须取自同一明确 tag 的 GitHub Release `checksums.txt`。目标机无需 Go。
   - 联机 Release 下载、元数据或 checksum 校验失败时立即终止，不会隐式改变信任来源。源码编译只在显式设置 `PROXYSCENE_BUILD_FROM_SOURCE=1` 时启用，并且只允许从当前 `install.sh` 所在、含 `go.mod` 的可信 checkout 编译。
5. 调用 `proxyscene install --skip-node` 初始化状态目录和 systemd 服务。

> 离线安装（见上文「方式一」）只在显式给出 `--offline` 时启用。安装器会先校验完整内部 manifest 和所有 ELF 架构，再进行文件替换；同目录存在二进制不会触发自动离线模式。
> 运行 `proxyscene version` 可查看已安装的版本与 commit（预编译二进制会在构建时注入 git tag 与 commit）。

安装器对本次管理程序、Xray、许可证、源码说明和版本标记文件的替换保留逐文件备份；下载、校验或文件替换失败时会尝试恢复。文件全部就绪后，安装器先提交这些依赖，再调用管理程序初始化：若初始化在写入用户、systemd unit、状态或配置后失败，已验证文件会保留，避免 unit 指向被回滚或不存在的二进制，并提示修复原因后执行 `sudo proxyscene install --skip-node` 重试。软件包管理器和管理程序内部副作用不属于文件事务。下载、备份、模块缓存、构建缓存和 GOPATH 位于 root-only 的 `/run/proxyscene-install-tmp/transaction.*`，提交或回滚时删除；因为 `/run` 通常挂载为 `noexec`，源码编译临时下载的可执行 Go 工具链会放在已验证 CoreDir 内的隐藏临时目录，并在提交或回滚时删除。安装器不会替换 `/usr/local/go`，固定的空事务根目录会保留供后续安装复用。

### 安装脚本是否交互式

默认不交互。

- `sudo bash ./install.sh`：非交互安装，不导入节点。
- `sudo proxyscene install`：交互式初始化，会提示输入一个节点链接，可以留空跳过。
- `sudo proxyscene install --skip-node`：非交互初始化，不录入节点。

安装器拒绝所有位置参数，不支持在安装命令中直接携带节点 URL。

订阅链接不在 `install` 中录入，订阅应通过节点管理导入：

```bash
sudo proxyscene node import --stdin
```

## 常用命令

### 主菜单

```bash
sudo proxyscene
```

主菜单包含：

1. 初始化/更新管理服务。
2. 切换全局代理。
3. 切换开发代理。
4. 切换 Telegram 服务代理。
5. 节点管理。
6. 测试代理。
7. 查看状态。
8. 卸载。
9. 退出。

### 初始化

```bash
sudo proxyscene install
sudo proxyscene install --skip-node
```

### 状态查看

```bash
sudo proxyscene status
```

### 场景开关

```bash
sudo proxyscene global on
sudo proxyscene global off
sudo proxyscene dev on
sudo proxyscene dev off
sudo proxyscene tg on
sudo proxyscene tg off
```

也可以不带 `on` / `off`，直接切换当前状态：

```bash
sudo proxyscene global
sudo proxyscene dev
sudo proxyscene tg
```

### 节点管理

打开节点管理菜单：

```bash
sudo proxyscene node
```

查看节点列表：

```bash
sudo proxyscene node list
```

添加节点：

```bash
sudo proxyscene node add --stdin '备注名'
```

导入订阅：

```bash
sudo proxyscene node import --stdin
```

节点测速：

```bash
sudo proxyscene node test
```

自动选择默认节点：

```bash
sudo proxyscene node auto default
```

按场景选择节点：

```bash
sudo proxyscene node use '节点ID' default
sudo proxyscene node use '节点ID' global
sudo proxyscene node use '节点ID' dev
sudo proxyscene node use '节点ID' telegram
sudo proxyscene node use '节点ID' all
```

删除节点：

```bash
sudo proxyscene node remove '节点ID'
```

修改节点备注：

```bash
sudo proxyscene node rename '节点ID' '新备注'
```

### 测试代理

```bash
sudo proxyscene test
```

### 查看版本

```bash
proxyscene version
```

## 三种代理场景

### 全局代理

命令：

```bash
sudo proxyscene global on
sudo proxyscene global off
```

开启后会写入：

- `/etc/profile.d/proxyscene-global-proxy.sh`
- `/etc/apt/apt.conf.d/99proxyscene-global-proxy`

首次写入前，程序会把这两个专用路径的原始存在状态、内容、权限和本次托管内容记录到
`/opt/proxyscene/global-proxy-journal.json`。关闭或卸载只恢复仍与 journal 中托管内容逐字节匹配的文件；
原文件会按原内容和权限恢复，原本不存在的文件才会删除。没有 journal 时不会按路径或内容猜测并删除文件；
托管期间被管理员修改的普通文件会作为管理员的新原值保留，并在恢复其它受管文件后释放 ownership。

默认监听地址：

```text
HTTP  : 127.0.0.1:7890
SOCKS : 127.0.0.1:7894
```

说明：

- 新登录的 shell 会自动读取 `/etc/profile.d/proxyscene-global-proxy.sh`。
- 当前已打开的 shell 需要重新登录，或手动 source 对应 profile 文件。
- 当前版本的全局代理主要是环境变量和 apt 代理配置，不等同于完整透明代理。

### 开发代理

命令：

```bash
sudo proxyscene dev on
sudo proxyscene dev off
```

开启后会为目标用户设置：

- git `http.proxy`
- git `https.proxy`
- npm `proxy`
- npm `https-proxy`

程序会备份原始配置，并记录本程序写入过的开发代理地址；如果开启期间调整了开发代理端口，关闭时也会识别并清理这些已记录的 managed 值，尽量避免误删用户手工配置。npm 对无路径代理 URL 自动补出的单个尾 `/` 会按同一受管值处理，其他差异仍视为管理员修改。

为保证 Git 配置能精确恢复，目标用户只能存在一个常规文件形式的 global 配置（`~/.gitconfig` 或 `~/.config/git/config`），且其中不能使用 `include`/`includeIf`；双 global 文件、符号链接/特殊文件或 include 拓扑会在任何写入前失败关闭。每次 Git 修改会持有实际配置文件的 `.lock`，覆盖读取、比较和提交，并保留已经完成的同键新增值。遇到其它 Git 写入者的锁会停止并保留恢复记录，待写入结束后重试。重复 `dev on` 或开机恢复遇到管理员改值、Git 受管值之外的追加值时，会保留配置并拒绝覆盖；请先 `dev off` 完成保守恢复，再 `dev on` 重新采集原值。

npm 只读取并原子修改记录用户的 `~/.npmrc`，不会以 root 运行 npm 配置命令，也不会读取当前项目的 `.npmrc` 或执行其中的重定向设置。其它原始配置行保留；代理键存在数组、环境变量插值或无法精确解释的值时会拒绝接管。项目级/全局级 npm 配置仍按 npm 自身优先级工作，项目代理可覆盖这里管理的用户级代理。所有跨用户工具命令在记录家目录中运行，使用显式最小环境，避免继承调用者凭据和运行时注入变量。

默认监听地址：

```text
HTTP: 127.0.0.1:7891
```

首次开启开发代理时的目标用户选择规则：

1. 优先使用环境变量 `PROXYSCENE_DEV_TARGET_USER`。
2. 其次使用 `sudo` 调用时的原始用户。
3. 再使用当前进程用户。
4. 最后回退到 `root`。

解析出的实际用户会随运行配置写入状态文件，后续关闭和开机恢复会继续使用该用户；不需要重复传入环境变量。显式传入新的有效值并成功执行管理命令后，会迁移到新用户并更新持久化配置。

示例：

```bash
sudo PROXYSCENE_DEV_TARGET_USER=alice proxyscene dev on
sudo PROXYSCENE_DEV_TARGET_USER=alice proxyscene dev off
```

### Telegram 服务代理

命令：

```bash
sudo proxyscene tg on
sudo proxyscene tg off
```

这里管理指定 gateway 的 Telegram 出站连接。它不接管 LLM、搜索、其他频道、第三方媒体 URL 下载，
也不构成禁止所有直连的网络隔离。服务外单独启动的 Hermes CLI 不继承服务 drop-in；Hermes 的独立发送工具
在代理初始化异常时还可能使用没有显式代理的 Bot。因此不能把本场景用于承诺“所有 Telegram 流量都不直连”。

Hermes 和 OpenClaw 使用不同的接管机制：

- Hermes 只消费 Telegram 专用的 `TELEGRAM_PROXY`。系统级和用户级目标都把该变量直接写进各自的
  `90-proxyscene-telegram-proxy.conf` systemd drop-in，不使用跨服务共享的环境文件。
  每个目标在写入前先进入 `/opt/proxyscene/telegram-proxy-journal.json` 及其备份；journal 按
  prepared/active/restoring 阶段记录精确托管内容，服务配置协调成功后才提交或释放 ownership；active 表示
  配置所有权已提交，不代表 Telegram 已连通。
  程序不会注入 `HTTP_PROXY`、`ALL_PROXY` 等会改变服务全部出网的通用变量。Hermes `v0.19.0` / `v0.21.5`
  会让匹配 Telegram API 或运行时 DoH 回退 IP 的 `NO_PROXY`/`no_proxy` 覆盖 `TELEGRAM_PROXY`；因此程序会在
  写入前检查 unit、systemd manager 最终环境和 `ExecStart`，发现 `api.telegram.org`、`*` 或可能匹配回退地址的
  公网 IPv4/CIDR 绕过项时拒绝接管。无法证明内容的有效 `EnvironmentFile`，以及可改变代码加载的
  `PYTHONHOME`、`PYTHONPATH`、`LD_PRELOAD`、`LD_LIBRARY_PATH`、`LD_AUDIT` 也会失败关闭。
  程序还会把 `HERMES_HOME`、`active_profile` 和 gateway 的 `PROJECT_ROOT` 绑定到服务用户的持久身份，并检查
  profile `.env`/`.op.env`、项目 `.env` 与 `/etc/hermes/.env`；其中声明 `TELEGRAM_PROXY`、`NO_PROXY`、
  `no_proxy`、`TELEGRAM_FALLBACK_IPS`、`HERMES_TELEGRAM_DISABLE_FALLBACK_IPS`、`HERMES_HOME`、`HERMES_MANAGED_DIR` 或
  `HERMES_S6_SUPERVISED_CHILD` 或上述代码加载变量时拒绝自动接管。dotenv 按 Hermes 实际支持的 UTF-8/带 BOM
  UTF-16 解析；UTF-32、无 BOM NUL 编码、非法 UTF-8/latin1 fallback 以及可被 Hermes 修复器从同一行拆出的
  粘连路由变量一律失败关闭。
  用户 profile 与 `/etc/hermes/config.yaml` 的顶层标量会被 Hermes 桥接为进程环境；其中声明上述任一
  路由变量时同样拒绝。托管配置必须是 root-owned、不可由组/其他用户写入的普通文件，且路径不能经过符号链接。
  `config.yaml` 中启用的外部 secret source 也必须可证明不会在启动后注入这些路由变量，否则同样拒绝。
  仅支持没有命名 profile 的单 profile gateway。Hermes `v0.21.5` 可自动 multiplex，并已不再把
  `multiplex_profiles=false` 作为可靠关闭方式；进程级 `TELEGRAM_PROXY` 无法保证次级 profile 的代理。
  因此启用 multiplex、使用命名 active profile 或 `profiles` 中存在命名目录/符号链接时会拒绝接管，
  恢复期间发现这些变化也会保留 ownership 记录并报错。NO_PROXY 检查同时覆盖逗号/Unicode 空白分隔、
  通配域名、`//host` 和 IPv4 点分掩码；dotenv 声明检查使用 Python/Node 实际空白语义。
  受管 drop-in 固定 `HERMES_TELEGRAM_DISABLE_FALLBACK_IPS=1`，关闭代理模式下不必要的 DNS/DoH 回退地址发现；
  此设置与代理一起记录、校验并恢复，不修改应用 YAML。若环境存在会让 Hermes 跳过 managed 配置的
  `PYTEST_CURRENT_TEST`，也会拒绝绑定该运行配置。
- 用户级 OpenClaw gateway 不消费 `TELEGRAM_PROXY`。程序直接托管
  `<用户家目录>/.openclaw/openclaw.json` 的 `channels.telegram.proxy`，不为它写 env drop-in。
  修改前会在 `/opt/proxyscene/openclaw-proxy-journal.json` 及其备份中持久化原值、原容器结构和共享目标；
  journal 按 prepared/active/restoring 阶段记录所有权，配置写入及相关频道/服务协调成功后才提交或删除记录。
  主 journal 和备份都在读取的同一个文件描述符上校验 root 所有权、root-only 权限与普通文件类型。
  自动接管仅限有效 unit 明确使用目标用户 `HOME`，且每条最终 `ExecStart` 都是绝对 `node`/`nodejs` 直接调用
  绝对 `.../openclaw/dist/{index.js,index.mjs,entry.js,entry.mjs} gateway` 的情况；允许官方生成的数字型 Node 内存参数
  （如 `--max-old-space-size=3969`），仅接受经实际 Node 验证的等号赋值形式。shell、`env`、`chroot` 等 wrapper
  不会被接管。任何非默认配置选择器、
  `--profile`/`--dev`、有效 `EnvironmentFile`、默认 `.env`/`gateway.env` 中的路径选择器、配置里的
  任意对象/数组层级的 `$include`、运行时 env 选择器、`NODE_PATH` 或动态链接器注入变量都会使程序失败关闭。
  `NODE_OPTIONS` 仅允许同一数字内存参数白名单，`--require`、`--import`、`--eval` 等代码加载选项始终拒绝；
  dotenv 中跨行或不能明确解析的声明也会拒绝。若任一 Telegram 账号定义了账号级 `proxy`，也会拒绝接管，
  因为 OpenClaw 的账号合并语义会让它覆盖顶层 `channels.telegram.proxy`。canonical 配置缺失但任一
  `~/.openclaw/clawdbot.json` 或 `~/.clawdbot/*.json` legacy 候选生效时也会拒绝；journal 不跨路径托管。
- 系统级 OpenClaw 无法可靠映射到配置所属用户，因此只告警并跳过配置接管，需由管理员手动设置
  `channels.telegram.proxy`。

OpenClaw 优先使用官方配置监视与 Telegram 频道重载。程序在写入前记录每个网关的配置代际和频道启动状态，
写入后通过本地只读 `config.get` / `channels.status` 确认：代理值符合待提交记录、配置代际已装载、
启用账号以新的启动代际就绪，并且 gateway 进程没有重启。CLI 以记录用户身份、最小环境执行，输出与时间均有上限；
不在命令行传 token，不发送 Telegram 消息。当前已按 OpenClaw 2026.9.6 验证这一协议。
当前自动频道确认支持本地非 TLS 网关和默认/`hybrid` 重载模式。旧版本、不支持的认证/启动方式、其他重载模式、
远端/TLS 网关、缺少确认字段或超时会明确回退整服务重启；身份或配置冲突仍报错并保留记录。

Hermes 0.21.5 默认冷启动会丢弃 Telegram 服务端积压更新，`systemctl reload` 也会重启整个网关。
因此，运行中的 Hermes 首次接管、代理修改或恢复需要能证明最终消息策略为字面布尔 `false`：

```yaml
platforms:
  telegram:
    extra:
      drop_pending_on_cold_boot: false
```

请将该项合并进现有 `~/.hermes/config.yaml`，保留原配置；不要用上述片段覆盖整个文件。
程序会按上游优先级检查用户配置、managed 配置、legacy `gateway.json` 和其他 Telegram 配置段的合并结果，
不能证明消息保留时会在首次/更新写入前拒绝；恢复中遇到策略变化则保留可重试记录和仍属于本程序的配置。
proxyscene 不会自动改写这项业务设置。已经停止的服务只保存/恢复代理配置，不会被启动；之后人工启动时仍遵循 Hermes 自身消息策略。

`proxyscene status` 分开显示“配置是否已写入并核对”“服务是否运行”“频道/代理连通性是否探测”。
状态命令只读，不发送消息，不把历史 active 记录或 systemctl 零退出码当作 Telegram 已连通。
重复开启、开机恢复或仅更换 Xray 上游节点，托管客户端配置未变时不会无意义地重启网关。

默认监听地址：

```text
HTTP  : 127.0.0.1:7892
SOCKS : 127.0.0.1:7893
```

默认目标服务（锚定规范的系统级 hermes 网关和 root 用户级 hermes 网关；OpenClaw 网关、hermes 的 profile 实例、其它用户级单元都由自动发现覆盖；目标不存在时会跳过，不生成 phantom drop-in）：

```text
hermes-gateway user:root:hermes-gateway
```

同时，程序会按 systemd 的有效单元语义自动发现系统级和每个本地用户的用户级网关：

- 按 systemd 搜索优先级选择同名单元，尊重高优先级覆盖、mask、alias、runtime/generator 单元和 drop-in；解析
  `Environment`、`UnsetEnvironment` 与 `ExecStart=` reset 后的最终结果，而不是按文件名或文本子串猜测。
- 运行时校验拒绝有效 systemd specifier、`ExecStart` 的 `$` 展开、启动前后钩子、`PAMName`、`DynamicUser`，以及会让服务看到
  不同文件树的 `RootDirectory`/`RootImage`、bind/image/extension/tmpfs/inaccessible namespace 和 `ProtectHome` 设置。
  Hermes 官方的 `ExecStop=-<PROJECT_ROOT>/venv/bin/python -m gateway.systemd_stop_mark` 和
  `ExecStopPost=-<PROJECT_ROOT>/venv/bin/python -m gateway.cgroup_cleanup` 经路径、模块和参数精确绑定后允许；任意其他钩子仍拒绝。
- OpenClaw 必须最终同时包含 `OPENCLAW_SERVICE_MARKER=openclaw` 和
  `OPENCLAW_SERVICE_KIND=gateway`；因此 node、guard 等角色不会误命中。
- Hermes 必须由 argv[0] 直接执行 `hermes_cli`/`hermes-agent gateway run`，或由明确的 Python 解释器直接执行
  `-m hermes_cli[.main] gateway run`；`env`、`chroot` 或其它 wrapper 不会被误认。
  精确的 `hermes-gateway.service` 另由默认目标锚定，不依赖名称泛匹配。
- 系统级 user-unit 目录中的单元因无法确定作用用户，只提示并跳过；需要时用
  `user:用户名:服务名` 显式指定。单个 unit 和合并后的有效内容都有大小上限。

目标服务支持两种写法：

- 系统级 systemd 服务：`hermes-gateway` 或实际消费 `TELEGRAM_PROXY` 的自定义服务名。
- 用户级 systemd 服务：`user:用户名:服务名`，例如 `user:alice:hermes-gateway-coder`。

最终目标由“配置的锚定目标 + 自动发现目标”合并去重得到。设置 `PROXYSCENE_TG_SERVICES` 会替换默认锚定列表，
但不会关闭精确自动发现。新接管的 Hermes 和 OpenClaw 目标分别记录在独立的持久化 ownership journal 中；
`state.json` 的 `telegram_targets` 只保留用于旧版本 drop-in 的保守迁移。关闭、卸载或崩溃后重试不依赖当前发现结果。

可以通过 `PROXYSCENE_TG_SERVICES` 替换默认锚定目标：

```bash
sudo PROXYSCENE_TG_SERVICES='user:alice:hermes-gateway-coder' proxyscene tg on
sudo PROXYSCENE_TG_SERVICES='user:alice:hermes-gateway-coder' proxyscene tg off
```

关闭 Telegram 服务代理时，程序只删除内容仍与 Hermes journal 匹配的 direct drop-in；对 OpenClaw 则恢复 journal 中记录的精确原值
（包括 absent、null 或字符串）和原容器结构。相关服务 `try-restart` 成功后才释放 ownership；若配置在托管期间被用户
改动，会保留用户的新值；ownership 只在相关服务成功重载该值后释放。清理以 journal 和严格的旧版迁移证据为准，不要求关闭时
重复开启时的 `PROXYSCENE_TG_SERVICES`。

每次 `tg on` 和 `boot-restore` 都会依据 Hermes journal 协调目标。prepared/restoring 阶段会重放未完成的
`daemon-reload` 与 `try-restart`；active 且字节未变化时不做无意义重启。若 reload/restart 失败，journal 保留待重试状态。

## 配置环境变量

### 安装脚本变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PROXYSCENE_VERSION` | 源码副本为 `latest`；Release 资产固定为自身 tag | 目标 immutable Release。显式版本必须是安全的 `v...` tag。 |
| `PROXYSCENE_REPO` | `longlannet/proxyscene` | 提供 Release 元数据和 `checksums.txt` 的 GitHub 仓库 `owner/name`。 |
| `PROXYSCENE_BASE_URL` | 空 | 自定义管理程序归档基址（必须是明确 tag 的 HTTPS 目录）。它不会改变 checksum 来源；归档必须与 GitHub Release 完全一致。 |
| `PROXYSCENE_BUILD_FROM_SOURCE` | `0` | 设为 `1` 时完全跳过 Release 路径，只从当前可信源码 checkout 编译；不会自动启用。 |
| `--offline`（命令行选项） | - | 显式启用离线 bundle；不会因同目录出现二进制而自动启用。要求 root 拥有且组/其他用户不可写的解压路径和完整 manifest。 |
| `GO_VERSION` | `1.27.1` | 显式源码编译所需 Go 版本下限。改为其他版本时必须同时显式设置 `GO_TARBALL_SHA256`，否则 fail closed。 |
| `GO_TARBALL_SHA256` | 空 | Go 安装包 SHA256。默认 Go 1.27.1 留空时使用仓库内审阅的 linux/386、amd64、arm64、armv6l 固定 SHA256 和精确大小；Go 官方不提供可依赖的逐归档 `.sha256` URL。 |
| `SKIP_GO_INSTALL` | `0` | 设为 `1` 时只使用 PATH 中已有且版本合格的 Go。 |
| `FORCE_GO_INSTALL` | `0` | 设为 `1` 时强制在 CoreDir 的 root-only 隐藏临时目录准备指定 Go；提交或回滚时删除，不会替换系统 Go。 |
| `PROXYSCENE_MANAGER_DIR` | `/opt/proxyscene` | 管理器核心目录；必须位于 `/opt`、`/var/lib` 或 `/var/opt` 下的专用目录，不能指向系统目录或用户家目录。 |
| `PROXYSCENE_SWITCH_BIN` | `/usr/local/bin/proxyscene` | 管理程序绝对安装路径；父目录必须可信，basename 必须是 `proxyscene`。 |
| `XRAY_RELEASE_BASE` | 固定 Xray v26.9.9 GitHub Release | 当前固定版本的 HTTPS 归档基址。自定义基址仍必须提供与仓库内置架构 SHA256 一致的字节。 |
| `XRAY_ZIP_URL` | 空 | 自定义当前架构 Xray zip HTTPS URL；必须同时设置 `XRAY_ZIP_SHA256`。 |
| `XRAY_ZIP_SHA256` | 空 | 自定义 Xray zip 的明确 SHA256；格式或内容不匹配即终止。 |
| `SKIP_XRAY_INSTALL` | `0` | 设为 `1` 时仅保留现有 root 所有、组/其他用户不可写、非符号链接且架构匹配的 ELF。 |
| `SKIP_MANAGER_INIT` | `0` | 设为 `1` 时只安装依赖和程序，不调用管理器初始化。 |

示例：

```bash
sudo SKIP_GO_INSTALL=1 bash ./install.sh
sudo PROXYSCENE_MANAGER_DIR=/opt/proxyscene bash ./install.sh
sudo SKIP_MANAGER_INIT=1 bash ./install.sh
sudo PROXYSCENE_VERSION=v1.0.0 \
  PROXYSCENE_BASE_URL=https://mirror.example/proxyscene/v1.0.0 \
  bash ./install.sh
sudo XRAY_RELEASE_BASE=https://mirror.example/xray/v26.9.9 bash ./install.sh
```

安装脚本会拒绝把核心目录设置为 `/etc`、`/usr`、`/home`、`/root`、`/tmp` 等敏感系统路径。入口和每个锁创建函数都会重申 `umask 077`，新目录还使用显式 `mkdir -m 0700`，因此即使调用者原先使用宽松 umask，也不会出现可由普通用户抢先写入的新目录或锁文件窗口。已有非空目录必须带可识别的 `.managed-by-proxyscene` 标记，且所有已有路径祖先必须属于 root、不可由组或其他用户写入、不能是符号链接。安装器把 `installation-ownership.json` 与已验证的管理程序放在同一文件事务中提交或回滚，即使显式 `SKIP_MANAGER_INIT=1` 也会绑定核心目录、管理程序路径和两个 systemd unit locator；后续状态读写、安装和卸载必须与该记录一致。若固定的 `/etc/proxyscene-host-ownership.json` 已存在，安装器会在任何目标文件替换前要求它是 root 所有、`0600`、非符号链接、只有一个严格 JSON 值，并与这四个 locator 精确匹配；损坏或冲突一律 fail closed。

已存在的管理程序只在 ownership 可证明时才可替换：新版安装和自定义路径要求精确匹配四个 locator 的 `installation-ownership.json`；为兼容旧版，历史默认路径 `/usr/local/bin/proxyscene` 还可由核心目录 marker 加上精确匹配该 CoreDir/二进制的旧主 unit 与恢复 unit 共同证明。marker 单独存在不能认领默认二进制，也不能认领任意自定义路径。两个 unit 名称必须位于 `proxyscene`、`proxyscene-*` 或 `proxyscene@*` 命名空间，已有 unit 还必须含 proxyscene ownership marker。Xray 默认从官方固定版本下载，并直接使用仓库审阅过的四架构 SHA256，不信任下载归档旁边的 checksum。源码编译的下载、备份和缓存位于固定 `/run` 事务目录；可执行 Go 工具链单独位于 CoreDir 隐藏临时目录，以兼容 `noexec` 的 `/run`，两者结束时都会删除。

### 运行期变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PROXYSCENE_MANAGER_DIR` | `/opt/proxyscene` | 状态、配置和 Xray 所在目录；必须位于 `/opt`、`/var/lib` 或 `/var/opt` 下的专用目录。 |
| `PROXYSCENE_SWITCH_BIN` | `/usr/local/bin/proxyscene` | 管理程序路径，用于生成开机恢复服务。 |
| `PROXYSCENE_SYSTEMD_SERVICE_NAME` | `proxyscene.service` | Xray 主服务名称。 |
| `PROXYSCENE_BOOT_RESTORE_SERVICE_NAME` | `proxyscene-restore.service` | 开机恢复服务名称。 |
| `PROXYSCENE_SERVICE_USER` | `proxyscene` | Xray 主服务运行用户；默认会自动创建专用系统用户。 |
| `PROXYSCENE_HOST` | `127.0.0.1` | 本地代理监听地址。必须是 IPv4/IPv6 字面量（不支持主机名；IPv6 会自动加方括号）。默认只允许环回地址，非环回需 `PROXYSCENE_ALLOW_PUBLIC_BIND=1`。 |
| `PROXYSCENE_GLOBAL_HTTP_PORT` | `7890` | 全局 HTTP 代理端口。 |
| `PROXYSCENE_DEV_HTTP_PORT` | `7891` | 开发 HTTP 代理端口。 |
| `PROXYSCENE_TG_HTTP_PORT` | `7892` | Telegram HTTP 代理端口。 |
| `PROXYSCENE_TG_SOCKS_PORT` | `7893` | Telegram SOCKS 代理端口。 |
| `PROXYSCENE_GLOBAL_SOCKS_PORT` | `7894` | 全局 SOCKS 代理端口。 |
| `PROXYSCENE_DEV_TARGET_USER` | 空 | 开发代理要修改 git/npm 配置的目标用户。 |
| `PROXYSCENE_TG_SERVICES` | `hermes-gateway user:root:hermes-gateway` | Telegram 代理的手动 systemd 目标服务列表（默认锚定系统级 hermes 网关和 root 用户级 hermes 网关，目标不存在时跳过）；程序还会自动发现 OpenClaw/Hermes 的系统级和用户级网关，用户级服务使用 `user:用户名:服务名`。 |
| `PROXYSCENE_MANAGE_OPENCLAW_CONFIG` | `1` | 设为 `0` 时不接管用户级 OpenClaw 的 `channels.telegram.proxy`；Hermes 注入不受影响。 |
| `PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION` | `0` | 默认拒绝明文 HTTP 订阅；确需导入 HTTP 订阅时设为 `1`，程序会打印风险警告。 |
| `PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION` | `0` | 默认拒绝订阅链接解析到环回/私网/链路本地/CGNAT 等非公网地址（含重定向跳转），以防 SSRF；订阅托管在内网时设为 `1`。 |
| `PROXYSCENE_ALLOW_PUBLIC_BIND` | `0` | 代理监听地址默认只允许环回。本地 HTTP/SOCKS 入站无认证，绑定 `0.0.0.0` 或公网 IP 会形成开放代理；确需对外监听时设为 `1`。 |
| `PROXYSCENE_TEST_URL` | `https://www.google.com/generate_204` | `proxyscene test` 通过全局代理测试连通性时请求的地址；必须是 http(s) URL，可改为在你的网络环境下更可达的目标。 |

监听地址、端口、Xray 服务用户、开发目标用户、Telegram 目标、OpenClaw 接管开关和公开监听开关会在成功的管理操作中写入 `state.json`。后续普通命令会先读取这些值，再应用本次显式提供且有效的环境覆盖；`boot-restore` 和卸载只使用已提交的持久化值。定位目录/二进制/unit 名称仍由 restore unit 保存，订阅安全开关和测试 URL 不持久化。

示例：

```bash
sudo PROXYSCENE_GLOBAL_HTTP_PORT=7898 proxyscene global on
sudo PROXYSCENE_DEV_TARGET_USER=alice proxyscene dev on
sudo PROXYSCENE_TG_SERVICES='user:alice:hermes-gateway-coder' proxyscene tg on
```

## 数据目录

默认核心目录：

```text
/opt/proxyscene
```

常见文件：

| 路径 | 说明 |
| --- | --- |
| `/opt/proxyscene/xray` | Xray 可执行文件。 |
| `/opt/proxyscene/xray-version.txt` | 官方固定源记录版本；自定义归档记录 `custom-sha256:<归档摘要>`；保留现有二进制记录 `existing-sha256:<二进制摘要>`。 |
| `/opt/proxyscene/LICENSE-Xray` | Xray-core 的 MPL-2.0 许可证。 |
| `/opt/proxyscene/SOURCE-Xray` | 固定 Xray ELF 的版本、提交和同一 Release 对应源码归档说明。 |
| `/opt/proxyscene/THIRD_PARTY_LICENSES-Xray` | 固定 Xray ELF 实际链接模块的版本、module sum、许可证和 NOTICE。 |
| `/opt/proxyscene/config.json` | 生成的 Xray 配置。 |
| `/opt/proxyscene/state.json` | 节点、场景、订阅、测速状态、运行配置和旧版 Telegram 目标迁移证据。 |
| `/opt/proxyscene/.state.lock` | 状态文件锁。 |
| `/opt/proxyscene/installation-ownership.json` | 绑定核心目录、管理程序和两个 systemd unit locator 的安装 ownership。 |
| `/opt/proxyscene/dev-proxy-backup.json` | 开发代理 git/npm 配置备份。 |
| `/opt/proxyscene/global-proxy-journal.json` | 两个全局代理系统文件的原值、属主、权限及 managed 内容；另有 `.bak` 和 `.lock`。 |
| `/opt/proxyscene/telegram-proxy-journal.json` | Hermes direct drop-in 的 root-only ownership journal；另有 `.bak` 和 `.lock`。 |
| `/opt/proxyscene/openclaw-proxy-journal.json` | OpenClaw 配置接管的 root-only ownership journal；另有 `.bak` 和 `.lock`。 |
| `/etc/proxyscene-host-ownership.json` | 绑定当前唯一可操作主机共享代理资源的 CoreDir、二进制和 unit locator；完整卸载后删除。 |
| `/run/proxyscene-install.lock` | 安装器和新版状态事务的最外层互斥锁。 |
| `/run/proxyscene-install-tmp/` | root-only 固定事务根；每次的 `transaction.*` 在提交或回滚时删除，空根目录保留复用。 |
| `/run/proxyscene-host-ownership.lock` | 主机共享资源 ownership 的互斥锁；安装器通过已持有 FD 与新管理程序无窗口交接。 |

### 升级与用户身份漂移

现行 Dev backup、Hermes journal 和 OpenClaw journal 的用户记录不只保存用户名，还绑定当时的 uid、主 gid 和 home。
如果账户被删除后以同名重建，或主 gid/home 发生变化，相关开启、关闭、恢复或卸载命令会 fail closed：保留 ownership
记录，不读写新 home，也不 reload/restart 同名新账户的 user service。不要为了绕过报错直接删除 journal 或 `.bak`；
这些文件是恢复原配置和证明文件归属的依据，并且可能包含代理凭据。

首选恢复原 uid/gid/home 后重试。若原身份无法恢复，应先停用相关服务，逐项审计旧 home 的 git/npm 配置、Hermes
drop-in、OpenClaw 配置以及对应 journal/backup，保留 root-only 证据副本，再人工隔离已确认属于旧身份的记录；当前没有
按用户名自动接管新账户的迁移命令。从 v0.7.1 升级时，如果 Dev 场景仍开启，优先用旧版先关闭再升级。旧 Dev backup
和旧 user Telegram target 没有稳定 uid/gid/home 身份，升级后会要求人工核验，不能按同名账户自动恢复或清理。

## systemd 服务

默认会创建两个 systemd 服务：

| 服务 | 说明 |
| --- | --- |
| `proxyscene.service` | Xray 主服务。 |
| `proxyscene-restore.service` | 开机恢复服务，读取保存的场景状态并恢复。 |

开机恢复 Telegram 场景时，Hermes/OpenClaw 只在 ownership journal 尚待协调或托管内容实际变化时重载相关服务。

常用检查命令：

```bash
systemctl status proxyscene.service
systemctl status proxyscene-restore.service
journalctl -u proxyscene.service -e
```

当全部场景关闭时，管理器会停止并禁用 Xray 主服务。当任意场景开启时，管理器会只为已开启场景生成对应监听端口，并启动 Xray 主服务。场景切换失败时会尽量回滚场景状态、代理环境和 Xray 服务配置。

Xray 主服务默认使用专用系统用户 `proxyscene` 运行，并启用 systemd 沙箱选项，包括 `NoNewPrivileges`、`PrivateTmp`、`PrivateDevices`、`ProtectSystem=strict`、`ProtectHome`、`RestrictAddressFamilies` 和最小化 capability 集。程序会把核心目录和 Xray 配置文件调整为该服务用户所属组可读，以便非 root 服务读取配置和数据文件。

## 卸载

程序有卸载命令：

```bash
sudo proxyscene uninstall
```

卸载命令会执行：

1. 关闭 Telegram 服务代理、开发代理、全局代理。
2. 停止并禁用 Xray 主服务。
3. 停止并禁用开机恢复服务。
4. 删除对应 systemd unit 文件。
5. 执行 `systemctl daemon-reload`。
6. 汇总并报告关键失败步骤，避免静默假成功。

卸载命令会保留数据目录（下面是 `PROXYSCENE_MANAGER_DIR` 默认值）：

```text
/opt/proxyscene
```

也会保留管理程序本身（下面是 `PROXYSCENE_SWITCH_BIN` 默认值）：

```text
/usr/local/bin/proxyscene
```

这样做是为了避免误删节点、订阅、状态和已安装的 Xray。如果确认要彻底清理，可以在卸载后手动删除实际的 `PROXYSCENE_SWITCH_BIN` 和 `PROXYSCENE_MANAGER_DIR`；默认安装的命令为：

```bash
sudo rm -f /usr/local/bin/proxyscene
sudo rm -rf /opt/proxyscene
```

卸载会同时读取 Global、Hermes、OpenClaw ownership journal 和旧版状态迁移证据清理已接管目标；不需要重新提供开启时的
`PROXYSCENE_TG_SERVICES`。外部固定路径可能包含已恢复的管理员原文件或无法证明归属的旧文件，不应在“彻底清理”时按路径盲删。
若卸载报告 ownership 恢复或服务重启失败，不要先删除核心目录中的 journal，修复原因后重试。
所有会修改主机状态的命令还会校验固定的主机 ownership 记录，防止两个不同 CoreDir 同时对 `/etc` 和同一用户配置做嵌套接管；只有共享资源和 systemd unit 都成功清理后，卸载才会释放它。

## 手动构建

如果只想构建管理程序，不运行安装脚本：

```bash
cd /opt/proxyscene/proxyscene
go mod tidy
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ./dist/proxyscene ./cmd/proxyscene
```

手动构建只会生成指定输出文件，不会安装依赖、不会安装 Xray、不会写入 systemd 服务。未注入发布元数据的源码构建会报告版本 `dev`；正式 Release 由构建流程注入版本和 commit。仓库默认忽略根目录构建产物 `proxyscene` 和临时构建文件。

## 发布 Release

发布不需要私钥或签名 Secret。首次发布前，仓库管理员必须先在 GitHub Settings 启用 **immutable releases**，并在发起工作流前用具备 Administration 读取权限的账号确认设置仍为 `enabled=true`。GitHub Actions 的 `GITHUB_TOKEN` 没有 Administration 权限，不能可靠读取该仓库设置；工作流会在发布后强制验证 Release 的 `immutable=true`。安装器也会拒绝可变 Release，防止发布后资产和同一份 checksum 被一起替换。

先在目标提交中准备 `docs/releases/vMAJOR.MINOR.PATCH.md`，首行必须是 `# proxyscene vMAJOR.MINOR.PATCH`，写明升级要求、依赖版本和兼容限制。工作流会把这份经过审阅的说明传给发布 job，并在发布后核对公开正文。

之后从 Actions 页面选择 `Release`、分支选择 `main`，输入 `vMAJOR.MINOR.PATCH`；也可以执行：

```bash
gh workflow run Release --ref main -f version=v0.9.0
```

不要预先创建或推送 tag。工作流只接受严格的稳定版本，在固定且仍为当前 `main` 的 commit 上运行模块、格式、普通测试、竞态、静态和漏洞检查，两次四架构构建、产物校验，以及 v0.7.1、v0.8.0 到新版本的 Debian systemd 安装/升级 canary。发布 job 是唯一拥有 `contents: write` 的 job：它先确认目标 tag 和 Release 都不存在，再创建 draft、上传全部构建产物，最后发布并标记为 Latest。随后只读 job 会要求 Release 已 immutable 且为 Latest，比较 GitHub SHA256 digest，重新下载全部资产逐字节比较，并校验 `checksums.txt`。

如果创建 draft、上传资产或发布期间中断，不要直接盲目重跑完整 workflow。先在 GitHub 核对同名 tag、draft/Release 和资产是否存在；确认残留内容及目标 commit 后，人工删除未发布的残留 draft/tag，或仅重跑尚未执行的只读验证。工作流不会自动删除发布对象。

`checksums.txt` 覆盖 `install.sh`、所有管理程序归档、离线 bundle 和固定 Xray 对应源码归档。SHA256 能发现下载损坏和资产不一致，但不是独立发布者签名：若 GitHub 仓库或控制面在 immutable 发布前已被攻陷，攻击者可以同时替换资产和 checksum。高威胁环境应通过独立可信渠道取得已知 checksum。

## 故障排查

### 提示没有可用节点

先导入 HTTPS 订阅或添加节点：

```bash
sudo proxyscene node import --stdin
sudo proxyscene node list
```

默认会拒绝明文 HTTP 订阅。如果必须导入 HTTP 订阅，可以显式开启兼容开关：

```bash
sudo PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION=1 proxyscene node import --stdin
```

### 开启场景失败

查看状态和 systemd 日志：

```bash
sudo proxyscene status
systemctl status proxyscene.service
journalctl -u proxyscene.service -e
```

### 开发代理无法确定目标用户

显式指定用户：

```bash
sudo PROXYSCENE_DEV_TARGET_USER=alice proxyscene dev on
```

### 修改端口后不生效

用环境变量执行一次会修改状态的管理命令。例如：

```bash
sudo PROXYSCENE_GLOBAL_HTTP_PORT=7898 proxyscene global on
```

命令成功后端口会写入 `state.json`，普通后续命令和开机恢复都会继续使用该值，不需要重复传入。若命令失败并完成回滚，旧运行配置仍然有效。

## 当前限制

- 当前全局代理主要通过环境变量和 apt 配置实现，不是完整透明代理。
- 当前节点解析覆盖常见基础链接，复杂客户端私有参数可能需要后续扩展。
- 节点测速是节点地址 TCP 连通性测试，不等同于完整代理链路测速。
- Hermes 通过 systemd 注入 `TELEGRAM_PROXY`；用户级 OpenClaw 则会修改 `openclaw.json` 的
  `channels.telegram.proxy`，因此配置文件的键序和缩进可能规范化，但其他 JSON 值会保留。
- Telegram 接管以“能证明实际运行时会消费所改配置”为前提。Hermes 的 Telegram `NO_PROXY` 绕过、任何有效
  `EnvironmentFile`、无法绑定的 home/profile/project 或会覆盖路由的应用 `.env`，以及 OpenClaw 的非默认配置
  路径/profile/dev/include/env 选择器或账号级 `proxy` 都会中止命令；需先由管理员消除冲突，程序不会猜测或只接管部分账号。
- Hermes 的受管 drop-in 还固定 `PYTHONSAFEPATH=1`，防止 `python -m` 把 `WorkingDirectory` 中的同名模块置于
  已绑定 venv 之前；协调后会同时核对该值、`TELEGRAM_PROXY` 和 `HERMES_TELEGRAM_DISABLE_FALLBACK_IPS=1`。unit、manager、dotenv 或 secret source 中冲突的
  `PYTHONSAFEPATH` 会 fail closed。
- 用户级 systemd 总线未运行或重启失败时，drop-in/OpenClaw journal 会保留，命令会报告部分失败；总线恢复后应重新执行
  开启/关闭命令，让 prepared/restoring 操作完成。
- `boot-restore` 会依据 Hermes/OpenClaw ownership journal 重放未完成的 prepared/restoring 操作；active 且托管内容未变化时不会无意义地 reload/restart 服务。
- 自动发现只接受最终有效的 OpenClaw gateway marker 或 Hermes gateway `ExecStart`。非标准/不可识别的 Hermes 服务需用
  `PROXYSCENE_TG_SERVICES` 显式指定；全局 user-unit 需显式绑定用户。发现过程会解析 canonical/alias、mask、模板实例和
  target-name 对应 drop-in 的有效配置；无法映射到具体用户或无法识别最终启动命令的单元仍需人工指定。
- 开发代理会修改目标用户的 git/npm 配置；关闭时会按备份和本程序写入值进行保守恢复，并支持识别开启期间记录过的多个 managed 代理地址。为避免跨文件或 include 顺序造成不可逆覆盖，双 Git global 文件、非常规配置文件及 `include`/`includeIf` 会失败关闭。

## 开发验证

```bash
cd /opt/proxyscene/proxyscene
gofmt -w ./cmd ./internal
go test ./...
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
bash -n ./install.sh ./scripts/*.sh
shellcheck ./install.sh ./scripts/*.sh
sudo -- bash ./scripts/install-test.sh
```

`scripts/verify-release-artifacts.sh` 读取 `DIST`、`VERSION`、`COMMIT` 和 `SOURCE_DATE_EPOCH`，可校验指定架构或默认四架构的完整 Release 产物。每个架构的 Xray 还会按固定 ELF SHA256、实际 Go 版本、精确源码提交/module sum 和完整依赖清单进行绑定，再扫描全部实际导入包；受影响的导入包会阻断构建，不设置漏洞忽略名单。官方裁剪符号的二进制扫描报告作为保守模块告警清单保留，不能将未导入包直接视为已证明可达。每个 Release 还包含架构无关的 `xray_source_v26.9.9.tar.gz`：它保存精确 Xray commit 及 ELF 中全部 47 个模块的 Go proxy source zip、go.mod、info、module sum 和独立 SHA256；verifier 会把该清单与 bundle 内实际 Xray ELF 逐项比较。`scripts/systemd-integration-test.sh` 会启动 systemd PID 1 的一次性 Debian 容器并执行真实安装/升级/卸载，只能显式设置 `PROXYSCENE_CONTAINER_TEST=1` 后传入当前 amd64 bundle 和已固定 SHA256 的 v0.7.1 或 v0.8.0 amd64 bundle；普通 CI 只检查它的语法和 ShellCheck，不在 runner 或宿主机执行安装，正式 Release 的只读 build job 则把它作为发布前强制门禁。测试按精确容器名和本轮唯一 label 清理容器；固定 Debian 镜像引用只有在运行前不存在、可证明是本轮新拉取时才尝试删除。

## 安全与敏感信息

请不要把以下内容提交到 GitHub issue、pull request、截图或日志中：

- 真实 VLESS、VMess、Trojan、Shadowsocks 节点链接。
- 订阅链接。
- 运行期生成的 `state.json`、`config.json`、`dev-proxy-backup.json`、所有 `*-proxy-journal.json` 及其备份。
- Telegram Bot Token、访问令牌、私钥或其他服务凭据。

运行期状态和构建产物已经在 `.gitignore` 中默认忽略。安全问题报告方式见 `SECURITY.md`。

## 许可证

本项目使用 MIT License，详见 `LICENSE`。manager 依赖的许可证与上游 NOTICE 见
`THIRD_PARTY_LICENSES`；离线 bundle 中 Xray-core 自身的 MPL-2.0 文本为
`LICENSE-Xray`，其实际链接 Go 模块的固定版本、module sum 与许可证见
`THIRD_PARTY_LICENSES-Xray`。构建会从 Xray ELF 重新生成该文件并逐字节核对；依赖漂移时
发布会失败。本程序的生成配置不使用 `geoip.dat` / `geosite.dat`，因此不安装或再分发它们。
