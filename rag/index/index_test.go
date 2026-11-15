package index

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestPlan(t *testing.T) {
	m := &Manifest{Files: map[string]FileState{
		"same.md":    {Hash: Hash([]byte("v1")), IDs: IDsFor("same.md", 3)},
		"changed.md": {Hash: Hash([]byte("old")), IDs: IDsFor("changed.md", 2)},
		"gone.md":    {Hash: Hash([]byte("x")), IDs: IDsFor("gone.md", 1)},
	}}
	disk := map[string]string{
		"same.md":    Hash([]byte("v1")),     // 哈希一致 → 不动
		"changed.md": Hash([]byte("new")),     // 哈希变了 → 重建
		"new.md":     Hash([]byte("brandnew")), // 账上没有 → 重建
	}
	rebuild, deleted, unchanged := m.Plan(disk)
	if !slices.Equal(rebuild, []string{"changed.md", "new.md"}) {
		t.Errorf("rebuild = %v", rebuild)
	}
	if !slices.Equal(deleted, []string{"gone.md"}) {
		t.Errorf("deleted = %v", deleted)
	}
	if !slices.Equal(unchanged, []string{"same.md"}) {
		t.Errorf("unchanged = %v", unchanged)
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.json")

	// 首次同步：账本不存在，返回空账而不是报错
	m, err := Load(p)
	if err != nil || len(m.Files) != 0 {
		t.Fatalf("Load(不存在) = %v, %v", m, err)
	}

	m.Files["a.md"] = FileState{Hash: "abc", IDs: []string{"a.md#0", "a.md#1"}}
	if err := m.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	st := got.Files["a.md"]
	if st.Hash != "abc" || !slices.Equal(st.IDs, []string{"a.md#0", "a.md#1"}) {
		t.Errorf("round trip 不一致: %+v", st)
	}
}

func TestIDsFor(t *testing.T) {
	ids := IDsFor("f.md", 3)
	want := []string{"f.md#0", "f.md#1", "f.md#2"}
	if !slices.Equal(ids, want) {
		t.Errorf("IDsFor = %v, want %v", ids, want)
	}
}
