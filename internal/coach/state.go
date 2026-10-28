package coach

import (
	"encoding/json"
	"errors"
	"os"
)

var (
	errFinished    = errors.New("面试已结束：所有题目都已完成")
	errNotFinished = errors.New("还有未完成的题目，不能生成报告")
)

// DefaultStateFile 是状态持久化文件（多轮记忆）：面试中断后
// 下次运行 `coach continue` 可从中断处接着面。
const DefaultStateFile = ".coach-state.json"

// Save 把面试状态写盘。每答完一题就调用，断电/明天都能续。
func Save(st *State, path string) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path) // 原子替换，写一半断电不会损坏状态
}

// Load 从磁盘恢复面试状态。
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Clear 面试完成后删除状态文件。
func Clear(path string) error {
	return os.Remove(path)
}
