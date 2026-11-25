# LLM 应用工程化③：自动化评测集，把「感觉变好了」变成 CI 里的一行数字

> 本系列同步发布于博客 [blog.911015.com](https://blog.911015.com/ai/llm-ops.html) 与公众号「Go2AI之路」。前两篇把「单次调用」修稳了，本篇解决一个更隐蔽的问题：**你怎么知道改动之后，系统整体是变好了还是变差了？**

团队里最常见的 LLM 迭代姿势是这样的：改 Prompt → 拿两三个自己熟悉的例子试一下 → 「感觉不错」→ 上线 → 一周后用户投诉某类问题变差了。整个过程没有任何留痕，回滚靠运气，复盘靠猜。

传统软件工程一百年前就解决了同类问题：**测试集 + 回归门禁**。LLM 应用没有理由例外——只是「判对错」这件事本身也要交给模型来做。本篇搭一套完整的自动化评测体系：黄金数据集、双轨判分、版本对比、CI 门禁。承接前两篇：数据集复用第 1 篇的 Prompt Registry（按版本跑评测），判分复用第 2 篇的结构化输出。

## 一、黄金数据集：评测的地基

评测集质量决定评测结论的可信度。一份合格的黄金数据集（Golden Dataset）有三条标准：

1. **来自真实分布，不是手工精品**：从生产日志里抽样失败案例和典型请求，而不是坐在办公室里编「理想问题」。手编数据集会系统性偏简单，评测分虚高，上线照样翻车。
2. **每条用例的期望必须可判定**：不要写 `expect: "回答得好"`，要写可执行的判定条件——「包含关键词 X」「sentiment 为 negative」「summary 不超过 50 字」。不可判定的期望等于没有期望。
3. **和 Prompt 一样版本化**：评测集本身就是资产，放在 `evals/` 目录进 Git，每次增删留痕。上线后发现新 badcase，第一反应是把它固化成一条用例。

数据格式用 JSONL（每行一条，便于追加和流式处理）：

```json
{"id":"case-001","input":"升级到v3后导入panic，什么时候能修？不能修就退款","expect":{"category":"refund|bug","must_keywords":["v3"]},"judge_criteria":"摘要必须提及 panic 或导入错误"}
{"id":"case-002","input":"请问你们的API支持批量导入吗？","expect":{"category":"consult","must_keywords":[]},"judge_criteria":"回答应说明支持情况，语气礼貌"}
```

## 二、双轨判分：确定性校验 + LLM-as-judge

没有一种判分手段能覆盖所有维度，实用的组合是两轨并行：

| 判分手段 | 能判什么 | 成本 | 稳定性 |
| --- | --- | --- | --- |
| **确定性校验**（关键词、schema、正则） | 格式、必含词、枚举值 | 几乎为零 | 100% 可复现 |
| **LLM-as-judge** | 相关性、完整性、语气、推理质量 | 每条一次模型调用 | 有噪声，需多采样式聚合 |
| 人工抽检 | 一切 | 高 |  gold standard，但不可常态化 |

原则：**能用确定性判的绝不用模型判**（免费且不会错），模型只判确定性判不了的部分。LLM-as-judge 的噪声用「同一个 judge 判所有版本」来控制——噪声在版本对比中是同向的，相对比较依然有效。

## 三、Go 实现：评测运行器

项目结构（沿用第 1 篇的 promptlab）：

```
evals/
├── dataset.jsonl          # 黄金数据集
└── report/                # 评测报告输出目录
eval/
├── runner.go              # 评测运行器
└── eval_test.go           # CI 入口
```

### 3.1 用例定义与数据集加载

```go
package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Expect 确定性判定条件（全部可选，但至少给一个）
type Expect struct {
	Category     string   `json:"category"`      // 要求输出分类属于此值（| 分隔表示多选一）
	MustKeywords []string `json:"must_keywords"` // 输出必须包含的关键词
}

// Case 一条评测用例
type Case struct {
	ID           string   `json:"id"`
	Input        string   `json:"input"`
	Expect       Expect   `json:"expect"`
	JudgeCrit    string   `json:"judge_criteria"` // 交给 LLM judge 的判定标准
}

// LoadDataset 从 JSONL 加载评测集
func LoadDataset(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cases []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024) // 防止长行被截断
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c Case
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, fmt.Errorf("解析用例失败: %w", err)
		}
		cases = append(cases, c)
	}
	return cases, sc.Err()
}
```

### 3.2 确定性判分

```go
import "strings"

// DeterministicScore 确定性判分：返回 (得分, 未通过的判定项)
func DeterministicScore(output string, expect Expect) (float64, []string) {
	var failed []string
	var passed int
	var total int

	if expect.Category != "" {
		total++
		ok := false
		for _, c := range strings.Split(expect.Category, "|") {
			// 业务侧抽取结果的 Category 字段；此处演示直接比对输出文本
			if strings.Contains(output, fmt.Sprintf("%q", c)) || strings.Contains(output, c) {
				ok = true
				break
			}
		}
		if ok {
			passed++
		} else {
			failed = append(failed, "category 不匹配: 期望 "+expect.Category)
		}
	}
	for _, kw := range expect.MustKeywords {
		total++
		if strings.Contains(output, kw) {
			passed++
		} else {
			failed = append(failed, "缺少关键词: "+kw)
		}
	}
	if total == 0 {
		return 1.0, nil // 无条件即视为通过，交由 judge 判
	}
	return float64(passed) / float64(total), failed
}
```

### 3.3 LLM-as-judge：复用第 2 篇的结构化输出

judge 自身也是一次模型调用，同样用 Structured Outputs 约束输出——**judge 自己的输出也要可解析，否则评测系统自己就成了新的故障源**：

```go
import (
	"context"

	"github.com/openai/openai-go/v3"
)

// JudgeResult judge 的结构化判分结果
type JudgeResult struct {
	Pass    bool   `json:"pass" jsonschema_description:"是否通过判定标准"`
	Reason  string `json:"reason" jsonschema_description:"一句话说明给分理由"`
	Score   int64  `json:"score" jsonschema_description:"1-10 分"`
}

// Judge 用模型当裁判。temperature=0 保证判分尽量确定
func Judge(ctx context.Context, client *openai.Client, criteria string, input, output string) (*JudgeResult, error) {
	// callModel 与 GenerateSchema 复用第 2 篇的实现（Extract 的底层原语）
	schema, err := GenerateSchema[JudgeResult]()
	if err != nil {
		return nil, err
	}
	prompt := fmt.Sprintf(`你是严格的评测裁判。判定标准：%s

原始输入：%s
被评测的输出：%s

只根据判定标准给出 pass/reason/score，不要因为输出风格额外扣分。`, criteria, input, output)
	text, err := callModel(ctx, client, schema, prompt)
	if err != nil {
		return nil, err
	}
	var jr JudgeResult
	if err := json.Unmarshal([]byte(*text), &jr); err != nil {
		return nil, fmt.Errorf("judge 输出解析失败: %w", err)
	}
	return &jr, nil
}
```

### 3.4 运行器：串起来并产出报告

```go
// Result 单条用例的评测结果
type Result struct {
	CaseID      string   `json:"case_id"`
	DetScore    float64  `json:"det_score"`
	DetFailed   []string `json:"det_failed,omitempty"`
	JudgePass   bool     `json:"judge_pass"`
	JudgeScore  int64    `json:"judge_score"`
	JudgeReason string   `json:"judge_reason"`
	FinalPass   bool     `json:"final_pass"`
}

// Summary 整体汇总
type Summary struct {
	Version    string  `json:"prompt_version"`
	Total      int     `json:"total"`
	Passed     int     `json:"passed"`
	PassRate   float64 `json:"pass_rate"`
	MeanJudge  float64 `json:"mean_judge_score"`
}

// Run 执行完整评测：先跑确定性判分，judge 只判确定性没过或需要语义判定的用例
func Run(ctx context.Context, cases []Case, evalFn func(string) (string, error)) ([]Result, error) {
	var results []Result
	for _, c := range cases {
		output, err := evalFn(c.Input)
		if err != nil {
			results = append(results, Result{CaseID: c.ID, DetFailed: []string{"执行出错: " + err.Error()}})
			continue
		}
		detScore, detFailed := DeterministicScore(output, c.Expect)
		r := Result{CaseID: c.ID, DetScore: detScore, DetFailed: detFailed}

		// 确定性全过且没有 judge 标准，直接通过；否则交给 judge 终审
		if detScore == 1.0 && c.JudgeCrit == "" {
			r.FinalPass = true
		} else {
			jr, err := Judge(ctx, judgeClient, c.JudgeCrit, c.Input, output)
			if err != nil {
				r.JudgeReason = "judge 调用失败: " + err.Error()
			} else {
				r.JudgePass, r.JudgeScore, r.JudgeReason = jr.Pass, jr.Score, jr.Reason
				r.FinalPass = detScore == 1.0 && jr.Pass
			}
		}
		results = append(results, r)
	}
	return results, nil
}
```

### 3.5 CI 门禁：测试即入口

Go 的最好载体就是 `go test`——评测就是一类集成测试：

```go
// eval/eval_test.go
package eval

import (
	"os"
	"testing"
)

// TestGoldenDataset 黄金数据集回归测试
// 需要 OPENAI_API_KEY；CI 中设置 EVAL_BASELINE_PASSRATE 作为门禁阈值
func TestGoldenDataset(t *testing.T) {
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("未设置 OPENAI_API_KEY，跳过评测")
	}
	cases, err := LoadDataset("../evals/dataset.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	// evalFn：走真实链路——第 1 篇 Registry 取 Prompt + 第 2 篇 Extract 结构化输出
	results, err := Run(t.Context(), cases, runRealPipeline)
	if err != nil {
		t.Fatal(err)
	}

	var passed int
	for _, r := range results {
		if r.FinalPass {
			passed++
		} else {
			t.Logf("FAIL %s: det=%v judge=%v (%s)", r.CaseID, r.DetFailed, r.JudgeScore, r.JudgeReason)
		}
	}
	passRate := float64(passed) / float64(len(cases))
	t.Logf("pass rate: %.1f%% (%d/%d)", passRate*100, passed, len(cases))

	// 门禁：基准值存仓库，低于基准 5 个百分点即失败
	baseline := 0.85 // 从 evals/baseline.txt 读取更佳
	if passRate < baseline-0.05 {
		t.Errorf("通过率 %.1f%% 低于门禁 %.1f%%，疑似回归", passRate*100, (baseline-0.05)*100)
	}
}
```

GitHub Actions 片段：

```yaml
name: llm-eval
on:
  push:
    paths: ["prompts/**", "evals/**", "eval/**"]
jobs:
  eval:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.26" }
      - run: go test ./eval/ -v -timeout 20m
        env:
          OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}
```

> **设计取舍说明**：`evalFn` 故意做成注入的函数而非直接调内部实现，让评测可以指向**任何一版 Prompt**——配合第 1 篇的 `Registry.Get(name, version)`，同一份数据集跑 `v2` 和 `latest` 两遍，输出两份报告 diff，这就是下一节的版本对比。

## 四、版本对比：评测集的真正用途

单次评测的价值有限，**对比评测才是杀器**。每次想改 Prompt 的流程变成：

1. `go test -run TestGoldenDataset -args -prompt=latest` → 新 Prompt 的评测报告；
2. 跑一遍 `prompt=v2`（当前线上稳定版）作为基准；
3. 对比通过率：整体不降、目标维度（比如你这次想改善的「退款类识别」）有提升，才允许灰度；
4. 灰度期间的线上决策日志（第 1 篇埋点）按版本聚合指标，与离线评测互相印证。

把报告写成 Markdown 存进 `evals/report/2026-11-27-latest.md` 并提交仓库，评审时直接附在 PR 描述里——「这次 Prompt 改动的影响」从此有了一行可查的数字。

## 五、踩坑清单

1. **评测集污染**：用模型自己生成的「标准答案」当期望，等于让考生自己出题。期望条件必须来自产品需求或人工标注。
2. **judge 与被测同源风险**：judge 用同一个模型时，它容易放过同类错误。预算允许就用**更强的模型当 judge**，且 temperature 设为 0。
3. **样本量幻觉**：10 条用例上 1 个百分点的差距是噪声不是信号。判断「回归」至少要有几十条覆盖各分类的用例，并关注分类维度而不只是整体通过率。
4. **别把 judge 分数当绝对值**：LLM-as-judge 的分数漂移是常态（模型更新、judge Prompt 微调都会移动基准），**版本对比用同一次评测会话内的相对值**。
5. **评测跑在 CI 里要限时限量**：给 `go test` 加 `-timeout`，用例多时加并发（`t.Parallel()` + 信号量），失败案例优先保留日志。
6. **确定性判分优先**：每加一条「用 judge 判」的用例，都先问自己能不能用关键词/schema 判——能就不用花模型钱。

## 六、本篇小结与下篇预告

至此我们有了迭代的「安全带」：Prompt 有版本（第 1 篇），输出有保险（第 2 篇），质量有度量（本篇）。下一篇进入成本维度：**Token 成本计量与限流**——请求级成本怎么算、预算怎么熔断、多层限流怎么防烧穿，让账单从「月底惊吓」变成「尽在掌握」。

---

## 版本说明（2026-11-25 核对）

| 依赖 | 本文使用版本 | 核对来源 |
| --- | --- | --- |
| Go | 1.26+（openai-go v3.107.0 硬性要求） | 官方 README Requirements |
| github.com/openai/openai-go/v3 | v3.107.0（2026-11-24） | proxy.golang.org `@latest` |

`callModel`、`GenerateSchema`、`finalJSONText` 等函数沿用第 2 篇的实现，其 API 形态已对照 v3.107.0 官方 README 逐字段核验。

## 相关阅读

- [LLM 应用工程化①：Prompt 管理别再硬编码，版本化、灰度发布与一键回滚](https://blog.911015.com/ai/llm-ops-1-prompt-mgmt.html) —— Registry 与决策日志是版本对比评测的前提
- [LLM 应用工程化②：结构化输出与 Schema 校验](https://blog.911015.com/ai/llm-ops-2-structured-output.html) —— judge 的结构化判分复用本篇防线
- [LLM 应用工程化系列聚合目录](https://blog.911015.com/ai/llm-ops.html)

> **声明**：本文由 AI 辅助生成，代码均对照官方文档与源码核验；如发现任何错误或不一致，欢迎在评论区指正。
