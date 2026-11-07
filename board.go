package main

// The board: every Claude Code session as a card, with what it's doing
// right now. Waiting on you comes first. Enter jumps to the agent's pane.

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Board layout, in points.
const (
	boardHeaderPt = 44.0
	cardPt        = 76.0 // before rounding to whole cells
)

type boardLayout struct {
	header, pitch float64 // snapped to cells
	visible       int     // whole cards that fit
}

func layoutBoard(s *screen) boardLayout {
	l := boardLayout{header: s.snap(boardHeaderPt), pitch: s.snap(cardPt)}
	l.visible = max(1, int((s.heightPt()-l.header)/l.pitch))
	return l
}

func runBoard(args []string) error {
	s, done, err := openScreen("")
	if err != nil {
		return err
	}
	defer done()

	agents := loadAgents()
	sel, top := 0, 0
	dirStamp := stampDir()
	poll := time.NewTicker(400 * time.Millisecond)
	defer poll.Stop()
	slow := time.NewTicker(15 * time.Second)
	defer slow.Stop()
	status := ""
	statusUntil := time.Time{}

	draw := func() {
		lay := layoutBoard(s)
		sel = max(0, min(sel, len(agents)-1))
		if sel < top {
			top = sel
		}
		if sel >= top+lay.visible {
			top = sel - lay.visible + 1
		}
		top = max(0, min(top, len(agents)-lay.visible))
		minute := time.Now().Truncate(30 * time.Second)
		msg := ""
		if time.Now().Before(statusUntil) {
			msg = status
		}
		s.begin()
		s.background()
		key := fmt.Sprint(s.ws, s.pal, agentsKey(agents), top, minute, msg)
		s.show("board", key, 1, s.ws.rows, 0, func() *image.RGBA {
			img := s.canvas(s.heightPt())
			drawBoard(s.renderer(), img, agents[top:], s.widthPt(), lay, msg)
			return img
		})
		if len(agents) > 0 {
			rowPt := lay.header + float64(sel-top)*lay.pitch
			rows := int(math.Round(lay.pitch / s.cellPt()))
			s.show("sel", fmt.Sprint(s.ws, s.pal), s.cellRow(rowPt), rows, -1, func() *image.RGBA {
				img := s.canvas(lay.pitch)
				r := s.renderer()
				r.fillRoundRect(img, selInset, 3, s.widthPt()-selInset, lay.pitch-3, selRadius, r.pal.selBg)
				return img
			})
		} else {
			s.hide("sel")
		}
		s.end()
	}
	reload := func() {
		var cur string
		if sel < len(agents) {
			cur = agents[sel].SessionID
		}
		agents = loadAgents()
		for i, a := range agents {
			if a.SessionID == cur {
				sel = i
			}
		}
	}
	say := func(m string) {
		status, statusUntil = m, time.Now().Add(3*time.Second)
	}
	jump := func(i int) {
		if i < 0 || i >= len(agents) {
			return
		}
		a := agents[i]
		if a.RexSession == "" || a.RexBlock == "" {
			say("That session isn't in Rex")
			return
		}
		if err := focusBlock("", a.RexSession, a.RexBlock); err != nil {
			say("Couldn't find its pane")
		}
	}
	pruneAgents(agents, time.Now())
	markGone(agents)
	reload()
	draw()

	for {
		select {
		case <-s.quit:
			return nil
		case <-s.winch:
			s.resize()
			draw()
		case <-s.snapSig:
			s.saveSnapshot("board", "sel")
		case <-poll.C:
			s.recheck()
			if st := stampDir(); st != dirStamp {
				dirStamp = st
				reload()
				draw()
			} else if status != "" && time.Now().After(statusUntil) {
				status = ""
				draw()
			}
		case <-slow.C:
			pruneAgents(agents, time.Now())
			markGone(agents)
			reload()
			draw()
		case ev, open := <-s.events:
			if !open {
				return nil
			}
			if s.event(ev) {
				draw()
				continue
			}
			lay := layoutBoard(s)
			if ev.click != nil {
				_, y := s.clickPt(ev.click)
				if i := top + int((y-lay.header)/lay.pitch); y >= lay.header && i < len(agents) {
					sel = i
					draw()
					jump(i)
				}
				continue
			}
			if ev.scroll != 0 {
				switch ev.scroll {
				case wheelDown, lineDown, pageDown, scrollBottom:
					sel++
				default:
					sel--
				}
				draw()
				continue
			}
			k := ev.key
			switch {
			case k.is('q', 0), k.is(keyEscape, 0), k.is('c', modCtrl):
				return nil
			case k.is('j', 0), k.is(keyDown, 0):
				sel++
			case k.is('k', 0), k.is(keyUp, 0):
				sel--
			case k.is('g', 0), k.is(keyHome, 0):
				sel = 0
			case k.is('G', 0), k.is('G', modShift), k.is(keyEnd, 0):
				sel = len(agents) - 1
			case k.is(keyEnter, 0), k.is('o', 0):
				jump(sel)
			case k.is('x', 0):
				if sel < len(agents) {
					forgetAgent(agents[sel].SessionID)
					reload()
				}
			default:
				continue
			}
			draw()
		}
	}
}

// stampDir changes whenever a session's record does.
func stampDir() string {
	paths, _ := filepath.Glob(filepath.Join(agentsDir(), "*.json"))
	plans, _ := filepath.Glob(filepath.Join(rexpadDir(), "block-*.json"))
	paths = append(paths, plans...)
	var b strings.Builder
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s%d;", filepath.Base(p), fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

func agentsKey(agents []Agent) string {
	var b strings.Builder
	for _, a := range agents {
		fmt.Fprintf(&b, "%s%d;", a.SessionID, a.Updated.UnixNano())
	}
	return b.String()
}

// markGone ends sessions whose Rex pane has closed without saying so (a
// crash, or the pane closed under it).
func markGone(agents []Agent) {
	blocks := map[string]map[string]bool{}
	for _, a := range agents {
		if a.State == stEnded || a.RexSession == "" || a.RexBlock == "" || a.RexServer != os.Getenv("REX_SERVER") {
			continue
		}
		known, ok := blocks[a.RexSession]
		if !ok {
			var res struct {
				Blocks []struct {
					BlockID string `json:"block_id"`
				} `json:"blocks"`
			}
			if err := rexAPI("", "session.list_blocks", map[string]any{"session_id": a.RexSession}, &res); err != nil {
				if !strings.Contains(err.Error(), "not found") {
					continue // can't tell
				}
			}
			known = map[string]bool{}
			for _, b := range res.Blocks {
				known[b.BlockID] = true
			}
			blocks[a.RexSession] = known
		}
		if !known[a.RexBlock] {
			updateAgent(a.SessionID, func(x *Agent) { x.State, x.Waiting, x.Activity = stEnded, 0, "" })
		}
	}
}

// --- drawing ----------------------------------------------------------------

func drawBoard(r *renderer, img *image.RGBA, agents []Agent, w float64, lay boardLayout, msg string) {
	// Header: a title and a count of what needs you.
	var waiting, working int
	for _, a := range agents {
		switch a.State {
		case stWaiting:
			waiting++
		case stWorking:
			working++
		}
	}
	hy := lay.header/2 + 6
	r.text(img, "Agents", padX, hy, 16, true, r.pal.text, 0)
	var parts []string
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d need%s you", waiting, map[bool]string{true: "s", false: ""}[waiting == 1]))
	}
	if working > 0 {
		parts = append(parts, fmt.Sprintf("%d working", working))
	}
	summary := strings.Join(parts, " · ")
	if msg != "" {
		summary = msg
	}
	if summary != "" {
		col := r.pal.gray
		if waiting > 0 && msg == "" {
			col = r.amber()
		}
		r.textRight(img, summary, w-padX, hy, 12, waiting > 0 && msg == "", col)
	}
	r.fillRect(img, padX, lay.header-0.5, w-padX, lay.header, r.pal.divider)

	if len(agents) == 0 {
		y := lay.header + 40
		r.text(img, "No Claude Code sessions yet", padX, y, 14, true, r.pal.gray, w-2*padX)
		for _, l := range r.wrap("Start claude in any Rex pane and it shows up here: what it's doing, and when it needs you.", 12.5, false, w-2*padX) {
			y += 19
			r.text(img, l, padX, y, 12.5, false, r.pal.faint, 0)
		}
		return
	}

	for i, a := range agents {
		y0 := lay.header + float64(i)*lay.pitch
		if y0 >= float64(img.Bounds().Dy())/r.scale {
			break
		}
		drawCard(r, img, a, y0, w, lay.pitch)
		if i < len(agents)-1 {
			r.fillRect(img, padX+22, y0+lay.pitch-0.5, w-padX, y0+lay.pitch, r.pal.divider)
		}
	}
}

func drawCard(r *renderer, img *image.RGBA, a Agent, y0, w, pitch float64) {
	x := padX + 4
	textX := x + 22
	maxW := w - textX - padX - 4
	mid := y0 + pitch/2
	l1, l2, l3 := mid-12, mid+5, mid+21

	// The status mark.
	cx, cy := x+6, l1-4.5
	var line2 string
	line2Col := r.pal.text
	switch a.State {
	case stWaiting:
		r.fillCircle(img, cx, cy, 6, r.amber())
		r.fillRect(img, cx-0.9, cy-3.4, cx+0.9, cy+0.8, rgb(255, 255, 255))
		r.fillCircle(img, cx, cy+2.4, 0.95, rgb(255, 255, 255))
		line2, line2Col = "Needs you: "+orDefault(a.Activity, "permission"), r.amber()
	case stWorking:
		r.fillCircle(img, cx, cy, 6, r.tinted(r.pal.accent, 0.25))
		r.fillCircle(img, cx, cy, 3.2, r.pal.accent)
		line2 = orDefault(a.Activity, "Working")
	case stIdle:
		r.fillCircle(img, cx, cy, 6, r.tinted(r.green(), 0.9))
		r.check(img, cx, cy, 8, rgb(255, 255, 255))
		line2 = "Done"
		if a.Activity != "" {
			line2 = a.Activity
		}
		if a.Said != "" {
			line2 += ": " + a.Said
		}
		line2Col = r.pal.gray
	default:
		r.fillCircle(img, cx, cy, 6, r.tinted(r.pal.text, 0.15))
		line2, line2Col = "Ended", r.pal.faint
	}

	// Line 1: the project, and how long ago.
	ago := agoText(r.now, a.Updated)
	if a.State == stWorking {
		ago = agoText(r.now, a.Started)
		if a.Tools > 0 {
			ago = fmt.Sprintf("%d tool%s · %s", a.Tools, plural(a.Tools), ago)
		}
	}
	aw := r.textRight(img, ago, w-padX-4, l1, 11.5, false, r.pal.faint)
	plan, hasPlan := planOf(a)
	if hasPlan && a.State != stEnded {
		label := fmt.Sprintf("%d/%d", plan.done, plan.total)
		lw := r.width(label, 11.5, true)
		rx := w - padX - 4 - aw - 10
		r.drawRaw(img, label, rx-lw, l1, 11.5, true, r.pal.gray)
		ringX := rx - lw - 11
		frac := (float64(plan.done) + 0.5*float64(plan.active)) / float64(plan.total)
		r.ring(img, ringX, l1-4.5, 6.5, frac, r.pal.accent)
		aw += 10 + lw + 11 + 8
	}
	name := a.Project()
	if a.Subagents > 0 && a.State == stWorking {
		name += fmt.Sprintf(" · %d subagent%s", a.Subagents, plural(a.Subagents))
	}
	r.text(img, name, textX, l1, 14, true, r.pal.text, maxW-aw-10)
	r.text(img, line2, textX, l2, 12.5, a.State == stWaiting, line2Col, maxW)
	switch {
	case hasPlan && plan.current != "" && a.State != stEnded:
		r.text(img, plan.current, textX, l3, 11.5, false, r.pal.gray, maxW)
	case a.Prompt != "":
		r.text(img, "“"+a.Prompt+"”", textX, l3, 11.5, false, r.pal.faint, maxW)
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func agoText(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < 45*time.Second:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(math.Round(d.Minutes())))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return t.Format("Jan 2")
}

// ring draws a progress ring: a faint track, and frac of it filled from
// the top, clockwise.
func (r *renderer) ring(img *image.RGBA, cx, cy, rad, frac float64, col color.RGBA) {
	const w = 1.6
	track := r.path(img)
	track.poly(arc(nil, cx, cy, rad, 0, 2*math.Pi))
	track.poly(reversed(arc(nil, cx, cy, rad-w, 0, 2*math.Pi)))
	track.fill(r.tinted(r.pal.text, 0.14))
	if frac <= 0 {
		return
	}
	frac = math.Min(1, frac)
	a0, a1 := -math.Pi/2, -math.Pi/2+2*math.Pi*frac
	p := r.path(img)
	p.poly(append(arc(nil, cx, cy, rad, a0, a1), reversed(arc(nil, cx, cy, rad-w, a0, a1))...))
	p.fill(col)
}
