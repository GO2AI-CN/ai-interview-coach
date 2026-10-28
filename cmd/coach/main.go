// AI 模拟面试教练——命令行版。
//
// 用法：
//
//	# 开一场新面试（可选传简历 PDF，面试官会先读简历）
//	coach new --pos "高级后端工程师" --resume ./resume.pdf
//
//	# 昨天的面试没面完，接着面
//	coach continue
//
// 环境变量：OPENAI_API_KEY（必填）、OPENAI_BASE_URL（可选，兼容 DeepSeek/Kimi 等）、
// COACH_MODEL（可选，默认 gpt-4o-mini）。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"ai-interview-coach/internal/bot"
	"ai-interview-coach/internal/coach"
	"ai-interview-coach/internal/llm"
	"ai-interview-coach/internal/report"
	"ai-interview-coach/internal/resume"
	"ai-interview-coach/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`AI 模拟面试教练

用法：
  coach new --pos "目标岗位" [--resume 简历.pdf]   开一场新面试
  coach continue                                   接着上次的面试继续
  coach web [--addr :8080]                         启动网页版（流式点评）
  coach bot                                          启动飞书机器人（长连接）

环境变量：
  OPENAI_API_KEY   API Key（必填）
  OPENAI_BASE_URL  兼容 OpenAI 协议的端点（可选，如 DeepSeek/Kimi）
  COACH_MODEL      模型名（可选，默认 gpt-4o-mini）
  FEISHU_APP_ID / FEISHU_APP_SECRET  飞书自建应用凭据（bot 命令必填）`)
}

func run() error {
	ctx := context.Background()
	client := llm.NewOpenAIClient(os.Getenv("COACH_MODEL"))
	var it *coach.Interview

	switch os.Args[1] {
	case "new":
		fs := flag.NewFlagSet("new", flag.ExitOnError)
		pos := fs.String("pos", "", "目标岗位（必填）")
		resumePath := fs.String("resume", "", "简历 PDF 路径（可选）")
		fs.Parse(os.Args[2:])
		if *pos == "" {
			return fmt.Errorf("请用 --pos 指定目标岗位")
		}

		resumeText := ""
		if *resumePath != "" {
			sum, err := resume.Summarize(ctx, client, *resumePath)
			if err != nil {
				return fmt.Errorf("简历解析失败: %w", err)
			}
			resumeText = sum.SummaryText()
			fmt.Println("已读取简历，面试官将针对你的项目经历出题。")
			fmt.Println()
		}

		var err error
		it, err = coach.NewInterview(ctx, client, *pos, resumeText)
		if err != nil {
			return fmt.Errorf("生成面试计划失败: %w", err)
		}
		if err := coach.Save(it.State, coach.DefaultStateFile); err != nil {
			return err
		}
		fmt.Printf("面试计划已生成：共 %d 题。\n\n", len(it.State.Plan.Questions))

	case "continue":
		st, err := coach.Load(coach.DefaultStateFile)
		if err != nil {
			return fmt.Errorf("没有找到上次的状态文件，请先运行 new: %w", err)
		}
		it = coach.ResumeInterview(st, client)
		fmt.Printf("继续面试：第 %d/%d 题。\n\n", st.Cursor+1, len(st.Plan.Questions))

	case "web":
		fs := flag.NewFlagSet("web", flag.ExitOnError)
		addr := fs.String("addr", ":8080", "监听地址")
		fs.Parse(os.Args[2:])
		return web.NewServer(client).ListenAndServe(*addr)

	case "bot":
		// 飞书机器人（长连接模式，无需公网 IP）。
		// 凭据从环境变量读，避免命令行历史泄露。
		appID, appSecret := os.Getenv("FEISHU_APP_ID"), os.Getenv("FEISHU_APP_SECRET")
		if appID == "" || appSecret == "" {
			return fmt.Errorf("请先设置环境变量 FEISHU_APP_ID / FEISHU_APP_SECRET")
		}
		b := bot.NewBot(bot.NewLarkSender(appID, appSecret), bot.CoachFactory(client), 0)
		return b.Run(ctx, appID, appSecret)

	default:
		usage()
		return fmt.Errorf("未知命令: %s", os.Args[1])
	}

	return interviewLoop(ctx, it, client)
}

// interviewLoop 逐题问答：作答 → 评估 → 追问 or 下一题 → 最后出报告。
func interviewLoop(ctx context.Context, it *coach.Interview, client llm.Client) error {
	in := bufio.NewReader(os.Stdin)
	for {
		q := it.CurrentQuestion()
		if q == nil {
			break
		}
		tag := fmt.Sprintf("[Q%d·%s]", it.State.Cursor+1, q.Dimension)
		if q.Source == "resume" {
			tag += "(简历深挖)"
		}
		fmt.Printf("%s %s\n> ", tag, q.Question)

		answer, _ := in.ReadString('\n')
		answer = strings.TrimSpace(answer)
		if answer == "" {
			fmt.Println("（跳过本题）")
			it.State.Cursor++ // 空输入直接跳过
			it.State.Pending = nil
			if err := coach.Save(it.State, coach.DefaultStateFile); err != nil {
				return err
			}
			continue
		}

		followUp, msg, err := it.Answer(ctx, answer)
		if err != nil {
			return err
		}
		ev := it.State.Pending.Evaluation
		fmt.Printf("  评估：置信度 %d/5，要点覆盖 %.0f%%，%s\n", ev.Confidence, ev.Coverage*100, ev.Suggestion)
		if err := coach.Save(it.State, coach.DefaultStateFile); err != nil {
			return err
		}
		if followUp {
			fmt.Printf("\n[追问] %s\n> ", msg)
		} else {
			fmt.Println()
		}
	}

	// 全部问完，生成结构化评分报告
	rep, err := it.Finish(ctx)
	if err != nil {
		return err
	}
	md := report.Markdown(rep)
	if err := os.WriteFile("report.md", []byte(md), 0o644); err != nil {
		return err
	}
	coach.Clear(coach.DefaultStateFile) // 面试结束，清除状态
	fmt.Printf("面试结束。综合得分 %d/100，报告已写入 report.md\n", rep.Overall)
	return nil
}
