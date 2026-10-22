# CLIProxyAPI 服务端拉取发布部署

面向 systemd 管理的 CLIProxyAPI 实例的服务端更新器。服务器通过 HTTPS 主动拉取已发布的 GitHub Release，不需要入站 SSH 部署通道，不在服务器上保存部署密钥或 GitHub Token。

安装脚本和 systemd 单元与本目录一起版本化。安装选项和单元名以 `install.sh --help` 为准；本文说明更新器 CLI 和运行契约。

## 概览

| 项目 | 行为 |
| --- | --- |
| 发布来源 | `SCNU-PIONEER/CLIProxyAPI` 的公开 GitHub Release |
| 发布元数据 | GitHub REST Releases API，不使用 Atom feed |
| 可部署版本 | 仅稳定版 `vX.Y.Z`；草稿和预发布版本忽略 |
| Linux 资产 | `CLIProxyAPI_<version>_linux_amd64.tar.gz` 或 `..._aarch64.tar.gz` |
| 构建类型 | 默认 glibc 构建（支持插件）；不会选择 `_no-plugin` |
| 完整性 | Release 中 `checksums.txt` 的 SHA-256，加上 tar 成员校验 |
| 替换范围 | 只替换运行中的 `cli-proxy-api` 二进制 |
| 更新器不会写入 | `config.yaml`、认证目录、应用日志目录、systemd 单元 |
| 失败处理 | 部署失败时自动回滚二进制；若回滚也失败，journal 需要人工恢复。配置快照仅供人工恢复 |
| 服务器凭据 | 无 GitHub Token、无 SSH 密钥、无 API Key |

## 环境要求

- Linux `x86_64`（amd64）或 `aarch64`（arm64）。
- glibc 2.17 或更新版本。默认发布构建是 CGO/glibc，支持插件；可移植的 `_no-plugin` 构建不在部署范围内。
- systemd，并且现有服务（默认 `cliproxyapi.service`）已经运行 `/root/cliproxyapi/cli-proxy-api`。
- 单元必须把 `WorkingDirectory` 设为应用目录（默认 `/root/cliproxyapi`）。缺失或指向别处时更新器会拒绝部署，因为相对 `--config` 和认证路径会解析到不同位置。
- 安装和部署需要 root。
- Python 3.8+，仅使用标准库，不需要 pip 安装依赖。
- 一次性检出需要 `git`；更新器运行时只需要 HTTPS。
- 出站 HTTPS 可访问 `github.com`、`api.github.com`、`objects.githubusercontent.com`。
- 仓库必须保持公开。未认证的 GitHub API 大约每小时每 IP 60 次请求；更新器每次运行只发起少量请求。

现有 `config.yaml`、`/root/.cli-proxy-api` 和日志保持原位。安装脚本不会修改 `cliproxyapi.service`。

## 工作原理

1. 调用 GitHub REST Releases API，只保留正式 Release（`draft=false`、`prerelease=false`、`published_at` 非空），且 tag 必须是稳定版 `vX.Y.Z`。
2. 要求存在对应架构的精确 Linux 资产，并且该 Release 的 `checksums.txt` 中有这个资产。只有 Release、缺少资产或校验文件的，都不算可部署。
3. 下载到 root 专属的暂存目录，校验 SHA-256，然后校验所有 tar 成员。只提取普通文件 `cli-proxy-api`；链接、设备文件、绝对路径和目录穿越都会被拒绝。
4. 在临时目录中执行 `<binary> -h -config /dev/null`，要求打印的版本等于 Release tag。Go 版本横幅在参数解析之前输出，因此不会启动服务，也不会读取真实配置。
5. 对 `config.yaml` 做快照（0600），备份运行中的二进制，写入并 fsync journal，然后用 `os.replace` 原子替换二进制。
6. 重启现有单元。等待健康端点，并等待 systemd 的 `MainPID` 稳定到新二进制；通过 `/proc/<pid>/exe` 及其 SHA-256 校验。
7. 成功后记录 tag、资产摘要、二进制摘要和配置快照路径。重启、健康检查或进程身份校验失败时，恢复旧二进制并再次重启。配置不会自动恢复。

拒绝降级。更新器保留最近 20 个失败 tag；仍在记录中的 tag 在操作员显式传入 `--retry` 前会被跳过。更旧的记录会被淘汰，因此失败已久、记录已淘汰的 Release 可能被定时器再次尝试。跨大版本默认跳过，只有操作员显式传入 `--allow-major` 才执行；定时器永远不会带这个参数。

## 安装

### 固定提交检出

使用经过评审的固定提交。不要从分支 HEAD 安装，也不要使用 `curl ... | bash`。

```bash
sudo git clone https://github.com/SCNU-PIONEER/CLIProxyAPI.git /opt/cliproxyapi-release
sudo git -C /opt/cliproxyapi-release checkout --detach <PINNED_COMMIT>
test "$(sudo git -C /opt/cliproxyapi-release rev-parse HEAD)" = "<PINNED_COMMIT>"
```

把 `<PINNED_COMMIT>` 替换为本次部署提供的、经过评审的提交 SHA。该检出用于安装和恢复；运行中的更新器通过 HTTPS 读取 Release，不依赖这个工作区。该检出必须属于 root，且不能被非特权用户写入，因为 `install.sh` 会以 `sudo` 执行其中的 `cpap_deploy.py`。

### 控制台安装，默认不启用

```bash
cd /opt/cliproxyapi-release
sudo bash deployment/cpap-gateway/install.sh
```

安装脚本会安装 `cpap-deploy` 命令和 systemd 单元；它还会执行源文件和已安装命令的 `--check`、在不存在基线时记录基线，并用 `--dry-run` 预览最新 Release。它不会执行部署。全新主机上定时器保持 disabled；重复安装时，已启用的定时器会保持启用，安装脚本从不会主动禁用它。不带 `--enable` 时，Release 预览失败只是警告，安装脚本仍会成功退出，且不会改变定时器状态。

确认最终状态：

```bash
systemctl is-enabled cpap-deploy.timer   # 全新安装应为 disabled
systemctl status cpap-deploy.timer --no-pager
```

### 首次检查

安装脚本已经执行过这些检查。你仍可在启用自动更新前从控制台重跑：

```bash
sudo cpap-deploy --check          # 主机、单元、二进制、配置、健康检查
sudo cpap-deploy --baseline       # 不存在基线时记录（探测并快照）
sudo cpap-deploy --dry-run        # 显示将要部署的 Release
```

`--baseline` 会在临时目录中运行实际二进制，解析其 `CLIProxyAPI Version: ...` 行。创建基线时，它会记录探测到的版本、二进制 SHA-256；如果存在配置文件，还会记录配置快照。如果已有基线，它不会重复记录。它不使用 `version.txt` 或任何历史版本记录。探测失败时命令会失败，而不是猜测；只有在已经通过其他方式确认版本时，才使用 `--baseline-version vX.Y.Z`。

首次安装建议先手动部署一次，并完成下文“验证与边界”中的操作员检查，然后再启用定时器：

```bash
sudo cpap-deploy --latest
```

这是针对首次切换/兼容性变化的建议，不是每个版本都要人工确认。`--enable` 才是后续自动部署的显式授权。

### 启用后续自动更新

安装器会重新执行预检、在不存在基线时记录基线、预览最新 Release，全部成功后才启用并启动定时器：

```bash
sudo bash deployment/cpap-gateway/install.sh --enable
```

如果定时器原本已启用，即使后续重装步骤失败，它也会保持启用；需要硬停止时请显式禁用。只有在已经自行检查过部署状态时，才使用下面这个高级的直接替代方式：

```bash
sudo systemctl enable --now cpap-deploy.timer
```

不要在未确认预览/状态前直接启用单元。`--enable` 是后续自动部署的显式授权。定时器随后约每五分钟执行一次 `cpap-deploy --latest`（5 分钟加最多 60 秒随机抖动），不会对每个 Release 再次询问。跨大版本仍然必须人工执行并带 `--allow-major`。

暂停自动更新：

```bash
sudo systemctl disable --now cpap-deploy.timer
```

### Releases 页面为空时

仓库当前可能显示 0 个已发布 Release。这只是起始状态，不是永久限制：通过正常发布流程推送第一个稳定 tag（见“发布 Release”），等 Release 和 `checksums.txt` 发布后，再运行 `--dry-run`。

## 命令行

| 模式 | 用途 |
| --- | --- |
| 不带模式 | 打印本地部署状态；不访问网络 |
| `--check` | 校验主机、Python、二进制、配置、认证目录、单元和健康检查 |
| `--baseline` | 记录初始基线（探测、摘要、配置快照）；已有基线时不重复记录 |
| `--dry-run` | 解析并打印部署计划；不下载、不写文件 |
| `--latest` | 部署最新的可部署稳定版 |
| `--tag vX.Y.Z` | 部署指定稳定版；拒绝降级 |
| `--abort-journal` | 仅在人工恢复后丢弃 journal；不触碰二进制 |

常用参数：

```text
--binary PATH          运行中的二进制（默认 /root/cliproxyapi/cli-proxy-api）
--app-dir PATH         应用目录（默认 /root/cliproxyapi）
--config PATH          config.yaml（默认 <app-dir>/config.yaml）
--auth-dir PATH        OAuth/认证目录（默认 /root/.cli-proxy-api）
--unit NAME            systemd 单元（默认 cliproxyapi.service）
--port PORT            /healthz 使用的本地端口（默认 8317）
--health-url URL       覆盖健康检查 URL
--state-dir PATH       更新器状态目录（默认 /root/cliproxyapi.deploy）
--repo OWNER/NAME      发布仓库（默认 SCNU-PIONEER/CLIProxyAPI）
--keep-backups N       保留的二进制备份数（默认 5）
--retry                重试曾经失败的 Release
--allow-major          允许跨大版本（仅手动运行）
--baseline-version TAG 探测不可用时的基线版本
--allow-non-root       仅用于离线预览/测试；不要放进定时器
```

高级/诊断参数：

```text
--github-base URL      GitHub 主页地址；必须正好是 https://github.com
--arch ARCH            覆盖自动识别的架构（amd64/aarch64）
--health-attempts N    健康检查次数（默认 20）
--health-interval SEC  健康检查间隔秒数（默认 2）
--http-timeout SEC     HTTP 超时秒数（默认 20）
--debug                意外错误时打印 traceback
--version              打印更新器版本后退出
```

退出码：

| 退出码 | 含义 |
| --- | --- |
| 0 | 成功或无需操作 |
| 1 | 部署或网络失败 |
| 2 | 配置或用法错误 |
| 3 | 策略拒绝（降级、未显式允许的大版本、仍在记录中的失败 tag 未加 `--retry`） |
| 4 | 另一个运行实例持有部署锁 |
| 5 | 需要操作员处理的不安全状态（journal 不一致、二进制漂移、Release 被改写） |

`--allow-major` 只能与 `--latest` 或 `--tag` 一起使用，定时器命令行不会包含它。

## 自动更新与策略

- 定时器约每五分钟执行一次 `cpap-deploy --latest`（5 分钟加最多 60 秒随机抖动，以安装的单元为准），并使用部署锁，避免并发运行。
- 执行 `--enable` 后，补丁版和小版本会自动部署。
- 大版本由定时器跳过。人工执行：
  `sudo cpap-deploy --tag vX.Y.Z --allow-major`。
- 更新器保留最近最多 20 个失败 tag，并在 `sudo cpap-deploy --latest --retry`（或 `--tag ... --retry`）前跳过仍在记录中的 tag。更旧的失败记录会被淘汰，可能被自动重试；`--retry` 只是立即重试。
- 缺少资产或缺少精确 `checksums.txt` 条目的 Release 会被忽略；更新器不会把“Release 存在”当作“可部署”。
- 如果已部署 tag 的已发布校验值后来发生变化，下一次运行会因完整性错误而 fail-closed。
- 定时器不会降级，也不会替换 systemd 单元实际未启动的二进制（`ExecStart` 必须解析到配置的二进制路径）。

## 回滚与配置恢复

### 二进制自动回滚

如果重启、`/healthz` 或 MainPID/可执行文件摘要校验失败，更新器会恢复交换前创建的备份，重启单元并记录失败。备份保存在运行中二进制旁边，命名为 `cli-proxy-api.bak.<tag>`（默认保留 5 份）。

如果回滚本身也无法让旧二进制恢复健康，journal 会保留（phase 为 `rollback_failed`），命令以退出码 5 结束。此状态需要人工恢复：后续运行（包括 `--retry`）都会拒绝继续，直到服务修复并用 `--abort-journal` 清理 journal。

### 配置不会自动回滚

更新器在每次部署前快照 `config.yaml`，但绝不会自动恢复。这是刻意设计：应用可能在启动时改写配置，其他操作员或管理 API 也可能并发修改；盲目恢复旧快照会丢掉有效变更。

v8 二进制在启动时可能规范化配置，例如把明文管理密钥替换为 bcrypt 哈希，或移除冲突的旧字段。这是应用行为，不是更新器写入。因此即使部署成功，`config.yaml` 也可能已经变化。

如果新二进制失败，需要恢复部署前的配置：

```bash
# 先停止服务：应用是在启动阶段写配置的。
sudo systemctl stop cliproxyapi

# 保留当前文件用于比较/合并。
sudo cp -a /root/cliproxyapi/config.yaml /root/cliproxyapi/config.yaml.before-restore

# 恢复前先查看差异。
sudo diff -u /root/cliproxyapi/config.yaml \
  /root/cliproxyapi.deploy/snapshots/config.<tag>.<stamp>.yaml

# 以严格权限恢复快照。
sudo install -o root -g root -m 0600 \
  /root/cliproxyapi.deploy/snapshots/config.<tag>.<stamp>.yaml \
  /root/cliproxyapi/config.yaml

sudo systemctl start cliproxyapi
```

注意：

- 快照之后通过管理 API 或手工做的修改，在整文件恢复时会丢失。先 diff；有并发修改时手工合并。
- 如果 `config.yaml` 是符号链接，应恢复链接目标，而不是替换链接本身。
- 如果服务不是以 root 运行，恢复时保留原来的 owner/group。
- 恢复后用下文的操作员检查验证；仅 `/healthz` 正常不够。

## 验证与边界

更新器认为部署成功，需要同时满足：

- 单元处于 active；
- `GET /healthz` 返回 HTTP 200，JSON 为 `{"status":"ok"}`；
- systemd 的 `MainPID` 连续两次采样稳定；
- `/proc/<pid>/exe` 指向已安装的二进制，且其 SHA-256 等于暂存二进制的摘要。

这证明的是进程身份和 HTTP 监听，不证明：

- API Key 或访问管理器已加载并可用；
- OAuth/认证凭据文件有效；
- Provider 已注册、已认证或可达；
- 插件已加载；
- 管理 API、面板资产，或其他系统使用的 RESP 用量队列仍然正常；
- 配置与旧版本语义兼容。

`/healthz` 是静态 handler。返回 200 的同时，带鉴权的数据面完全可能不可用。

### 操作员检查（首次升级和任何大版本变化）

用现有凭据手工执行。部署脚本不得读取、保存或传递 API Key。

1. 用现有 Key 调用鉴权后的 models 接口，使用客户端相同的请求头：

   ```bash
   read -r -s -p "Existing API key: " CPA_KEY; echo
   curl -fsS -H "Authorization: Bearer $CPA_KEY" \
     http://127.0.0.1:8317/v1/models >/dev/null && echo "models OK"
   unset CPA_KEY
   ```

2. 通过现有 Provider/客户端路径发一个低成本的请求。
3. 检查门户和集成使用的管理 v0 接口。
4. 检查 RESP 用量队列的消费方（该部署环境历史上涉及 35+ 集成消费方；检查你负责的那部分）。
5. 检查应用日志中的插件加载信息和 `plugins.enabled` 行为。
6. 检查运维使用的管理/面板资产。

首次从当前商业构建切换到本 fork 的 Release 时，即使大版本号没变，也应当作兼容性测试：保留二进制备份和配置快照，观察启动日志，并准备同时恢复两者。如果交换后服务反复重启，更新器应自动回滚二进制；配置仍可能需要按上文手工恢复。

## 发布 Release

维护者使用正常的 GitHub 流程发布；服务器不需要发布或打 tag 权限。

```bash
git tag v8.0.39
git push origin v8.0.39
```

发布会构建 Linux glibc 资产（amd64 和 aarch64）以及 `checksums.txt`。等 GitHub Release 正式发布（不是草稿、不是预发布），并且 `checksums.txt` 列出精确资产后，才可部署。

说明：

- 工作流由任意 tag 触发，但只有稳定版 `vX.Y.Z` 会被部署。临时 tag 产生的 Release 会被更新器忽略。
- Release 资产可能被工作流重跑替换（`--clobber`）。更新器会记录已部署摘要；如果该 tag 的已发布校验值后来变化，会 fail-closed。
- 仓库最初可能没有已发布 Release。启用定时器前先发布一个；这不是永久限制。

## 文件与状态

| 路径 | 用途 |
| --- | --- |
| `/root/cliproxyapi/cli-proxy-api` | 运行中的二进制；唯一被替换的应用文件 |
| `/root/cliproxyapi/cli-proxy-api.bak.<tag>` | 回滚备份（默认保留 5 份） |
| `/root/cliproxyapi/config.yaml` | 应用配置；更新器不写入 |
| `/root/.cli-proxy-api/` | OAuth/认证材料；不触碰 |
| `/root/cliproxyapi/logs/` | 应用日志；不触碰 |
| `/root/cliproxyapi.deploy/deployed.json` | 当前 tag、资产/二进制摘要、快照路径 |
| `/root/cliproxyapi.deploy/last_failure.json` | 最近失败 tag 的有界映射（最多 20 个）及原因 |
| `/root/cliproxyapi.deploy/journal.json` | 进行中的事务；用于恢复 |
| `/root/cliproxyapi.deploy/deploy.log` | 更新器日志（2 MiB 轮转） |
| `/root/cliproxyapi.deploy/snapshots/` | 配置快照，权限 0600 |
| `/root/cliproxyapi.deploy/staging/` | 下载和解压工作区 |
| `cliproxyapi.service` | 现有应用单元；安装脚本不修改 |
| `cpap-deploy.service`、`cpap-deploy.timer` | 安装脚本创建的独立更新器单元 |

## 故障排查

| 现象 | 处理 |
| --- | --- |
| `no published stable releases` | 发布稳定版 `vX.Y.Z`；草稿/预发布会被忽略 |
| Release 存在，但 `--dry-run` 找不到 | 等精确资产和 `checksums.txt` 条目发布；Release 存在不等于可部署 |
| GitHub API 403/429 | 未认证限流；等待后重试。本地没有发生修改 |
| 退出码 3，大版本被拒绝 | 正常。人工执行 `--tag ... --allow-major` |
| 退出码 3，失败 Release 被拒绝 | 先检查失败原因，在记录仍保留时加 `--retry` |
| 退出码 4 | 另一个部署实例持有锁；等待其结束 |
| 退出码 5，存在 journal | 检查 `journal.json` 和服务。若 phase 为 `failed` 或 `rollback_failed`，更新器会拒绝继续，`--retry` 也不能绕过；人工恢复服务后再用 `--abort-journal`。其他 phase 会在下一次运行时自动恢复 |
| 退出码 5，二进制漂移 | 二进制被本工具之外的操作替换。需要有意识地核对；已记录的 baseline 不会被静默覆盖 |
| `ExecStart` 不匹配 | 手工修复单元，或传入正确的 `--binary`；安装脚本不会修改 `cliproxyapi.service` |
| `WorkingDirectory` 缺失或不匹配 | 在单元中把 `WorkingDirectory` 设为应用目录，执行 `systemctl daemon-reload`，再运行 `--check`；安装脚本不会修改 `cliproxyapi.service` |
| 重启后健康检查失败 | 更新器会自动恢复二进制。检查 `deploy.log`、`journalctl -u cliproxyapi`；必要时按上文手工恢复配置快照 |
| 定时器没有运行 | `systemctl status cpap-deploy.timer`；重新执行 `install.sh --enable` |
| 下载时磁盘满 | 清理 `/root/cliproxyapi.deploy`；校验完成前更新器不会触碰运行中的二进制 |

## 安全说明

- 传输信任来自 GitHub HTTPS 和对公开仓库的控制。Release 的 `checksums.txt` 没有签名，不是密码学签名。
- 服务器不安装 GitHub Token、SSH 部署密钥或 API Key。定时器不能发布 Release，也不能推送 tag。
- 解压前会校验所有归档成员，只写入归档中的普通文件 `cli-proxy-api`。
- 配置快照包含与 `config.yaml` 相同的秘密；快照位于 root 专属状态目录，权限 0600。请按敏感文件保护和清理。
- 仓库必须保持公开；私有仓库需要在服务器上保存凭据，不在此设计范围内。
