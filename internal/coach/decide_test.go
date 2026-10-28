package coach

import "testing"

func TestShouldFollowUp(t *testing.T) {
	cases := []struct {
		name     string
		ev       Evaluation
		used     int
		expected bool
	}{
		{"low confidence triggers", Evaluation{Confidence: 2, Coverage: 0.9}, 0, true},
		{"low coverage triggers", Evaluation{Confidence: 4, Coverage: 0.5}, 0, true},
		{"good answer no follow-up", Evaluation{Confidence: 5, Coverage: 0.9}, 0, false},
		{"max follow-ups exhausted", Evaluation{Confidence: 1, Coverage: 0.1}, MaxFollowUpsPerQuestion, false},
	}
	for _, c := range cases {
		if got := ShouldFollowUp(c.ev, c.used); got != c.expected {
			t.Errorf("%s: ShouldFollowUp=%v, want %v", c.name, got, c.expected)
		}
	}
}
