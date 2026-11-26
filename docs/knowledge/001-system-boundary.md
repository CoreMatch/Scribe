# 系统边界与不变量

## 部署拓扑

```text
Launcher
   |
   v
Scribe (旧公共 API Root)
   |-- internal TLS --> Legacy Yggdrasil (black box)
   `-- TLS -----------> Armorial Yggdrasil / migration API
                              |
                              `-- internal service auth --> Armorial Core
```

部署者控制 Scribe 与两个上游，并负责将旧上游移动到内部地址。Scribe 不负责修改启动器配置，不推断旧上游内部结构。

Scribe 的“兼容”仅指对范围内 Yggdrasil HTTP 端点做透明代理，不实现认证、角色、材质或 Minecraft 服务端业务。首版公开范围保持 `GET /` 与 authserver 端点；标准列出的 session-server 不因此进入首版。

## 必须保持的不变量

1. 旧上游行为只能由外部标准与可观察响应判断。
2. 新上游只能是 Armorial。
3. 首次认证完成后，成功用户永久绑定到唯一胜者。
4. 状态未知时不绑定、不写库、不切换胜者。
5. 只迁移本次 `selectedProfile`。
6. 正常迁移保持同 UUID、同凭据，不重新生成 UUID。
7. 明文密码只存在于请求内存，不进入 Redis、数据库、日志或追踪；目标 Core 仅保存正常凭据哈希。
8. 迁移提交前不得产生持久化半成品。
9. 失败迁移只能回滚明确由本次 `migration_id` 创建的资源。
10. 已有 Core 账号不得因迁移回滚被删除。
11. Scribe 不直接访问 Armorial Core。
12. 凭据冲突、用户名冲突、profile 冲突或资产冲突必须 fail closed。
13. `COMMITTED` 后禁止回滚，路由锁定为 Armorial。
14. 不迁移协议未暴露的数据，不承诺迁移邮件、安全设置、权限或全部 profiles。

## 不在范围内

- WinnerProxy 正版身份吸收。
- 删除账号恢复。
- UUID 重生成。
- 预注册或提前落库。
- 增量 dirty database state。
- 旧上游私有端点探测。
- 为迁移而调用 Minecraft server join/hasJoined。
- 迁移后的占位邮箱认领、密码找回或账号安全补全流程。
