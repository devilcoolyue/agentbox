package collaboration

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type reviewDiff struct {
	Text          string
	Hunks         int
	IncludedHunks int
	Omission      string
}

type diffLine struct {
	op   byte
	text string
}

// compactReviewDiff makes independent unified hunks, bounded by both CPU work
// and output bytes. Only whole hunks are included; every omitted change is
// signalled separately, so a truncated prefix can never look like a full diff.
func compactReviewDiff(ctx context.Context, name, before, after string, maxBytes int) (reviewDiff, error) {
	if !utf8.ValidString(before) || !utf8.ValidString(after) || strings.ContainsRune(before, 0) || strings.ContainsRune(after, 0) {
		return reviewDiff{Omission: "binary"}, nil
	}
	if before == after {
		return reviewDiff{}, nil
	}
	lines := func(text string) []string {
		if text == "" {
			return nil
		}
		out := strings.SplitAfter(text, "\n")
		if out[len(out)-1] == "" {
			out = out[:len(out)-1]
		}
		return out
	}
	a, b := lines(before), lines(after)
	if len(a)+len(b) > 65536 {
		return reviewDiff{Omission: "diff_complexity"}, nil
	}
	edits, ok, err := reviewEditScript(ctx, a, b)
	if err != nil {
		return reviewDiff{}, err
	}
	if !ok {
		return reviewDiff{Omission: "diff_complexity"}, nil
	}
	var result reviewDiff
	header := fmt.Sprintf("--- %s\n+++ %s\n", strconv.Quote("a/"+name), strconv.Quote("b/"+name))
	var out strings.Builder
	oldLine, newLine := 0, 0
	for i := 0; i < len(edits); {
		if err := ctx.Err(); err != nil {
			return reviewDiff{}, err
		}
		if edits[i].op == ' ' {
			oldLine++
			newLine++
			i++
			continue
		}
		start := max(0, i-3)
		end, last := i+1, i
		for end < len(edits) {
			if edits[end].op != ' ' {
				last = end
			} else if end-last > 6 {
				break
			}
			end++
		}
		end = min(len(edits), last+4)
		oldStart, newStart := oldLine-(i-start), newLine-(i-start)
		oldCount, newCount := 0, 0
		for _, line := range edits[start:end] {
			if line.op != '+' {
				oldCount++
			}
			if line.op != '-' {
				newCount++
			}
		}
		var hunk strings.Builder
		fmt.Fprintf(&hunk, "@@ -%d,%d +%d,%d @@\n", oldStart+min(1, oldCount), oldCount, newStart+min(1, newCount), newCount)
		for _, line := range edits[start:end] {
			hunk.WriteByte(line.op)
			hunk.WriteString(line.text)
			if !strings.HasSuffix(line.text, "\n") {
				hunk.WriteString("\n\\ No newline at end of file\n")
			}
		}
		result.Hunks++
		if len(header)+out.Len()+hunk.Len() <= maxBytes {
			out.WriteString(hunk.String())
			result.IncludedHunks++
		} else {
			result.Omission = "diff_budget"
		}
		for _, line := range edits[i:end] {
			if line.op != '+' {
				oldLine++
			}
			if line.op != '-' {
				newLine++
			}
		}
		i = end
	}
	if result.IncludedHunks != 0 {
		result.Text = header + out.String()
	}
	return result, nil
}

// Bounded Myers edit script. Large rewrites remain visible in the inventory
// with an explicit omission, instead of unbounded quadratic work on a server.
func reviewEditScript(ctx context.Context, a, b []string) ([]diffLine, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(a) == 0 || len(b) == 0 {
		out := make([]diffLine, 0, len(a)+len(b))
		for _, line := range a {
			out = append(out, diffLine{'-', line})
		}
		for _, line := range b {
			out = append(out, diffLine{'+', line})
		}
		return out, true, nil
	}
	const maxDistance = 256
	const maxWork = 2 << 20
	distance := min(maxDistance, len(a)+len(b))
	offset := distance + 1
	v := make([]int, 2*distance+3)
	trace := make([][]int, 0, distance+1)
	work := 0
	for d := 0; d <= distance; d++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			x := 0
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < len(a) && y < len(b) && a[x] == b[y] {
				x++
				y++
				work++
			}
			work++
			if work > maxWork {
				return nil, false, nil
			}
			v[offset+k] = x
			if x >= len(a) && y >= len(b) {
				out := make([]diffLine, 0, len(a)+len(b))
				for at := len(trace) - 1; at >= 0; at-- {
					previous := trace[at]
					k := x - y
					previousK := k - 1
					if k == -at || (k != at && previous[offset+k-1] < previous[offset+k+1]) {
						previousK = k + 1
					}
					previousX := previous[offset+previousK]
					previousY := previousX - previousK
					for x > previousX && y > previousY {
						out = append(out, diffLine{' ', a[x-1]})
						x--
						y--
					}
					if at != 0 {
						if x == previousX {
							out = append(out, diffLine{'+', b[y-1]})
							y--
						} else {
							out = append(out, diffLine{'-', a[x-1]})
							x--
						}
					}
				}
				for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
					out[l], out[r] = out[r], out[l]
				}
				return out, true, nil
			}
		}
	}
	return nil, false, nil
}
