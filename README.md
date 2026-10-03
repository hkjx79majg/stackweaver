# StackWeaver

这是一个面向基础设施即代码的基础设施即代码与云接口 SDK/Provider 平台。长期目标是提供资源声明与类型校验、依赖图与拓扑排序、计划与执行两阶段、状态文件与并发锁、漂移检测、Provider 多后端适配和审计导出，把基础设施编排沉淀为可复用平台。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/stackweaver
```

服务默认监听 `127.0.0.1:8080`。可通过 `STACKWEAVER_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 只读命令行

不带参数运行 `stackweaver` 时按上述规则启动 HTTP 服务；带子命令时只执行本地只读流水线，不监听端口、不调用 Provider、不读取或写入状态文件，也不受 `STACKWEAVER_ADDR` 影响：

```bash
stackweaver validate            # 等价于 POST /v1/configurations/validate
stackweaver order               # 等价于 POST /v1/configurations/order
stackweaver plan                # 等价于 POST /v1/plans
```

三个子命令接收与对应 HTTP 入口完全相同的 JSON 文档：默认从标准输入读取，也可用 `--file PATH`（或 `--file=PATH`）读取一个 UTF-8 文件；两种来源都只允许恰好一个 JSON 值。成功时向标准输出写入一行 JSON 和换行，对象内容、错误排序、计划顺序、数值等价与依赖集合语义与对应 HTTP 入口逐字节一致，退出码为 0。请求为空、畸形、带尾随值或未通过既有业务校验时，把对应入口会返回的 `invalid_json` 或验证错误对象写到标准输出（不写标准错误），退出码为 2。

未知子命令、位置参数、未知选项、重复的 `--file` 或缺少其值均为命令行参数错误：向标准错误输出单行 JSON（`error.code` 固定为 `invalid_cli_arguments`，`error.message` 非空），标准输出保持为空，退出码为 2。`--file` 指定的文件不存在、不可读或读取失败时向标准错误输出 `error.code` 固定为 `input_read_error` 的单行 JSON，退出码为 1，且不会退回读取标准输入。

## 执行计划

`POST /v1/apply` 与 `POST /v1/plans` 使用相同的 `configuration` / `priorState` 信封、校验规则与确定性变更顺序；它不读取文件、环境变量或远端状态，也不在请求之间保存状态。

变更的实际执行由可注入的 `server.Provider` 完成，服务自身不做任何持久化：

```go
type Provider interface {
    Apply(req ChangeRequest) error
}
```

`server.HandlerWithProvider(provider)` 注入执行器并返回完整 HTTP 表面；`server.Handler()` 等价于注入 `nil`。单项请求 `ChangeRequest` 携带请求的 `context.Context`、`action`、`address`、`before` 与 `after`，以及幂等标识 `idempotencyKey` 与尝试序号 `attempt`（初次调用为 1）。一次顶层请求内，每个非 noop 变更获得非空且互不相同的 `idempotencyKey`，同一变更的各次尝试复用该值，新的顶层请求不复用旧值；不需要去重的 Provider 可忽略这两个字段。服务按顺序逐项调用 Provider（noop 不调用）：全部成功返回 200 及 `applied`、计划 `summary` 和由 `priorState` 演进而来的 `state`；某项失败时立即停止、不回滚，返回 502、`provider_error`（path 为 `/changes/{索引}`）及已成功的部分；有变更但未注入 Provider 时返回 503 `provider_unavailable`，空计划在无 Provider 时仍成功。

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

## 远端收敛

`POST /v1/state/reconcile` 以已保存状态为准收敛远端漂移：请求体只接受一个 JSON 对象，字段仅为可选的 `expectedRevision`（非负整数）。畸形或含多个 JSON 值返回 400 `invalid_json`；非对象、未知字段或非法版本返回 422 `invalid_reconcile_request`；非 POST 返回 405（`Allow: POST`）。未配置状态文件返回 503 `state_unavailable`；Provider 未实现 `Observer` 时在加锁前返回 503 `observer_unavailable`；锁被占用返回 409 `state_locked`；状态文件不可读返回 500 `state_read_error`；`expectedRevision` 与当前版本不符返回 409 `state_conflict` 并携带 `currentRevision`，且不调用 `Observe` 或 `Apply`。

合法请求在同一把状态锁内读取确定版本，随后按地址 Unicode 升序逐一 `Observe` 已保存资源（空状态不调用 Observer），沿用漂移检测的快照校验、规范化与等价比较：远端缺失记为 `create`，远端存在但不同记为 `update`，一致记为 `noop`；`delete` 恒为 0。`update` 的 `before` 为观察快照、`after` 为保存快照；`create` 省略 `before`、`after` 为保存快照。全部观察成功后，按保存状态的确定拓扑顺序调用 `Apply`，使被依赖资源先执行；`noop` 不调用 `Apply`。

观察报错或结果非法时立即停止，分别返回 502 `observer_error` / `observer_invalid_result`，path 为 `/resources/{观察序号}`，且不执行任何变更。全部执行成功返回 200，携带按执行顺序排列的 `applied`、含 `create`/`update`/`delete`/`noop` 的完整 `summary`、原 `state` 与 `revision`。Provider 失败时立即停止且不回滚，返回 502 `provider_error`，path 为 `/changes/{执行序号}`，响应保留已成功的 `applied` 和完整 `summary`。任何结果都不写状态文件、不增加版本；锁覆盖读取、观察与执行并在所有结局下释放。

## 受控重试与幂等标识

Provider 可通过公开接口声明失败可重试：

```go
type RetryableError interface {
    error
    Retryable() bool
}
```

`POST /v1/apply`、`POST /v1/state/apply` 与 `POST /v1/state/reconcile`（含按资源类型分派的 Provider）对 `Apply` 返回的错误统一处理：普通错误或 `Retryable()` 为 false 时沿用现状，立即返回 502 `provider_error`；`Retryable()` 为 true 时就地重试当前变更，最多调用三次（`attempt` 依次为 1、2、3），期间不推进后续变更，也不并发处理同一变更。任一次成功后该变更只记入 `applied` 一次并按既有顺序继续；后续尝试变为不可重试错误时立即停止；三次均失败时使用最后一次错误文本和该变更的全局索引返回 `provider_error`。准备重试前若请求 context 已结束，不再调用 Provider，并以 `context.Err()` 的文本返回相同的 502 `provider_error`。

重试期间状态锁继续持有；成功重试的状态与版本效果等同于一次成功调用，最终失败仍沿用立即停止、不回滚、部分 `applied`、部分 state、原子提交与 revision 递增语义。只读入口与 `Observer` 不重试；路由预检、空计划、版本冲突、观察失败与请求校验不触发 `Apply`，行为不变。

## 多 Provider 分派

`server.HandlerWithProviders(providers)` 与 `server.HandlerWithProvidersAndStateFile(providers, path)` 接收 `map[string]server.Provider`，按资源类型把变更分派给对应 Provider；空类型键或 nil 值视为未注册，同一实例可服务多个类型。构造入口、健康检查、校验、排序、计划与只读 CLI 与单 Provider 模式完全一致。

`POST /v1/apply` 与 `POST /v1/state/apply` 仍产生全局确定顺序的变更流：create、update 按 `after.type` 路由（类型变化的 update 交给目标类型 Provider，并原样传递 `before`、`after` 与请求 context），delete 按 `before.type` 路由。校验及计划成功后先预检全部非 noop 变更；任一项类型未注册时返回 503 `provider_unavailable`，path 为 `/changes/{全局索引}`，不调用任何 Provider，也不改变状态文件或 revision。预检通过后按原顺序执行，Provider 失败仍返回 502 `provider_error`，沿用立即停止、部分 applied、部分 state、原子提交与版本递增语义。空计划无需注册表即可成功。

`GET /v1/state/drift` 与 `POST /v1/state/reconcile` 按已保存资源的 `type` 选择实例，并要求该实例实现 `Observer`。持有状态锁并读得确定 revision 后，按地址升序预检全部资源：缺少路由返回 503 `provider_unavailable`，不支持 `Observer` 返回 503 `observer_unavailable`，path 均为 `/resources/{升序索引}`；任一预检失败都不调用 `Observe` 或 `Apply`，也不写状态。全部通过后沿用单 Provider 模式的观察校验、漂移比较、拓扑执行、错误停止与响应结构，不按 Provider 分组重排资源；锁在所有结果下释放，未配置状态文件的行为不变。

## 验证

```bash
go test ./...
```

当前基线刻意不包含资源声明模型、依赖图与计划执行两阶段的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。
