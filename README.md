# proxyscene

[![CI](https://github.com/longlannet/proxyscene/actions/workflows/ci.yml/badge.svg)](https://github.com/longlannet/proxyscene/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/longlannet/proxyscene)](https://github.com/longlannet/proxyscene/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/longlannet/proxyscene)](go.mod)

`proxyscene` 是一个面向 Linux 服务器的 Xray 代理管理器。项目由一个安装脚本和一个 Go 编写的单二进制管理程序组成：

- `install.sh`：负责安装期工作——安装 Xray、安装 `proxyscene` 管理程序、初始化 systemd。支持离线整合包安装（推荐，解压即装、不联网、不需要 Go）、联机下载预编译二进制（失败回退源码编译）、源码编译三种方式。
- `proxyscene`：负责运行期管理，包括节点管理、场景开关、Xray 配置生成、systemd 服务管理和开机恢复。

> 当前项目适合在使用 systemd 的 Linux 服务器上运行。大多数管理命令需要 root 权限，建议统一使用 `sudo` 执行。

## 功能特性

- 单二进制 Go 管理程序，安装后命令为 `proxyscene`。
- 安装脚本默认下载预编译二进制（带 SHA256 校验，装有 minisign 时默认验证签名），目标机无需安装 Go；下载失败时自动回退到本仓库源码编译。
- 支持离线安装：屏蔽 GitHub / 气隙环境下，下载自包含整合包（含 `install.sh` + 管理程序 + Xray + geo），解压后运行包内 `install.sh` 即可，全程不联网、不需要 Go。
- 通过 GitHub Actions 在打 tag 时自动交叉编译多架构（amd64/arm64/386/armv7）、用 minisign 签名并发布 Release。
- 支持 Xray 主服务和开机恢复服务的 systemd 管理。
- 支持三类代理场景：
  - 全局代理：写入系统 profile 和 apt 代理配置。
  - 开发代理：为目标用户设置 git/npm 代理，并在关闭时恢复。
  - Telegram 服务代理：为指定 systemd 服务注入代理环境。
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
│       └── release.yml     # 打 tag 交叉编译多架构 + 校验和/签名 + 发布 Release
├── .gitignore
├── LICENSE
├── NOTICE              # 离线整合包再分发 Xray(MPL-2.0) 的署名声明
├── SECURITY.md
├── go.mod
├── install.sh
├── README.md
├── cmd/
│   └── proxyscene/
│       └── main.go
└── internal/
    └── manager/
        ├── app.go
        ├── dev.go
        ├── node.go
        ├── scenes.go
        ├── store.go
        ├── systemd.go
        ├── telegram_discovery.go
        ├── types.go
        ├── util.go
        └── xray.go
```

## 系统要求

- Linux。
- systemd。
- root 或 sudo 权限。
- 可用的软件包管理器之一：apt、dnf、yum、apk、zypper。
- 可访问网络，用于安装依赖、Xray 和管理程序。
- Go 1.22 或更高版本**仅在源码编译时需要**（默认走预编译二进制，目标机无需 Go）。若需源码编译且系统没有可用 Go，安装脚本会自动准备。

## 快速开始

**推荐用方式一（离线整合包）安装**：下载一个自包含 `tar.gz`，解压后运行包内的 `install.sh` 即可，**全程不联网、不需要 Go**，最适合屏蔽 GitHub 的网络环境。能直连 GitHub 的机器也可以用方式二一行联机安装。

### 方式一：下载整合包，解压即装（推荐 / 默认）

用 Release 里的**自包含整合包**安装——**解压后直接运行包内的 `install.sh`**，全程不联网、不需要 Go。整合包是一个带顶层目录的 `tar.gz`，内含 `install.sh` + 管理程序 + Xray + `geoip.dat`/`geosite.dat` + `NOTICE`。

1. 在任意能上网的机器上，从 Release 下载对应架构的 `proxyscene_bundle_linux_<arch>.tar.gz`（想验签的话连同 `.minisig` 一起下）。
2. 用任意带外渠道（scp、网盘、U 盘等）把它拷到目标机。
3. 解压并运行包内脚本：

```bash
tar xzf proxyscene_bundle_linux_amd64.tar.gz
cd proxyscene_bundle_linux_amd64
sudo ./install.sh                 # 解压即装
sudo ./install.sh 'vless://...'   # 同时导入节点
```

`install.sh` 检测到同目录的 `proxyscene`/`xray` 二进制，就走离线本地安装。

#### 想要密码学保证？解压前先验签

自包含整合包**无法验证它自身**（脚本和二进制都在包内），所以"解压即装"本质是信任这个 tar 的来源。如果你要确定性的完整性保证，请在**解压前**用随包的 `.minisig` + 公钥（见下文「发布签名公钥」）验证整个 tar：

```bash
minisign -Vm proxyscene_bundle_linux_amd64.tar.gz \
  -x proxyscene_bundle_linux_amd64.tar.gz.minisig \
  -P RWSwCDZeUKUXxnGQfkQwePkJyg1uKh7LcKXgia4Lto4MeC6lKStdotYb
# 验签通过后再 tar xzf 解压、运行 install.sh
```

### 方式二：一行联机安装（能直连 GitHub 时）

> 前提：仓库已发布对应架构的 Release。一行命令通过管道运行时拿不到源码，**无法回退到源码编译**，发布 Release 之前请用方式三从源码安装。

```bash
curl -fsSL https://raw.githubusercontent.com/longlannet/proxyscene/main/install.sh | sudo bash
```

安装时导入一个节点链接：

```bash
curl -fsSL https://raw.githubusercontent.com/longlannet/proxyscene/main/install.sh | sudo bash -s -- 'vless://...'
```

脚本内置了发布公钥：**只要目标机装了 `minisign`，默认就会验证 `checksums.txt.minisig` 签名**（best-effort），并始终校验二进制 SHA256。想把签名校验设为**强制**（缺 minisign 或验签失败即中止），显式传入公钥并可固定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/longlannet/proxyscene/main/install.sh \
  | sudo PROXYSCENE_VERSION=v0.4.0 \
         PROXYSCENE_MINISIGN_PUBKEY=RWSwCDZeUKUXxnGQfkQwePkJyg1uKh7LcKXgia4Lto4MeC6lKStdotYb \
         bash
```

> 安全提示：`curl | sudo bash` 适合全新自管 VPS。生产环境建议先下载审阅脚本，或固定 `PROXYSCENE_VERSION=vX.Y.Z`；使用镜像 `PROXYSCENE_BASE_URL` 时只用你信任的 HTTPS 源。

#### 发布签名公钥

本项目 Release 的 `checksums.txt` 与各离线整合包均由以下 minisign 公钥签名：

```text
RWSwCDZeUKUXxnGQfkQwePkJyg1uKh7LcKXgia4Lto4MeC6lKStdotYb
```

也可手动校验已下载的产物：

```bash
minisign -Vm checksums.txt -x checksums.txt.minisig \
  -P RWSwCDZeUKUXxnGQfkQwePkJyg1uKh7LcKXgia4Lto4MeC6lKStdotYb
sha256sum -c checksums.txt
```

### 方式三：从源码安装

进入仓库目录后执行（缺 Go 会自动准备；如想强制源码编译，设 `PROXYSCENE_BUILD_FROM_SOURCE=1`）：

```bash
sudo bash ./install.sh
sudo bash ./install.sh 'vless://...'        # 同时导入节点
sudo PROXYSCENE_BUILD_FROM_SOURCE=1 bash ./install.sh
```

安装完成后，管理程序会安装为：

```text
/usr/local/bin/proxyscene
```

因此后续可以在任意目录运行：

```bash
sudo proxyscene
```

### 安装后：导入订阅或添加节点

导入订阅链接：

```bash
sudo proxyscene node import 'https://example.com/subscription'
```

添加单个节点：

```bash
sudo proxyscene node add 'vless://...' '我的节点'
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

`install.sh` 支持三种用法（见上文「快速开始」）：离线整合包安装（默认推荐）、联机一行安装（下载预编译二进制）、源码目录安装。下面以联机安装为例说明各步骤；离线安装会跳过其中的下载与编译。

脚本会执行以下步骤：

1. 检查 root 权限。
2. 安装基础依赖：curl、ca-certificates、tar、unzip。
3. 安装 Xray 到核心目录。
4. 安装管理程序 `proxyscene` 到 `/usr/local/bin/`：
   - 默认下载对应架构的预编译二进制（`proxyscene_linux_<arch>.tar.gz`），用 `checksums.txt` 校验 SHA256；脚本内置发布公钥，装了 `minisign` 就默认验证签名（显式设 `PROXYSCENE_MINISIGN_PUBKEY` 则强制验签）。目标机无需 Go。
   - 下载失败，或设置 `PROXYSCENE_BUILD_FROM_SOURCE=1` 时，检查/准备 Go 并从本仓库源码编译 `cmd/proxyscene`（仅在源码目录可行）。
5. 调用 `proxyscene install` 初始化状态目录和 systemd 服务。

> 离线安装（见上文「方式一」）：在解压后的整合包目录里运行包内的 `install.sh`，脚本检测到同目录的二进制即跳过第 2-4 步的下载与编译，直接安装包内的管理程序与 Xray，再做第 5 步初始化。
> 运行 `proxyscene version` 可查看已安装的版本与 commit（预编译二进制会在构建时注入 git tag 与 commit）。

### 安装脚本是否交互式

默认不交互。

- `sudo bash ./install.sh`：非交互安装，不导入节点。
- `sudo bash ./install.sh '节点链接'`：非交互安装，并导入这个节点。
- `sudo proxyscene install`：交互式初始化，会提示输入一个节点链接，可以留空跳过。
- `sudo proxyscene install --skip-node`：非交互初始化，不录入节点。

订阅链接不在 `install` 中录入，订阅应通过节点管理导入：

```bash
sudo proxyscene node import '订阅链接'
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
sudo proxyscene install '节点链接'
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
sudo proxyscene node add '节点链接' '备注名'
```

导入订阅：

```bash
sudo proxyscene node import '订阅链接'
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

程序会备份原始配置，并记录本程序写入过的开发代理地址；如果开启期间调整了开发代理端口，关闭时也会识别并清理这些已记录的 managed 值，尽量避免误删用户手工配置。

默认监听地址：

```text
HTTP: 127.0.0.1:7891
```

目标用户选择规则：

1. 优先使用环境变量 `PROXYSCENE_DEV_TARGET_USER`。
2. 其次使用 `sudo` 调用时的原始用户。
3. 再使用当前进程用户。
4. 最后回退到 `root`。

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

开启后会写入：

- `/etc/openclaw-hermes-tg-proxy.env`
- `/etc/systemd/system/<service>.service.d/10-openclaw-hermes-telegram-proxy.conf`
- `<用户家目录>/.config/systemd/user/<service>.service.d/10-openclaw-hermes-telegram-proxy.conf`

默认监听地址：

```text
HTTP  : 127.0.0.1:7892
SOCKS : 127.0.0.1:7893
```

默认目标服务：

```text
openclaw hermes hermes-gateway user:root:hermes-gateway
```

同时，程序会自动发现系统级和用户级 OpenClaw/Hermes 相关服务：

- 系统级目录：`/etc/systemd/system`、`/lib/systemd/system`、`/usr/lib/systemd/system`。
- 用户级目录：所有本地用户的 `.config/systemd/user`。
- 匹配关键词：`openclaw`、`hermes`。
- 自动发现会跳过符号链接，限制读取单个 unit 文件的内容大小，并优先按服务名以及有限的 systemd 字段匹配，降低误匹配和读取异常风险。

目标服务支持两种写法：

- 系统级 systemd 服务：`openclaw`、`hermes`、`hermes-gateway`。
- 用户级 systemd 服务：`user:用户名:服务名`，例如 `user:root:hermes-gateway`。

最终注入目标会由“默认目标 + 自动发现目标 + `PROXYSCENE_TG_SERVICES` 手动目标”合并去重得到。实际注入过的目标会记录在状态文件中，关闭或卸载时会按记录清理，避免自动发现规则变化导致残留。

可以通过 `PROXYSCENE_TG_SERVICES` 追加或覆盖特定目标：

```bash
sudo PROXYSCENE_TG_SERVICES='openclaw hermes user:root:hermes-gateway' proxyscene tg on
sudo PROXYSCENE_TG_SERVICES='openclaw hermes user:root:hermes-gateway' proxyscene tg off
```

关闭 Telegram 服务代理时，程序会删除对应环境文件和 systemd drop-in，并对目标服务执行 try-restart，让已运行进程尽快卸载代理环境。

如果开启时自定义了 `PROXYSCENE_TG_SERVICES`，关闭或卸载时建议使用同样的变量，确保清理相同服务的 systemd drop-in。

## 配置环境变量

### 安装脚本变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PROXYSCENE_VERSION` | `latest` | 要下载的预编译管理程序版本，例如 `v0.4.0`。 |
| `PROXYSCENE_REPO` | `longlannet/proxyscene` | 预编译二进制所在的 GitHub 仓库 `owner/name`。 |
| `PROXYSCENE_BASE_URL` | 空 | 自定义预编译下载基址（必须 `https`），优先级高于 `PROXYSCENE_VERSION`/仓库默认地址。 |
| `PROXYSCENE_MINISIGN_PUBKEY` | 内置发布公钥 | 默认用内置公钥验签（装有 minisign 时 best-effort，缺则跳过并告警、仅校验 SHA256）。显式设置本变量会把验签变为**强制**：缺 minisign 或验签失败即中止。 |
| `PROXYSCENE_BUILD_FROM_SOURCE` | `0` | 设为 `1` 时跳过预编译下载，强制本地源码编译（需要 Go）。 |
| `--offline`（命令行选项） | — | 强制走离线本地安装，要求在解压后的整合包目录内运行（同目录有 `proxyscene`/`xray`）；通常无需显式指定，脚本会自动检测。 |
| `GO_VERSION` | `1.22.12` | 源码编译时准备的 Go 版本（仅回退编译时用到）。 |
| `GO_TARBALL_SHA256` | 空 | Go 安装包 SHA256。留空时安装脚本会从 go.dev 官方 `.sha256` 文件获取并校验。 |
| `GO_INSTALL_DIR` | `/usr/local` | Go 安装父目录。 |
| `SKIP_GO_INSTALL` | `0` | 设为 `1` 时不安装 Go，要求系统已有 `go` 命令。 |
| `FORCE_GO_INSTALL` | `0` | 设为 `1` 时强制重新准备指定 Go 版本。 |
| `PROXYSCENE_MANAGER_DIR` | `/opt/proxyscene` | 管理器核心目录；必须位于 `/opt`、`/var/lib` 或 `/var/opt` 下的专用目录，不能指向系统目录或用户家目录。 |
| `PROXYSCENE_SWITCH_BIN` | `/usr/local/bin/proxyscene` | 管理程序安装路径。 |
| `XRAY_DOWNLOAD_SOURCE` | `official` | Xray 预设下载源；可选 `official`（官方 GitHub Release）或 `xxv`（`xxv.cc` 镜像）。 |
| `XRAY_GITHUB_RELEASE_BASE` | `https://github.com/XTLS/Xray-core/releases/latest/download` | 官方 Xray 发布下载基础地址。 |
| `XRAY_XXV_ZIP_URL` | `https://xxv.cc/7c9fxLN4nm4BFU8fjD.zip` | `xxv.cc` Xray zip 镜像地址。 |
| `XRAY_ZIP_URL` | 空 | 自定义 Xray zip 下载地址；非空时优先级高于 `XRAY_DOWNLOAD_SOURCE`。使用自定义地址时建议同时设置 `XRAY_ZIP_SHA256`。 |
| `XRAY_ZIP_SHA256` | 空 | Xray zip 的 SHA256；非空时安装脚本会校验。使用官方源时留空，安装脚本会自动拉取官方 `.dgst` 校验文件并校验；使用 `xxv` 或自定义下载源时建议显式设置。 |
| `SKIP_XRAY_INSTALL` | `0` | 设为 `1` 时跳过 Xray 安装，要求核心目录已有可执行 `xray`。 |
| `SKIP_MANAGER_INIT` | `0` | 设为 `1` 时只安装依赖和程序，不调用管理器初始化。 |

示例：

```bash
sudo SKIP_GO_INSTALL=1 bash ./install.sh
sudo PROXYSCENE_MANAGER_DIR=/opt/proxyscene bash ./install.sh
sudo SKIP_MANAGER_INIT=1 bash ./install.sh
sudo XRAY_DOWNLOAD_SOURCE=official bash ./install.sh
sudo XRAY_DOWNLOAD_SOURCE=xxv bash ./install.sh
```

安装脚本会校验 Go 安装包 SHA256，并会拒绝把核心目录设置为 `/etc`、`/usr`、`/home`、`/root`、`/tmp` 等敏感系统路径。对于已经存在的核心目录，安装脚本不会再无条件修改目录权限；只有新建核心目录时才设置为 `0700`。Xray 默认从官方 GitHub Release 下载，并会自动拉取同目录的官方 `.dgst` 校验文件校验 SHA256；如果需要使用 `xxv.cc` 镜像，可以设置 `XRAY_DOWNLOAD_SOURCE=xxv`。使用 `xxv` 或自定义 `XRAY_ZIP_URL` 下载源时，官方 `.dgst` 不可用，建议显式设置 `XRAY_ZIP_SHA256`，避免下载内容被篡改。

### 运行期变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PROXYSCENE_MANAGER_DIR` | `/opt/proxyscene` | 状态、配置和 Xray 所在目录；必须位于 `/opt`、`/var/lib` 或 `/var/opt` 下的专用目录。 |
| `PROXYSCENE_SWITCH_BIN` | `/usr/local/bin/proxyscene` | 管理程序路径，用于生成开机恢复服务。 |
| `PROXYSCENE_SYSTEMD_SERVICE_NAME` | `proxyscene.service` | Xray 主服务名称。 |
| `PROXYSCENE_BOOT_RESTORE_SERVICE_NAME` | `proxyscene-restore.service` | 开机恢复服务名称。 |
| `PROXYSCENE_SERVICE_USER` | `proxyscene` | Xray 主服务运行用户；默认会自动创建专用系统用户。 |
| `PROXYSCENE_HOST` | `127.0.0.1` | 本地代理监听地址。 |
| `PROXYSCENE_GLOBAL_HTTP_PORT` | `7890` | 全局 HTTP 代理端口。 |
| `PROXYSCENE_DEV_HTTP_PORT` | `7891` | 开发 HTTP 代理端口。 |
| `PROXYSCENE_TG_HTTP_PORT` | `7892` | Telegram HTTP 代理端口。 |
| `PROXYSCENE_TG_SOCKS_PORT` | `7893` | Telegram SOCKS 代理端口。 |
| `PROXYSCENE_GLOBAL_SOCKS_PORT` | `7894` | 全局 SOCKS 代理端口。 |
| `PROXYSCENE_DEV_TARGET_USER` | 空 | 开发代理要修改 git/npm 配置的目标用户。 |
| `PROXYSCENE_TG_SERVICES` | `openclaw hermes hermes-gateway user:root:hermes-gateway` | Telegram 代理要注入环境变量的手动 systemd 服务列表；程序还会自动发现 OpenClaw/Hermes 系统级和用户级服务，用户级服务使用 `user:用户名:服务名`。 |
| `PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION` | `0` | 默认拒绝明文 HTTP 订阅；确需导入 HTTP 订阅时设为 `1`，程序会打印风险警告。 |
| `PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION` | `0` | 默认拒绝订阅链接解析到环回/私网/链路本地/CGNAT 等非公网地址（含重定向跳转），以防 SSRF；订阅托管在内网时设为 `1`。 |
| `PROXYSCENE_ALLOW_PUBLIC_BIND` | `0` | 代理监听地址默认只允许环回。本地 HTTP/SOCKS 入站无认证，绑定 `0.0.0.0` 或公网 IP 会形成开放代理；确需对外监听时设为 `1`。 |
| `PROXYSCENE_TEST_URL` | `https://www.google.com/generate_204` | `proxyscene test` 通过全局代理测试连通性时请求的地址；必须是 http(s) URL，可改为在你的网络环境下更可达的目标。 |

示例：

```bash
sudo PROXYSCENE_GLOBAL_HTTP_PORT=7898 proxyscene global on
sudo PROXYSCENE_DEV_TARGET_USER=alice proxyscene dev on
sudo PROXYSCENE_TG_SERVICES='openclaw hermes user:root:hermes-gateway' proxyscene tg on
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
| `/opt/proxyscene/config.json` | 生成的 Xray 配置。 |
| `/opt/proxyscene/state.json` | 节点、场景、订阅、测速状态和电报代理实际注入目标。 |
| `/opt/proxyscene/.state.lock` | 状态文件锁。 |
| `/opt/proxyscene/dev-proxy-backup.json` | 开发代理 git/npm 配置备份。 |

## systemd 服务

默认会创建两个 systemd 服务：

| 服务 | 说明 |
| --- | --- |
| `proxyscene.service` | Xray 主服务。 |
| `proxyscene-restore.service` | 开机恢复服务，读取保存的场景状态并恢复。 |

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

卸载命令会保留数据目录：

```text
/opt/proxyscene
```

也会保留管理程序本身：

```text
/usr/local/bin/proxyscene
```

这样做是为了避免误删节点、订阅、状态和已安装的 Xray。如果确认要彻底清理，可以在卸载后手动删除：

```bash
sudo rm -f /usr/local/bin/proxyscene
sudo rm -rf /opt/proxyscene
sudo rm -f /etc/profile.d/proxyscene-global-proxy.sh
sudo rm -f /etc/apt/apt.conf.d/99proxyscene-global-proxy
sudo rm -f /etc/openclaw-hermes-tg-proxy.env
```

如果曾经使用自定义 `PROXYSCENE_TG_SERVICES` 开启 Telegram 服务代理，建议卸载时也带上相同变量：

```bash
sudo PROXYSCENE_TG_SERVICES='openclaw hermes hermes-gateway user:root:hermes-gateway' proxyscene uninstall
```

## 手动构建

如果只想构建管理程序，不运行安装脚本：

```bash
cd /opt/proxyscene/proxyscene
go mod tidy
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ./dist/proxyscene ./cmd/proxyscene
```

手动构建只会生成指定输出文件，不会安装依赖、不会安装 Xray、不会写入 systemd 服务。仓库默认忽略根目录构建产物 `proxyscene` 和临时构建文件。

## 故障排查

### 提示没有可用节点

先导入 HTTPS 订阅或添加节点：

```bash
sudo proxyscene node import 'https://example.com/subscription'
sudo proxyscene node list
```

默认会拒绝明文 HTTP 订阅。如果必须导入 HTTP 订阅，可以显式开启兼容开关：

```bash
sudo PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION=1 proxyscene node import 'http://example.com/subscription'
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

运行期环境变量需要在执行命令时传入。例如：

```bash
sudo PROXYSCENE_GLOBAL_HTTP_PORT=7898 proxyscene global on
```

如果需要长期固定自定义端口，建议在自己的运维脚本中统一传入相同环境变量。

## 当前限制

- 当前全局代理主要通过环境变量和 apt 配置实现，不是完整透明代理。
- 当前节点解析覆盖常见基础链接，复杂客户端私有参数可能需要后续扩展。
- 节点测速是节点地址 TCP 连通性测试，不等同于完整代理链路测速。
- Telegram 服务代理面向 systemd 服务注入环境变量，支持系统级服务和用户级服务，不会自动修改应用自身配置文件。
- 用户级 systemd 服务的总线未运行时，程序会保留 drop-in 配置并打印警告，服务启动后可重新执行开启命令或手动重启服务。
- Telegram 服务代理会自动发现 OpenClaw/Hermes 相关系统级和用户级服务；如果服务名和 unit 内容都不包含 `openclaw` 或 `hermes`，需要用 `PROXYSCENE_TG_SERVICES` 手动指定。
- 开发代理会修改目标用户的 git/npm 配置；关闭时会按备份和本程序写入值进行保守恢复，并支持识别开启期间记录过的多个 managed 代理地址。

## 开发验证

```bash
cd /opt/proxyscene/proxyscene
gofmt -w ./cmd ./internal
go test ./...
go vet ./...
bash -n ./install.sh
```

## 安全与敏感信息

请不要把以下内容提交到 GitHub issue、pull request、截图或日志中：

- 真实 VLESS、VMess、Trojan、Shadowsocks 节点链接。
- 订阅链接。
- 运行期生成的 `state.json`、`config.json`、`dev-proxy-backup.json`。
- Telegram Bot Token、访问令牌、私钥或其他服务凭据。

运行期状态和构建产物已经在 `.gitignore` 中默认忽略。安全问题报告方式见 `SECURITY.md`。

## 许可证

本项目使用 MIT License，详见 `LICENSE`。
