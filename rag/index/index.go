// Package index 是 RAG 系列第 7 篇《生产化》的配套代码：增量索引与文档变更检测。
//
// 核心思路一句话：给每个已索引文件记一笔账（manifest）——内容哈希 + 块 ID 清单。
// 每次同步时重新计算磁盘文件的哈希，两相对比：
//   - 哈希一致      → 文件没动，整块跳过（embedding 都不必重算）；
//   - 哈希变了/新增  → 只重建这一个文件：先按旧账删掉残块，再切分嵌入写回；
//   - 账上有、盘上无 → 文件被删了，按旧账把它的块从向量库清掉。
//
// manifest 落盘为向量库目录下的 manifest.json，与 chromem-go 的持久化文件放一起，
// 备份/迁移时整个目录拷走即可。
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// FileState 是单个文件在向量库里的"账"：内容哈希 + 它占用的块 ID。
type FileState struct {
	Hash string   `json:"hash"` // 文件内容的 SHA-256（hex）
	IDs  []string `json:"ids"`  // 该文件写入向量库的全部块 ID
}

// Manifest 是全部已索引文件的账本。
type Manifest struct {
	Files map[string]FileState `json:"files"`
}

// Load 从 path 读账本；文件不存在（首次同步）时返回空账，不视为错误。
func Load(path string) (*Manifest, error) {
	m := &Manifest{Files: map[string]FileState{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, err
	}
	return m, nil
}

// Save 把账本写回 path（先写临时文件再改名，避免半写状态损坏账本）。
func (m *Manifest) Save(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Hash 计算文件内容的 SHA-256，十六进制编码。内容变一字节，哈希全变。
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// IDsFor 生成 file 第 0..n-1 块的 ID，约定与 store.AddChunks 一致（"文件名#序号"）。
func IDsFor(file string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = file + "#" + strconv.Itoa(i)
	}
	return ids
}

// Plan 对比磁盘现状（file→hash）与账本，把文件分成三类：
//
//	rebuild   新增或内容已变更，需要重建索引的文件（稳定排序，方便复现）
//	deleted   账上有但磁盘上已消失，需要清掉残块的文件
//	unchanged 哈希一致，整块跳过
func (m *Manifest) Plan(disk map[string]string) (rebuild, deleted, unchanged []string) {
	for f, h := range disk {
		if st, ok := m.Files[f]; ok && st.Hash == h {
			unchanged = append(unchanged, f)
		} else {
			rebuild = append(rebuild, f)
		}
	}
	for f := range m.Files {
		if _, ok := disk[f]; !ok {
			deleted = append(deleted, f)
		}
	}
	sort.Strings(rebuild)
	sort.Strings(deleted)
	sort.Strings(unchanged)
	return rebuild, deleted, unchanged
}

// Path 返回向量库目录下账本文件的约定路径。
func Path(dbDir string) string {
	return filepath.Join(dbDir, "manifest.json")
}
