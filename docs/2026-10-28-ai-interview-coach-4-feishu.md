# 把 AI 面试官接进飞书：Go 长连接机器人实战（四）

前三篇我们把一个 AI 面试教练从命令行做到了网页版。但有个场景一直没覆盖：通勤路上、排队间隙，这种碎片时间才是刷题模拟的高发场景——而碎片时间的入口不是浏览器，是 IM。

今天这篇把它接进飞书：在群里 @机器人 说一句「面试 Go 后端工程师」，就能开始一轮模拟面试，答完自动出评分报告。全程**不需要公网 IP、不需要买服务器**，本地 `go run` 就能跑。

## 一、为什么是飞书长连接

给机器人找消息入口，主流方案其实就三个：

| 方案 | 合规性 | 接入成本 | 结论 |
|---|---|---|---|
| 微信个人号协议库 | ❌ 违反微信用户协议，封号风险 | 低 | 直接排除，不碰 |
| 公众号消息接口 | ✅ 官方 | 客服消息有 48 小时会话窗口，多轮面试会被卡死 | 不适合本产品 |
| 飞书开放平台 | ✅ 官方 | 官方 Go SDK + 长连接模式 | ✅ 主选 |

（企业微信也有几乎一致的官方 API，本文思路完全复用，文末说。）

飞书的事件订阅有两种模式：**Webhook** 和**长连接**。区别是本文最关键的技术决策：

| | Webhook 模式 | 长连接模式 |
|---|---|---|
| 你需要提供 | 公网可访问的回调 URL | 什么都没有 |
| 需要配置 | 域名、HTTPS、验签 | 零配置 |
| 本地调试 | ngrok 内网穿透 | `go run` 直接跑 |
| 重连保活 | 自己实现 | SDK 内置 |

Webhook 那套是传统的"平台推给你"，长连接是你主动和飞书建一条 WebSocket，事件顺着这条连接推过来。**对开发者和教程读者来说，长连接把"部署"这个最大的劝退点直接干掉了**——这也是选它做选题的原因。

## 二、10 分钟建好飞书机器人（配置清单）

代码之前，先在[飞书开放平台](https://open.feishu.cn)把应用建好，一共 6 步：

1. **创建企业自建应用**，记下 `App ID` 和 `App Secret`
2. **权限管理**里添加两个权限：`im:message`（发消息）、`im:message.receive_v1`（收消息）
3. **事件订阅**里把订阅方式改成「使用长连接接收事件」，添加事件 `im.message.receive_v1`（接收消息）
4. **应用能力**里启用「机器人」
5. **版本管理与发布**：创建一个版本并发布（企业自建应用管理员审批即可，几分钟通过）
6. 把机器人拉进一个测试群，或者在飞书里直接跟它单聊

这套配置以后做任何一个飞书机器人都是同一套，建议收藏。

## 三、架构：IM 只是一层 I/O 适配器

这是本系列一直在贯彻的工程决策，这篇体现得最彻底：

```
飞书群消息 → onP2MessageReceive（过滤/提文本/剥@）
                    ↓
              bot.HandleText（指令解析 + 会话路由）   ← 本层新增
                    ↓
              coach.Interview 状态机               ← 前三篇原样复用
              （出题 / 评估 / 追问 / 报告）
```

前三篇写的面试状态机**一行不改**。新增的 `internal/bot` 只做三件事：

- **会话路由**：用 `chat_id + open_id` 做 key，多人同群互不干扰，一人同时只进行一场面试
- **指令解析**：`面试 <岗位>` 开考、`结束` 提前交卷、其余纯文本当作答
- **文本翻译**：把状态机的「评估 + 追问/下一题」拼成一条 IM 回复

为此定义了三个小接口把边界锁死（`internal/bot/bot.go`）：

```go
// Sender 把回复文本发回 IM。真实实现走飞书 OpenAPI，测试时替换为 fake。
type Sender interface {
    Reply(ctx context.Context, messageID, text string) error
}

// Interview 是 bot 对状态机的最小依赖面，便于用 fake 单测。
type Interview interface {
    Question() string            // 当前问题；答完返回 ""
    Answer(ctx context.Context, answer string) (followUp bool, feedback string, err error)
    Finish(ctx context.Context) (string, error) // Markdown 评分报告
}
```

`coach.Interview` 通过十几行的适配器（`coachInterview`）实现这个接口，真实实现和单测用的脚本 fake 可以互换——这也是第三篇立下的规矩：**没有真实凭据也能跑全链路测试**。

## 四、核心代码：三段就够了

**第一段：启动长连接**。官方 SDK `larksuite/oapi-sdk-go/v3`（本文写作时 v3.12.2，`go get` 时以最新为准）已经把重连保活全部内置：

```go
// Run 启动飞书长连接（WebSocket）。阻塞运行，ctx 取消即退出。
func (b *Bot) Run(ctx context.Context, appID, appSecret string) error {
    // 长连接下事件不经公网回调，校验 token / 加密 key 传空串即可
    disp := dispatcher.NewEventDispatcher("", "")
    disp.OnP2MessageReceiveV1(b.onP2MessageReceive)
    cli := larkws.NewClient(appID, appSecret, larkws.WithEventHandler(disp))
    cli.SetOnReady(func() { log.Println("[bot] 飞书长连接已就绪") })
    cli.SetOnReconnecting(func() { log.Println("[bot] 断开，重连中…") })
    return cli.Start(ctx) // 阻塞；断线 SDK 自动重连
}
```

**第二段：事件入口**。四类过滤——忽略机器人自己发的消息（防自循环）、只收文字、群聊只响应 @、剥掉 `@_user_1` 这种提及占位符：

```go
func (b *Bot) onP2MessageReceive(ctx context.Context, e *larkim.P2MessageReceiveV1) error {
    msg := e.Event.Message
    // 1) 忽略机器人消息，防自循环；2) 只处理文字
    if *e.Event.Sender.SenderType != "user" || *msg.MessageType != "text" {
        return nil
    }
    text, _ := extractText(*msg.Content) // {"text":"@_user_1 面试 Go"}
    if *msg.ChatType == "group" && !mentionRE.MatchString(text) {
        return nil // 群聊只响应 @ 机器人
    }
    text = strings.TrimSpace(mentionRE.ReplaceAllString(text, ""))
    // 会话 key：群/单聊 ID + 发送者，两人同群互不干扰
    key := *msg.ChatId + "|" + *e.Event.Sender.SenderId.OpenId
    reply, err := b.HandleText(ctx, key, text)
    if err != nil || reply == "" {
        return err
    }
    return b.sender.Reply(ctx, *msg.MessageId, reply)
}
```

**第三段：指令 + 会话路由**（省略细节后的骨架）：

```go
func (b *Bot) HandleText(ctx context.Context, key, text string) (string, error) {
    if pos, ok := strings.CutPrefix(text, "面试 "); ok { // 开考
        it, err := b.factory(ctx, strings.TrimSpace(pos))
        b.sessions.Store(key, &session{it: it, expireAt: b.now().Add(b.ttl)})
        return "面试计划已生成：" + it.Question(), nil
    }
    s := b.load(key) // 惰性过期检查
    if s == nil {
        return "输入「面试 <目标岗位>」开始", nil
    }
    s.mu.Lock() // 同一人多条消息串行化
    defer s.mu.Unlock()
    followUp, feedback, err := s.it.Answer(ctx, text)
    if followUp || s.it.Question() != "" {
        return feedback, nil // 追问中 / 还有下一题
    }
    b.sessions.Delete(key)
    rep, _ := s.it.Finish(ctx)
    return feedback + "\n\n面试结束 🎉 评分报告：\n\n" + rep, nil
}
```

发送侧就是调 OpenAPI 的「回复消息」接口，`msg_type: text`，内容 JSON `{"text":"..."}` 序列化后塞进 `Content` 字段，SDK 会处理转义和 token 刷新，十几行。

## 五、踩坑清单

1. **`@_user_1` 占位符**：群聊消息正文里 @ 不是昵称，是 `@_user_数字` 占位符，不过滤掉会被当成岗位名的一部分
2. **自循环**：机器人自己发的消息也会触发消息事件，必须按 `sender_type != "user"` 过滤，否则它会回复自己的回复直到限流
3. **并发乱序**：同一个人连点几条消息，评估顺序就乱了。每个会话一把 `sync.Mutex` 串行化，状态机不用做任何线程改造
4. **会话泄漏**：人走了会话还在。给每个会话加 2 小时闲置 TTL，惰性检查（收到消息时才看过期没），不用起定时器
5. **IM 里没有 SSE**：网页版逐字流式的效果在 IM 里做不了。折中方案是回复里带即时评估，想要更好的体验可以把「思考中…」先发出去再 patch 更新（用更新消息接口），或改用消息卡片承载报告

## 六、跑起来

```bash
export OPENAI_API_KEY=你的Key
export FEISHU_APP_ID=你的AppID
export FEISHU_APP_SECRET=你的AppSecret
go run ./cmd/coach bot
```

终端打印「飞书长连接已就绪」后，去飞书里 @机器人：`面试 Go 后端工程师`。完整代码（含 10 个单测，Mock 驱动全链路）在 `https://github.com/LittleMoreInteresting/ai-interview-coach`，`go test ./...` 全绿。端到端那一段需要你自己按第二节建个应用跑一遍——这大概是全系列里门槛最低的动手环节。

## 七、总结

四篇下来，同一个面试状态机已经有了三个 I/O 适配层：终端、浏览器、飞书。这就是把 Agent 当产品做和写一次性的 demo 最大的区别——**业务核心稳定，接入层随场景长**。下一篇聊聊给它接上语音，让面试官真的能「听」你作答，或者把 bot 部署到服务器上 7×24 对外服务，想先看哪个，评论区告诉我。

如果对你有帮助，欢迎点赞、在看、转发，关注「Go2AI之路」，系列更新不迷路。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
