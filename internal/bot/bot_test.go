package bot

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeSender 记录所有回复，不做真实发送。
type fakeSender struct{ replies []string }

func (f *fakeSender) Reply(_ context.Context, _, text string) error {
	f.replies = append(f.replies, text)
	return nil
}

// fakeInterview 按脚本模拟一场 2 题的面试：第 1 题触发追问，第 2 题直接过。
type fakeInterview struct {
	answers int
}

func (f *fakeInterview) Question() string {
	if f.answers >= 2 {
		return ""
	}
	return "【第 1 题·基础】Q"
}

func (f *fakeInterview) Answer(_ context.Context, _ string) (bool, string, error) {
	f.answers++
	if f.answers == 1 {
		return true, "评估：答浅了\n\n【追问】为什么？", nil // 触发追问
	}
	return false, "评估：答得不错", nil
}

func (f *fakeInterview) Finish(_ context.Context) (string, error) {
	return "# 评分报告\n综合得分：80/100", nil
}

func newTestBot(t *testing.T) (*Bot, *fakeInterview, *fakeSender) {
	t.Helper()
	fi := &fakeInterview{}
	fs := &fakeSender{}
	b := NewBot(fs, func(_ context.Context, _ string) (Interview, error) {
		return fi, nil
	}, 0)
	return b, fi, fs
}

func TestHelp(t *testing.T) {
	b, _, fs := newTestBot(t)
	got, err := b.HandleText(context.Background(), "k", "帮助")
	if err != nil || !strings.Contains(got, "面试 <目标岗位>") {
		t.Fatalf("帮助文案不对: %q, err=%v", got, err)
	}
	if len(fs.replies) != 0 {
		t.Fatal("HandleText 不应直接发消息，发送是 lark 层的事")
	}
}

func TestNoSessionAnswer(t *testing.T) {
	b, _, _ := newTestBot(t)
	got, err := b.HandleText(context.Background(), "k", "channel 和 mutex 的区别")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "面试 <目标岗位>") {
		t.Fatalf("无会话时应提示开考, got=%q", got)
	}
}

func TestFullInterviewFlow(t *testing.T) {
	b, fi, _ := newTestBot(t)
	ctx := context.Background()

	// 开考
	got, err := b.HandleText(ctx, "k", "面试 Go 后端工程师")
	if err != nil || !strings.Contains(got, "面试计划已生成") {
		t.Fatalf("开考失败: %q, err=%v", got, err)
	}

	// 第 1 次作答 → 追问
	got, err = b.HandleText(ctx, "k", "channel 用于通信")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "追问") {
		t.Fatalf("应触发追问, got=%q", got)
	}

	// 第 2 次作答 → 下一题；fake 第 2 题答完即结束
	got, err = b.HandleText(ctx, "k", "补充：mutex 用于保护临界区")
	if err != nil {
		t.Fatal(err)
	}
	if fi.answers != 2 {
		t.Fatalf("应收到 2 次作答, got=%d", fi.answers)
	}
	if !strings.Contains(got, "面试结束") || !strings.Contains(got, "评分报告") {
		t.Fatalf("应出报告, got=%q", got)
	}

	// 会话已回收，再答按无会话处理
	got, _ = b.HandleText(ctx, "k", "再来一句")
	if !strings.Contains(got, "开始一场模拟面试") {
		t.Fatalf("会话应已回收, got=%q", got)
	}
}

func TestEarlyFinish(t *testing.T) {
	b, _, _ := newTestBot(t)
	ctx := context.Background()
	if _, err := b.HandleText(ctx, "k", "面试 SRE"); err != nil {
		t.Fatal(err)
	}
	got, err := b.HandleText(ctx, "k", "结束")
	if err != nil || !strings.Contains(got, "提前结束") || !strings.Contains(got, "80/100") {
		t.Fatalf("提前结束失败: %q, err=%v", got, err)
	}
}

func TestFinishWithoutSession(t *testing.T) {
	b, _, _ := newTestBot(t)
	got, err := b.HandleText(context.Background(), "k", "结束")
	if err != nil || !strings.Contains(got, "还没有进行中") {
		t.Fatalf("got=%q, err=%v", got, err)
	}
}

func TestSessionIsolation(t *testing.T) {
	b, _, _ := newTestBot(t)
	ctx := context.Background()
	if _, err := b.HandleText(ctx, "chat1|u1", "面试 Go"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.HandleText(ctx, "chat1|u2", "面试 Rust"); err != nil {
		t.Fatal(err)
	}
	got, _ := b.HandleText(ctx, "chat1|u2", "我的答案")
	if !strings.Contains(got, "评估") {
		t.Fatalf("u2 应有自己的会话, got=%q", got)
	}
}

func TestSessionExpiry(t *testing.T) {
	b, _, _ := newTestBot(t)
	now := time.Now()
	b.now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := b.HandleText(ctx, "k", "面试 Go"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(3 * time.Hour) // 超过 2 小时闲置
	got, err := b.HandleText(ctx, "k", "我的答案")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "开始一场模拟面试") {
		t.Fatalf("过期会话应失效, got=%q", got)
	}
}

func TestFactoryError(t *testing.T) {
	fs := &fakeSender{}
	b := NewBot(fs, func(_ context.Context, _ string) (Interview, error) {
		return nil, errors.New("LLM 挂了")
	}, 0)
	_, err := b.HandleText(context.Background(), "k", "面试 Go")
	if err == nil || !strings.Contains(err.Error(), "LLM 挂了") {
		t.Fatalf("工厂错误应向上抛, err=%v", err)
	}
}

// fakeTranscriber 按脚本返回识别文本，text 为空串模拟「没听清」。
type fakeTranscriber struct{ text string }

func (f *fakeTranscriber) Transcribe(_ context.Context, _ io.Reader, _ string) (string, error) {
	return f.text, nil
}

// newTestBotWith 注入 fake 转写器的测试机器人（fetcher 传 nil，HandleAudio 不依赖它）。
func newTestBotWith(t *testing.T, tr Transcriber) (*Bot, *fakeInterview) {
	t.Helper()
	fi := &fakeInterview{}
	b := NewBot(&fakeSender{}, func(_ context.Context, _ string) (Interview, error) {
		return fi, nil
	}, 0, WithAudio(nil, tr))
	return b, fi
}

func TestAudioAnswer(t *testing.T) {
	tr := &fakeTranscriber{text: "channel 用于协程间通信"}
	b, fi := newTestBotWith(t, tr)
	ctx := context.Background()
	if _, err := b.HandleText(ctx, "k", "面试 Go 后端"); err != nil {
		t.Fatal(err)
	}

	// 语音作答：转写结果前置展示，并进入正常评估管线
	got, err := b.HandleAudio(ctx, "k", strings.NewReader("fake-opus-bytes"), "voice.ogg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "【语音识别】channel 用于协程间通信") {
		t.Fatalf("应前置识别结果, got=%q", got)
	}
	if !strings.Contains(got, "追问") || fi.answers != 1 {
		t.Fatalf("识别结果应走作答管线并触发追问, got=%q answers=%d", got, fi.answers)
	}
}

func TestAudioVoiceFinishCommand(t *testing.T) {
	tr := &fakeTranscriber{text: "结束"} // 语音说「结束」也应能出报告
	b, _ := newTestBotWith(t, tr)
	ctx := context.Background()
	if _, err := b.HandleText(ctx, "k", "面试 Go 后端"); err != nil {
		t.Fatal(err)
	}
	got, err := b.HandleAudio(ctx, "k", strings.NewReader("x"), "")
	if err != nil || !strings.Contains(got, "提前结束") {
		t.Fatalf("语音指令失败: %q err=%v", got, err)
	}
}

func TestAudioEmptyTranscript(t *testing.T) {
	tr := &fakeTranscriber{text: "  "} // 没听清：友好提示，不进作答管线
	b, fi := newTestBotWith(t, tr)
	got, err := b.HandleAudio(context.Background(), "k", strings.NewReader("x"), "")
	if err != nil || !strings.Contains(got, "没听清") {
		t.Fatalf("空识别应提示重试, got=%q err=%v", got, err)
	}
	if fi.answers != 0 {
		t.Fatal("没听清的语音不应进入评估管线")
	}
}

func TestAudioNotConfigured(t *testing.T) {
	b, _, _ := newTestBot(t) // 未注入 ASR
	got, err := b.HandleAudio(context.Background(), "k", strings.NewReader("x"), "")
	if err != nil || !strings.Contains(got, "没开语音识别") {
		t.Fatalf("未配置时应优雅拒绝, got=%q err=%v", got, err)
	}
}

func TestExtractText(t *testing.T) {
	got, ok := extractText(`{"text":"@_user_1 面试 Go"}`)
	if !ok || got != "@_user_1 面试 Go" {
		t.Fatalf("extractText 解析失败: %q ok=%v", got, ok)
	}
	if _, ok := extractText(`{}`); ok {
		t.Fatal("空文本应返回 false")
	}
	if _, ok := extractText(`not-json`); ok {
		t.Fatal("坏 JSON 应返回 false")
	}
}

func TestMentionStrip(t *testing.T) {
	in := "@_user_1 @_user_2 面试 Go 工程师"
	out := strings.TrimSpace(mentionRE.ReplaceAllString(in, ""))
	if out != "面试 Go 工程师" {
		t.Fatalf("提及占位符剥离失败: %q", out)
	}
}
