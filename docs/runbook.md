# Scribe 运维 Runbook

## 适用范围

首版 Scribe 是单实例、临时部署的惰性迁移设施。它只透明代理根接口与 authserver 端点，不实现认证、角色、材质或 Minecraft 服务端业务，不运行周期性恢复 worker。

本 Runbook 覆盖上线检查、日常观测、启动恢复、故障处置和演练。Armorial 的内部迁移契约仍是 commit、reconcile、rollback 行为的权威来源。

## 不可破坏的不变式

- 迁移成功前不创建永久绑定，也不在 SQLite 中写入半成品。
- 明文密码只存在于当前请求内存，不进入 Redis、SQLite、日志、trace、metrics label 或错误报告。
- Redis 是迁移暂存区，不是账号真相来源；不得通过删除暂存数据来“修复”未知状态。
- 既有 Core 账号不得删除或覆盖；回滚只能删除明确由本次 `migration_id` 创建的资源。
- `COMMITTED` 不允许回滚；结果未知时保持阻断，尤其是 signout。
- signout 与同一规范化 identifier 的迁移共享进程内串行锁；锁覆盖快照捕获至迁移完成与绑定路由，也覆盖 signout 的 pending 检查至上游调用结束。
- authenticate 与 signout 使用独立限流桶，按直接连接来源 IP 计数；signout 超限返回 `429 TooManyRequestsException` 与 `Retry-After: 1`。
- 已提交但缺权威结果、带资源漂移或权属不明时不建立永久绑定，也不自动回滚。
- 永久绑定不随 token 摘要过期；token 保留期必须覆盖上游 token 的最长有效期。
- 首版不得启动第二个 Scribe 实例共享同一 SQLite 文件或 Redis 迁移命名空间。

## 上线前检查

1. 确认旧上游已移至内部地址，Scribe 接管原公共 Root；启动器无需修改配置。
2. 确认 `SCRIBE_PUBLIC_BASE_URL`、`SCRIBE_LEGACY_BASE_URL`、`SCRIBE_ARMIORIAL_BASE_URL` 均为 HTTPS 且不含路径、查询或 fragment。
3. 确认 Redis 使用 `rediss://`，证书与主机名校验通过，ACL 仅允许 Scribe 所需键空间；不得启用 `skip_verify`。
4. 确认 `SCRIBE_MIGRATION_SERVICE_TOKEN` 仅注入到 Scribe 进程和受信任的运维主机，不写入镜像、Git 或共享日志。
5. 确认 SQLite 路径位于持久卷，容器以非 root 用户可写；备份现有数据库后再升级。
6. 确认 `SCRIBE_TOKEN_BINDING_TTL` 覆盖上游 access token 的最长有效期。
7. 确认 `SCRIBE_WRITE_TIMEOUT` 不小于 `SCRIBE_MIGRATION_TIMEOUT`。
8. 确认 Armorial commit 内部复用正常材质管线，并返回迁移后的权威会话与 user 信息。
9. 确认 Prometheus 可从受信任网络抓取 `/metrics`，并按实际环境修改 `deploy/alerts.yml` 的 `job="scribe"` 标签。
10. 执行 `make verify`，并在预发布环境完成本文末尾的故障演练。

## 启动与健康检查

容器部署：

```bash
docker compose -f deploy/compose.yaml up --build -d
docker compose -f deploy/compose.yaml logs --since=10m scribe
```

裸二进制部署时，使用部署环境的进程管理器启动 `scribe`，确保工作目录和 `SCRIBE_SQLITE_PATH` 的父目录可写。

健康检查：

```bash
curl -fsS "$SCRIBE_PUBLIC_BASE_URL/healthz"
curl -fsS "$SCRIBE_PUBLIC_BASE_URL/readyz"
curl -fsS "$SCRIBE_PUBLIC_BASE_URL/metrics"
```

- `/healthz` 只表示进程可响应。
- `/readyz` 同时检查 Redis 与 SQLite；返回 `503` 时不要接收新迁移。
- `/metrics` 只包含低基数聚合值；生产环境仍应在网络层限制抓取来源。

`SCRIBE_PURE_PROXY=true` 时，根接口与全部 authserver 端点直接转发到旧上游，不执行 JSON 解析、竞速、迁移或绑定路由。该模式不连接 Redis、SQLite 或 Armorial，也不执行启动恢复；`/readyz` 仅表示进程可响应。

正常迁移模式下，启动时先执行 Redis 暂存扫描与恢复，再开始监听 HTTP。若扫描失败，进程退出；恢复单条记录失败时记录 `migration recovery blocked`、计入 `result="blocked"` 指标并继续处理其他记录。committed 只有完整匹配 Core 权属与 ID、account、固定 profile、暂存资产摘要且无漂移时才重绑；自动回滚前先把权威对账中的既有 Core UUID/权属与 account ID 写入 Redis，`ROLLING_BACK`/`ROLLED_BACK` 每次启动都重新对账并逐项比对这份回滚身份。未知状态、缺失既有 Core 用户、迁移新建 Core 用户仍存在、身份或资产不符、缺少回滚身份的旧记录、已提交记录缺权威结果时，不会自动回滚、删除暂存或建立绑定。

## 正常迁移观测

一次迁移应出现以下稳定字段或指标：

- 认证决策：`scribe_authentication_decisions_total{outcome="MIGRATE_REUSE_CORE"}` 或 `MIGRATE_CREATE_CORE`。
- 迁移成功：`scribe_migration_operations_total{result="success",phase="bookkeeping"}`。
- 迁移耗时：`scribe_migration_duration_seconds_*`。
- 日志：`migration applying`，随后建立 Armorial 永久绑定与 token 摘要绑定。

`migration_id` 可以出现在日志中用于关联 Armorial，但不得作为 metrics label。不得在日志中附加请求体、密码、token、user properties 或纹理 URL。

## 告警

`deploy/alerts.yml` 提供以下 Prometheus 规则：

- Scribe 实例不可达。
- 迁移失败、资产冲突、p95 延迟过高。
- 启动恢复被阻断、出现 `UNKNOWN` 状态。
- Armorial 目标状态未知导致连续阻断。
- 迁移索引状态未知。
- SQLite 绑定或 token 绑定失败。

启动时 Redis 扫描失败会让进程在监听 `/metrics` 前退出，因此通过 `ScribeDown` 与进程/容器退出日志发现；不能依赖本次启动中未暴露的 `scribe_recovery_scans_total{result="failed"}`。

资产冲突可通过 `scribe_migration_failures_total{reason="asset_conflict"}` 单独观测；Armorial 纹理端点 5xx 和签名验证失败仍需结合 Armorial 侧指标告警。

## 迁移仪表盘

`deploy/grafana/dashboards/scribe.json` 是可导入的 Grafana 仪表盘，覆盖认证决策、迁移结果与阶段、白名单失败原因（含资产冲突）、迁移 p95 耗时、阻断原因、启动恢复状态与恢复 p95 耗时。导入时选择 Prometheus 数据源，并将 `Prometheus job` 变量设置为实际的 `scribe` 抓取任务。

仪表盘只使用低基数运维标签，不包含用户名、邮箱、UUID、密码、token 或 `migration_id`。启动扫描失败发生在 `/metrics` 暴露前，仍以 `ScribeDown` 和进程退出日志为准。

## 故障处置

### Redis 不可用

现象：`/readyz` 返回 `503`，认证出现 `migration_state_unknown` 或 `target_state_unknown`；若发生在启动扫描阶段，进程会退出。

处置：

1. 保持用户阻断，不创建新绑定。
2. 检查 Redis TLS、证书、ACL、网络和容量。
3. 恢复 Redis 后重启单实例 Scribe，让启动恢复重新扫描。
4. 不得删除 `scribe:migration:*` 键或 identifier 索引来绕过未知状态。

### SQLite 不可用或卷损坏

现象：`/readyz` 返回 `503`，出现 `binding_lookup_failed`、`permanent_binding_failed` 或 token 绑定失败。

处置：

1. 停止接收新认证，检查进程用户、目录权限、磁盘空间和文件锁。
2. 从最近的安全备份恢复；恢复前先保留当前数据库文件用于取证。
3. 若 Armorial 已提交但 SQLite 绑定失败，不要手工猜写 binding；启动恢复会使用 identifier hash 与权威 commit 结果重新绑定。
4. 不得删除数据库来强制重新竞速。

### Armorial 超时、5xx 或畸形响应

现象：`BLOCK_RETRY` 与 `target_state_unknown` 增加。

处置：

1. 确认没有新永久绑定或迁移提交。
2. 检查 Armorial 健康、TLS、熔断和响应契约。
3. 恢复后让用户重新认证；不要自动改判旧上游胜出。
4. 若请求已进入 commit，按迁移失败或启动恢复流程处理，不能仅凭客户端超时认定未提交。

### 旧上游不可用

现象：Armorial 凭据有效但无 profile 时，出现 `legacy_unavailable` 阻断。

处置：

1. 恢复旧上游的协议端点与 TLS。
2. 不导入不完整快照，不建立永久绑定。
3. 对已经存在永久绑定的用户，Scribe 仍只访问其胜出上游，不重新竞速。

### Commit 失败或响应丢失

现象：`scribe_migration_operations_total{result="failed",phase="commit"}`，暂存状态进入 `RECONCILING`。

处置：

1. 保持用户阻断，signout 不解除阻断。
2. 记录日志中的 `migration_id`，通过 Armorial reconcile 获取权威状态。
3. 若已提交，等待启动恢复完成 Armorial 绑定并进入 `LOCKED`。
4. 若未提交且资源权属明确，由 Armorial rollback 清理本次创建的本地资源；既有 Core 账号始终保留。
5. 若迁移创建了 Core 用户，Core 补偿契约落地前 Armorial 会以 `migration_rollback_not_allowed` 拒绝回滚，必须保持阻断并告警，不得手工删除 Core 用户。
6. 若权属不明或状态未知，停止自动回滚并告警。

### 资产冲突

现象：Armorial 返回 `migration_asset_conflict`，Scribe 记录 commit 阶段失败。

处置：

1. 不覆盖目标皮肤或披风。
2. 核对双方规范化 RGBA 像素摘要，而不是 PNG 压缩字节。
3. 由运维与账号所有者确认目标资产；首版不提供自动覆盖开关。
4. 未提交资源按迁移契约回滚，既有 Core 账号保留。

### Token 摘要过期

现象：旧 token 在 refresh/validate 上返回 `403`，invalidate 返回幂等 `204`；永久绑定仍存在。

处置：

1. 确认 `SCRIBE_TOKEN_BINDING_TTL` 覆盖上游 token 最长有效期。
2. 让用户重新 authenticate；已绑定用户仍路由到原胜出上游。
3. 过期映射在下次启动清理；不要删除永久 binding。

## 启动恢复流程

1. 确认只有一个 Scribe 实例。
2. 检查 Redis、SQLite、Armorial 和迁移服务凭据。
3. 启动 Scribe，观察 `scribe_recovery_scans_total`、`scribe_recovery_operations_total` 与恢复日志。
4. 对阻断记录使用日志中的 `migration_id` 查询 Armorial 权威状态。
5. 已提交记录完成绑定后进入 `LOCKED`；未提交记录完成回滚后删除暂存；未知记录保持阻断。
6. 已提交但 reconcile 未返回权威结果，或返回资源漂移时，保持 `COMMITTED` 与用户阻断；不得猜测会话、手工写 binding 或调用 rollback。
7. committed 重绑前逐项核对 Core 权属与 ID、account、固定 profile 和暂存资产摘要；任何缺失、额外、重复或身份不符的资源都会阻断。
8. 自动回滚前核对既有 Core 用户存在、固定 profile 与暂存资产身份一致，并原子持久化权威 Core UUID/权属与 account ID；`ROLLING_BACK` 与 `ROLLED_BACK` 重启时再次执行权威对账，Core/account 身份、缺省语义或资产清单不符不得删除暂存。
9. 重复启动应幂等：`LOCKED` 不重复迁移，已验证的回滚暂存删除后不再处理，缺结果或清单漂移的已提交记录每次均保持阻断。

在受信任的运维主机上需要手动 reconcile 时，关闭 shell tracing，使用环境变量注入服务凭据：

```bash
set +x
curl -fsS -X POST \
  "$SCRIBE_ARMIORIAL_BASE_URL/internal/migrations/$MIGRATION_ID/reconcile" \
  -H "Authorization: Bearer $SCRIBE_MIGRATION_SERVICE_TOKEN"
```

只有 Armorial 明确证明迁移未提交且资源权属清晰时，才调用同契约的 `rollback`。不得对 `COMMITTED` 调用 rollback，也不得仅凭 Redis 或客户端超时判断状态。

## 紧急停止与回切

需要紧急回切时，将 `SCRIBE_PURE_PROXY=true` 注入单实例部署并重启 Scribe。该模式会把根接口与全部 authserver 端点透明转发到 `SCRIBE_LEGACY_BASE_URL`，不连接 Redis、SQLite 或 Armorial，不执行启动恢复，不读取或写入永久绑定与 token 绑定。`/readyz` 仅检查进程响应，不再表示迁移依赖健康。

纯代理模式会停止所有迁移。已经迁移到 Armorial 的用户可能没有对应的旧上游账号或 token，因此只能作为紧急降级措施，不能当作无损回滚。恢复时必须设置 `SCRIBE_PURE_PROXY=false` 并重启；正常模式会先执行启动恢复，再监听 HTTP，已有永久绑定不会重新竞速。

切换前后都不得删除 SQLite、清空 Redis 或修改 binding。已提交迁移和永久绑定必须保留；必要时先保存日志、数据库和 Redis 状态用于对账。

## 故障演练

所有演练仅在预发布环境进行，使用专用账号和可恢复资源。

| 演练 | 注入方式 | 预期结果 |
|---|---|---|
| Redis 中断 | 预发布环境短暂阻断 Redis 端口 | `/readyz=503`，不创建新绑定；恢复并重启后重新扫描 |
| Armorial 5xx/超时 | 在 Armorial 前置故障代理返回 5xx 或延迟 | `target_state_unknown`，用户阻断，无迁移写入 |
| Commit 响应丢失 | Armorial 提交后、Scribe 收到响应前切断连接 | 暂存进入 `RECONCILING`；重启后 reconcile，已提交只重新锁定 |
| 部分回滚 | rollback 返回仍存在的迁移资源 | 不删除暂存、不宣称完成，恢复阻断指标触发告警 |
| 重复启动 | 对 committed、rolled_back 与缺结果记录连续重启两次 | 不重复迁移；已回滚暂存消失；缺结果记录保持阻断且无 binding |
| 恢复清单漂移 | reconcile 返回错误 account/profile/资产摘要，或 rolled_back 清单仍有残留 | 不建立 binding、不删除暂存，恢复操作计入 blocked |
| signout 与迁移竞态 | 分别阻塞迁移快照/提交和 signout 上游调用 | 同 identifier 的后续操作等待前者完成，不越过 pending 门闸 |
| signout 限流 | 对同一来源 IP 超过 `SCRIBE_AUTH_BURST` 连续调用 signout | 返回 `429 TooManyRequestsException`；authenticate 额度不受影响 |
| 恢复阻塞监听 | 在启动恢复完成前观察监听端口 | 端口不可连接；恢复扫描完成后才开始监听，扫描失败则进程退出 |
| SQLite 绑定失败 | commit 成功后令 SQLite 写入失败 | 认证返回 `503`，不手工写 binding；恢复后启动重绑 |
| Token 过期 | 使用短 TTL 并等待映射过期 | 旧 token 不可路由，永久 binding 保留，重新认证路由不变 |
| 紧急纯代理 | 设置 `SCRIBE_PURE_PROXY=true` 并重启 | 所有公开请求只到旧上游；Redis/SQLite 不可用时仍可启动；切换不删除绑定或暂存 |
| 敏感信息扫描 | 在日志与指标中加入 canary 字符串 | 日志不出现明文凭据；非白名单指标标签归为 `unknown` |

演练结束后确认：无新增脏 binding、无重复 Core/profile、无遗留未知暂存、告警可触发且可从运行记录追溯到 `migration_id`。
