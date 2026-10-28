package coach

import "fmt"

// 追问判断规则：两把尺子 + 套娃上限。纯函数，可单测。

const MaxFollowUpsPerQuestion = 2 // 同一题最多追问 2 轮，防止无限套娃

const (
	confidenceThreshold = 3    // 自评置信度 < 3：模型自己都觉得虚
	coverageThreshold   = 0.6  // 关键点覆盖率 < 60%：没答到点子上
)

// ShouldFollowUp 根据评估结果决定是否追问。
// 任一信号不达标都要追问，但追问次数用尽则放行。
func ShouldFollowUp(ev Evaluation, followUpsUsed int) bool {
	if followUpsUsed >= MaxFollowUpsPerQuestion {
		return false
	}
	if ev.Confidence < confidenceThreshold {
		return true
	}
	if ev.Coverage < coverageThreshold {
		return true
	}
	return false
}

// FollowUpPrompt 生成有方向的追问：把缺失的 checkpoints 喂回去，
// 而不是复读机式地说"再详细点"。
func FollowUpPrompt(missed []string) string {
	const tmpl = "你的回答还没有覆盖这些要点：%v。" +
		"请结合你的实际项目经历补充说明。"
	return fmt.Sprintf(tmpl, missed)
}
