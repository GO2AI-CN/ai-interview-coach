// Package asr 语音转文字（ASR）：把面试作答的语音字节流转成文本，
// 再走与文字作答完全相同的评估管线。生产实现是 OpenAIClient——
// 任何兼容 OpenAI /v1/audio/transcriptions 协议的端点都能直接用，
// 换供应商只改环境变量，代码零改动。
package asr

import (
	"context"
	"fmt"
	"io"

	"github.com/openai/openai-go/v3"
)

// Transcriber 语音转文字的最小抽象，测试时替换为 fake。
type Transcriber interface {
	// Transcribe 把一段音频流转成文本。filename 仅用于推断格式（扩展名），
	// 可为空。返回空串表示没听清（不是错误）。
	Transcribe(ctx context.Context, audio io.Reader, filename string) (string, error)
}

// OpenAI 基于 openai-go v3 的实现，兼容任何 OpenAI 协议端点
// （OpenAI / Groq / SiliconFlow 等），通过环境变量切换：
// OPENAI_API_KEY / OPENAI_BASE_URL / COACH_ASR_MODEL。
type OpenAI struct {
	cli   openai.Client
	model string
}

// New 构造转写器。model 为空时用 SiliconFlow 免费的 SenseVoiceSmall。
func New(model string) *OpenAI {
	if model == "" {
		model = "FunAudioLLM/SenseVoiceSmall" // 硅基流动免费 ASR 模型
	}
	return &OpenAI{cli: openai.NewClient(), model: model}
}

// Transcribe 调 POST /v1/audio/transcriptions，multipart 上传音频。
// openai-go 会把 File 字段自动编码成 multipart；响应直接取 .Text。
func (o *OpenAI) Transcribe(ctx context.Context, audio io.Reader, filename string) (string, error) {
	resp, err := o.cli.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		File:  audio,
		Model: openai.AudioModel(o.model),
	})
	if err != nil {
		return "", fmt.Errorf("transcribe: %w", err)
	}
	return resp.Text, nil
}
