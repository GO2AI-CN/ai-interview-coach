# AI 面试官先读简历再出题：Go 简历解析实战（下）

上篇我们搭好了 AI 模拟面试教练的骨架：出题、追问、打分全自动。但很多读者反馈一个问题：通用题太泛了，面试官应该**先看我的简历，再针对我的项目问**。

这就是下篇要解决的：接入简历解析（PDF 文本提取 → 模型摘要 → 针对性出题），加上多轮记忆（面试可以今天面一半、明天接着面），最后放出完整工程仓库。

## 一、简历解析：为什么不让模型直接读 PDF

大模型 API 吃的是文本，而简历大多是 PDF。两条路：

| 方案 | 做法 | 评价 |
|------|------|------|
| 多模态上传 | 把 PDF 当图片/文件直接传给多模态模型 | 贵、慢，而且很多兼容端点不支持文件上传 |
| 本地提取文本 | 纯 Go 库提取文本，再把文本喂给模型 | 便宜、快、可控，**我们的选择** |

选本地提取后，库怎么选又是第二个坑：

| 库 | 特点 | 结论 |
|----|------|------|
| `ledongthuc/pdf` | 纯 Go、无 CGO、API 简单 | ✅ 选用，交叉编译无压力 |
| `pdfcpu` | 功能全，但偏文档处理 | 提取文本不是它的主战场 |
| 调用 `pdftotext` | 准确率高 | 引入外部二进制依赖，部署麻烦 |

## 二、提取 + 摘要：让模型只读精华

直接拿提取出的几千字简历原文塞进出题提示词有两个问题：费 token，而且长文本会稀释出题指令。所以提取之后先让模型生成一份**结构化摘要**，只把摘要注入出题提示词：

```go
// internal/resume/resume.go
func Summarize(ctx context.Context, s Summarizer, pdfPath string) (*Summary, error) {
    text, err := ExtractText(pdfPath) // 纯 Go 提取，逐页拼接
    if err != nil {
        return nil, err
    }
    // 摘要而非全文进提示词：省 token，也避免长文本稀释指令
    var sum Summary
    err = s.ChatJSON(ctx, system, fmt.Sprintf("输出结构化摘要(JSON)："+
        "skills、projects(名称/技术栈/量化亮点)、suspect_points(需要验证的可疑点)…\n%s",
        truncate(text, 6000)), "resume_summary", "简历结构化摘要", summarySchema, &sum)
    return &sum, err
}
```

注意 `suspect_points` 这个设计：让模型自己列出简历里**可疑的点**——量化数据是否夸张、技能与年限是否匹配。这些点就是简历深挖题的弹药。

## 三、针对性出题：面试官的"你简历里写了……"

有了摘要，出题提示词里追加一段指令即可（关键片段）：

```go
// internal/coach/prompts.go
if resume != "" {
    user += fmt.Sprintf(`另附候选人简历摘要，请额外生成 2~3 道基于简历的
深挖题(source 标 "resume")：针对简历中的项目经历、技术栈和量化成果提问，
验证其真实性。禁止问候选人明显不具备的技能。简历摘要：%s`, resume)
}
```

效果就是面试时你会看到带 `(简历深挖)` 标记的题目，比如：「你简历里写了做过百万级 QPS 的订单网关，那我们来聊聊它的限流设计」。这类题在真实面试里杀伤力最大，也是通用八股文给不了的。

## 四、多轮记忆：今天面 3 题，明天接着面

真实面试不会 10 分钟草草结束，但命令行版很可能被 Ctrl+C 打断。所以每答完一题就把面试状态落盘，下次 `coach continue` 从断点续面：

```go
// internal/coach/state.go
func Save(st *State, path string) error {
    b, _ := json.MarshalIndent(st, "", "  ")
    tmp := path + ".tmp"
    os.WriteFile(tmp, b, 0o600)
    return os.Rename(tmp, path) // 原子替换，写一半断电不损坏状态
}
```

`State` 里存了面试计划、当前题号、已完成的问答记录（含每轮评估），恢复后连之前的评估上下文都在——模型做最终打分时能"记得"你前面所有的回答。

## 五、完整工程与验证

工程结构如下，完整代码已开源：`https://github.com/<OWNER>/ai-interview-coach`

```text
cmd/coach/        命令行入口（交互问答循环）
internal/coach/   面试状态机：计划/评估/追问判断/状态持久化
internal/llm/     openai-go v3 封装（JSON Schema + 纯文本）
internal/resume/  PDF 文本提取 + 简历结构化摘要
internal/report/  评分报告 Markdown 渲染
```

跑法：

```bash
export OPENAI_API_KEY=你的Key
go run ./cmd/coach new --pos "高级后端工程师" --resume ./resume.pdf
# 面到一半退出，下次：
go run ./cmd/coach continue
```

**没有 Key 也能验证**：所有 LLM 调用都走 `llm.Client` 接口，测试里用 Mock 客户端驱动完整状态机（追问 → 套娃上限 → 强制放行 → 状态恢复），`go test ./...` 全绿才敢发出来。

## 六、踩坑清单

| 坑 | 现象 | 解法 |
|----|------|------|
| 扫描件简历 | 提取出来是空字符串 | 检测空文本并报错提示需先 OCR |
| 追问无限套娃 | 同一题被追问十几轮 | `MaxFollowUpsPerQuestion=2` 硬上限 |
| 追问没方向 | 模型复读"再详细点" | 把 missed checkpoints 换成本轮考察标准 |
| 状态写坏 | 断电后无法续面 | 临时文件 + `os.Rename` 原子落盘 |
| 简历稀释指令 | 题目质量下降 | 只注入摘要，且截断到 6000 字符 |

## 七、总结

下篇做完了两件事：简历解析让出题从「通用八股」变成「针对你的项目深挖」，多轮记忆让面试可以跨天进行。整套工程不到 800 行 Go，核心逻辑全部有测试覆盖。

如果这个项目对你有帮助，欢迎点赞、在看、转发，关注「GO语言入门之路」，后续还会用它做更多 AI 应用实战。

---

*本文系 AI 辅助生成，内容已经人工核对，如有错误欢迎评论区留言指正。*
