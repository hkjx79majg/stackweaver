# StackWeaver

这是一个面向基础设施即代码的基础设施即代码与云接口 SDK/Provider 平台。长期目标是提供资源声明与类型校验、依赖图与拓扑排序、计划与执行两阶段、状态文件与并发锁、漂移检测、Provider 多后端适配和审计导出，把基础设施编排沉淀为可复用平台。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/stackweaver
```

服务默认监听 `127.0.0.1:8080`。可通过 `STACKWEAVER_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 配置校验与依赖排序

- `POST /v1/configurations/validate`：校验配置文档的结构与类型，不创建或持久化任何资源。
- `POST /v1/configurations/order`：先执行与 validate 相同的基础校验；基础配置有效后解释每个 resources 条目的可选 `dependsOn`（非空资源地址字符串数组），返回确定的拓扑顺序 `order`（依赖在前；可选节点中地址按 Unicode 码点最小者优先）。依赖引用错误（`unknown_dependency`、`self_dependency`、`duplicate_dependency` 及字段形状错误）一次性收集，按路径再按错误码排序；依赖字段合法但图中有环时返回唯一一个 `dependency_cycle` 错误，路径为 `/resources`，消息列出所有实际处于环中的地址。该端点同样无副作用、不持久化状态。

非 POST 方法返回 `405` 与 `Allow: POST`；空请求体、非法 JSON 或多个 JSON 值返回 `400 invalid_json`。

## 验证

```bash
go test ./...
```

当前已提供资源声明模型、结构与类型校验，以及依赖图拓扑排序；计划执行两阶段等后续能力仍按独立题目从已冻结事实出发逐步实现。
