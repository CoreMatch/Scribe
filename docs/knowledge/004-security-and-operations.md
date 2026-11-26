# 安全与运行约束

## 凭据

- 明文密码只存在于 authenticate/signout/commit 当前请求内存；目标 Core 仅保存正常凭据哈希。
- signout 与同一规范化 identifier 的迁移共享进程内串行锁，覆盖快照捕获至迁移完成与绑定路由，以及 pending 检查至 signout 上游调用结束。
- 禁止写入 Redis、数据库、访问日志、审计日志、错误上下文、trace、metrics label 与崩溃转储。
- 禁止将 multipart 原始请求体或完整认证请求写入调试日志。
- 错误报告只记录迁移 ID、阶段和稳定错误码。
- 不保存旧 accessToken/clientToken；如必须保存用于诊断，只保存不可逆摘要并设置短 TTL。

## 网络与资源

- 所有 Scribe 到上游流量使用 TLS，并校验主机名与证书。
- authenticate 与 signout 在昂贵工作之前分别限流，使用独立 token bucket 和直接连接来源 IP；不信任 `X-Forwarded-For` 等请求头。
- signout 超限返回标准 `429 TooManyRequestsException` 与 `Retry-After: 1`，不访问 pending 状态或上游。
- 上传设置总请求大小、单文件大小、解压后像素数和并发上限。
- 纹理下载禁止访问 loopback、link-local、私网、云元数据地址；每次重定向重新校验。
- DNS rebinding 通过在连接时固定并校验解析结果防护。
- PNG 解码限制尺寸、像素数、内存与执行时间。
- 规范化资产剥离 EXIF、iTXP、颜色 profile 等非必要元数据。

## Redis

Redis 是迁移暂存区，不是账号真相来源。建议：

- 迁移包设置短 TTL，例如 24 小时。
- 资产和快照使用独立键，提交摘要使用 SHA-256。
- 状态更新使用 CAS/版本号，避免重复 worker 同时 apply。
- 不保存密码、完整令牌或签名私钥。
- 生产环境启用 TLS、ACL 与持久化策略；即使 Redis 丢失，也只能触发 reconcile，不得猜测提交状态。

## 签名密钥

- 首版不要求 Scribe 维护签名私钥。
- Armorial 在迁移提交内部复用正常材质管线，生成并签名 textures property，提供公共纹理 URL。
- Scribe 只验证旧 signed profile，并透明代理根元数据中的 `signaturePublickey`。
- 私钥不得写入 Git、镜像、日志或 Redis。

## 恢复与对账

- 首版只在进程启动时扫描迁移并恢复可恢复状态，不运行周期性恢复 worker；恢复完成前不监听 HTTP。
- 对账比较迁移日志、Core 用户、Yggdrasil profile 与资产记录。
- committed 重绑前必须完整匹配 Core 权属与 ID、account、固定 profile、暂存资产摘要及资源存在性；缺失、额外、重复或身份不符均失败关闭。
- `ROLLING_BACK` 与 `ROLLED_BACK` 每次启动都重新权威对账并验证回滚清单，不能仅凭 Redis 状态删除暂存。
- 自动回滚前将权威对账中的既有 Core UUID/权属与 migration-owned account ID 原子写入 Redis；回滚后及重启时逐项比对，Core/account 未分配时允许缺省，身份缺失或变化均失败关闭。
- 资源存在但权属不明、状态未知或身份与暂存包不一致时停止自动回滚并告警。
- 已提交迁移只允许使用权威结果重新绑定并锁定，不允许回滚；缺结果或存在漂移时保持阻断。
- 回滚必须可重试且只删除本次迁移明确创建的资源。
- 既有 Core 账号永久保留，即使其用户名在后续发生变化。
- 迁移结果未明确前，signout 保持阻断，不取消迁移或修改旧上游凭据状态。

## Token 绑定保留

- SQLite 只保存 token 的不可逆摘要。
- 保留期必须覆盖上游 token 的最长有效期，并可通过 `SCRIBE_TOKEN_BINDING_TTL` 配置。
- 过期映射在查询时视为不存在，并在进程启动时清理；不运行周期性 token 清理 worker。

## 观测

允许记录：

- migration_id、阶段、耗时、结果、目标服务、稳定错误码。
- 不使用用户名、邮箱、UUID、密码、令牌或纹理 URL 作为高基数 metrics label。

## 指标

- `GET /metrics` 输出 Prometheus 文本格式；生产环境应在网络层限制抓取来源。
- `scribe_authentication_decisions_total{outcome,reason}` 记录首次认证竞速结果。
- `scribe_authentication_blocked_total{reason}` 记录迁移门闸或绑定失败导致的阻断。
- `scribe_migration_operations_total{result,phase}` 与 `scribe_migration_duration_seconds{result,phase}` 记录迁移结果、失败阶段和耗时。
- `scribe_migration_failures_total{reason,phase}` 按白名单原因区分资产、凭据、profile 冲突及超时、上游和校验失败。
- `scribe_recovery_operations_total{result,state}`、`scribe_recovery_duration_seconds{result,state}` 与 `scribe_recovery_scans_total{result}` 记录启动恢复。
- 标签值使用白名单，未知值归为 `unknown`；`migration_id` 只允许进入日志，不进入指标标签。
- 告警规则见 `deploy/alerts.yml`，处置流程与演练清单见 `docs/runbook.md`。
- `SCRIBE_PURE_PROXY=true` 是仅用于紧急降级的旧上游纯代理模式；不连接 Redis/SQLite/Armorial，不执行恢复或绑定路由，也不删除既有迁移数据。

## 告警条件

- `UNKNOWN` 状态超过阈值。
- reconcile/rollback 连续失败。
- 权属不明资源。
- 资产冲突率异常。
- 签名验证失败或 Armorial 纹理端点 5xx 升高。

## 上线检查

- 新旧上游均可访问，旧上游仅内部可达。
- Armorial reconcile/rollback API 已部署；Core 补偿契约仍待决策，未实现前迁移新建 Core 用户的 rollback 保持失败封闭。
- Redis 与 Armorial 签名配置已就绪。
- 目标 authenticate 已通过同 UUID、皮肤、披风与签名验证。
- 限流、超时、熔断与 fail-closed 行为已演练。
- 契约测试、竞速状态机测试和故障注入测试通过。
