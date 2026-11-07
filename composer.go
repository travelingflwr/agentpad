package main

// The chat's composer: text with a cursor that moves by character, word and
// line, wrapped to the box with spaces kept, so the cursor lands where it
// looks like it should.

import (
	"unicode"
)

type composer struct {
	text []rune
	cur  int // the cursor, as an index into text
	// goalX is the x the cursor keeps while moving up and down, -1 when
	// unset.
	goalX float64
}

func (c *composer) String() string { return string(c.text) }
func (c *composer) empty() bool    { return len(c.text) == 0 }

func (c *composer) set(s string) {
	c.text = []rune(s)
	c.cur = len(c.text)
	c.goalX = -1
}

func (c *composer) insert(rs []rune) {
	c.cur = max(0, min(c.cur, len(c.text)))
	c.text = append(c.text[:c.cur], append(append([]rune(nil), rs...), c.text[c.cur:]...)...)
	c.cur += len(rs)
	c.goalX = -1
}

func (c *composer) backspace() {
	if c.cur > 0 {
		c.text = append(c.text[:c.cur-1], c.text[c.cur:]...)
		c.cur--
	}
	c.goalX = -1
}

func (c *composer) deleteForward() {
	if c.cur < len(c.text) {
		c.text = append(c.text[:c.cur], c.text[c.cur+1:]...)
	}
	c.goalX = -1
}

func (c *composer) wordStart() int {
	i := c.cur
	for i > 0 && unicode.IsSpace(c.text[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(c.text[i-1]) {
		i--
	}
	return i
}

func (c *composer) wordEnd() int {
	i := c.cur
	for i < len(c.text) && unicode.IsSpace(c.text[i]) {
		i++
	}
	for i < len(c.text) && !unicode.IsSpace(c.text[i]) {
		i++
	}
	return i
}

func (c *composer) deleteWord() {
	i := c.wordStart()
	c.text = append(c.text[:i], c.text[c.cur:]...)
	c.cur = i
	c.goalX = -1
}

func (c *composer) move(to int) {
	c.cur = max(0, min(to, len(c.text)))
	c.goalX = -1
}

// lineStart and lineEnd are the ends of the hard line the cursor is on.
func (c *composer) lineStart() int {
	i := c.cur
	for i > 0 && c.text[i-1] != '\n' {
		i--
	}
	return i
}

func (c *composer) lineEnd() int {
	i := c.cur
	for i < len(c.text) && c.text[i] != '\n' {
		i++
	}
	return i
}

// inLine is one wrapped line of the composer: text[start:end].
type inLine struct{ start, end int }

// layout wraps the text to maxW points at size, breaking after spaces, and
// splitting a word too long for a line.
func (c *composer) layout(r *renderer, size, maxW float64) []inLine {
	var lines []inLine
	width := func(a, b int) float64 { return r.width(string(c.text[a:b]), size, false) }
	start := 0
	for start <= len(c.text) {
		end := start
		for end < len(c.text) && c.text[end] != '\n' {
			end++
		}
		// Wrap text[start:end].
		ls := start
		for {
			if width(ls, end) <= maxW {
				lines = append(lines, inLine{ls, end})
				break
			}
			// The furthest break after a space that fits.
			brk := -1
			for i := ls + 1; i <= end; i++ {
				if i < end && unicode.IsSpace(c.text[i-1]) && !unicode.IsSpace(c.text[i]) {
					if width(ls, i) <= maxW {
						brk = i
					} else {
						break
					}
				}
			}
			if brk < 0 { // one long word: as many characters as fit
				brk = ls + 1
				for brk < end && width(ls, brk+1) <= maxW {
					brk++
				}
			}
			lines = append(lines, inLine{ls, brk})
			ls = brk
		}
		start = end + 1
	}
	return lines
}

// caret is the line the cursor is on and its x within it.
func (c *composer) caret(r *renderer, lines []inLine, size float64) (int, float64) {
	for i, l := range lines {
		last := i == len(lines)-1
		// At a soft break the cursor belongs to the start of the next line.
		if c.cur >= l.start && (c.cur < l.end || (c.cur == l.end && (last || lines[i+1].start != l.end))) {
			return i, r.width(string(c.text[l.start:c.cur]), size, false)
		}
	}
	return len(lines) - 1, 0
}

// vertical moves the cursor a line up (-1) or down (1), keeping its x, and
// reports whether there was a line to move to.
func (c *composer) vertical(r *renderer, lines []inLine, size float64, dir int) bool {
	line, x := c.caret(r, lines, size)
	if c.goalX >= 0 {
		x = c.goalX
	}
	to := line + dir
	if to < 0 || to >= len(lines) {
		return false
	}
	l := lines[to]
	best, bestD := l.start, 1e9
	for i := l.start; i <= l.end; i++ {
		d := r.width(string(c.text[l.start:i]), size, false) - x
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = i, d
		}
	}
	c.cur = best
	c.goalX = x
	return true
}
