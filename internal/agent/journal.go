package agent

// journal 是自治执行的进度载体（docs/15 §7.3）：每行一条、追加写 + fsync
//（断电安全）。断点续跑从 journal 重放"已 ok/changed 的 idx"，从首个未完成
// idx 继续——幂等不能替代续跑：重跑意味着重做全部远程探测、金丝雀/健康门
// 语义被重置。

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JournalEntry 是一条进度记录（journal.ndjson 的一行）。
type JournalEntry struct {
	Seq     int64     `json:"seq"`  // 全 run 递增序号（增量拉取的游标）
	Host    string    `json:"host"` // 主机名（中继模式下多主机共用一本 journal）
	Idx     int       `json:"idx"`  // plan 任务序号（主机内稳定）
	Label   string    `json:"label"`
	State   string    `json:"state"` // ok|changed|failed|skipped|unreachable|ignored
	Changed bool      `json:"changed,omitempty"`
	Msg     string    `json:"msg,omitempty"`
	At      time.Time `json:"at"`
}

// Journal 是追加写日志（fsync 每条；多 goroutine 安全）。
type Journal struct {
	mu   sync.Mutex
	path string
	f    *os.File
	seq  int64
}

// OpenJournal 打开（或续写）journal 文件。
func OpenJournal(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	j := &Journal{path: path, f: f}
	j.seq = j.lastSeq()
	return j, nil
}

// Append 写入一条记录（分配递增 seq；fsync 保证断电后已提交条目不丢）。
func (j *Journal) Append(e JournalEntry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	e.Seq = j.seq
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		j.seq--
		return err
	}
	if _, err := j.f.Write(append(line, '\n')); err != nil {
		return err
	}
	return j.f.Sync()
}

// Close 关闭句柄。
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}

// lastSeq 扫描既有文件取最大 seq（重启后续写不回退）。
func (j *Journal) lastSeq() int64 {
	entries, err := ReadJournal(j.path, 0)
	if err != nil {
		return 0
	}
	if len(entries) == 0 {
		return 0
	}
	return entries[len(entries)-1].Seq
}

// ReadJournal 读取 seq > sinceSeq 的全部记录（增量拉取；文件损坏行跳过并
// 附带告警——journal 是追加行协议，中间坏行不应抹掉前后有效记录）。
func ReadJournal(path string, sinceSeq int64) ([]JournalEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []JournalEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var e JournalEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// CompletedIdx 重放 journal：返回主机 → 已完成（ok/changed，或显式 ignored
// 的失败豁免）任务序号集合。failed/skipped 不算完成——续跑要重做它们。
func CompletedIdx(path string) (map[string]map[int]bool, error) {
	entries, err := ReadJournal(path, 0)
	if err != nil {
		return nil, err
	}
	done := map[string]map[int]bool{}
	for _, e := range entries {
		switch e.State {
		case "ok", "changed", "ignored":
			if e.Idx <= 0 {
				continue
			}
			if done[e.Host] == nil {
				done[e.Host] = map[int]bool{}
			}
			done[e.Host][e.Idx] = true
		}
	}
	return done, nil
}

// runsDirDefault 是自治执行持久化根目录的内置默认。
const runsDirDefault = "/var/lib/wdp/runs"

// SetRunsDir 覆盖持久化根目录（测试注入用）。
func (s *Server) SetRunsDir(dir string) { s.runsDir = dir }

// runsRoot 返回生效的持久化根目录。
func (s *Server) runsRoot() string {
	if s.runsDir != "" {
		return s.runsDir
	}
	return runsDirDefault
}

// runDir 返回单个 run 的目录。
func (s *Server) runDir(runID string) string {
	return filepath.Join(s.runsRoot(), runID)
}

// WriteStateFile 原子写 state 文件（running|done|failed|cancelled）。
func WriteStateFile(runDir, state string) error {
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(runDir, ".state.tmp")
	if err := os.WriteFile(tmp, []byte(state), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(runDir, "state"))
}

// ReadStateFile 读取 state（缺失返回空串）。
func ReadStateFile(runDir string) string {
	b, err := os.ReadFile(filepath.Join(runDir, "state"))
	if err != nil {
		return ""
	}
	return string(b)
}
