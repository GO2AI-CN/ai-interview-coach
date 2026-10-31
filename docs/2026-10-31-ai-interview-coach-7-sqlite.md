# AI 模拟面试教练（七）：给面试官装上记忆，SQLite 历史落库

上周你用我们的 AI 面试官面了 Go 后端工程师，拿了 65 分；这周又面了一场，78 分。进步了，但进步在哪？反复栽跟头的还是不是同一个维度？——报告躺在聊天记录里越滚越远，这些问题根本答不上来。

答案是：让面试官有记忆。本篇给机器人加上 SQLite 落库：每场面试的问答明细、评估、评分报告全部入库，飞书里发「历史」就能回看任何一场。

## 落库时机与依赖方向：一次可选的能力注入

先定两个设计原则，都是产品级 Agent 的经验之谈：

1. **落库是副作用，绝不能影响主流程**。报告生成成功但落库失败时，用户手里的报告一文不少，只在末尾附一行警告。
2. **接口定义在消费侧，而不是实现侧**。`Store` 接口由 `bot` 包定义，`internal/store` 去实现它——这样 bot 不 import 任何具体存储，传 `nil` 就是整体关闭历史功能。这条依赖方向让「换 PostgreSQL」变成只新增一个包的事。

落库点选在 `Finish` 之后两处：正常面完和「结束」指令提前结束，走的是同一个 `saveHistory`。报告由 `report.Markdown()` 生成后，连同状态机里的整场记录一起交给 Store：

```go
// saveHistory 报告落库。失败不阻塞回复（报告正文已经发出），只附一行警告。
func (b *Bot) saveHistory(ctx context.Context, key string, s *session, reportMD string) string {
	if b.store == nil {
		return ""
	}
	rec, ok := s.it.(Recorder) // 状态机可选实现：交出整场记录
	if !ok {
		return ""
	}
	pos, overall, turns := rec.Record()
	if _, err := b.store.Save(ctx, key, pos, overall, reportMD, turns); err != nil {
		return "\n\n（⚠️ 本场历史落库失败：" + err.Error() + "）"
	}
	return "\n\n本场记录已存入历史，回复「历史」随时回看。"
}
```

注意 `Recorder` 是个可选接口：bot 对状态机的依赖面（`Interview` 接口）一个方法没加，想落库的状态机实现 `Record()` 即可，测试里的 fake 不实现也照常跑。

## 表结构设计：两张表，一个取舍

表结构刻意保持简单——两张表起步，**报告 Markdown 整段存 `interviews.report_md`，不拆维度表**：

| 表 | 字段 | 设计理由 |
|---|---|---|
| interviews | id / chat_key / position / overall / report_md / created_at | 一场一行；chat_key 隔离不同会话；报告整段存，给人读的文档不做聚合分析 |
| answers | interview_id / seq / dimension / question / answer / confidence / coverage / followups | 一题一行，按 (interview_id, seq) 有序；维度、置信度、覆盖度留作以后的对比分析素材 |
| 索引 | idx_interviews(chat_key, created_at DESC)、idx_answers(interview_id) | 「历史」列表按会话+时间倒序取，高频查询必须走索引 |

「报告不拆维度表」值得展开一句：报告本质是给人阅读的文档，不是要做 BI 聚合的数据。把它拆成维度分数表，唯一的收益是「按维度查历史均分」——这个需求真出现时再加一张统计表即可，别为想象中的需求预付设计复杂度。

SQLite 驱动选 `modernc.org/sqlite`（v1.61.0，写本文时核对的最新版）：纯 Go 实现，**不需要 CGO**。意味着交叉编译不用装 gcc、Docker 镜像可以直接用 `alpine`/`distroless`，部署体验和无 CGO 的简历解析包（ledongthuc/pdf）一脉相承。

## SQLite 并发：单写者的坑，一行配置填平

SQLite 的经典限制：**同一时刻只允许一个写入者**。我们的 bot 每个会话虽然有 mutex 串行化，但 Store 是进程级共享资源，多个会话同时落库时写入方可能互撞 `SQLITE_BUSY`。

与其在代码里写重试，不如在连接池这一层直接串行化：

```go
db, err := sql.Open("sqlite", path)
// 所有读写排队走单连接，从根上消灭并发写互撞
db.SetMaxOpenConns(1)
for _, p := range []string{
	"PRAGMA journal_mode=WAL",  // WAL：读写不互堵，崩溃可恢复
	"PRAGMA busy_timeout=5000", // 撞锁等 5s 而不是立刻报错
	"PRAGMA foreign_keys=ON",
} {
	if _, err := db.Exec(p); err != nil { /* 处理错误 */ }
}
```

`MaxOpenConns(1)` 把所有读写串成单连接队列，配合 `busy_timeout` 兜底，调用方完全不用感知并发。代价是吞吐——一个面试机器人的写入量，离 SQLite 的天花板还差着几个数量级。这个取舍我们有对应测试背书：20 个 goroutine 并发 `Save`，全部成功、一条不丢。

## 「历史」指令：IM 里的列表交互怎么做

IM 和网页不同：没法渲染可点击的表格，交互全靠纯文本对话。经典做法是**列表 + 序号缓存**——先发带序号的列表并缓存在机器人内存里，用户回复序号即定位：

```
📋 最近 5 场面试：
1. SRE · 综合 91 分 · 10-31 09:20
2. Go 后端工程师 · 综合 80 分 · 10-30 18:22

回复「历史 序号」查看对应完整报告。
```

```go
// 「历史」→ 查库、缓存列表、发序号列表
items, _ := b.store.Recent(ctx, key, 5)
b.history.Store(key, items)
// 「历史 2」→ 从缓存按序号取，再用 ID 查完整报告
item := v.([]HistoryItem)[n-1]
rep, err := b.store.Report(ctx, key, item.ID)
```

另一个容易忽略的细节是**越权防护**：`Report` 查询带 `AND chat_key = ?` 条件，A 会话没法通过猜 ID 看到 B 会话的报告。机器人处理的是多人消息，这类防护在一开始就要做进去，而不是等出事再补。

还有个数据一致性的坑：用户「结束」提前打断时，最后一题可能已作答、已评估，但还挂在状态机的 `Pending` 字段里没归档。`Record()` 实现时要把 `Pending`（如有作答）一并入库，否则报告和问答明细对不上账。

## 测试：fake 验证行为，真库验证 SQL

新增两类测试，各司其职：

- **bot 包用内存 fakeStore**：验证「落库时机、失败警告、无 Recorder 不落库、历史列表与选号」这些**行为契约**，不碰数据库；
- **store 包用临时文件真库**（`t.TempDir()` 下的真实 SQLite）：验证 SQL 本身——会话隔离、倒序、limit、**重开进程数据还在**、以及前面说的并发写串行化。

两类加起来 12 个用例，`go test ./...` 依旧零外部依赖、秒级完成。

## 小结与预告

至此，这个系列的产品闭环基本完整：出题、追问、打分 → 简历解析 → Web/飞书双端 → 语音 → 7×24 部署与成本监控 → 历史落库。一个可以从零跟到部署上线的 Go + LLM 项目。

下一篇是系列收官：架构演进全回顾 + 「fork 改造成你自己的 Agent」路线图——面试教练改成英语口语陪练、公务员结构化面试，各要动哪几层。我们下篇见。

觉得有用的话，点个「赞」和「在看」，转发给正在准备面试的朋友，关注「Go语言入门之路」不走丢。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
