# xray-proxy-go

`xray-proxy-go` 是一个面向 Linux 服务器的 Xray 代理管理器。项目由一个安装脚本和一个 Go 编写的单二进制管理程序组成：

- `install.sh`：负责安装期工作，包括准备 Go 环境、安装 Xray、编译本仓库源码并安装 `xray-proxy`。
- `xray-proxy`：负责运行期管理，包括节点管理、场景开关、Xray 配置生成、systemd 服务管理和开机恢复。

> 当前项目适合在使用 systemd 的 Linux 服务器上运行。大多数管理命令需要 root 权限，建议统一使用 `sudo` 执行。

## 功能特性

- 单二进制 Go 管理程序，安装后命令为 `xray-proxy`。
- 安装脚本从本仓库源码编译管理程序。
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
xray-proxy-go/
├── .gitignore
├── LICENSE
├── SECURITY.md
├── go.mod
├── install.sh
├── README.md
├── cmd/
│   └── xray-proxy/
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
- 可访问网络，用于安装依赖和 Xray。
- Go 1.22 或更高版本。若系统没有可用 Go，安装脚本会自动准备。

## 快速开始

### 1. 进入源码目录

```bash
cd /opt/xray-proxy/xray-proxy-go
```

如果你的仓库在其他位置，请进入实际的 `xray-proxy-go` 目录。

### 2. 安装

不导入节点，只安装环境、编译程序并初始化 systemd：

```bash
sudo bash ./install.sh
```

安装时导入一个节点链接：

```bash
sudo bash ./install.sh 'vless://...'
```

安装完成后，管理程序会安装为：

```text
/usr/local/bin/xray-proxy
```

因此后续可以在任意目录运行：

```bash
sudo xray-proxy
```

### 3. 导入订阅或添加节点

导入订阅链接：

```bash
sudo xray-proxy node import 'https://example.com/subscription'
```

添加单个节点：

```bash
sudo xray-proxy node add 'vless://...' '我的节点'
```

查看节点：

```bash
sudo xray-proxy node list
```

### 4. 开启代理场景

开启全局代理：

```bash
sudo xray-proxy global on
```

开启开发代理：

```bash
sudo xray-proxy dev on
```

开启 Telegram 服务代理：

```bash
sudo xray-proxy tg on
```

查看当前状态：

```bash
sudo xray-proxy status
```

## 安装脚本说明

安装脚本需要在 `xray-proxy-go` 源码目录运行：

```bash
cd /opt/xray-proxy/xray-proxy-go
sudo bash ./install.sh [节点链接]
```

脚本会执行以下步骤：

1. 检查 root 权限。
2. 安装基础依赖：curl、ca-certificates、tar、unzip。
3. 检查 Go 版本，必要时安装 Go。
4. 安装 Xray 到核心目录。
5. 从本仓库源码编译 `cmd/xray-proxy`。
6. 安装管理程序到 `/usr/local/bin/xray-proxy`。
7. 调用 `xray-proxy install` 初始化状态目录和 systemd 服务。

### 安装脚本是否交互式

默认不交互。

- `sudo bash ./install.sh`：非交互安装，不导入节点。
- `sudo bash ./install.sh '节点链接'`：非交互安装，并导入这个节点。
- `sudo xray-proxy install`：交互式初始化，会提示输入一个节点链接，可以留空跳过。
- `sudo xray-proxy install --skip-node`：非交互初始化，不录入节点。

订阅链接不在 `install` 中录入，订阅应通过节点管理导入：

```bash
sudo xray-proxy node import '订阅链接'
```

## 常用命令

### 主菜单

```bash
sudo xray-proxy
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
sudo xray-proxy install
sudo xray-proxy install --skip-node
sudo xray-proxy install '节点链接'
```

### 状态查看

```bash
sudo xray-proxy status
```

### 场景开关

```bash
sudo xray-proxy global on
sudo xray-proxy global off
sudo xray-proxy dev on
sudo xray-proxy dev off
sudo xray-proxy tg on
sudo xray-proxy tg off
```

也可以不带 `on` / `off`，直接切换当前状态：

```bash
sudo xray-proxy global
sudo xray-proxy dev
sudo xray-proxy tg
```

### 节点管理

打开节点管理菜单：

```bash
sudo xray-proxy node
```

查看节点列表：

```bash
sudo xray-proxy node list
```

添加节点：

```bash
sudo xray-proxy node add '节点链接' '备注名'
```

导入订阅：

```bash
sudo xray-proxy node import '订阅链接'
```

节点测速：

```bash
sudo xray-proxy node test
```

自动选择默认节点：

```bash
sudo xray-proxy node auto default
```

按场景选择节点：

```bash
sudo xray-proxy node use '节点ID' default
sudo xray-proxy node use '节点ID' global
sudo xray-proxy node use '节点ID' dev
sudo xray-proxy node use '节点ID' telegram
sudo xray-proxy node use '节点ID' all
```

删除节点：

```bash
sudo xray-proxy node remove '节点ID'
```

修改节点备注：

```bash
sudo xray-proxy node rename '节点ID' '新备注'
```

### 测试代理

```bash
sudo xray-proxy test
```

## 三种代理场景

### 全局代理

命令：

```bash
sudo xray-proxy global on
sudo xray-proxy global off
```

开启后会写入：

- `/etc/profile.d/xray-global-proxy.sh`
- `/etc/apt/apt.conf.d/99xray-global-proxy`

默认监听地址：

```text
HTTP  : 127.0.0.1:7890
SOCKS : 127.0.0.1:7894
```

说明：

- 新登录的 shell 会自动读取 `/etc/profile.d/xray-global-proxy.sh`。
- 当前已打开的 shell 需要重新登录，或手动 source 对应 profile 文件。
- 当前版本的全局代理主要是环境变量和 apt 代理配置，不等同于完整透明代理。

### 开发代理

命令：

```bash
sudo xray-proxy dev on
sudo xray-proxy dev off
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

1. 优先使用环境变量 `DEV_PROXY_TARGET_USER`。
2. 其次使用 `sudo` 调用时的原始用户。
3. 再使用当前进程用户。
4. 最后回退到 `root`。

示例：

```bash
sudo DEV_PROXY_TARGET_USER=alice xray-proxy dev on
sudo DEV_PROXY_TARGET_USER=alice xray-proxy dev off
```

### Telegram 服务代理

命令：

```bash
sudo xray-proxy tg on
sudo xray-proxy tg off
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

最终注入目标会由“默认目标 + 自动发现目标 + `TG_PROXY_SERVICES` 手动目标”合并去重得到。实际注入过的目标会记录在状态文件中，关闭或卸载时会按记录清理，避免自动发现规则变化导致残留。

可以通过 `TG_PROXY_SERVICES` 追加或覆盖特定目标：

```bash
sudo TG_PROXY_SERVICES='openclaw hermes user:root:hermes-gateway' xray-proxy tg on
sudo TG_PROXY_SERVICES='openclaw hermes user:root:hermes-gateway' xray-proxy tg off
```

关闭 Telegram 服务代理时，程序会删除对应环境文件和 systemd drop-in，并对目标服务执行 try-restart，让已运行进程尽快卸载代理环境。

如果开启时自定义了 `TG_PROXY_SERVICES`，关闭或卸载时建议使用同样的变量，确保清理相同服务的 systemd drop-in。

## 配置环境变量

### 安装脚本变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GO_VERSION` | `1.22.12` | 安装脚本准备的 Go 版本。 |
| `GO_TARBALL_SHA256` | 空 | Go 安装包 SHA256。留空时安装脚本会从 go.dev 官方 `.sha256` 文件获取并校验。 |
| `GO_INSTALL_DIR` | `/usr/local` | Go 安装父目录。 |
| `SKIP_GO_INSTALL` | `0` | 设为 `1` 时不安装 Go，要求系统已有 `go` 命令。 |
| `FORCE_GO_INSTALL` | `0` | 设为 `1` 时强制重新准备指定 Go 版本。 |
| `XRAY_PROXY_MANAGER_DIR` | `/opt/xray-proxy-manager` | 管理器核心目录；必须位于 `/opt`、`/var/lib` 或 `/var/opt` 下的专用目录，不能指向系统目录或用户家目录。 |
| `XRAY_PROXY_SWITCH_BIN` | `/usr/local/bin/xray-proxy` | 管理程序安装路径。 |
| `XRAY_GITHUB_RELEASE_BASE` | `https://github.com/XTLS/Xray-core/releases/latest/download` | 默认 Xray 官方发布下载基础地址。 |
| `XRAY_ZIP_URL` | 空 | 自定义 Xray zip 下载地址；留空时使用官方 GitHub Release。使用镜像时建议同时设置 `XRAY_ZIP_SHA256`。 |
| `XRAY_ZIP_SHA256` | 空 | 自定义或默认 Xray zip 的 SHA256；非空时安装脚本会校验。 |
| `SKIP_XRAY_INSTALL` | `0` | 设为 `1` 时跳过 Xray 安装，要求核心目录已有可执行 `xray`。 |
| `SKIP_MANAGER_INIT` | `0` | 设为 `1` 时只安装依赖和程序，不调用管理器初始化。 |

示例：

```bash
sudo SKIP_GO_INSTALL=1 bash ./install.sh
sudo XRAY_PROXY_MANAGER_DIR=/opt/xray-proxy-manager bash ./install.sh
sudo SKIP_MANAGER_INIT=1 bash ./install.sh
```

安装脚本会校验 Go 安装包 SHA256，并会拒绝把核心目录设置为 `/etc`、`/usr`、`/home`、`/root`、`/tmp` 等敏感系统路径。对于已经存在的核心目录，安装脚本不会再无条件修改目录权限；只有新建核心目录时才设置为 `0700`。Xray 默认从官方 GitHub Release 下载；如果使用自定义 `XRAY_ZIP_URL` 或镜像源，建议同时设置 `XRAY_ZIP_SHA256`，避免下载内容被篡改。

### 运行期变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `XRAY_PROXY_MANAGER_DIR` | `/opt/xray-proxy-manager` | 状态、配置和 Xray 所在目录；必须位于 `/opt`、`/var/lib` 或 `/var/opt` 下的专用目录。 |
| `XRAY_PROXY_SWITCH_BIN` | `/usr/local/bin/xray-proxy` | 管理程序路径，用于生成开机恢复服务。 |
| `XRAY_SYSTEMD_SERVICE_NAME` | `xray-proxy-manager.service` | Xray 主服务名称。 |
| `XRAY_PROXY_BOOT_RESTORE_SERVICE_NAME` | `xray-proxy-state.service` | 开机恢复服务名称。 |
| `XRAY_PROXY_SERVICE_USER` | `xray-proxy` | Xray 主服务运行用户；默认会自动创建专用系统用户。 |
| `XRAY_PROXY_HOST` | `127.0.0.1` | 本地代理监听地址。 |
| `XRAY_GLOBAL_HTTP_PORT` | `7890` | 全局 HTTP 代理端口。 |
| `XRAY_DEV_HTTP_PORT` | `7891` | 开发 HTTP 代理端口。 |
| `XRAY_TG_HTTP_PORT` | `7892` | Telegram HTTP 代理端口。 |
| `XRAY_TG_SOCKS_PORT` | `7893` | Telegram SOCKS 代理端口。 |
| `XRAY_GLOBAL_SOCKS_PORT` | `7894` | 全局 SOCKS 代理端口。 |
| `DEV_PROXY_TARGET_USER` | 空 | 开发代理要修改 git/npm 配置的目标用户。 |
| `TG_PROXY_SERVICES` | `openclaw hermes hermes-gateway user:root:hermes-gateway` | Telegram 代理要注入环境变量的手动 systemd 服务列表；程序还会自动发现 OpenClaw/Hermes 系统级和用户级服务，用户级服务使用 `user:用户名:服务名`。 |
| `XRAY_PROXY_ALLOW_HTTP_SUBSCRIPTION` | `0` | 默认拒绝明文 HTTP 订阅；确需导入 HTTP 订阅时设为 `1`，程序会打印风险警告。 |

示例：

```bash
sudo XRAY_GLOBAL_HTTP_PORT=7898 xray-proxy global on
sudo DEV_PROXY_TARGET_USER=alice xray-proxy dev on
sudo TG_PROXY_SERVICES='openclaw hermes user:root:hermes-gateway' xray-proxy tg on
```

## 数据目录

默认核心目录：

```text
/opt/xray-proxy-manager
```

常见文件：

| 路径 | 说明 |
| --- | --- |
| `/opt/xray-proxy-manager/xray` | Xray 可执行文件。 |
| `/opt/xray-proxy-manager/config.json` | 生成的 Xray 配置。 |
| `/opt/xray-proxy-manager/state.json` | 节点、场景、订阅、测速状态和电报代理实际注入目标。 |
| `/opt/xray-proxy-manager/.state.lock` | 状态文件锁。 |
| `/opt/xray-proxy-manager/dev-proxy-backup.json` | 开发代理 git/npm 配置备份。 |

## systemd 服务

默认会创建两个 systemd 服务：

| 服务 | 说明 |
| --- | --- |
| `xray-proxy-manager.service` | Xray 主服务。 |
| `xray-proxy-state.service` | 开机恢复服务，读取保存的场景状态并恢复。 |

常用检查命令：

```bash
systemctl status xray-proxy-manager.service
systemctl status xray-proxy-state.service
journalctl -u xray-proxy-manager.service -e
```

当全部场景关闭时，管理器会停止并禁用 Xray 主服务。当任意场景开启时，管理器会只为已开启场景生成对应监听端口，并启动 Xray 主服务。场景切换失败时会尽量回滚场景状态、代理环境和 Xray 服务配置。

Xray 主服务默认使用专用系统用户 `xray-proxy` 运行，并启用 systemd 沙箱选项，包括 `NoNewPrivileges`、`PrivateTmp`、`PrivateDevices`、`ProtectSystem=strict`、`ProtectHome`、`RestrictAddressFamilies` 和最小化 capability 集。程序会把核心目录和 Xray 配置文件调整为该服务用户所属组可读，以便非 root 服务读取配置和数据文件。

## 卸载

程序有卸载命令：

```bash
sudo xray-proxy uninstall
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
/opt/xray-proxy-manager
```

也会保留管理程序本身：

```text
/usr/local/bin/xray-proxy
```

这样做是为了避免误删节点、订阅、状态和已安装的 Xray。如果确认要彻底清理，可以在卸载后手动删除：

```bash
sudo rm -f /usr/local/bin/xray-proxy
sudo rm -rf /opt/xray-proxy-manager
sudo rm -f /etc/profile.d/xray-global-proxy.sh
sudo rm -f /etc/apt/apt.conf.d/99xray-global-proxy
sudo rm -f /etc/openclaw-hermes-tg-proxy.env
```

如果曾经使用自定义 `TG_PROXY_SERVICES` 开启 Telegram 服务代理，建议卸载时也带上相同变量：

```bash
sudo TG_PROXY_SERVICES='openclaw hermes hermes-gateway user:root:hermes-gateway' xray-proxy uninstall
```

## 手动构建

如果只想构建管理程序，不运行安装脚本：

```bash
cd /opt/xray-proxy/xray-proxy-go
go mod tidy
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ./dist/xray-proxy ./cmd/xray-proxy
```

手动构建只会生成指定输出文件，不会安装依赖、不会安装 Xray、不会写入 systemd 服务。仓库默认忽略根目录构建产物 `xray-proxy` 和临时构建文件。

## 故障排查

### 提示没有可用节点

先导入 HTTPS 订阅或添加节点：

```bash
sudo xray-proxy node import 'https://example.com/subscription'
sudo xray-proxy node list
```

默认会拒绝明文 HTTP 订阅。如果必须导入 HTTP 订阅，可以显式开启兼容开关：

```bash
sudo XRAY_PROXY_ALLOW_HTTP_SUBSCRIPTION=1 xray-proxy node import 'http://example.com/subscription'
```

### 开启场景失败

查看状态和 systemd 日志：

```bash
sudo xray-proxy status
systemctl status xray-proxy-manager.service
journalctl -u xray-proxy-manager.service -e
```

### 开发代理无法确定目标用户

显式指定用户：

```bash
sudo DEV_PROXY_TARGET_USER=alice xray-proxy dev on
```

### 修改端口后不生效

运行期环境变量需要在执行命令时传入。例如：

```bash
sudo XRAY_GLOBAL_HTTP_PORT=7898 xray-proxy global on
```

如果需要长期固定自定义端口，建议在自己的运维脚本中统一传入相同环境变量。

## 当前限制

- 当前全局代理主要通过环境变量和 apt 配置实现，不是完整透明代理。
- 当前节点解析覆盖常见基础链接，复杂客户端私有参数可能需要后续扩展。
- 节点测速是节点地址 TCP 连通性测试，不等同于完整代理链路测速。
- Telegram 服务代理面向 systemd 服务注入环境变量，支持系统级服务和用户级服务，不会自动修改应用自身配置文件。
- 用户级 systemd 服务的总线未运行时，程序会保留 drop-in 配置并打印警告，服务启动后可重新执行开启命令或手动重启服务。
- Telegram 服务代理会自动发现 OpenClaw/Hermes 相关系统级和用户级服务；如果服务名和 unit 内容都不包含 `openclaw` 或 `hermes`，需要用 `TG_PROXY_SERVICES` 手动指定。
- 开发代理会修改目标用户的 git/npm 配置；关闭时会按备份和本程序写入值进行保守恢复，并支持识别开启期间记录过的多个 managed 代理地址。

## 开发验证

```bash
cd /opt/xray-proxy/xray-proxy-go
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
