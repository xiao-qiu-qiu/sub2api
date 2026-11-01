# 登录 Worker 随主程序自动管理

Linux amd64 / arm64 的内置管理器已经支持自动下载、校验、启动与重试。主程序升级并重启后，按构建时选定的运行包版本使用对应的 owner Release 运行包；旧版本目录仍保留，2FA 恢复日志放在版本目录外。服务器不编译应用，也不安装 Python/Node 依赖。

旧部署若设置了 `OPENAI_REAUTH_WORKER_TOKEN`，会保留外部 Worker 模式。单独升级主程序不会接管这个 systemd 服务，可能出现主程序已有 2FA 功能、外部 Worker 仍是旧版的情况。

## 自定义构建的运行包版本

展示版本与托管 Worker 运行包版本分别设置：

- `main.Version` 用于应用版本展示，可以通过 `-X main.Version=...` 添加自定义构建后缀。
- `main.ReauthRuntimeVersion` 用于选择 Worker 运行包，缺省读取编译时内嵌的 `backend/cmd/server/VERSION`，通过 `BuildInfo.ReauthRuntimeVersion` 传给运行包管理器。单独覆盖展示版本不会改变运行包选择。
- 需要固定其他运行包版本时，构建参数显式指定 `-X main.ReauthRuntimeVersion=...`。该值必须对应 owner 已发布且包含目标架构运行包及 SHA-256 摘要的 Release，并与当前主程序的 Worker 协议兼容；自定义展示版本本身不代表存在同名运行包。

例如，在 `backend` 目录执行以下构建；`1.2.3` 仅为示例，应替换为实际可用的运行包版本：

```bash
go build -ldflags "-X main.Version=1.2.3-custom -X main.ReauthRuntimeVersion=1.2.3" \
  -o sub2api ./cmd/server
```

省略 `-X main.ReauthRuntimeVersion=...` 时仍使用内嵌 `VERSION`，不从 `1.2.3-custom` 这样的展示版本推导。正式发布时应确保内嵌版本与配套运行包一致；自定义构建若修改了 Worker 协议，也需提供兼容运行包并显式选择其版本。这里的版本规则适用于托管 Worker；外部 Worker 由其部署方式单独升级和验收。

三个版本输入都会去除首尾空白；仅包含空白的覆盖值按未设置处理。内嵌 `VERSION` 为空时，未覆盖的展示版本与运行包版本各自回退为 `0.0.0-dev`。这是开发构建的缺省值，不保证存在对应 Release 运行包；使用托管 Worker 时需选定实际已发布的兼容版本。

构建后可通过 `./sub2api -version` 查看展示版本，以及单独输出的 `Re-login runtime version: ...`。运行中的托管 Worker 状态通过 `worker.status.version` 标明所选运行包版本；管理器会去掉版本开头的 `v`。该字段表示选定版本，须结合状态和实际通信判断 Worker 是否已就绪。

## 应用与 Worker 分别验收

应用进程启动、API 健康检查成功或容器显示 `healthy`，只证明相应的应用健康检查通过。Worker 下载、校验或启动失败时，应用仍可能正常提供其他功能，必须单独检查：

1. 应用：确认预期构建已运行，API 健康检查通过。
2. 托管 Worker：在已触发 Worker 准备的实例中，确认所选运行包版本、下载与校验结果、托管子进程存活，并确认该 API 收到成功鉴权的 `totp-claim`，验证领取接口与持久恢复存储可用。外部模式则检查对应外部 Worker 服务与通信。
3. 自动重登：在允许执行真实登录的验收场景中，单独核实任务已创建、被 Worker 领取并完成，凭据成功写回；涉及 2FA 时还需核实相应恢复状态。Worker 存活或领取成功本身不证明真实登录成功。

## 将标准旧 sidecar 一次迁入自动管理

适用于已有 `sub2api-openai-reauth-worker.service`、只使用密码/TOTP 的二进制安装。自定义 Turb 邮箱协议需继续使用外部 Worker；迁移脚本会拒绝检测到该协议的进程。迁移前确认没有其他机器或实例共用这个外部 Worker。

1. 暂停凭证运营自动重登及新的人工登录/2FA 提交，等待所有排队、运行中的登录任务结束；处理所有非终态 2FA 任务。记下原开关，迁移后恢复。`--drained` 是运维者对这个条件的明确声明，脚本不能凭进程在线状态推断队列已经排空。
2. 核实主程序为 owner 正式 Release、API 的实际持久数据目录、服务用户和磁盘余量。需要 root、systemd、Python 3.10+ 及可访问 GitHub；Python 仅用标准库执行迁移工具。脚本不会读取或修改数据库。
3. 使用包含本工具的安装包执行一次：

   ```bash
   sudo bash /opt/sub2api/install.sh manage-worker \
     --data-dir /opt/sub2api/data --drained
   ```

   如果安装包没有包含 `install.sh`，直接运行同包中的脚本：

   ```bash
   sudo python3 /opt/sub2api/migrate-reauth-managed.py \
     --data-dir /opt/sub2api/data --drained
   ```

迁移前先下载与当前应用相同版本/架构的官方运行包，要求 Release asset SHA-256，拒绝缺摘要、篡改、越界路径、符号链接和特殊文件；完成 `--check` 后才停止旧服务。该检查不领取任务，不调用真实登录或更换接口。

脚本持有 `/run/lock/sub2api-deploy.lock`，切换前复核 API/Worker PID，将原恢复目录及其 key 完整复制到 `<DATA_DIR>/credential-worker/totp-recovery` 并限制权限；原目录不删除。存在目标恢复目录、目标运行包或迁移 drop-in 时拒绝覆盖，必须先核对上次迁移结果。恢复目录来自旧 Worker 的进程环境，缺省为 `/var/lib/sub2api-reauth/totp-recovery`。

停用旧 sidecar 的开机启动，写入 `90-managed-reauth.conf`，使用 systemd 的 `UnsetEnvironment` 在最终进程环境中去掉旧共享 Token，无须编辑或输出已有密钥文件。原有 Worker 并发启动值会保留，页面保存过的并发值继续优先。

**迁移会重启一次主 API。** 持有凭证运营记录的实例启动时会准备 Worker；验收要求主 API 的托管子进程存活，且该 API 收到一次成功鉴权的 `totp-claim`，证明持久恢复存储可用。原配置、旧运行包、原恢复目录和迁移记录均保留；失败自动撤除本次 drop-in、重启原模式并恢复旧 sidecar。后续升级主程序即可由内置管理器自动跟随版本，不再单独更换外部 `current` 链接。

迁移不创建 2FA 任务、不重新申请密钥、不撤销会话，也不证明真实 OpenAI MFA 协议成功。重新开始处理任务属于已有凭证运营行为。恢复暂时关闭的巡检/自动重登设置后，按原任务状态继续。

## 回退与备份

记录位于 `/var/backups/sub2api-worker/<timestamp-id>/migration.json`，权限为 0600；不包含 Token 或凭据。迁移只增加一个 drop-in、复制缓存/恢复数据并切换服务，不改原 unit、配置和 `current`。

回退前再次排空并处理未完成 2FA；迁移后产生的新恢复记录必须和 key 一起保留，不能用原目录覆盖新目录。默认服务名的控制回退命令如下，**仅供获授权回退时执行**：

```bash
sudo systemctl stop sub2api.service
sudo rm /etc/systemd/system/sub2api.service.d/90-managed-reauth.conf
sudo systemctl daemon-reload
sudo systemctl start sub2api.service
# 原 sidecar 为 enabled 时恢复 enable；原为 disabled 则只 start。
sudo systemctl enable --now sub2api-openai-reauth-worker.service
```

数据库、凭据加密 key、原恢复目录和新 `credential-worker/totp-recovery` 一起备份。回退旧二进制不能撤销已发生的真实 2FA 更换；该状态需按 [2FA 恢复说明](../docs/openai-2fa-rotation.md) 处理。
