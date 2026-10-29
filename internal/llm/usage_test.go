package llm

import "testing"

func TestUsageCounterAddAndSnapshot(t *testing.T) {
	before := TotalUsageSnapshot()
	globalUsage.add(120, 30) // 模拟一次请求：prompt 120 / completion 30

	got := TotalUsageSnapshot()
	if got.PromptTokens != before.PromptTokens+120 {
		t.Fatalf("prompt 累计不对: %d", got.PromptTokens)
	}
	if got.CompletionTokens != before.CompletionTokens+30 {
		t.Fatalf("completion 累计不对: %d", got.CompletionTokens)
	}
	if got.TotalTokens != got.PromptTokens+got.CompletionTokens {
		t.Fatalf("total 应等于 prompt+completion: %+v", got)
	}
}

func TestUsageSub(t *testing.T) {
	start := Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}
	end := Usage{PromptTokens: 250, CompletionTokens: 60, TotalTokens: 310}
	got := end.Sub(start)
	want := Usage{PromptTokens: 150, CompletionTokens: 40, TotalTokens: 190}
	if got != want {
		t.Fatalf("Sub 差值不对: got %+v want %+v", got, want)
	}
}
