# StackWeaver

这是一个面向基础设施即代码的基础设施即代码与云接口 SDK/Provider 平台。长期目标是提供资源声明与类型校验、依赖图与拓扑排序、计划与执行两阶段、状态文件与并发锁、漂移检测、Provider 多后端适配和审计导出，把基础设施编排沉淀为可复用平台。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/stackweaver
```

服务默认监听 `127.0.0.1:8080`。可通过 `STACKWEAVER_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 端点

- `POST /v1/configurations/validate`：校验配置文档。
- `POST /v1/configurations/order`：返回资源的确定性拓扑顺序。
- `POST /v1/plans`：计算 configuration 与 priorState 之间的只读有序变更计划。
- `POST /v1/apply`：按计划顺序执行变更。执行通过注入的 `server.Provider`
  完成，服务自身不持久化任何状态。

`server.Handler()` 返回不含 Provider 的 HTTP 表面：既有端点全部可用，
`/v1/apply` 仅在计划为空（全部 noop）时成功；有变更时返回 503
`provider_unavailable`。注入 `server.HandlerWithProvider(provider)` 后，
Provider 依次收到携带 `context.Context` 的 `server.ChangeRequest`
（`action`、`address`、`before`、`after`）；某项返回 error 时立即停止、
不回滚，返回 502 `provider_error` 与部分应用结果。

## 验证

```bash
go test ./...
```

当前基线刻意不包含资源声明模型、依赖图与计划执行两阶段的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。
