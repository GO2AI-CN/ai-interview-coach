# 把面试教练的历史库包装成 MCP 服务（上）

**没看过「AI 模拟面试教练」系列也不影响本文。** 你只需要知道一件事：那个项目每做完一场模拟面试，都会把评分报告和逐题问答存进一个本地 SQLite 文件。数据躺在库里很安全，但大模型够不着它——Agent 想帮你分析「最近三次面试哪类题总丢分」，只能干瞪眼。

系列第一篇说过，MCP 的本质是「让 Agent 安全地够到外部资源」。今天我们就把这个沉睡的 SQLite 库包装成一个 MCP 服务，让任何支持 MCP 的 Agent（Claude Desktop、Cursor、自研 Agent）都能查询面试历史。

## 一、设计决策：只读消费者，schema 即契约

第一个问题：MCP server 要不要 import 项目里的 `internal/store` 包？**不要。** 业务包是为面试机器人设计的，方法都带会话参数；而 MCP 服务是一个独立的只读消费者。它应该只依赖两件东西：已落库的 schema（两张表的结构，系列一第七篇写过）和 SQLite 文件路径。

```
mcp/coachstore  ──只读查询──▶  coach.db（interviews / answers 两张表）
      ↑ 不依赖
internal/store（业务写入方）
```

这个「消费者拥有自己读模型」的决策带来两个好处：MCP 服务和业务代码可以独立演进，且服务崩了不影响面试主流程。既然是只读消费者，就要把只读做到底——双保险：

```go
func openDB(path string) (*sql.DB, error) {
    db, err := sql.Open("sqlite", path)
    // ...
    db.SetMaxOpenConns(4)          // 只读查询可并发；写入方才需要单连接串行化
    for _, p := range []string{
        "PRAGMA busy_timeout=5000", // 与正在写入的教练进程撞锁时等 5s
        "PRAGMA query_only=ON",     // SQLite 层面物理禁写
    } { /* exec... */ }
    return db, nil
}
```

`query_only` 是硬承诺：即使模型幻觉出再奇怪的参数，工具也写不坏历史库。我们的测试里直接执行 `DELETE FROM interviews` 会被 SQLite 以 readonly 拒绝——这条测试比任何注释都有说服力。

## 二、三个查询工具：职责怎么切

工具设计跟 API 设计同一条原则：**一个工具只做一件事，返回越小越好**（工具结果会整个塞进模型上下文）。

- `list_sessions`：最近场次列表，回答「我面过几场、都是什么岗位」；
- `get_session`：某场逐题指标（维度/置信度/覆盖度/追问次数），回答「哪类题总被追问」，特意**不返回答案原文**——那是报告的职责；
- `get_report`：按 ID 取完整 Markdown 报告，回答「详细评语和改进建议是什么」。

列表工具的参数有个细节值得学：limit 不做硬校验，而是**纠偏**——

```go
limit := in.Limit
if limit <= 0 { limit = defaultLimit }  // 没传或传错，用默认 10
if limit > maxLimit { limit = maxLimit } // 超上限截到 50，模型不用重试

rows, err := s.db.Query(
    `SELECT id, chat_key, position, overall, created_at
     FROM interviews ORDER BY id DESC LIMIT ?`, limit)
if err != nil {
    return "", fmt.Errorf("查询失败: %w", err)
}
defer rows.Close()
// 逐行拼文本：工具结果要克制，一行一个场次
```

模型的参数是生成的，不是人填的，`limit=-5` 直接报错只会让模型无谓重试一轮。对模型的「差一点」宽容，是工具设计里被低估的美德。

## 三、收藏：现有数据源 → MCP 工具五步清单

把一个已有的数据源（数据库、文件、内部 API）包装成 MCP 工具，套路是固定的：

| 步骤 | 要点 | 本文的落点 |
|---|---|---|
| 1. 定边界 | 读写还是只读？只读就物理禁写 | `PRAGMA query_only=ON` |
| 2. 选耦合面 | 依赖 schema 而不是业务代码 | 直接 SQL 查两张表 |
| 3. 切工具职责 | 一个工具一件事，返回可控 | 列表 / 指标 / 全文 三件套 |
| 4. 宽容参数 | 纠偏优于报错，limit 截断 | `limit<=0` 回默认、超 50 截断 |
| 5. 错误引路 | 业务错误文本里告诉模型下一步 | 「可先调用 list_sessions 查看可用 ID」 |

第 5 步是 Agent 时代的特色：错误信息不是给人看的日志，而是给模型看的**下一步指令**。实测调用 `get_report` 传了个不存在的 ID，返回的是业务错误（`isError: true`）：

```json
{"jsonrpc":"2.0","id":4,"result":{
  "content":[{"type":"text","text":"场次 #99 不存在，可先调用 list_sessions 查看可用 ID"}],
  "isError":true}}
```

模型读到这段话，自己就会回头调 `list_sessions` 拿正确 ID——这就是上篇说的「恢复策略留给模型」。

## 四、实测：四连请求看输出

对着编译好的二进制（`COACH_DB_PATH=./coach.db`）发四个请求，输出全部符合预期：

1. `tools/list` → 三个工具，schema 完整；
2. `list_sessions` → `#2 | 2026-11-02T16:05:28+08:00 | SRE | 综合 91 分 | 会话 feishu:g-1` 新→旧排列；
3. `get_session`（id=1）→ 表头 + 三行逐题指标：`第3题 [工程素养] 置信度3 覆盖度50% 追问2次 | 如何定位线上 goroutine 泄漏？`——哪类题被追问最多，一眼可见；
4. `get_report`（id=99）→ `isError: true` + 引路文案。

测试照旧两层：8 个单测（含 query_only 禁写、limit 截断、错误引路的契约断言）+ 1 个编译二进制走 stdio 的端到端测试，全绿。夹具数据用 `internal/store` 的 `Save` 写入、经本服务的 SQL 读出——两个包对 schema 的理解不一致会当场暴露，等于免费做了一轮契约测试。另外踩了个 Windows 专属的小坑：`go run` 起的进程在测试里 kill 不掉父进程背后的编译产物，句柄不释放会导致临时目录清理失败，所以端到端测试改成先 `go build` 出二进制再直接运行它——跨平台的测试代码，这类平台差异值得进 checklist。

至此，面试历史对 Agent 开放了读通道。但工具摆在那儿，Agent 不会自己知道什么时候调——**下篇我们写 MCP client 侧：用 Go 实现函数调用循环（function calling loop），让大模型看着 tools/list 的结果，自己决定调哪个工具、传什么参数、拿到结果后怎么继续聊。** 那才是「面试官 Agent 记得每一位候选人」的最后一块拼图。

觉得有用的话，欢迎点赞、在看、转发，关注「GO语言入门之路」，下篇见。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
