# StackWeaver

这是一个面向基础设施即代码的基础设施即代码与云接口 SDK/Provider 平台。长期目标是提供资源声明与类型校验、依赖图与拓扑排序、计划与执行两阶段、状态文件与并发锁、漂移检测、Provider 多后端适配和审计导出，把基础设施编排沉淀为可复用平台。

仓库采用 Go，当前冻结基线提供进程健康检查与配置校验入口。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/stackweaver
```

服务默认监听 `127.0.0.1:8080`。可通过 `STACKWEAVER_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 配置校验

`POST /v1/configurations/validate` 接收同一 JSON 文档中的资源类型目录与资源声明，只返回校验结果，不创建或持久化任何资源。

根对象包含必填数组 `resourceTypes` 与 `resources`（两者允许为空，根对象不接受其他字段）：

- 类型项：非空且唯一的 `name` 与 `schema`。schema 递归支持 `string`、`integer`、`number`、`boolean`、`array`（需声明 `items`）、`object`（可声明 `properties`、`required`，`additionalProperties` 缺省为 `false`）。
- 资源项：非空且唯一的 `address`、已声明的 `type` 与 `properties` 对象；属性按对应 schema 递归校验。
- `integer` 不接受小数或指数写法，`number` 同时接受整数与小数，各类型不做隐式转换。

响应：

- `200`：`{"valid":true,"errors":[]}`
- `422`：文档可解析但不合约，`valid` 为 `false`，报告全部可独立确定的问题；每项错误含 `code`、`message` 与 RFC 6901 JSON Pointer `path`。错误按 `path` 再按 `code` 排序。
- `400`：空请求体、非法 JSON 或尾随第二个 JSON 值，错误码 `invalid_json`。
- `405`：其他方法，带 `Allow: POST`。

错误码：`invalid_document`（文档或 schema 结构非法、标识为空）、`duplicate_identifier`（类型名或资源地址重复，定位到后出现者）、`unknown_resource_type`、`type_mismatch`、`missing_required_property`、`unexpected_property`。非法或重复的类型不再触发引用资源的属性级错误。所有响应均为 `application/json`。

## 验证

```bash
go test ./...
```

当前基线不包含依赖图与计划执行两阶段的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。
