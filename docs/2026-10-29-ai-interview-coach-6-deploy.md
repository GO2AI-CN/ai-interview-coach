# 让 AI 面试官 7×24 在线：Go 服务部署与成本监控（六）

前四篇加第五篇，我们的 AI 面试教练已经能出题、追问、打分、读简历、开网页、进飞书、听语音。但它一直住在你的电脑里：关机就下线，断网就失联，朋友想玩还得等你开机。demo 和产品的区别，往往就差这最后一公里。

今天这篇聊部署：一台低配云服务器 + 一个 systemd 单元，让机器人 7×24 在线；再加一套 token 用量统计，让「每场面试花多少钱」变成看得见的数字。

## 一、服务器选型：先算资源账，再买机器

部署第一件事不是买服务器，是算资源账。这个 bot 的资源画像非常清晰：

| 资源 | 消耗在哪 | 量级 |
|---|---|---|
| CPU | LLM 调用全部走 API，本地不算模型 | 几乎闲置 |
| 内存 | Go 运行时 + 会话表 | 空载几十 MB 量级 |
| 网络 | 飞书长连接（WebSocket 客户端）+ API 请求 | 出口流量很小 |
| 磁盘 | 二进制 + 日志 | 忽略不计 |

结论：**最便宜的入门款（1 核 1G）绰绰有余**。关键认知是长连接模式——bot 是 WebSocket **客户端**，主动连飞书，不监听端口，不需要公网 IP，安全组全封也能跑。如果哪天改成 Webhook 模式才需要考虑域名、HTTPS、备案那一套。

## 二、systemd 守护：三行配置管生死

Go 编译出单个静态二进制，拷到服务器就能跑，没有运行时依赖。但「能跑」和「能活」是两回事：进程 panic 了怎么办？服务器重启了怎么办？这些不用自己写守护循环，systemd 三行配置解决：

```ini
[Service]
ExecStart=/opt/ai-interview-coach/coach bot
Restart=always        # 进程退出（含 panic、OOM 被杀）就拉起
RestartSec=5          # 5 秒后再起，避免崩溃风暴
EnvironmentFile=/opt/ai-interview-coach/coach.env  # 秘钥走环境文件，权限 600
```

完整 unit 文件在仓库 `deploy/coach-bot.service`，包含 `After=network-online.target`（等网络就绪再启动，否则开机自启时连不上飞书）和独立运行用户。安装就四条命令：

```bash
sudo cp deploy/coach-bot.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now coach-bot   # enable=开机自启，now=立刻启动
```

## 三、日志：别落地文件，交给 journald

很多部署教程教你配 logrotate 轮转文件日志。对单一小服务来说，这是过度设计——systemd 已经内置了日志收集：

```bash
journalctl -u coach-bot -f          # 实时跟踪，排障第一命令
journalctl -u coach-bot --since today  # 今天的日志
journalctl -u coach-bot -p err      # 只看错误级别
```

我们的代码里所有异常路径都有 `log.Printf`（长连接断开重连、语音下载失败、消息处理失败），配合 journald 的持久化和按级别过滤，足够支撑这个量级的服务。等真有上规模的那一天，再上结构化日志和 Loki 不迟。

## 四、成本监控：每次面试烧多少 token

部署上线后，一个新的焦虑出现了：别人天天拿我的 bot 面试，账单会不会爆炸？AI 应用和 traditional 服务最大的区别就是**每次请求都在花钱**，所以成本监控必须是一等功能，不能后补。

好消息是 OpenAI 兼容协议的每个响应都带 `usage` 字段，供应商已经替我们数好了。统计逻辑极简：全局原子计数器累加，会话开始时取快照，结束时求差值：

```go
// 进程级累计计数器：并发安全，随报告输出
var globalUsage struct {
	prompt, completion atomic.Int64
}

func TotalUsageSnapshot() Usage { /* 读两个计数器返回快照 */ }

// 累加一次请求的用量（来自响应里的 usage 字段）
globalUsage.prompt.Add(resp.Usage.PromptTokens)
globalUsage.completion.Add(resp.Usage.CompletionTokens)
```

有一个流式输出专属的坑要单独说：**SSE 流式的 usage 默认不返回**，要显式开启，而且只在最后一个 chunk 里：

```go
params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
	IncludeUsage: param.NewOpt(true), // 最后多发一个带 usage 的 chunk
}
// 循环里捕获：chunk.JSON.Usage.Valid() 为 true 的即最终用量
```

算出数字之后，机器人会在每场面试的报告末尾自动附上：「本场 LLM 消耗：X tokens（输入 Y / 输出 Z）」。为什么是 tokens 而不是人民币？因为单价随供应商和模型而变，代码里写死价格就是埋雷——tokens 乘上你自己供应商的单价，才是真成本。经验上输入 token 是大头（题面、评估上下文、rubric 每次请求都要带），这也是第一篇文章坚持「计划一次性生成」的隐性收益。

## 五、上线检查清单

最后是这个系列的传统艺能，收藏这张表就够：

| 维度 | 检查项 | 本篇方案 |
|---|---|---|
| 进程守护 | 崩溃/重启能否自愈 | `Restart=always` + `enable` 开机自启 |
| 网络时序 | 开机时网络未就绪 | `After=network-online.target` |
| 秘钥管理 | Key 不进命令行历史/日志 | `EnvironmentFile` + 文件权限 600 + 专用运行用户 |
| 可观测 | 出错能不能定位 | journald 集中日志 + 代码全路径 `log.Printf` |
| 成本可控 | 用量是否可见 | 响应 usage 累计，报告末尾附本场消耗 |

## 六、总结

五篇做功能，这篇做工程。回头看，让一个 AI Agent 从 `go run` 到 7×24 在线，真正要写的新代码只有用量统计那二十来行——其余全是「选对的现成件」：systemd 管生死，journald 管日志，OpenAI 协议的 usage 字段管成本。**AI 应用开发的工程能力，很多时候就是知道哪些轮子不用自己造。**

下一篇给机器人加记忆：每场面试的问答和评分落进 SQLite，飞书里发「历史」就能回看报告、看薄弱点的进步曲线——`modernc.org/sqlite`，纯 Go 无 CGO，和第二篇的思路一脉相承。

如果对你有帮助，欢迎点赞、在看、转发，关注「GO语言入门之路」，系列更新不迷路。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
