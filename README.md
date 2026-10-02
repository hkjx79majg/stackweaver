# StackWeaver

这是一个面向基础设施即代码的基础设施即代码与云接口 SDK/Provider 平台。长期目标是提供资源声明与类型校验、依赖图与拓扑排序、计划与执行两阶段、状态文件与并发锁、漂移检测、Provider 多后端适配和审计导出，把基础设施编排沉淀为可复用平台。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/stackweaver
```

服务默认监听 `127.0.0.1:8080`。可通过 `STACKWEAVER_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 执行计划

`POST /v1/apply` 与 `POST /v1/plans` 使用相同的 `configuration` / `priorState` 信封、校验规则与确定性变更顺序；它不读取文件、环境变量或远端状态，也不在请求之间保存状态。

变更的实际执行由可注入的 `server.Provider` 完成，服务自身不做任何持久化：

```go
type Provider interface {
    Apply(req ChangeRequest) error
}
```

`server.HandlerWithProvider(provider)` 注入执行器并返回完整 HTTP 表面；`server.Handler()` 等价于注入 `nil`。单项请求 `ChangeRequest` 携带请求的 `context.Context`、`action`、`address`、`before` 与 `after`。服务按顺序逐项调用 Provider（noop 不调用）：全部成功返回 200 及 `applied`、计划 `summary` 和由 `priorState` 演进而来的 `state`；某项失败时立即停止、不回滚，返回 502、`provider_error`（path 为 `/changes/{索引}`）及已成功的部分；有变更但未注入 Provider 时返回 503 `provider_unavailable`，空计划在无 Provider 时仍成功。

## 状态文件

`server.HandlerWithProviderAndStateFile(provider, path)` 在完整表面上额外提供由状态文件支撑的两个端点；未配置文件路径的处理器访问这两个端点时返回 503 `state_unavailable`。

- `GET /v1/state` 返回文件的 `revision`（非负整数）与 `state`（沿用既有 `resources` 结构）。文件不存在时视为版本 0、资源为空；文件不可读或内容不合法时返回 500 `state_read_error`，且不会改写文件。
- `POST /v1/state/apply` 只接受 `configuration` 与可选 `expectedRevision` 信封（非法信封或非法版本号返回 422 `invalid_state_request`），以文件当前 state 作为 priorState 校验、计划并执行。整个读取到提交过程占用规范化路径的非阻塞排他锁，同一路径忙时返回 409 `state_locked`；`expectedRevision` 与当前版本不符时返回 409 `state_conflict` 及 `currentRevision`，且不调用 Provider。

空计划成功且版本不变；全部成功后原子替换文件，revision 恰增 1，响应携带 `applied`、`summary`、`state` 与 `revision`。Provider 失败仍返回 502 `provider_error`：首项失败保持文件不变，已有成功项则保存部分 state 并增加一次版本。提交失败返回 500 `state_write_error`，不留下半写文件，响应保留已执行项和待提交 state。

## 验证

```bash
go test ./...
```

当前基线刻意不包含资源声明模型、依赖图与计划执行两阶段的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。
