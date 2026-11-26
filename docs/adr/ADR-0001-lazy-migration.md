# ADR-0001：基于标准 Yggdrasil 协议的惰性迁移

- 状态：Accepted
- 日期：2026-11-26
- 范围：Scribe 的认证竞速、迁移提交、资产迁移、补偿与路由锁定

## 背景

Scribe 部署在旧公共 Yggdrasil API Root 的地址上，使启动器无需修改配置即可从黑箱旧上游迁移到 Armorial。部署者同时控制新旧上游，并将旧上游移至内部地址。

旧上游的内部实现完全黑箱。Scribe 只能依据 authlib-injector / Yggdrasil 的外部协议形状与 Armorial 契约判断行为，不得通过阅读 Armorial、HRPAuth-Yggdrasil-API、WinnerProxy 或旧上游实现来推断旧服务行为。

标准 Yggdrasil 协议不提供“导出全部账号数据”的能力。可迁移范围限定为：用户 ID、可选 user properties、本次认证选中的 profile、该 profile 的当前皮肤与披风，以及完成迁移所需的凭据和标识符。

## 决策

### 1. 首次认证竞速

启动器被引导重新输入密码后，Scribe 使用本次凭据并行请求新旧上游的 `POST /authserver/authenticate`。竞速不是单纯比较 HTTP 状态码，而是比较新上游是否已经拥有可玩的 `selectedProfile`。

| Armorial 结果 | 旧上游结果 | 决策 |
|---|---|---|
| `200` 且包含 `selectedProfile` | 任意 | Armorial 胜出；不导入、不覆盖，直接建立永久绑定 |
| `200` 但不包含 `selectedProfile` | `200` 且包含 `selectedProfile` | 复用可能存在的 Core 账号，按旧 UUID 创建 profile 并迁移资产 |
| `403` | `200` | 创建 Core 账号，再迁移旧 profile 与资产 |
| 超时、`5xx` 或响应畸形 | `200` | 目标状态未知；阻断并重试，禁止写入 |
| `403` | 失败 |不建立绑定，不迁移 |

竞速只发生在首次认证。成功后永久绑定；失败或状态未知不建立绑定，不自动改判胜者。

用户未表达迁移异议时，Armorial 凭据有效但无 `selectedProfile`、旧上游存在可迁移 profile 的组合默认按导入处理。首版不要求启动器传递 `migration_intent`；只有显式 opt-out 才阻断迁移。

新上游 `403` 只表示本次 Yggdrasil authenticate 失败，不能证明 Core 账号不存在。迁移提交必须非破坏性地创建或复用 Core 账号；若已有账号的凭据、用户名或邮箱映射冲突，返回 `migration_credential_conflict` 或 `migration_profile_conflict`，禁止覆盖已有凭据。

### 2. 迁移范围与身份映射

- 只迁移旧认证结果中的本次 `selectedProfile`，不枚举或迁移全部 `availableProfiles`。
- 正常路径要求同凭据、同 `selectedProfile.id`；UUID 不重新生成。
- 登录标识符可能是邮箱，也可能是 profile 名称，不能据此反推 Core username。
- Core username 使用旧 `selectedProfile.name`；若与已有 Core 账号冲突则迁移失败并保持阻断。
- 登录标识符是邮箱时直接使用；是用户名时生成唯一的 `migration-{migration_id}@placeholder.invalid` 占位邮箱。
- 占位邮箱不发送邮件、不参与找回密码，也不是已确认的联系方式。
- 迁移成功且客户端请求 `requestUser:true` 时，返回 Armorial commit 提供的权威 `user.id/properties`；不得返回旧用户 ID 或自行推断映射。

### 3. 提交前不得产生脏数据库行

迁移完成前，快照、规范化资产、哈希和迁移 ID 只能保存在进程内存或 Redis。禁止提前创建 Core 用户、Yggdrasil profile、纹理记录或其他持久化半成品。

提交阶段允许短暂存在资源，但必须由迁移日志记录；失败时通过 reconcile 与 rollback 清理本次迁移创建的资源。既有 Core 账号必须保留。

### 4. 原子语义与补偿

Armorial 侧新增迁移提交、查询/对账和回滚契约：

- `POST /internal/migrations/commit`
- `POST /internal/migrations/{migration_id}/reconcile`
- `POST /internal/migrations/{migration_id}/rollback`

状态机：

```text
RECEIVED
  -> STAGED_IN_REDIS
  -> APPLYING
  -> COMMITTED
  -> LOCKED
```

异常路径：

```text
STAGED / APPLYING / UNKNOWN
  -> RECONCILE
  -> ROLLBACK_IF_NOT_COMMITTED
  -> DISCARD_STAGING
```

`COMMITTED` 是终态，不允许回滚。回滚只能删除明确由该 `migration_id` 创建的资源；遇到权属不明、资源被后续修改或 Core 用户已不存在等情况时 fail closed。

### 5. 权限边界

Scribe 只调用 Yggdrasil 服务的外部标准端点及迁移内部端点，不直接访问 Armorial Core。需要 Core 配合的账号创建与补偿由 Yggdrasil 服务内部完成，并使用独立、最小权限的服务身份。

Scribe 不调用 `/admin/force-bind`，不吸收 WinnerProxy 正版身份，不恢复已删除账号，不处理 UUID 重生成，不维护增量 dirty database state。

### 6. 资产与签名

- 资产冲突矩阵：
  - 旧有、目标无：上传；
  - 双方规范化像素相同：跳过；
  - 双方不同：迁移失败；
  - 旧无、目标有：迁移失败。
- 比较规范化 RGBA 像素，不比较 PNG 压缩字节。
- Scribe 将规范化 PNG 提交给 Armorial 迁移端点；Armorial 在迁移事务与补偿日志内部复用正常材质存储和上传管线。
- 迁移后的 textures property 由 Armorial 重写并签名，纹理 URL 由 Armorial 公共端点提供。Scribe 不代理纹理，也不维护签名私钥。
- 皮肤同步必须关闭；不得在注册或迁移提交时触发官方皮肤同步覆盖导入资产。

### 7. 凭据与敏感数据

明文密码只存在于当前认证/迁移请求的内存中，不写入 Redis、数据库、日志、追踪或错误报告。目标 Core 仅保存其正常的凭据哈希。Redis 中只保存非凭据快照、规范化资产、哈希和迁移日志。令牌优先不落盘；必须保存时只保存不可逆摘要。

所有上游通信使用 TLS。认证前限流；纹理下载执行 SSRF 防护、PNG 解压炸弹防护、尺寸校验和元数据剥离。

## 被否决的方案

1. **阅读参考实现推断旧行为**：违反黑箱约束，且旧上游不保证与 Armorial 相同。
2. **放弃竞速，始终先认证旧端再原子导入**：会伤害已经在 Armorial 拥有可玩 profile 的用户。
3. **迁移全部 profiles**：超出最小迁移目的，并放大冲突与失败范围。
4. **提前写数据库再修复**：会产生 dirty 数据，违反提交前不得落库的不变量。
5. **使用 force-bind 或 WinnerProxy 代注册强行绑定 UUID**：超出权限边界和设计范围。
6. **持久化明文密码以便重试**：不可接受的凭据风险。
7. **以压缩 PNG 字节判断资产冲突**：编码差异会制造假冲突。
8. **自动重竞速或失败后切换胜者**：破坏首次认证后的确定性与永久绑定语义。

## 后果

### 优点

- 启动器配置不变，用户无感。
- 已有 Armorial profile 的用户不会被错误迁移。
- 旧上游内部实现变化不影响 Scribe 的协议判断。
- 失败迁移不会留下持久化半成品。
- 迁移责任与 Core 权限被明确隔离。

### 代价

- Scribe 需要维护 Redis 暂存和迁移恢复机制。
- Armorial 需要新增跨服务补偿契约，单凭现有 texture/profile API 无法保证全部或全无。
- 占位邮箱账号需要明确的后续认领/找回流程；该流程不属于首版迁移范围。
- 标准协议只能导出有限账号资产，不能承诺迁移邮件、安全设置、权限或其他非 Yggdrasil 数据。

## 实施决策（2026-11-26）

- 用户未表达迁移意愿时默认导入：Armorial 凭据有效但无 profile，且旧上游有可迁移 profile 时，执行迁移；首版不要求启动器传递 intent。
- Scribe 的“兼容”仅指对范围内 Yggdrasil HTTP 端点做透明代理，不实现认证、角色、材质或 Minecraft 服务端业务。首版范围保持根接口与 authserver，不因标准列出 session-server 而扩展。
- 首版限定单实例部署。该中间件属于临时部署设施，不提供水平扩容；跨实例迁移串行化不在首版范围内。
- 迁移成功且客户端请求 `requestUser:true` 时，必须返回 Armorial 提交后的权威用户信息。Armorial commit 契约需提供该信息，Scribe 不得返回旧用户 ID 或自行推断映射。
- 首版只在进程启动时执行迁移恢复，不运行周期性恢复 worker。
- token 绑定仅保存不可逆摘要，并按上游 token 的最长有效期限设置可配置的保留与清理策略。
- 迁移结果未明确前，signout 保持阻断，不尝试取消迁移或修改旧上游凭据状态。
- 首版提供迁移结果、恢复耗时和失败告警指标，并保留紧急回滚到纯代理模式的运维开关；指标标签不得包含用户名、邮箱、UUID、密码或 token。
- `SCRIBE_PURE_PROXY=true` 时，根接口与 authserver 全部直接代理到旧上游；不连接迁移依赖、不执行恢复、不读写绑定。该开关只用于紧急降级，不删除已提交迁移或既有绑定，也不是无损回滚。

## 关联文档

- 项目知识库：`docs/knowledge/README.md`
- 运维 Runbook：`docs/runbook.md`
- Prometheus 告警规则：`deploy/alerts.yml`
- Roadmap：`docs/roadmap.md`
- 权威契约：`/home/lnb/Desktop/HA/HA-Contract/docs/api/openapi/hrpauth-yggdrasil-business.yaml`
- 外部协议标准：`/home/lnb/Desktop/HA/HA-Contract/docs/references/`
