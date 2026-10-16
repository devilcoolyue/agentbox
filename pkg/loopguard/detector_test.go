package loopguard

import (
	"fmt"
	"strings"
	"testing"
)

func TestDetectorChunkIndependent(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("Go.\n\nEmit.\n\nWriting.\n\nLet me write.\n\n", 100),
		strings.Repeat("执行。继续！开始？好的。", 100),
		strings.Repeat("Now. Writing. OK. Emit. Go. Writing. Emit. OK. ", 100),
	} {
		for _, size := range []int{1, 2, 7, 100000} {
			var d Detector
			runes := []rune(text)
			var hit *Detection
			for i := 0; i < len(runes) && hit == nil; i += size {
				hit = d.Push(string(runes[i:min(i+size, len(runes))]))
			}
			if hit == nil || hit.Window != 256 {
				t.Fatalf("size %d: did not detect loop", size)
			}
		}
	}
}

func TestDetectorControls(t *testing.T) {
	var unique strings.Builder
	for i := 0; i < 600; i++ {
		// Distinct words, without relying on the numeric/code exclusion.
		fmt.Fprintf(&unique, "Investigate the %c%c%c dependency carefully.\n", 'a'+i/676, 'a'+i/26%26, 'a'+i%26)
	}
	for name, text := range map[string]string{
		"long reasoning":      unique.String(),
		"code":                strings.Repeat("if ready {\nrun()\n}\n", 400),
		"fenced repetition":   "```text\n" + strings.Repeat("Go.\n", 400) + "```\n",
		"logs":                strings.Repeat("Waiting for job 123.\n", 400),
		"large units":         strings.Repeat(strings.Repeat("word ", 100)+"\n", 400),
		"few repetitions":     strings.Repeat("Go. Writing. Emit. ", 20),
		"code interrupts run": strings.Repeat("Go.\n", 200) + "result = run()\n" + strings.Repeat("Go.\n", 200),
		"empty punctuation":   strings.Repeat("}.\n---\n\n", 400),
	} {
		t.Run(name, func(t *testing.T) {
			var d Detector
			if hit := d.Push(text); hit != nil {
				t.Fatalf("false positive: %+v", hit)
			}
		})
	}
}

func TestDetectorWindowMovesPastNovelPrefix(t *testing.T) {
	var d Detector
	for i := 0; i < 300; i++ {
		d.Push(fmt.Sprintf("Distinct %c%c words.\n", 'a'+i/26, 'a'+i%26))
	}
	if d.Push(strings.Repeat("Let me write.\n", 256)) == nil {
		t.Fatal("prefix prevented later loop detection")
	}
}
