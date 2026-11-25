// Package registry 实现《LLM 应用工程化》系列第 1 篇的 Prompt Registry：
// Prompt 是带元数据的版本化资产，支持确定性灰度、决策日志与一键回滚。
package registry

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Prompt 一个带元数据的 Prompt 版本
type Prompt struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
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

// Render 渲染模板并校验变量是否全部提供
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

// Registry Prompt 注册表：name -> 按版本升序排列的 Prompt 列表
type Registry struct {
	byName map[string][]*Prompt
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

// Load 从 fs.FS 加载全部 Prompt 文件（可传入 embed.FS）
func Load(fsys fs.FS, dir string) (*Registry, error) {
	r := &Registry{byName: map[string][]*Prompt{}}
	err := fs.WalkDir(fsys, dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		data, err := fs.ReadFile(fsys, path)
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

// Get 取指定版本；version 传 "" 或 "latest" 取最新
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

// History 返回某 name 的全部版本（升序），用于 changelog
func (r *Registry) History(name string) []*Prompt {
	return r.byName[name]
}

// PickVersion 灰度分流：ratio 比例的流量走最新版，其余走 stableVersion。
// 用 requestID 的 fnv 哈希做确定性分流：同一请求永远命中同一桶。
func PickVersion(r *Registry, name, stableVersion string, ratio float64, requestID string) (*Prompt, error) {
	if ratio >= 1 {
		return r.Get(name, "latest")
	}
	if ratio <= 0 {
		return r.Get(name, stableVersion)
	}
	h := fnv.New32a()
	h.Write([]byte(requestID))
	bucket := float64(h.Sum32()%10000) / 10000.0
	if bucket < ratio {
		return r.Get(name, "latest")
	}
	return r.Get(name, stableVersion)
}

// DecisionLog 一次 Prompt 决策的日志记录
type DecisionLog struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id"`
	PromptName string    `json:"prompt_name"`
	Version    string    `json:"version"`
	RolloutHit bool      `json:"rollout_hit"`
}

// LogDecision 输出结构化决策日志（评测按版本对比、Trace 关联都依赖它）
func LogDecision(l *DecisionLog) {
	slog.Info("prompt_decision",
		"time", l.Time.Format(time.RFC3339),
		"request_id", l.RequestID,
		"prompt", l.PromptName,
		"version", l.Version,
		"rollout_hit", l.RolloutHit,
	)
}
