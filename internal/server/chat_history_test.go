package server

import (
	"strings"
	"testing"
)

// 历史切分：divider 领起新段，最近一段全量返回，更早的段出元数据；
// 连续重置产生的空段不进列表，但 seg 序号保持稳定可寻址。
func TestSplitConversations(t *testing.T) {
	log := strings.Join([]string{
		`{"ts":"2026-07-01T10:00:00Z","kind":"user","text":"第一段问题"}`,
		`{"ts":"2026-07-01T10:00:05Z","kind":"event","event":{"type":"assistant"}}`,
		`{"ts":"2026-07-02T09:00:00Z","kind":"divider"}`,
		`{"ts":"2026-07-02T09:00:01Z","kind":"user","text":"第二段问题"}`,
		`{"ts":"2026-07-03T08:00:00Z","kind":"divider"}`, // 双重置：空段
		`{"ts":"2026-07-04T08:00:00Z","kind":"divider"}`,
		`{"ts":"2026-07-04T08:00:01Z","kind":"user","text":"当前段"}`,
		``,
		`not json`,
	}, "\n")

	segs := splitConversations([]byte(log))
	if len(segs) != 4 {
		t.Fatalf("segments = %d, want 4", len(segs))
	}
	// 最近一段：divider + user，divider 保留供前端画「新对话」线
	last := segs[len(segs)-1]
	if len(last) != 2 || last[0].meta.Kind != "divider" || last[1].meta.Text != "当前段" {
		t.Fatalf("last segment unexpected: %+v", last)
	}
	// 空段（只有 divider）不进列表，其余段元数据正确
	var archives []histArchive
	for i := 0; i < len(segs)-1; i++ {
		if a, ok := summarizeSeg(i, segs[i]); ok {
			archives = append(archives, a)
		}
	}
	if len(archives) != 2 {
		t.Fatalf("archives = %d, want 2", len(archives))
	}
	if archives[0].Seg != 0 || archives[0].Turns != 1 || archives[0].Preview != "第一段问题" {
		t.Fatalf("archive[0] unexpected: %+v", archives[0])
	}
	if archives[1].Seg != 1 || archives[1].Preview != "第二段问题" {
		t.Fatalf("archive[1] unexpected: %+v", archives[1])
	}
}

func TestTruncRunes(t *testing.T) {
	if got := truncRunes("你好世界", 2); got != "你好…" {
		t.Fatalf("truncRunes = %q", got)
	}
	if got := truncRunes("short", 120); got != "short" {
		t.Fatalf("truncRunes = %q", got)
	}
}
