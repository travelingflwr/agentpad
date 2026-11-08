package main

// The approval sheet: what Claude wants to do, drawn as a card, with Allow,
// Always allow and Deny. It writes your answer next to the request and
// exits, which closes its floating pane. If you answer Claude Code's own
// prompt instead, it notices and closes.

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// armDelay keeps keys typed into another pane just as the sheet appears
// from answering it.
const armDelay = 600 * time.Millisecond

type button struct {
	decision, label, key string
	primary              bool
	x0, y0, x1, y1       float64 // where it drew, in points
}

func runApprove(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: agentpad approve REQUEST.json")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var req approvalRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return err
	}
	resPath := strings.TrimSuffix(args[0], ".json") + ".result.json"
	if req.Tool == "AskUserQuestion" {
		return runQuestion(req, resPath)
	}
	answer := func(decision, note string) {
		out, _ := json.Marshal(approvalResult{Decision: decision, Note: note})
		writeAtomic(resPath, out)
	}

	s, done, err := openScreen("")
	if err != nil {
		return err
	}
	defer done()

	sheet := newSheet(req)
	opened := time.Now()
	armed := false
	gone := "" // why the sheet is closing on its own
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	watch := newTranscriptWatch(req.Transcript, req.Tool, req.Input)
	watch.answered() // read the history

	draw := func() {
		s.begin()
		s.background()
		key := fmt.Sprint(s.ws, s.pal, armed, gone)
		s.show("sheet", key, 1, s.ws.rows, 0, func() *image.RGBA {
			img := s.canvas(s.heightPt())
			r := s.renderer()
			sheet.draw(r, img, s.widthPt(), s.heightPt(), armed, gone)
			return img
		})
		s.end()
	}
	draw()
	fitLayer(s, sheet.height(s.renderer(), s.widthPt()))
	closeIn := time.Time{}

	for {
		select {
		case <-s.quit:
			return nil
		case <-s.winch:
			s.resize()
			draw()
		case <-s.snapSig:
			s.saveSnapshot("sheet")
		case <-tick.C:
			s.recheck()
			now := time.Now()
			if !closeIn.IsZero() {
				if now.After(closeIn) {
					return nil
				}
				continue
			}
			if !armed && now.Sub(opened) >= armDelay {
				armed = true
				draw()
			}
			switch {
			case watch.answered():
				gone = "Answered in the terminal"
			case fileExists(resPath):
				gone = "Answered in Activity"
			case req.HookPID > 0 && syscall.Kill(req.HookPID, 0) != nil:
				gone = "Claude moved on"
			case now.After(req.Deadline):
				gone = "Timed out: answer in the terminal"
			}
			if gone != "" {
				draw()
				closeIn = now.Add(900 * time.Millisecond)
			}
		case ev, open := <-s.events:
			if !open {
				return nil
			}
			if s.event(ev) {
				draw()
				continue
			}
			if gone != "" {
				continue
			}
			if ev.key.is(keyEscape, 0) {
				answer("terminal", "")
				return nil
			}
			if !armed {
				continue
			}
			decision := ""
			if ev.click != nil {
				x, y := s.clickPt(ev.click)
				for _, b := range sheet.buttons {
					if x >= b.x0 && x <= b.x1 && y >= b.y0 && y <= b.y1 {
						decision = b.decision
					}
				}
			} else if ev.key.mods&^modShift == 0 {
				for _, b := range sheet.buttons {
					if ev.key.code == rune(b.key[0]) || ev.key.code == rune(strings.ToUpper(b.key)[0]) ||
						b.primary && ev.key.code == keyEnter {
						decision = b.decision
					}
				}
			}
			if decision != "" {
				answer(decision, "")
				return nil
			}
		}
	}
}

// --- the card ---------------------------------------------------------------

type lineKind int

const (
	lnPlain lineKind = iota
	lnAdd
	lnDel
	lnNote // "… 12 more lines"
)

type codeLine struct {
	kind lineKind
	text string
}

type sheet struct {
	req     approvalRequest
	title   string
	detail  string // what Claude said it's for, or the file
	code    []codeLine
	prose   string // for a fetch: what it will ask of the page
	always  string // what Always allow adds, or "" for no such option
	buttons []button
}

func newSheet(req approvalRequest) *sheet {
	sh := &sheet{req: req}
	var in map[string]any
	json.Unmarshal(req.Input, &in)
	str := func(k string) string { v, _ := in[k].(string); return v }
	rel := func(p string) string {
		if r, err := filepath.Rel(req.Cwd, p); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return p
	}
	plain := func(s string) []codeLine {
		var out []codeLine
		for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			out = append(out, codeLine{lnPlain, l})
		}
		return out
	}
	switch req.Tool {
	case "Bash":
		sh.title = "Run a command"
		sh.detail = str("description")
		sh.code = plain(str("command"))
	case "Edit":
		sh.title = "Edit " + filepath.Base(str("file_path"))
		sh.detail = rel(str("file_path"))
		sh.code = diffLines(str("old_string"), str("new_string"))
		if b, _ := in["replace_all"].(bool); b {
			sh.detail += " · every match"
		}
	case "MultiEdit":
		sh.title = "Edit " + filepath.Base(str("file_path"))
		sh.detail = rel(str("file_path"))
		edits, _ := in["edits"].([]any)
		for i, e := range edits {
			m, _ := e.(map[string]any)
			o, _ := m["old_string"].(string)
			n, _ := m["new_string"].(string)
			if i > 0 {
				sh.code = append(sh.code, codeLine{lnNote, "⋯"})
			}
			sh.code = append(sh.code, diffLines(o, n)...)
		}
	case "Write":
		sh.title = "Write " + filepath.Base(str("file_path"))
		sh.detail = rel(str("file_path"))
		if _, err := os.Stat(str("file_path")); err == nil {
			sh.detail += " · replaces the file"
		} else {
			sh.detail += " · a new file"
		}
		for _, l := range strings.Split(strings.TrimRight(str("content"), "\n"), "\n") {
			sh.code = append(sh.code, codeLine{lnAdd, l})
		}
	case "NotebookEdit":
		sh.title = "Edit " + filepath.Base(str("notebook_path"))
		sh.detail = rel(str("notebook_path"))
		sh.code = plain(str("new_source"))
	case "Read":
		sh.title = "Read " + filepath.Base(str("file_path"))
		sh.detail = rel(str("file_path"))
	case "Grep", "Glob":
		sh.title = "Search files"
		sh.code = plain(str("pattern"))
		if p := str("path"); p != "" {
			sh.detail = "in " + rel(p)
		}
	case "WebFetch":
		sh.title = "Read a web page"
		sh.code = plain(str("url"))
		sh.prose = str("prompt")
	case "WebSearch":
		sh.title = "Search the web"
		sh.code = plain(str("query"))
	default:
		sh.title = "Use " + toolTitle(req.Tool)
		pretty, _ := json.MarshalIndent(in, "", "  ")
		sh.code = plain(string(pretty))
	}
	if req.Subagent {
		sh.title += " (subagent)"
	}
	sh.always = alwaysSummary(req.Suggestions, req.Cwd)
	sh.buttons = []button{{decision: "allow", label: "Allow", key: "y", primary: true}}
	if sh.always != "" {
		sh.buttons = append(sh.buttons, button{decision: "always", label: "Always allow", key: "a"})
	}
	sh.buttons = append(sh.buttons, button{decision: "deny", label: "Deny", key: "n"})
	return sh
}

func toolTitle(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		parts := strings.Split(name, "__")
		if len(parts) >= 3 {
			return strings.ReplaceAll(parts[len(parts)-1], "_", " ") + " (" + strings.ReplaceAll(parts[1], "_", " ") + ")"
		}
	}
	return name
}

// alwaysSummary says what Claude Code suggests remembering, such as
// "Bash(npm test:*) for this project", or "" if it suggests nothing.
func alwaysSummary(raw json.RawMessage, cwd string) string {
	var sugg []struct {
		Type     string `json:"type"`
		Behavior string `json:"behavior"`
		Rules    []struct {
			ToolName    string `json:"toolName"`
			RuleContent string `json:"ruleContent"`
		} `json:"rules"`
		Mode        string   `json:"mode"`
		Directories []string `json:"directories"`
		Destination string   `json:"destination"`
	}
	if json.Unmarshal(raw, &sugg) != nil || len(sugg) == 0 {
		return ""
	}
	where := map[string]string{
		"session": "for this session", "localSettings": "in this project",
		"projectSettings": "in this project, for everyone", "userSettings": "everywhere",
	}
	var parts []string
	dest := ""
	for _, s := range sugg {
		dest = where[s.Destination]
		switch s.Type {
		case "addRules":
			for _, r := range s.Rules {
				parts = append(parts, ruleText(r.ToolName, r.RuleContent))
			}
		case "addDirectories":
			for _, d := range s.Directories {
				if r, err := filepath.Rel(cwd, d); err == nil && !strings.HasPrefix(r, "..") {
					d = r
				}
				parts = append(parts, "files in "+d)
			}
		case "setMode":
			parts = append(parts, modeText(s.Mode))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	out := strings.Join(parts, ", ")
	if strings.HasPrefix(out, "switching") {
		dest = strings.Replace(dest, "for this session", "", 1)
		if strings.TrimSpace(dest) == "" {
			return out + " for this session"
		}
	}
	if dest != "" {
		out += " " + dest
	}
	return out
}

// ruleText says what a permission rule allows, in words.
func ruleText(tool, content string) string {
	home, _ := os.UserHomeDir()
	path := func(p string) string {
		p = strings.TrimSuffix(strings.TrimSuffix(p, "/**"), "/*")
		if strings.HasPrefix(p, "//") {
			p = p[1:]
		}
		if home != "" && strings.HasPrefix(p, home) {
			p = "~" + p[len(home):]
		}
		return p
	}
	verbs := map[string]string{"Read": "reading", "Edit": "editing", "Write": "writing", "Glob": "searching", "Grep": "searching"}
	if content == "" {
		return "every " + tool + " call"
	}
	if v, ok := verbs[tool]; ok {
		return v + " files in " + path(content)
	}
	switch tool {
	case "Bash":
		if strings.HasSuffix(content, ":*") {
			return "commands starting with " + strings.TrimSuffix(content, ":*")
		}
		return "the command " + content
	case "WebFetch":
		return "fetching from " + strings.TrimPrefix(content, "domain:")
	}
	return tool + "(" + content + ")"
}

func modeText(mode string) string {
	switch mode {
	case "acceptEdits":
		return "switching to accept edits"
	case "auto":
		return "switching to auto mode"
	case "plan":
		return "switching to plan mode"
	case "bypassPermissions":
		return "switching off permission prompts"
	}
	return "switching to " + mode + " mode"
}

// Sheet layout, in points.
const (
	sheetPad   = 18.0
	codeSize   = 11.5
	codeLineH  = 16.0
	codePad    = 10.0
	buttonH    = 30.0
	buttonGap  = 8.0
	footerHint = 22.0
)

// height is how tall the sheet wants to be at width w, in points, showing
// up to maxCodeLines of code.
const maxCodeLines = 24

func (sh *sheet) height(r *renderer, w float64) float64 {
	x0, x1 := sheetPad, w-sheetPad
	h := sheetPad + 4 + 26 + 8
	if sh.detail != "" {
		h += 18 * float64(min(2, len(r.wrap(sh.detail, 12.5, false, x1-x0))))
	}
	h += 14
	if len(sh.code) > 0 {
		m := r.mono()
		cols := max(10, int((x1-x0-2*codePad-14)/m.width("M", codeSize, false)))
		n := min(maxCodeLines, len(wrapCode(sh.code, cols)))
		h += float64(n)*codeLineH + 2*codePad
	}
	if sh.prose != "" {
		h += 18 * float64(len(r.wrap(sh.prose, 12.5, false, x1-x0)))
	}
	h += 8 + 10 // gap above the footer
	if sh.always != "" {
		h += 20
	}
	return h + buttonH + footerHint + sheetPad
}

func (sh *sheet) draw(r *renderer, img *image.RGBA, w, h float64, armed bool, gone string) {
	x0, x1 := sheetPad, w-sheetPad
	y := sheetPad + 4

	// Eyebrow: the project, and where the agent is if it isn't here.
	eyebrow := sh.req.Project()
	if sh.req.Elsewhere != "" {
		eyebrow += " · " + sh.req.Elsewhere
	}
	r.fillCircle(img, x0+4, y-4, 4, r.amber())
	r.text(img, "Claude · "+eyebrow, x0+14, y, 11.5, true, r.pal.gray, x1-x0-14)
	y += 26
	r.text(img, sh.title, x0, y, 18, true, r.pal.text, x1-x0)
	y += 8
	if sh.detail != "" {
		for i, l := range r.wrap(sh.detail, 12.5, false, x1-x0) {
			if i == 2 {
				break
			}
			y += 18
			r.text(img, l, x0, y, 12.5, false, r.pal.gray, x1-x0)
		}
	}
	y += 14

	// The footer, laid out from the bottom up.
	bottom := h - sheetPad
	hintY := bottom
	btnY1 := hintY - footerHint
	btnY0 := btnY1 - buttonH
	alwaysY := btnY0 - 10
	if sh.always != "" {
		alwaysY -= 4
	}
	codeBottom := alwaysY - 8
	if sh.always != "" {
		codeBottom -= 16
	}

	// The code box: as many lines as fit.
	if len(sh.code) > 0 && codeBottom-y > codeLineH+2*codePad {
		m := r.mono()
		charW := m.width("M", codeSize, false)
		cols := max(10, int((x1-x0-2*codePad-14)/charW))
		lines := wrapCode(sh.code, cols)
		proseLines := []string(nil)
		if sh.prose != "" {
			proseLines = r.wrap(sh.prose, 12.5, false, x1-x0)
		}
		avail := codeBottom - y - float64(len(proseLines))*18
		fit := int((avail - 2*codePad) / codeLineH)
		if len(proseLines) > 0 {
			fit = min(fit, len(lines))
		}
		if fit < len(lines) {
			more := len(lines) - (fit - 1)
			lines = append(lines[:fit-1], codeLine{lnNote, fmt.Sprintf("… %d more line%s", more, plural(more))})
		}
		boxH := float64(len(lines))*codeLineH + 2*codePad
		r.fillRoundRect(img, x0, y, x1, y+boxH, 8, r.tinted(r.pal.text, 0.06))
		ly := y + codePad
		for _, l := range lines {
			col := r.pal.text
			mark := ""
			switch l.kind {
			case lnAdd:
				r.fillRect(img, x0, ly, x1, ly+codeLineH, r.tinted(r.green(), 0.14))
				col, mark = r.green(), "+"
			case lnDel:
				r.fillRect(img, x0, ly, x1, ly+codeLineH, r.tinted(r.red(), 0.14))
				col, mark = r.red(), "−"
			case lnNote:
				col = r.pal.faint
			}
			base := ly + codeLineH - 4.5
			if mark != "" {
				m.drawRaw(img, mark, x0+codePad-2, base, codeSize, true, col)
			}
			if l.kind == lnAdd || l.kind == lnDel {
				col = mix(col, r.pal.text, 0.35)
				if r.clear {
					col = r.pal.text
				}
			}
			m.drawRaw(img, codeText(m, l.text), x0+codePad+12, base, codeSize, false, col)
			ly += codeLineH
		}
		y += boxH
		for _, l := range proseLines {
			y += 18
			r.text(img, l, x0, y, 12.5, false, r.pal.gray, x1-x0)
		}
	}

	if gone != "" {
		r.fillRoundRect(img, x0, btnY0, x1, btnY1, buttonH/2, r.tinted(r.pal.text, 0.07))
		tw := r.width(r.clean(gone, true), 13, true)
		r.text(img, gone, (x0+x1)/2-tw/2, btnY0+buttonH/2+4.5, 13, true, r.pal.gray, 0)
		return
	}

	if sh.always != "" {
		r.text(img, "Always: "+sh.always, x0, alwaysY, 11.5, false, r.pal.faint, x1-x0)
	}

	// Buttons share the row; the primary one is filled.
	fade := 1.0
	if !armed {
		fade = 0.45
	}
	labels := make([]string, len(sh.buttons))
	widths := make([]float64, len(sh.buttons))
	total := 0.0
	for i, b := range sh.buttons {
		labels[i] = b.label
		widths[i] = r.width(b.label, 13, true) + 50
		total += widths[i]
	}
	total += buttonGap * float64(len(sh.buttons)-1)
	if total > x1-x0 { // narrow: share the row evenly
		each := (x1 - x0 - buttonGap*float64(len(sh.buttons)-1)) / float64(len(sh.buttons))
		for i := range widths {
			widths[i] = each
			if r.width(labels[i], 13, true)+40 > each && sh.buttons[i].decision == "always" {
				labels[i] = "Always"
			}
		}
	}
	bx := x0
	white := rgb(255, 255, 255)
	if luminance(r.pal.accent) > 0.3 {
		white = rgb(20, 20, 20)
	}
	for i := range sh.buttons {
		b := &sh.buttons[i]
		bw := widths[i]
		b.x0, b.y0, b.x1, b.y1 = bx, btnY0, bx+bw, btnY1
		bg, fg := r.tinted(r.pal.text, 0.09*fade), premulOr(r, r.pal.text, fade)
		if b.primary {
			bg, fg = r.tinted(r.pal.accent, fade), white
		}
		if b.decision == "deny" {
			fg = premulOr(r, r.red(), fade)
		}
		r.fillRoundRect(img, bx, btnY0, bx+bw, btnY1, buttonH/2, bg)
		lw := r.width(r.clean(labels[i], true), 13, true)
		capW := 16.0
		cx := bx + bw/2 - (lw+6+capW)/2
		r.drawRaw(img, r.clean(labels[i], true), cx, btnY0+buttonH/2+4.5, 13, true, fg)
		kx := cx + lw + 6
		capBg := r.tinted(r.pal.text, 0.1*fade)
		if b.primary {
			capBg = premul(white, 0.22*fade)
		}
		r.fillRoundRect(img, kx, btnY0+7, kx+capW, btnY1-7, 4, capBg)
		m := r.mono()
		kw := m.width(b.key, 10.5, true)
		m.drawRaw(img, b.key, kx+capW/2-kw/2, btnY0+buttonH/2+3.8, 10.5, true, fg)
		bx += bw + buttonGap
	}
	hint := "Esc answers in the terminal instead"
	if !armed {
		hint = "Getting ready…"
	}
	r.text(img, hint, x0, hintY-2, 11, false, r.pal.faint, x1-x0)
}

// premulOr is c at opacity a, the way the image wants it.
func premulOr(r *renderer, c color.RGBA, a float64) color.RGBA {
	if a >= 1 {
		return c
	}
	return r.tinted(c, a)
}

func (req approvalRequest) Project() string {
	return Agent{Cwd: req.Cwd}.Project()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// wrapCode breaks lines longer than cols characters, keeping indentation.
func wrapCode(lines []codeLine, cols int) []codeLine {
	var out []codeLine
	for _, l := range lines {
		t := strings.ReplaceAll(l.text, "\t", "    ")
		if utf8.RuneCountInString(t) <= cols {
			out = append(out, codeLine{l.kind, t})
			continue
		}
		rs := []rune(t)
		for len(rs) > 0 {
			n := min(cols, len(rs))
			out = append(out, codeLine{l.kind, string(rs[:n])})
			rs = rs[n:]
		}
	}
	return out
}

// codeText drops characters the mono font can't draw, keeping spaces.
func codeText(m *renderer, s string) string {
	var b strings.Builder
	for _, c := range s {
		if c == ' ' || m.fonts.has(false, c) {
			b.WriteRune(c)
		} else if c >= 0x20 {
			b.WriteRune('·')
		}
	}
	return b.String()
}

// diffLines is old to new, line by line: what's the same, what's gone, and
// what's added.
func diffLines(old, new string) []codeLine {
	a := strings.Split(strings.TrimRight(old, "\n"), "\n")
	b := strings.Split(strings.TrimRight(new, "\n"), "\n")
	if old == "" {
		a = nil
	}
	if len(a)*len(b) > 250000 { // too big to compare: all out, all in
		var out []codeLine
		for _, l := range a {
			out = append(out, codeLine{lnDel, l})
		}
		for _, l := range b {
			out = append(out, codeLine{lnAdd, l})
		}
		return out
	}
	// Longest common subsequence, then walk it.
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []codeLine
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			out = append(out, codeLine{lnPlain, a[i]})
			i, j = i+1, j+1
		case i < n && (j >= m || lcs[i+1][j] >= lcs[i][j+1]):
			// What's gone comes before what replaces it.
			out = append(out, codeLine{lnDel, a[i]})
			i++
		default:
			out = append(out, codeLine{lnAdd, b[j]})
			j++
		}
	}
	return trimContext(out, 3)
}

// trimContext keeps unchanged lines only near changes.
func trimContext(lines []codeLine, keep int) []codeLine {
	near := make([]bool, len(lines))
	for i, l := range lines {
		if l.kind != lnPlain {
			for k := max(0, i-keep); k <= min(len(lines)-1, i+keep); k++ {
				near[k] = true
			}
		}
	}
	var out []codeLine
	skipped := false
	for i, l := range lines {
		if near[i] {
			if skipped && len(out) > 0 {
				out = append(out, codeLine{lnNote, "⋯"})
			}
			skipped = false
			out = append(out, l)
		} else {
			skipped = true
		}
	}
	return out
}

// fitLayer resizes the sheet's floating layer to the sheet, keeping its
// bottom edge where it was.
func fitLayer(s *screen, wantPt float64) {
	session, me := os.Getenv("REX_SESSION"), os.Getenv("REX_BLOCK")
	if session == "" || me == "" {
		return
	}
	v, err := viewSession("", session)
	if err != nil {
		return
	}
	for _, w := range v.Windows {
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				if b.BlockID != me || l.Kind != "floating" {
					continue
				}
				// The window's height in cells, from the pane's share of it.
				b0 := approvalBounds
				winRows := float64(s.ws.rows) / b0[3]
				want := (math.Ceil(wantPt/s.cellPt()) + 2) / winRows // + the pane's border
				h := math.Max(0.22, math.Min(want, 0.9))
				// On a window as narrow as a phone, the sheet takes the
				// whole width (and a narrower share of the height).
				x, w := b0[0], b0[2]
				if s.widthPt() < 380 {
					x, w = 0.02, 0.96
					h = math.Min(h*b0[2]/w*1.15, 0.9)
				}
				if math.Abs(h-b0[3]) < 0.02 && w == b0[2] {
					return
				}
				bottom := b0[1] + b0[3]
				rexAPI("", "session.resize_layer", map[string]any{"session_id": session, "layer_id": l.LayerID,
					"bounds": rect{x, bottom - h, w, h}}, nil)
				return
			}
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
