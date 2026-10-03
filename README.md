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

`server.HandlerWithProviderAndStateFile(provider, path)` 在上述表面之外增加文件状态后端，执行结果成为后续请求的输入：

- `GET /v1/state` 返回状态文件的 `revision` 与 `state`（沿用既有 `resources` 结构）。文件不存在视为版本 0、资源为空；文件不可读或内容不合法返回 500 `state_read_error`，且不会改写文件。
- `POST /v1/state/apply` 只接受 `configuration` 与可选 `expectedRevision` 信封（非法信封返回 422 `invalid_state_request`），以状态文件为 priorState 校验、计划并执行。整个执行从读取到提交持有规范化路径的非阻塞排他锁（分别创建的处理器同样互斥），锁被占用时返回 409 `state_locked`；`expectedRevision` 与当前版本不符返回 409 `state_conflict` 及 `currentRevision`，且不调用 Provider。

空计划成功且版本不变；全部成功后原子替换文件，`revision` 恰增 1，响应携带 `applied`、`summary`、`state` 与 `revision`。Provider 失败仍返回 502 `provider_error` 并立即停止、不回滚：首项失败保持文件不变，已有成功项则保存部分 state 并增加一次版本。提交失败返回 500 `state_write_error`，不留半写文件，原文件保持完整可读，响应保留已执行项与待提交 state。未注入 Provider 而有变更时返回 503 `provider_unavailable`；未经状态文件创建的处理器访问这两个端点返回 503 `state_unavailable`。

## 漂移检测

Provider 可同时实现可选的 `server.Observer` 接口，开放只读漂移检测：

```go
type Observer interface {
    Observe(ctx context.Context, address string) (*Snapshot, error)
}
```

`GET /v1/state/drift` 在与 apply 相同的非阻塞排他锁下读取状态文件的确定版本，随后按地址 Unicode 升序逐一调用 `Observe`（请求上下文原样传入；空状态不调用 Observer），比较存储快照与远端快照。比较沿用计划语义：对象键序与等值数字写法不影响结果，数组保序，依赖按集合比较且输出升序。成功返回 200，携带 `revision`、按地址排序的 `drifts`（`changed` 条目含 `before` 与 `observed`，远端不存在记为 `missing` 并省略 `observed`，一致资源不出现）以及含 `unchanged`/`changed`/`missing` 计数的 `summary`。

该端点不调用 `Apply`、不写状态、不增加版本。未配置状态文件返回 503 `state_unavailable`；Provider 未实现 `Observer` 时在加锁前返回 503 `observer_unavailable`；锁被占用返回 409 `state_locked`；状态文件无效返回 500 `state_read_error`。观察报错或快照违反契约（type 为空、properties 为 nil、依赖含空值或重复值、properties 无法编码为 JSON）时立即停止，分别返回 502 `observer_error` / `observer_invalid_result`，path 为 `/resources/{排序后索引}`，失败响应不含部分 drifts。

## 状态收敛

`POST /v1/state/reconcile` 在同一把非阻塞排他锁内把远端向**已保存状态**收敛：读取确定版本后，按地址 Unicode 升序逐一 `Observe`（空状态不调用 Observer），复用漂移检测的快照校验、规范化与等价比较。远端缺失生成 `create`，存在但不同生成 `update`，一致计为 `noop`；`update` 的 `before` 为观察快照（规范化后）、`after` 为保存快照，`create` 省略 `before`，`after` 均为保存快照。全部观察成功后，按保存状态的确定拓扑顺序调用 `Apply`，使被依赖资源先执行；`noop` 不调用 Provider，`delete` 恒为 0。

请求体是仅含可选 `expectedRevision`（非负整数）的 JSON 对象：畸形或含多个 JSON 值返回 400 `invalid_json`；非对象、未知字段或非法版本返回 422 `invalid_reconcile_request`。未配置状态文件返回 503 `state_unavailable`；Provider 未实现 `Observer` 在加锁前返回 503 `observer_unavailable`；锁冲突返回 409 `state_locked`；状态不可读返回 500 `state_read_error`。`expectedRevision` 与当前版本不符时返回 409 `state_conflict` 及 `currentRevision`，且不调用 `Observe` 或 `Apply`。

观察报错或结果非法时立即停止且不执行任何变更，分别返回 502 `observer_error` / `observer_invalid_result`，path 为 `/resources/{观察序号}`。Provider 失败立即停止且不回滚，返回 502 `provider_error`，path 为 `/changes/{执行序号}`，响应保留已成功的 `applied` 与完整 `summary`。全部成功返回 200，携带按执行顺序的 `applied`、`create`/`update`/`delete`/`noop` 汇总、原 `state` 与 `revision`。任何结果都不写状态文件、不增加版本；锁覆盖读取、观察、执行并始终释放。非 POST 返回 405 且 `Allow: POST`。

## 验证

```bash
go test ./...
```

当前基线刻意不包含资源声明模型、依赖图与计划执行两阶段的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。
