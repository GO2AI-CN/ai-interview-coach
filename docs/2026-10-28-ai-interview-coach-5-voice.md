# 让 AI 面试官听懂语音：Go + 飞书语音消息实战（五）

上一篇把面试教练接进了飞书，但留了个明显的产品缺口：真实面试里你是「说」答案的，不是打字。对着打字框组织语言，和开口讲话，是完全两种压力测试——很多人打字能对答如流，一开口就逻辑断片，这才是面试要练的东西。

今天这篇给机器人装上耳朵：飞书里发一条语音消息，自动转文字、走评估、出追问，和文字作答完全同一套管线。识别结果会回显给你，答完照样出评分报告。

## 一、先想清楚：为什么「听」比「说」优先

做语音功能，先「听」（ASR 语音转文字）还是先「说」（TTS 文字转语音）？这是个产品决策，不是技术决策：

| | 语音作答（ASR） | 语音提问（TTS） |
|---|---|---|
| 对产品价值的贡献 | 改变作答方式，训练「开口表达」这个面试核心能力 | 锦上添花，题目文字展示完全够用 |
| 技术链路 | 接收 → 下载 → 转文字 → 走现有管线 | 调 TTS 接口 → 发音频消息 |
| 成本 | 有靠谱的免费方案（下文详述） | 要花钱，或者忍受机械音 |

结论很直接：**ASR 改变产品形态，TTS 只是体验优化**。所以本篇先把「听」做扎实，TTS 留了个零成本的彩蛋在第五节。

## 二、链路拆解：收、转、答三段式

整条链路分三段，核心原则一句话：**第三段「答」一行代码都不改**。

```
飞书语音消息 → [收] 事件解析 + 资源下载 → [转] ASR 转文字 → [答] 现有文字作答管线
```

「收」是飞书特有逻辑：语音消息事件里只有一个 `file_key`，音频二进制要走资源下载接口另取。「转」是供应商特有逻辑：OpenAI 兼容的 `/v1/audio/transcriptions`。「答」是已经写好的面试状态机。

三段各管各的，用接口隔离——这正是前三篇反复强调的 I/O 适配层思路在语音场景的复用。`internal/coach` 状态机从第一篇到现在，一个字没动过。

## 三、收：audio 事件与资源下载

飞书语音消息事件的 `MessageType` 是 `audio`，`content` 是一段 JSON：

```go
// 语音消息的 content：{"file_key":"...","duration":<毫秒>}
var body struct {
	FileKey  string `json:"file_key"`
	Duration int64  `json:"duration"` // 顺手取时长，做长度兜底
}
if err := json.Unmarshal([]byte(*msg.Content), &body); err != nil || body.FileKey == "" {
	return b.reply(ctx, msg, "语音消息解析失败，请重发一次。")
}
```

拿到 `file_key` 之后，音频本体要走另一个接口下载：`GET /open-apis/im/v1/messages/:message_id/resources/:file_key`（type=file）。SDK 里就是一行：

```go
func (s *LarkSender) Fetch(ctx context.Context, messageID, fileKey string) (io.Reader, string, error) {
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(fileKey).
		Type("file"). // file 类型覆盖音频/视频/普通文件
		Build()
	resp, err := s.cli.Im.MessageResource.Get(ctx, req)
	if err != nil {
		return nil, "", err
	}
	if !resp.Success() {
		return nil, "", fmt.Errorf("下载语音失败: code=%d", resp.Code)
	}
	return resp.File, resp.FileName, nil // File 即二进制流，FileName 带原始后缀
}
```

这里有个容易踩的权限坑：**应用必须在飞书开放平台后台申请 `im:resource` 权限**，否则下载接口直接返回权限错误码，报错信息还不直观。另外语音消息没法 @ 机器人，所以群聊里的响应规则要单独设计：只响应**已有进行中面试**的用户的语音，否则群里随便一条语音机器人都插话，太吵了。

## 四、转：ASR 免费方案选型

转写服务怎么选？我把主流方案实测/调研了一遍，先上结论表（建议收藏）：

| 方案 | 协议 | 免费额度 | 国内网络 | 接入成本 |
|---|---|---|---|---|
| **SiliconFlow**（硅基流动） | ✅ OpenAI 兼容 | 免费模型：FunAudioLLM/SenseVoiceSmall、TeleAI/TeleSpeechASR | ✅ 直连 | 换个 BASE_URL 即可 |
| Groq | ✅ OpenAI 兼容 | whisper-large-v3 免费档 | ⚠️ 国内访问不稳 | 同上（适合做海外备份） |
| 讯飞开放平台 | ❌ 自有协议 | 每天 500 次 | ✅ | 要单独接 SDK |
| 阿里千问（Paraformer） | ❌ 自有协议 | 每月约 10 小时 | ✅ | 要单独接 SDK |
| whisper.cpp / sherpa-onnx | 本地离线 | 无限 | — | 要部署模型，中文效果一般 |

首选 **SiliconFlow**：OpenAI 协议兼容意味着**代码零改动**，`OPENAI_BASE_URL` 一换就行，这和本系列 LLM 客户端的设计完全同构。注册拿 Key，`https://api.siliconflow.cn/v1` 就是端点。

转写封装只有一个文件，关键调用就这几行：

```go
resp, err := o.cli.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
	File:  audio,                        // io.Reader，SDK 自动编码 multipart
	Model: openai.AudioModel(o.model),   // 默认 FunAudioLLM/SenseVoiceSmall
})
if err != nil {
	return "", fmt.Errorf("transcribe: %w", err)
}
return resp.Text, nil // 直接取文本；空串 = 没听清
```

然后是最体现设计价值的一步——转写完直接喂给现成的文字管线：

```go
// 转写 → 走与文字消息完全相同的指令/作答管线
reply, err := b.HandleText(ctx, key, text)
if err != nil {
	return "", err
}
// 识别结果前置展示：评估以它为准，用户能当场发现识别偏差
return "【语音识别】" + text + "\n\n" + reply, nil
```

注意这个设计：**对着机器人说「结束」也能结束面试**，因为语音转文字后走的就是同一个 `HandleText`，指令解析天然生效。文字答一题、语音答一题混着来也互不影响——同一个会话 key，同一台状态机。

## 五、彩蛋：零成本的「说」

正经 TTS 要等后续接入，但网页版有个浏览器内置彩蛋值得先玩：Web Speech API 的 `speechSynthesis`，不用任何 Key，全部浏览器自带。把「说」加进第三篇的网页版，十行以内：

```javascript
// 面试教练开口念题：浏览器原生 TTS，零依赖零 Key
function speak(text) {
  speechSynthesis.cancel();                    // 打断上一条，避免叠音
  const u = new SpeechSynthesisUtterance(text);
  u.lang = "zh-CN";                            // 中文
  u.rate = 1.05;                               // 面试节奏，稍快一点点
  speechSynthesis.speak(u);
}
// 出题后调用：speak(question)
```

机械音确实不如商业 TTS，但演示「面试官开口提问」的完整闭环，够用了。

## 六、踩坑清单

语音这块的坑比想象中密，都替你们踩过了：

1. **权限**：`im:resource` 没申请，下载接口必挂，报错码还不直观。
2. **opus 格式**：飞书语音原始流是 opus 封装。OpenAI 协议端点大多接受 ogg 容器（Whisper 系支持 ogg），资源接口返回的 `FileName` 带后缀，原样透传即可；如果换成只认 wav 的供应商，本地 ffmpeg 转一道。
3. **时长兜底**：转写服务按次或按时长计费且都有上限，我们的做法是超 3 分钟的语音直接提示分段作答，不硬转。
4. **识别失败的语义**：ASR 返回空串不是错误，是「没听清」。要单独提示重试，绝不能把空文本送进评估管线——那会让面试官对着空气打分。
5. **回显识别结果**：评估基于识别文本，用户有权知道自己说的话被听成了什么。不回显的语音功能都是半成品。
6. **群聊噪音**：语音无法 @ 机器人，响应规则必须挂「有进行中的面试」这个条件，否则机器人在群里会失控接话。

## 七、总结

五篇下来，这个项目的骨架越来越清晰：**一个稳定的状态机核心（出题/评估/追问/报告），加一层一层的 I/O 适配器**（终端 → 浏览器 → 飞书 → 语音），每加一层，产品多一个真实使用场景，核心代码一行不动。AI 应用开发的大部分工程含金量，就在这些适配层里。

本篇完整代码（含语音全链路单测）在 `https://github.com/LittleMoreInteresting/ai-interview-coach` 的 `internal/asr/` 和 `internal/bot/` 下，`go test ./...` 全绿。端到端需要你自己建个飞书应用发一条语音——记得申请 `im:resource` 权限。

系列还没完：下一篇把它部署到服务器上 7×24 运行，聊聊一个 AI 产品的最后一公里的工程问题（进程守护、日志、成本监控），或者提前收官做一期完整回顾，评论区告诉我。

如果对你有帮助，欢迎点赞、在看、转发，关注「Go2AI之路」，系列更新不迷路。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
