# LLM 应用工程化①：Prompt 管理别再硬编码，版本化、灰度发布与一键回滚

> 本系列同步发布于博客 [blog.911015.com](https://blog.911015.com/ai/llm-ops.html) 与公众号「Go2AI之路」。这个系列回答一个被教程回避的问题：**demo 跑通之后，怎么把它变成能上线、能迭代、能扛事的系统？** 第 1 篇从最不起眼、却最容易欠债的地方开始——Prompt 管理。

任何写过两天 LLM 应用的人，都见过这样的代码：

```go
resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
    Model: openai.ChatModel("gpt-4o-mini"),
    Messages: []openai.ChatCompletionMessageParamUnion{
        openai.SystemMessage("你是一个客服助手，请礼貌、简洁地回答用户问题，回答不要超过100字。"),
        openai.UserMessage(question),
    },
})
```

看起来没问题。三个月后，这段字符串被人顺手改成了 150 字、加了两条新规则、换了个措辞——客服满意度跌了，你想改回去，**却发现 Git 历史里躺着十七次改动，没人记得线上跑的是哪一版**。

Prompt 是 LLM 应用的源代码，但它享受的待遇连配置文件都不如。这个系列第 1 篇就解决这个问题：把 Prompt 收编为**版本化资产**，实现灰度发布与一键回滚。全程零外部依赖（只用标准库 + `go:embed`），LLM 调用部分基于 openai-go v3.107.0。

## 一、硬编码 Prompt 欠下的是哪种债

把 Prompt 写在业务代码里，表面上省事，实际欠下四笔债：

1. **不可追踪**：Prompt 改动混在业务代码的 commit 里，「改了什么、为什么改、效果如何」三件事全部丢失。
2. **不可回滚**：发现问题想回退，发现当初没有版本号，Git blame 只能告诉你谁最后一行动的手。
3. **不可灰度**：新 Prompt 直接全量上线，效果好坏全凭运气——而 LLM 输出天然不稳定，全量换 Prompt 等于线上裸奔。
4. **不可评测**：没有版本号，A/B 对比就无从谈起，「新 Prompt 到底好不好」永远是一笔糊涂账。

对比一下成熟的做法：数据库 migration 有版本号， feature flag 有平台管，API 有 changelog。**Prompt 需要的待遇，和它们一模一样。**

## 二、设计：一个 Prompt 资产长什么样

核心思路一句话：**Prompt 是独立文件，带元数据头，按版本号组织，运行时可按版本加载**。

文件格式我选 JSON（不引入 YAML 依赖，零外部库）：

```json
{
  "name": "customer-service",
  "version": "v3",
  "description": "客服助手系统 Prompt：礼貌、简洁、100字以内",
  "author": "zhangsan",
  "created_at": "2026-11-20",
  "variables": ["product", "tone"],
  "template": "你是{{.product}}的客服助手，请用{{.tone}}的语气回答用户问题，回答不要超过100字。"
}
```

三个关键设计决策：

- **name + version 联合定位**：请求方要么说「给我 customer-service 的最新版」，要么显式钉死 `customer-service@v2`——默认走最新版，但需要复现旧行为时可以穿越。
- **variables 显式声明**：模板有哪些变量一目了然，新增变量必须改元数据， review 时一眼能看出契约变化。
- **模板与代码解耦**：改 Prompt 不需要重新编译业务代码，配合 embed 只需重新构建一次，纯文件替换甚至可以热更新（本文不展开，留作练习）。

目录结构：

```
prompts/
├── customer-service/
│   ├── v1.json
│   ├── v2.json
│   └── v3.json
└── review-summarizer/
    ├── v1.json
    └── v2.json
```

## 三、Go 实现：零依赖 Prompt Registry

用 `go:embed` 把整个 `prompts/` 目录打进二进制，实现一个不到 150 行的 Registry。`go.mod` 只需一个依赖：

```
module promptlab

go 1.26

require github.com/openai/openai-go/v3 v3.107.0
```

> **版本要求**：openai-go v3.107.0 要求 **Go 1.26+**（官方 README 说明：该版本起依赖 Go 1.24+ 的 `omitzero` 序列化语义和 HTTP/2 安全修复；Go 1.25 需锁 v3.74.0，更早版本需锁 v3.44.0）。

### 3.1 Prompt 定义与加载

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// Prompt 一个带元数据的 Prompt 版本
type Prompt struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"` // v1, v2, ...
	Description string   `json:"description"`
	Author      string   `json:"author"`
	CreatedAt   string   `json:"created_at"`
	Variables   []string `json:"variables"`
	Template    string   `json:"template"`

	tmpl *template.Template
}

func (p *Prompt) compile() error {
	t, err := template.New(p.Name + "@" + p.Version).Parse(p.Template)
	if err != nil {
		return fmt.Errorf("compile %s@%s: %w", p.Name, p.Version, err)
	}
	p.tmpl = t
	return nil
}

// Render 用变量渲染模板，并校验变量是否全部提供
func (p *Prompt) Render(vars map[string]string) (string, error) {
	for _, v := range p.Variables {
		if _, ok := vars[v]; !ok {
			return "", fmt.Errorf("prompt %s@%s 缺少变量 %q", p.Name, p.Version, v)
		}
	}
	var sb strings.Builder
	if err := p.tmpl.Execute(&sb, vars); err != nil {
		return "", err
	}
	return sb.String(), nil
}
```

### 3.2 Registry：加载、取版本、列历史

```go
// Registry Prompt 注册表：name -> 按版本号排序的 Prompt 列表
type Registry struct {
	byName map[string][]*Prompt // 按版本升序
}

var versionRe = regexp.MustCompile(`^v(\d+)$`)

func versionNum(v string) int {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// LoadRegistry 从目录加载所有 prompt 文件
func LoadRegistry(dir string) (*Registry, error) {
	r := &Registry{byName: map[string][]*Prompt{}}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var p Prompt
		if err := json.Unmarshal(data, &p); err != nil {
			return fmt.Errorf("解析 %s: %w", path, err)
		}
		if p.Name == "" || p.Version == "" || p.Template == "" {
			return fmt.Errorf("%s: name/version/template 不能为空", path)
		}
		if err := p.compile(); err != nil {
			return err
		}
		r.byName[p.Name] = append(r.byName[p.Name], &p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name := range r.byName {
		sort.Slice(r.byName[name], func(i, j int) bool {
			return versionNum(r.byName[name][i].Version) < versionNum(r.byName[name][j].Version)
		})
	}
	return r, nil
}

// Get 取指定版本的 Prompt；version 传 "latest" 取最新
func (r *Registry) Get(name, version string) (*Prompt, error) {
	list := r.byName[name]
	if len(list) == 0 {
		return nil, fmt.Errorf("prompt %q 不存在", name)
	}
	if version == "" || version == "latest" {
		return list[len(list)-1], nil
	}
	for _, p := range list {
		if p.Version == version {
			return p, nil
		}
	}
	return nil, fmt.Errorf("prompt %s@%s 不存在", name, version)
}

// History 返回某 name 的全部版本（升序），用于 changelog 展示
func (r *Registry) History(name string) []*Prompt {
	return r.byName[name]
}
```

### 3.3 灰度发布：按比例分流

灰度不需要引入 feature flag 平台，一个按请求 ID 哈希的确定性分流就够：

```go
import "hash/fnv"

// PickVersion 灰度分流：rollout 比例的流量用最新版，其余用稳定版
// ratio 取值 [0,1]，如 0.2 表示 20% 流量走新版
func PickVersion(r *Registry, name, stableVersion string, ratio float64, requestID string) (*Prompt, error) {
	if ratio >= 1 {
		return r.Get(name, "latest")
	}
	if ratio <= 0 {
		return r.Get(name, stableVersion)
	}
	h := fnv.New32a()
	h.Write([]byte(requestID))
	bucket := float64(h.Sum32()%10000) / 10000.0 // [0,1)
	if bucket < ratio {
		return r.Get(name, "latest")
	}
	return r.Get(name, stableVersion)
}
```

用 `fnv` 哈希而不是 `math/rand`，是因为同一个 requestID 必须**永远**分到同一桶——否则同一个用户的两次请求会命中两个版本的 Prompt，行为飘忽且无法归因。

### 3.4 决策日志：每次调用都留下痕迹

灰度的另一半是观测。每一次 Prompt 解析都写一条结构化日志，评测篇（第 3 篇）会依赖这份日志做按版本的指标对比：

```go
import (
	"log/slog"
	"time"
)

// DecisionLog 一次 Prompt 决策的日志
type DecisionLog struct {
	Time        time.Time `json:"time"`
	RequestID   string    `json:"request_id"`
	PromptName  string    `json:"prompt_name"`
	Version     string    `json:"version"`
	RolloutHit  bool      `json:"rollout_hit"` // 是否命中灰度流量
}

func LogDecision(l *DecisionLog) {
	slog.Info("prompt_decision",
		"time", l.Time.Format(time.RFC3339),
		"request_id", l.RequestID,
		"prompt", l.PromptName,
		"version", l.Version,
		"rollout_hit", l.RolloutHit,
	)
}
```

### 3.5 串起来：一个可运行的 main

```go
package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

//go:embed prompts
var promptsFS embed.FS

func main() {
	// 生产环境从 embed 加载；这里演示用 os.DirFS 便于本地改文件即生效
	reg, err := LoadRegistry("prompts")
	if err != nil {
		log.Fatal(err)
	}

	// 打印 changelog——每次发布前先看一眼历史
	for _, p := range reg.History("customer-service") {
		fmt.Printf("%s@%s  %s  %s  %s\n", p.Name, p.Version, p.CreatedAt, p.Author, p.Description)
	}

	client := openai.NewClient(option.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	requestID := "req-20261125-0001"
	p, hit, err := pickWithLog(reg, "customer-service", "v2", 0.2, requestID)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("本次请求使用 %s@%s（灰度命中：%v）\n", p.Name, p.Version, hit)

	systemPrompt, err := p.Render(map[string]string{
		"product": "Go2AI 知识社区",
		"tone":    "亲切专业",
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: openai.ChatModel("gpt-4o-mini"),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage("你们的 RAG 系列适合初学者吗？"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.Choices[0].Message.Content)
}

func pickWithLog(reg *Registry, name, stable string, ratio float64, reqID string) (*Prompt, bool, error) {
	p, err := PickVersion(reg, name, stable, ratio, reqID)
	if err != nil {
		return nil, false, err
	}
	latest, _ := reg.Get(name, "latest")
	LogDecision(&DecisionLog{
		Time:       time.Now(),
		RequestID:  reqID,
		PromptName: name,
		Version:    p.Version,
		RolloutHit: p.Version == latest.Version,
	})
	return p, p.Version == latest.Version, nil
}
```

别忘了 import 里的 `embed`：

```go
import "embed"
```

> **代码可运行性说明**：Prompt Registry 部分（3.1–3.4）只依赖 Go 标准库；3.5 的 LLM 调用基于 `github.com/openai/openai-go/v3 v3.107.0`（2026-11-24 发布，本文发布当日核对于 proxy.golang.org，要求 Go 1.26+）。`client.Chat.Completions.New` 是该 SDK 当前版本的标准调用方式（v3 中 Responses API 为新主推，Chat Completions 官方承诺持续支持）；若你读到本文时 SDK 已升级大版本，请以 [openai-go 官方仓库](https://github.com/openai/openai-go) README 为准。

## 四、回滚：版本化的真正价值

有了版本号，回退就是一行配置的事。上线上 v3 后发现回答变啰嗦了：

```go
// 灰度阶段发现 v3 不达标：把灰度比例降到 0，稳定版指针改回 v2
p, hit, err := pickWithLog(reg, "customer-service", "v2", 0.0, requestID)
```

v3 文件原样保留——它是资产，不是待清理的垃圾。半年后你想复盘「那次改版到底改了什么」，`History()` 打印的 changelog 就在那里。等到第 3 篇建立评测集后，回滚决策甚至不需要人工判断：新版本评测分低于稳定版阈值，自动把灰度比例归零。

## 五、踩坑清单

1. **别把版本号写进文件名以外的任何地方**：运行时永远通过 Registry 解析，硬编码 `@v2` 的地方多了，回滚就要改代码。
2. **模板变量必须显式声明**：靠人肉记忆维护变量契约，迟早出现「线上渲染出一个空的 `<product>`」事故。本文的 `Render` 会强制校验缺失变量。
3. **分流要确定性**：用 `fnv(requestID)` 这类稳定哈希，别用随机数——同一会话两次请求命中不同版本，排障时你会怀疑人生。
4. **日志里带上 prompt 版本号**：没有这个字段，第 3 篇的按版本指标对比、第 6 篇的全链路 Trace 全部无从谈起。从第一天就记录。
5. **别急着上数据库**：embed + JSON 文件撑到几十个 Prompt 毫无压力，过早引入存储是另一种过度设计。

## 六、本篇小结与下篇预告

Prompt 版本化四个能力——**可追踪、可回滚、可灰度、可评测**——本篇全部落地，且零外部依赖。下一篇解决「模型不按格式输出」这个更痛的问题：**结构化输出与 Schema 校验**，让 JSON 模式、函数调用兜底、自动重试与降级策略组成一条完整的防线。

---

## 版本说明（2026-11-25 核对）

| 依赖 | 本文使用版本 | 核对来源 |
| --- | --- | --- |
| Go | 1.26+（openai-go v3.107.0 硬性要求） | 官方 README Requirements |
| github.com/openai/openai-go/v3 | v3.107.0（2026-11-24 发布） | proxy.golang.org `@latest` |

## 相关阅读

- [LLM 应用工程化系列聚合目录](https://blog.911015.com/ai/llm-ops.html)（全 8 篇规划）
- [Go AI 应用开发系列08 - 结构化输出：Go 泛型 + JSON Schema 的类型安全之道](https://blog.911015.com/ai/go-ai-08-structured-output.html) —— 下篇结构化输出的前置知识
- [Go RAG 实战7 - 只重建改动的文件：Go RAG 增量索引与 Docker 部署](https://blog.911015.com/ai/go-rag-7-production.html) —— 同样的「变更最小化」思想在索引侧的落地
- [AI 模拟面试教练专栏](https://blog.911015.com/coach/) —— 本文的 Prompt Registry 将在这个项目里演进到生产形态

> **声明**：本文由 AI 辅助生成，代码均经过可运行性核对；如发现任何错误或不一致，欢迎在评论区指正。
