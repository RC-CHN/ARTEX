package tool

// Frozen compatibility oracle for the pre-optimization Go search backend.
import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func referenceScanFile(path, base string, re *regexp.Regexp, mode string, showLines, multiline bool, before, after int) *grepHit {
	data, err := os.ReadFile(path)
	if err != nil || isBinary(data) {
		return nil
	}
	rel := path
	if r, e := filepath.Rel(base, path); e == nil {
		rel = r
	}
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // drop the empty element from a trailing newline
	}

	var matches []int
	if multiline {
		// Match against the whole file; a line "matches" if it overlaps any match span.
		lineStart := make([]int, len(lines))
		off := 0
		for i, ln := range lines {
			lineStart[i] = off
			off += len(ln) + 1 // + newline
		}
		hit := make([]bool, len(lines))
		for _, loc := range re.FindAllIndex(data, -1) {
			s, e := loc[0], loc[1]
			for i := range lines {
				ls, le := lineStart[i], lineStart[i]+len(lines[i])
				if s <= le && e > ls {
					hit[i] = true
				}
			}
		}
		for i, h := range hit {
			if h {
				matches = append(matches, i)
			}
		}
	} else {
		for i, ln := range lines {
			if re.MatchString(ln) {
				matches = append(matches, i)
			}
		}
	}
	if len(matches) == 0 {
		return nil
	}
	res := &grepHit{path: rel, count: len(matches)}
	if mode != "content" {
		return res
	}

	isMatch := make(map[int]bool, len(matches))
	for _, m := range matches {
		isMatch[m] = true
	}
	// Collect the set of line indices to print (matches plus context), in order.
	var order []int
	seen := map[int]bool{}
	for _, m := range matches {
		lo, hi := m-before, m+after
		if lo < 0 {
			lo = 0
		}
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		for j := lo; j <= hi; j++ {
			if !seen[j] {
				seen[j] = true
				order = append(order, j)
			}
		}
	}
	sort.Ints(order)

	hasContext := before > 0 || after > 0
	prev := -2
	for _, j := range order {
		if hasContext && prev >= 0 && j != prev+1 {
			res.lines = append(res.lines, "--") // separate non-contiguous groups
		}
		sep := "-" // context line
		if isMatch[j] {
			sep = ":" // matching line
		}
		if showLines {
			res.lines = append(res.lines, fmt.Sprintf("%s%s%d%s%s", rel, sep, j+1, sep, lines[j]))
		} else {
			res.lines = append(res.lines, fmt.Sprintf("%s%s%s", rel, sep, lines[j]))
		}
		prev = j
	}
	return res
}
