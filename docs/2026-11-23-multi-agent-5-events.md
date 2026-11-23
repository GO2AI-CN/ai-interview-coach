# Agent 空转轮询太蠢？给黑板装上事件通知

上一篇的黑板 demo 里藏了一个偷懒：主流程用 `sync.WaitGroup` 写死「等研究员和压测员都干完，顾问才读板」。听起来合理，细想很浪费——压测员的 QPS 数据一旦贴上板，结论已经可以出了，顾问却还得等研究员把无关的便签写完。

真实场景更糟：假如第三棒等的是一个可能永远不来的条件（比如等某个 Agent 报错重试），WaitGroup 连「等不到怎么办」都表达不了。本文给黑板装上事件通知，让等待的 Agent 睡得着、醒得来、等不到还能体面收场。

## WaitGroup 是粗粒度同步，不是协作

先把问题说清楚。WaitGroup 解决的是「所有 goroutine 都结束」这一种同步需求，而 Agent 协作里的等待五花八门：

- 顾问要等的不是「压测员下班」，而是「压测分区出现 QPS 数据」——条件同步；
- 编排者等任一 Worker 报错就熔断——或语义同步；
- 等 30 秒没结果就降级用缓存——超时语义。

用 WaitGroup 硬凑这些语义，等于让所有人陪跑。正确的抽象是**条件等待**：订阅一个条件，满足或超时之前，线程阻塞不占 CPU。

各种同步需求该用什么原语，先给一张选型表备查：

| 同步需求 | 推荐原语 | 反模式 |
| --- | --- | --- |
| 所有并行任务收尾 | `sync.WaitGroup` | 用它表达条件等待 |
| 等待某数据上板 | 黑板订阅 + `WaitFor` | 定时轮询 `Section()` |
| 任一结果可用即继续 | `select` 多通道 | 串行等每个任务 |
| 有超时的等待 | `context.WithTimeout` | 裸 chan + time.Sleep |

## 订阅机制：信号不携带内容

通知系统有个经典设计选择：事件里带不带上板数据？我们选不带。`Pin` 写完便签后，只向该分区的订阅者发一个**空信号**，订阅者收到信号后自己调用 `Section()` 去读最新内容：

```go
// Pin 往指定分区贴一张便签，写完后向订阅者发信号（非阻塞，信号可缓存）。
func (b *Blackboard) Pin(section, author, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.board[section] = append(b.board[section], Note{Author: author, Content: content})
	for _, ch := range b.subs[section] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
```

这个「pull 而非 push」的设计有两个好处。一是天然防竞态：信号发出后、订阅者醒来前，可能又有别人贴了便签，订阅者一次 `Section()` 读到全部增量，不会丢；二是订阅者拿到的永远是读锁保护下的完整快照，不用自己拼增量。

通道缓冲设为 1 并配合非阻塞发送：信号只表示「有新东西，醒来看看」，攒一个就够，攒一百个没有意义，发不进去直接丢弃即可。

## WaitFor：先查现状，再等通知

有了订阅，条件等待的核心逻辑就十几行。但有一个经典竞态必须防：条件在**订阅之前**就已经满足（便签已上板，信号在订阅前发完了）。解法是先查一次现状，不满足才进入等待循环：

```go
// WaitFor 事件驱动地等待某分区满足条件 cond：
// 先检查一次现状，不满足就阻塞等通知，被叫醒后再查，直到满足或 ctx 取消。
func WaitFor(ctx context.Context, bb *Blackboard, section string, cond func([]Note) bool) error {
	sub := bb.Subscribe(section)
	defer sub.Close()
	for {
		if cond(bb.Section(section)) { // 先查现状：避免"贴完才订阅"漏掉
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err() // 超时/取消，把责任交还给调用方
		case <-sub.C():
		}
	}
}
```

注意这个函数的两个工程细节。`defer sub.Close()` 保证退订——不退订，信号通道会越攒越多，每次 `Pin` 都白发一轮通知；返回 `ctx.Err()` 而不是默默吞掉超时，让调用方自己决定是降级、重试还是报错。

## 实测：睡着被叫醒，等不到有兜底

demo 模拟完整时序：顾问 goroutine 里调用 `WaitFor`，条件是「压测分区出现带 QPS 的便签」；主流程 300 毫秒后贴出压测数据，顾问应声而起。随后又演示了反例——等一个永远不会来的条件，靠 `context.WithTimeout` 在 200 毫秒后体面收场。完整时序如下：

```text
0ms    顾问调用 WaitFor：订阅「压测」分区，先查现状
1ms    条件不满足，阻塞在 select，goroutine 挂起不占 CPU
300ms  压测员 Pin QPS 数据，Pin 内向订阅者发信号
301ms  顾问被叫醒，重查条件：满足，WaitFor 返回 nil
500ms  反例：等不到的条件由 ctx 超时收场，返回 deadline exceeded
```

实际运行的打印输出与时序完全一致：等待的 300 毫秒里主 goroutine 没有任何轮询动作，「顾问被事件叫醒」紧随 Pin 发生；反例则干净地返回 `context deadline exceeded`，没有 goroutine 泄漏。对比上一版的 WaitGroup 写法：即使压测数据第 1 毫秒就上板，顾问也要等到研究员写完所有便签才能动；现在 latency 取决于条件何时满足，而不是最慢的那个 Worker。

## 踩坑记录：通知系统的三个细节

**第一，信号可能重复、可能丢失，条件判断必须幂等。** 连续贴两张便签，订阅者可能只收到一个信号；信号通道满了会被丢弃。所以醒来后要重新跑完整条件判断，不能假设「一个信号 = 一条新便签」。我们的实现里 `WaitFor` 每轮都全量检查，天然幂等。

**第二，条件函数要保持轻量。** 它会在每次醒来时执行，里面如果又发起大模型调用，就把事件驱动退化成同步阻塞了。条件只读黑板、做字符串或计数判断，真正的重活放到 `WaitFor` 返回之后。

**第三，超时不等于失败。** `ctx.Err()` 交给调用方处理：可以降级用现有数据先出草稿结论，也可以把待办挂进下一棒。超时策略我们还会在第 7 篇展开——那里会给整条协作链路上熔断与降级。

还有一个和 Go 运行时相关的细节值得一提：被 `WaitFor` 挂起的 goroutine 不占线程，数百万个阻塞中的协程也只吃内存不吃 CPU，这和操作系统线程的代价完全不同。所以在 Agent 系统里「为一个等待条件起一个 goroutine」是廉价操作，可以放心把等待逻辑从主流程里拆出来，每个关注不同条件的 Agent 各等各的，互不拖累。这也是事件驱动相比轮询在资源开销上的本质优势：轮询的成本随 Agent 数量线性增长，而休眠等待几乎为零。

完整代码在仓库的 `multi/events.go` 和 `cmd/multi-events-demo/`，`go run ./cmd/multi-events-demo` 复现输出。

## 小结与预告

至此 multi 包集齐了三种协作原语：主从编排（Orchestrator）、接力交接（Handoff）、共享黑板（Blackboard）加事件驱动（Subscribe/WaitFor）。调度问题解决之后，下一个问题是质量：多个 Agent 各写一段，整条链路跑完，**你怎么知道它干得对不对？**

**下篇预告：《Agent 干得好不好？给协作链路加上轨迹评测》**，我们给每次协作记录运行轨迹，用规则加评分模型给链路打分，让问题可回放、可归因。

完整工程代码已开源：[github.com/GO2AI-CN/ai-interview-coach](https://github.com/GO2AI-CN/ai-interview-coach)，欢迎 Star 关注后续更新。

如果这篇文章对你有启发，欢迎点赞、在看、转发，关注「Go2AI之路」，第一时间获取系列更新。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
