package plan

import "strings"

// Line is one line of a diff: ' ' for a line both sides have, '-' for a removed line, '+' for
// an added one.
type Line struct {
	Op   byte
	Text string
}

// Diff returns the line diff of the change, every line of both sides included, so that a viewer
// can fold the unchanged ones.
func (c Change) Diff() []Line {
	return lineDiff(string(c.Before), string(c.After))
}

// lineDiff compares two texts line by line. Edits are local, so after trimming the common
// beginning and end, the longest common subsequence of what is left is small.
func lineDiff(a, b string) []Line {
	x, y := splitLines(a), splitLines(b)
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	var out []Line
	for _, l := range x[:pre] {
		out = append(out, Line{' ', l})
	}
	out = append(out, lcs(x[pre:len(x)-suf], y[pre:len(y)-suf])...)
	for _, l := range x[len(x)-suf:] {
		out = append(out, Line{' ', l})
	}
	return out
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func lcs(x, y []string) []Line {
	n, m := len(x), len(y)
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else {
				table[i][j] = max(table[i+1][j], table[i][j+1])
			}
		}
	}
	var out []Line
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			out = append(out, Line{' ', x[i]})
			i, j = i+1, j+1
		case table[i+1][j] >= table[i][j+1]:
			out = append(out, Line{'-', x[i]})
			i++
		default:
			out = append(out, Line{'+', y[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, Line{'-', x[i]})
	}
	for ; j < m; j++ {
		out = append(out, Line{'+', y[j]})
	}
	return out
}
