# 发布到 dl.ll.cd

proxyscene 正式发布默认同时包含 GitHub 和 `https://dl.ll.cd/proxyscene`，先完成 GitHub 检查和发布，再发布镜像。镜像保存 GitHub 已公开、稳定且不可变 Release 的原始资产，不重建程序，不改写安装器。发布端继续以 GitHub 验证版本身份和 SHA256；安装及默认自更新客户端信任镜像 HTTPS 和受控发布流程，无需访问 GitHub。镜像不提供独立发布者签名。

## 发布流程

1. 从 `main` 启动正式 `Release`，输入新稳定版本。构建前必须确认仓库变量 `PROXYSCENE_RELEASE_MIRROR_CONFIGURED=true`；缺失或禁用会阻止本次发布。
2. GitHub Actions 完成全部构建、测试、漏洞扫描、双重构建比对和 systemd canary 门禁，然后创建并公开 GitHub immutable Latest。
3. GitHub 发布后验证确认 tag、commit、不可变状态、Latest、正文及全部资产 digest 和下载字节。仅当这些检查全部成功，才自动启动必须执行的镜像 job。
4. 镜像工作流从固定 GitHub 仓库验证 tag、commit、Release ID、不可变状态和精确 11 项资产；下载并核对 GitHub digest、实际大小及 `checksums.txt`。
5. 专用 SSH 接收端执行 `proxyscene-mirror sync vMAJOR.MINOR.PATCH`。服务器独立从 GitHub 拉取并校验相同资产，在私有暂存目录准备完整版本，以不覆盖的原子目录重命名提交；另原子发布 `metadata/<tag>.json`，保存 schema_version=1、版本身份、更新说明和原 11 项资产的大小/SHA256。
6. 工作流从公开 HTTPS 镜像下载全部 11 项文件，与原始 GitHub 下载逐字节比较，并核对公开版本元数据。
7. 仅当选中 tag 仍是 GitHub Latest，才执行 `proxyscene-mirror promote TAG`。服务器重新核对 GitHub Latest、本地完整版本及单调版本顺序，原子更新 `latest.json`；工作流再次验收。

镜像失败时整个发布仍未完成，不会删除或重建已经发布的 GitHub Release；修复后从 `main` 单独运行 `Mirror release`，输入相同 tag 重试。该入口也可归档已发布的旧版本，但不能降低 Latest。已存在的版本只能以完全相同内容幂等重试，不能覆盖或修补公开的半成品目录。格式损坏或不规范的现存 `latest.json` 会阻断更新，不能被当作首次发布。已有索引用于记录本地版本高水位；同一 tag 的身份信息不能改写，新 tag 则独立按 GitHub 身份与资产重新验证。

```bash
gh workflow run 'Mirror release' --ref main -f tag=v0.9.2
```

镜像 job 依赖正式 `Release` 的发布后 `verify` job，不与 GitHub 发布并行。手动同步也要求仓库变量 `PROXYSCENE_RELEASE_MIRROR_CONFIGURED=true`。自动与手动入口使用相同 concurrency group，并由服务器上的文件锁串行化实际写入。

自动发布和手动重试都调用同一个镜像工作流，并显式传递 `MIRROR_SSH_KEY`、`MIRROR_KNOWN_HOSTS` 两个 Secret 名称；实际部署凭据仍从仅允许 main 的 `release-mirror` Environment 取得。

## 一次性部署

在实际提供 `dl.ll.cd` 的服务器上单独配置 `psmirror` 账户。该账户无 sudo 权限、密码锁定；保留 `/bin/sh` 仅供 sshd 执行固定命令。不要复用 linux-temp-admin 的账号或密钥。

| 路径 | 属主与权限 | 用途 |
| --- | --- | --- |
| `/usr/local/libexec/proxyscene-mirror/` | root:root 0755 | 可信接收端代码目录 |
| 其中 `mirror-receiver.py`、`mirror_release.py`、`bootstrap-install.sh` | root:root 0644 | 来自经过测试的同一仓库提交 |
| `/www/wwwroot/dl.ll.cd/proxyscene/` | psmirror:www 0755 | 公开版本及 Latest 索引 |
| `/var/lib/proxyscene-mirror/` | psmirror:psmirror 0700 | 私有暂存、持久发布锁 |
| `/home/psmirror/.ssh/` | psmirror:psmirror 0700 | 专用公钥目录 |
| 其中 `authorized_keys` | psmirror:psmirror 0600 | 唯一受限部署公钥 |

`www` 应替换为站点实际使用的 Web 组。所有父目录由 root 拥有且不可由普通账号写入；代码目录不可由 psmirror 修改。公开目录与私有暂存目录必须位于同一文件系统，并支持 Linux `renameat2(RENAME_NOREPLACE)`。服务器需要 Python 3.10+、curl、OpenSSH，以及到 GitHub API 和公开 Release 资产的 HTTPS 访问。

公钥必须配置以下限制，替换末尾为本项目专用公钥：

```text
restrict,command="/usr/bin/python3 -I /usr/local/libexec/proxyscene-mirror/mirror-receiver.py" ssh-ed25519 <dedicated-proxyscene-public-key>
```

接收端只允许 `proxyscene-mirror sync TAG` 和 `proxyscene-mirror promote TAG`，不接受调用方自定义 URL、路径、文件内容或环境配置。私钥仅放在 GitHub Environment 的部署 Secret；服务器只保存公钥。主机指纹必须经现有可信 SSH 记录或独立管理通道核验，不能在发布时临时 `ssh-keyscan` 后直接信任。

先检查现存账号、目录和配置，禁止以初始化操作覆盖它们。部署脚本应明确核对 root 所有权、文件摘要、同文件系统及专用账户隔离，再把这两个 Python 文件作为同一版本安装；更新时先停用配置门禁，并避免新旧模块混用。

## Web 路由

将 [deploy/nginx/proxyscene.conf](../deploy/nginx/proxyscene.conf) 安装到 dl.ll.cd 现有 HTTPS server 的 include 目录。宝塔常用路径是 `/www/server/panel/vhost/nginx/extension/dl.ll.cd/proxyscene.conf`；须先核对实际站点 include、document root 与 nginx 可执行文件。

该文件只增加 `/proxyscene` 路由，不改其它项目：固定 tag 文件长期 immutable 缓存；`latest.json` 禁缓存；禁止符号链接、目录索引、隐藏文件、非白名单路径和写方法。先执行实际 nginx 的 `-t`，成功后才 reload。保持原站点 TLS 和证书配置。

根目录 `install.sh` 是固定引导入口，Nginx 精确 alias 到 `/usr/local/libexec/proxyscene-mirror/bootstrap-install.sh`（root:root 0644）；发布账号不能修改它。入口文件完整下载后才执行，客户端仅访问 dl.ll.cd。它不写入默认版本号，正常发版只通过 `latest.json` 切换版本；引导逻辑修改经过仓库 CI 后单独原子部署，禁止缓存。首次执行该入口意味着信任镜像域名及服务器。

`metadata/` 是独立的版本清单目录，保持公开版本目录和 GitHub Release 的 11 项原资产不变。升级接收端时先暂停镜像门禁，持有现有 `/var/lib/proxyscene-mirror/.deploy.lock` 的 flock（不能删除或重建锁文件），备份并原子替换相邻模块及引导脚本；释放部署锁后，以 `psmirror` 身份执行现有 Latest 的同 tag `sync`，补齐缺失清单。随后验证 Nginx 配置并 reload。已有版本内容和冲突清单不能覆盖；补齐并完成公开验证后恢复镜像门禁。引导入口支持 v0.11.0 起的安装器；旧版本仍可使用 README 的明确版本安装方式。

公开 `metadata/` 后，旧接收端的目录校验不再兼容新布局。需要撤回固定安装入口时，可以还原 Nginx 入口配置，但应保留新版接收端及其配套模块；不能回退到旧接收端或删除已公开元数据。

## GitHub 配置

使用独立 Environment `release-mirror`，只允许 main 部署。配置环境变量 `MIRROR_HOST`、`MIRROR_PORT`、`MIRROR_USER`；后者为本项目的 `psmirror`。配置环境 Secret `MIRROR_SSH_KEY`、`MIRROR_KNOWN_HOSTS`。按组织策略设置部署保护；不要借用其它项目的发布 Secret。

全部部署检查通过后，最后设置仓库级变量 `PROXYSCENE_RELEASE_MIRROR_CONFIGURED=true`。将其改为 false 或删除变量，会同时阻止新正式发布和手动镜像同步，不会只跳过镜像，也不会改动已有 GitHub Release 或镜像版本文件。镜像工作流只有 contents:read 权限，不执行待镜像 tag 内的代码，所有验证代码来自发起工作流的 main 提交。

## 验证与故障恢复

本地运行：

```bash
python3 -B -m unittest discover -s scripts -p '*mirror*test.py' -v
sudo -- bash scripts/bootstrap-test.sh
```

独立准备和验收（输出目录须不存在）：

```bash
python3 -I scripts/mirror_release.py prepare --tag v0.9.2 \
  --directory /tmp/proxyscene-canonical --record /tmp/proxyscene-release.json
python3 -I scripts/mirror_release.py verify --tag v0.9.2 \
  --directory /tmp/proxyscene-canonical --output /tmp/proxyscene-public --stable
```

验收结果写入 `<output>.result.json`，不混入 11 项资产目录。工作流保留相同结果及版本记录。接收端中断、磁盘不足、现存目录冲突或公开验收失败时，先保留证据并核查错误；不可为了重试删除或覆盖一个已经公开的版本。版本及元数据同步成功而 Latest 未更新时，可重复同版本工作流，已有完整版本会通过幂等检查。

生产接入前运行隔离验收。它需要 Docker 权限、已存在的 Debian 13 镜像，以及容器访问 Debian 软件源和 GitHub；不会自动拉取镜像：

```bash
PROXYSCENE_MIRROR_CONTAINER_TEST=1 \
  PROXYSCENE_MIRROR_TEST_TAG=v0.9.2 \
  bash scripts/mirror-integration-test.sh
```

选择的 tag 必须仍是 GitHub Latest，以便验证 promotion。脚本在单个无宿主端口映射的临时容器中运行真实 SSH forced-command 和 Nginx；测试密钥、临时 TLS 信任和域名映射只存在于容器。它验收完整发布、幂等、并发锁、命令拒绝、原样 HTTPS 校验器及 Web 路由，保留无私钥的证据后清理容器。接收端中断和原子提交故障由 Python 测试注入验证。

普通单元测试及安装器夹具不操作宿主服务。公开镜像与已验证 GitHub bundle 字节相同并不替代目标机器上的真实应用或 Telegram 端到端测试。
