package coach

// 面试领域模型：计划、题目、评估记录、面试状态。

// Question 面试计划中的一道题。Checkpoints 是评分标准，
// 也是追问判断（关键点覆盖度）的依据——评分标准提前从模型嘴里"套"出来。
type Question struct {
	Dimension   string   `json:"dimension"`   // 能力维度，如 "Go 语言内核"
	Question    string   `json:"question"`    // 题目
	Checkpoints []string `json:"checkpoints"` // 考察要点，3~5 条，具体可验证
	Weight      int      `json:"weight"`      // 维度权重 1-100
	Source      string   `json:"source"`      // "rubric" 或 "resume"（简历深挖题）
}

// Plan 面试计划：开场前一次性生成，之后按 plan 逐题推进。
type Plan struct {
	Position string     `json:"position"`
	Questions []Question `json:"questions"`
}

// Evaluation 对一轮回答的结构化评估（模型按 JSON Schema 输出）。
type Evaluation struct {
	Confidence int      `json:"confidence"` // 1-5 自评置信度
	Coverage   float64  `json:"coverage"`   // 命中 checkpoints 的比例 0-1
	HitPoints  []string `json:"hit_points"`
	Missed     []string `json:"missed"`
	Suggestion string   `json:"suggestion"`
	DepthOK    bool     `json:"depth_ok"` // 模型综合判断：这一题是否挖到位
}

// Turn 一次问答记录（含追问轮次），用于跨轮记忆与最终报告聚合。
type Turn struct {
	Question   Question   `json:"question"`
	Answer     string     `json:"answer"`
	Evaluation Evaluation `json:"evaluation"`
	FollowUps  int        `json:"followups"` // 本题已追问次数
}

// Report 结构化评分报告（JSON Schema 约束输出，Go struct 解析）。
type Report struct {
	Position    string             `json:"position"`
	Dimensions  []DimensionScore   `json:"dimensions"`
	WeakPoints  []string           `json:"weak_points"`
	Suggestions []string           `json:"suggestions"`
	Overall     int                `json:"overall"`
}

type DimensionScore struct {
	Name    string `json:"name"`
	Score   int    `json:"score"` // 0-100
	Comment string `json:"comment"`
}

// State 可持久化的面试状态：今天面 3 题，明天可以接着面。
type State struct {
	Position   string     `json:"position"`
	Plan       Plan       `json:"plan"`
	Cursor     int        `json:"cursor"` // 当前进行到的题号
	Done       []Turn     `json:"done"`   // 已完成的题目（含评估）
	Pending    *Turn      `json:"pending,omitempty"` // 当前题未完成的追问轮
	Resume     string     `json:"resume,omitempty"`  // 简历摘要（针对性出题依据）
}
