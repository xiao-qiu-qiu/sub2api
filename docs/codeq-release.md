# codeq 版本与更新

本分支的应用更新、版本回滚和安装脚本默认使用 [xiao-qiu-qiu/sub2api](https://github.com/xiao-qiu-qiu/sub2api) 的 Releases；容器镜像为 `ghcr.io/xiao-qiu-qiu/sub2api`。

## 版本号

应用版本维护在 `backend/cmd/server/VERSION`，格式为 `X.Y.Z-codeq`，例如 `2.19.2-codeq`。每次完成功能时递增补丁号；需要调整主版本或次版本时，由维护者按实际变更设置。版本名不追加日期、功能名或提交哈希，提交信息仍可用于追踪具体实现。

源码构建读取该文件，发布 tag 使用 `vX.Y.Z-codeq`。发布工具将这个固定后缀视为稳定发布，更新检查比较前三段数字，因此 `2.19.10-codeq` 高于 `2.19.9-codeq`。`rc`、`beta` 等其他预发布后缀继续按预发布处理。

功能完成后提交并推送到自己的对应分支；发布 tag 和部署按当前任务范围执行。

## 自动重登运行包

应用版本与自动重登运行包版本分别维护。`backend/cmd/server/REAUTH_RUNTIME_VERSION` 固定兼容的上游运行包版本，运行包仍使用其原发布源。合并上游涉及 Worker 协议变化时，核对运行包兼容性并更新这个文件。

设置 `-X main.Version=...` 仅覆盖应用版本；`-X main.ReauthRuntimeVersion=...` 才覆盖运行包版本。构建完成后可以通过 `./sub2api -version` 查看二者，详见[运行包说明](../deploy/MANAGED_WORKER.md)。

## 更新缓存

更新检查缓存带有仓库标识；升级后会忽略之前来自其他仓库的缓存，重新查询自己的 Releases。仓库尚未发布 Release 时，检查不会回退到上游的发布流。
