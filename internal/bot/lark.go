// 飞书长连接接入层：事件订阅 → 纯文本 → bot.HandleText → 回复消息。
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"ai-interview-coach/internal/coach"
	"ai-interview-coach/internal/llm"
	"ai-interview-coach/internal/report"
)

// mentionRE 匹配消息体里的 @机器人 占位符（@_user_1 这种）。
var mentionRE = regexp.MustCompile(`@_user_\d+`)

// maxAudioDuration 单条作答语音的最长时长，超出提示分段（自己的兜底，
// 飞书资源接口本身限 100MB，ASR 供应商另有上限，取最严的）。
const maxAudioDuration = 3 * 60 * 1000 // 3 分钟，毫秒

// LarkSender 用飞书 OpenAPI 的「回复消息」接口把文本发回会话。
type LarkSender struct {
	cli *lark.Client
}

// Fetch 实现 bot.AudioFetcher：下载语音消息的二进制流。
// 走 GET /open-apis/im/v1/messages/:message_id/resources/:file_key（type=file），
// 返回字节流和原始文件名（响应头里带，用于推断 opus/ogg 格式）。
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
		return nil, "", fmt.Errorf("下载语音失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return resp.File, resp.FileName, nil
}

// NewLarkSender 构造发送器；appID/appSecret 即自建应用的凭据。
func NewLarkSender(appID, appSecret string) *LarkSender {
	return &LarkSender{cli: lark.NewClient(appID, appSecret)}
}

// Reply 回复指定消息。text 中的换行直接放 \n，SDK 会完成 JSON 转义。
func (s *LarkSender) Reply(ctx context.Context, messageID, text string) error {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	_, err = s.cli.Im.Message.Reply(ctx, req)
	return err
}

// CoachFactory 把 internal/coach 状态机适配成 bot.Factory。
// 简历解析在 IM 场景先不接（发文件体验重），pos 即全部输入。
func CoachFactory(client llm.Client) Factory {
	return func(ctx context.Context, position string) (Interview, error) {
		it, err := coach.NewInterview(ctx, client, position, "")
		if err != nil {
			return nil, err
		}
		return &coachInterview{it: it}, nil
	}
}

// coachInterview 是 coach.Interview → bot.Interview 的适配器，
// 负责把「状态机语言」翻译成「IM 回复语言」（评估摘要 + 追问/下一题）。
type coachInterview struct {
	it *coach.Interview
}

func (c *coachInterview) Question() string {
	q := c.it.CurrentQuestion()
	if q == nil {
		return ""
	}
	return fmtQuestion(c.it, q)
}

func (c *coachInterview) Answer(ctx context.Context, answer string) (bool, string, error) {
	followUp, msg, err := c.it.Answer(ctx, answer)
	if err != nil {
		return false, "", err
	}
	ev := c.it.State.Pending.Evaluation
	feedback := fmt.Sprintf("评估：置信度 %d/5，要点覆盖 %.0f%%，%s",
		ev.Confidence, ev.Coverage*100, ev.Suggestion)
	if followUp {
		return true, feedback + "\n\n【追问】" + msg, nil
	}
	if q := c.it.CurrentQuestion(); q != nil {
		return false, feedback + "\n\n" + fmtQuestion(c.it, q), nil
	}
	return false, feedback, nil
}

func (c *coachInterview) Finish(ctx context.Context) (string, error) {
	rep, err := c.it.Finish(ctx)
	if err != nil {
		return "", err
	}
	return report.Markdown(rep), nil
}

// fmtQuestion 给题目加上进度和维度标签，IM 里一眼看清进度。
func fmtQuestion(it *coach.Interview, q *coach.Question) string {
	tag := fmt.Sprintf("【第 %d 题·%s】", it.State.Cursor+1, q.Dimension)
	if q.Source == "resume" {
		tag = fmt.Sprintf("【第 %d 题·%s·简历深挖】", it.State.Cursor+1, q.Dimension)
	}
	return tag + q.Question
}

// Run 启动飞书长连接（WebSocket）。阻塞运行，ctx 取消即退出。
// 长连接模式不需要公网 IP、不需要配回调地址，本地 go run 即可调试。
func (b *Bot) Run(ctx context.Context, appID, appSecret string) error {
	// 长连接模式不需要公网 IP、不需要配回调地址，本地 go run 即可调试。
	// 长连接下事件不经过公网回调，因此校验 token 和加密 key 传空串即可。
	disp := dispatcher.NewEventDispatcher("", "")
	disp.OnP2MessageReceiveV1(b.onP2MessageReceive)
	cli := larkws.NewClient(appID, appSecret, larkws.WithEventHandler(disp))
	cli.SetOnReady(func() { log.Println("[bot] 飞书长连接已就绪") })
	cli.SetOnReconnecting(func() { log.Println("[bot] 连接断开，正在重连…") })
	log.Println("[bot] 正在连接飞书…")
	return cli.Start(ctx)
}

// 会话标识：群/单聊 ID + 发送者 open_id，两人同群互不干扰。
// 文字和语音走同一个 key，混着答（文字一题、语音一题）互不影响。
func sessionKey(e *larkim.P2MessageReceiveV1, msg *larkim.EventMessage) string {
	senderID := ""
	if e.Event.Sender.SenderId != nil && e.Event.Sender.SenderId.OpenId != nil {
		senderID = *e.Event.Sender.SenderId.OpenId
	}
	return *msg.ChatId + "|" + senderID
}

// onP2MessageReceive 消息事件入口：过滤 → 提文本/语音 → 走核心 → 回复。
func (b *Bot) onP2MessageReceive(ctx context.Context, e *larkim.P2MessageReceiveV1) error {
	msg := e.Event.Message

	// 1) 忽略机器人自己发的消息，防自循环
	if e.Event.Sender.SenderType == nil || *e.Event.Sender.SenderType != "user" {
		return nil
	}

	// 2) 语音消息：转写后走与文字相同的作答管线
	if msg.MessageType != nil && *msg.MessageType == "audio" {
		return b.onAudioMessage(ctx, e, msg)
	}

	// 3) 只处理文字消息
	if msg.MessageType == nil || *msg.MessageType != "text" {
		return b.reply(ctx, msg, "目前只支持文字和语音消息，请用其中一种作答～")
	}
	text, ok := extractText(*msg.Content)
	if !ok {
		return b.reply(ctx, msg, "没读懂这条消息，请重新发一次。")
	}
	// 4) 群聊里只响应 @ 机器人的消息；p2p 单聊全响应
	if msg.ChatType != nil && *msg.ChatType == "group" && !mentionRE.MatchString(text) {
		return nil
	}
	text = strings.TrimSpace(mentionRE.ReplaceAllString(text, ""))

	reply, err := b.HandleText(ctx, sessionKey(e, msg), text)
	if err != nil {
		log.Printf("[bot] 处理消息失败: %v", err)
		reply = "系统开小差了，请稍后再试 🙏"
	}
	if reply == "" {
		return nil
	}
	return b.reply(ctx, msg, reply)
}

// onAudioMessage 语音消息入口。语音没法 @ 机器人，群聊响应规则与文字不同：
// 只有该用户已有进行中的面试时才响应（否则群里每条语音都回话就太吵了）。
func (b *Bot) onAudioMessage(ctx context.Context, e *larkim.P2MessageReceiveV1, msg *larkim.EventMessage) error {
	key := sessionKey(e, msg)

	// 群聊守卫：没有进行中的面试就静默忽略
	if msg.ChatType != nil && *msg.ChatType == "group" && b.load(key) == nil {
		return nil
	}
	if b.fetcher == nil || b.asr == nil {
		return b.reply(ctx, msg, "这个机器人没开语音识别，请用文字作答～")
	}

	// 语音消息的 content 是 JSON：{"file_key":"...","duration":<毫秒>}
	var body struct {
		FileKey  string `json:"file_key"`
		Duration int64  `json:"duration"`
	}
	if err := json.Unmarshal([]byte(*msg.Content), &body); err != nil || body.FileKey == "" {
		return b.reply(ctx, msg, "语音消息解析失败，请重发一次。")
	}
	if body.Duration > maxAudioDuration {
		return b.reply(ctx, msg, fmt.Sprintf("语音太长了（限 %d 分钟），请分段作答～", maxAudioDuration/time.Minute))
	}

	reader, filename, err := b.fetcher.Fetch(ctx, *msg.MessageId, body.FileKey)
	if err != nil {
		log.Printf("[bot] 下载语音失败: %v", err)
		return b.reply(ctx, msg, "语音下载失败，请改用文字作答。")
	}

	reply, err := b.HandleAudio(ctx, key, reader, filename)
	if err != nil {
		log.Printf("[bot] 语音处理失败: %v", err)
		reply = "系统开小差了，请稍后再试 🙏"
	}
	if reply == "" {
		return nil
	}
	return b.reply(ctx, msg, reply)
}

// reply 回复消息的小工具，吃掉 nil 指针的边角情况。
func (b *Bot) reply(ctx context.Context, msg *larkim.EventMessage, text string) error {
	if msg.MessageId == nil {
		return nil
	}
	return b.sender.Reply(ctx, *msg.MessageId, text)
}

// extractText 从 text 消息的 content JSON（{"text":"..."}）里取文本。
func extractText(content string) (string, bool) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &body); err != nil || body.Text == "" {
		return "", false
	}
	return body.Text, true
}
