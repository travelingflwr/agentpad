package main

// Activity: what every agent is up to, in the order it matters to you.
// Agents that need you come first, with the request on the card and its
// buttons; then the ones working, with how full their context is; then
// the ones that finished. The plan's usage sits above them, since every
// agent on the account shares it. It draws in the pane's own colours, so it
// follows the Rex theme, light or dark.

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Layout, in points.
const (
	avMaxW   = 560.0
	avHeadH  = 104.0 // the pinned title and tabs
	avPad    = 14.0  // inside a card
	avGap    = 10.0  // between cards
	avBtnH   = 36.0
	avRadius = 14.0
)

type avTab int

const (
	tabAll avTab = iota
	tabNeeds
	tabWorking
	tabDone
)

// pendingRequest is an open permission request or question for a session.
type pendingRequest struct {
	req     approvalRequest
	resPath string
	opened  time.Time
}

func (p *pendingRequest) armed() bool { return time.Since(p.opened) >= armDelay }

func (p *pendingRequest) answer(decision string) {
	out, _ := json.Marshal(approvalResult{Decision: decision})
	writeAtomic(p.resPath, out)
}

// pendingRequests is every open request, by Claude Code session.
func pendingRequests() map[string]*pendingRequest {
	out := map[string]*pendingRequest{}
	files, _ := filepath.Glob(filepath.Join(requestsDir(), "*.json"))
	for _, f := range files {
		if strings.HasSuffix(f, ".result.json") {
			continue
		}
		res := strings.TrimSuffix(f, ".json") + ".result.json"
		if _, err := os.Stat(res); err == nil {
			continue // answered
		}
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var req approvalRequest
		if json.Unmarshal(data, &req) != nil || time.Now().After(req.Deadline) {
			continue
		}
		if old, ok := out[req.SessionID]; !ok || fi.ModTime().Before(old.opened) {
			out[req.SessionID] = &pendingRequest{req: req, resPath: res, opened: fi.ModTime()}
		}
	}
	return out
}

// avHit is a place on the pane that does something when clicked.
type avHit struct {
	x0, y0, x1, y1 float64
	action         string // tab:N, allow:SESSION, always:…, deny:…, open:SESSION
}

type activityView struct {
	agents  []Agent
	pending map[string]*pendingRequest
	plan    planUsage
	hasPlan bool
	tab     avTab
	scroll  float64
	content float64 // how tall the list is, for scrolling
	hits    []avHit
	host    string
	msg     string
	demoCtx map[string]contextUse // the demo's, in place of the real ones
}

func (v *activityView) contextOf(a Agent) (contextUse, bool) {
	if v.demoCtx != nil {
		c, ok := v.demoCtx[a.SessionID]
		return c, ok
	}
	return contextOf(a)
}

func (v *activityView) reload() {
	v.agents = v.agents[:0]
	for _, a := range loadAgents() {
		if a.State != stEnded {
			v.agents = append(v.agents, a)
		}
	}
	v.pending = pendingRequests()
	v.plan, v.hasPlan = loadPlan()
}

// sections splits the agents the way the tabs do.
func (v *activityView) sections() (needs, working, done []Agent) {
	for _, a := range v.agents {
		switch {
		case a.State == stWaiting || v.pending[a.SessionID] != nil:
			needs = append(needs, a)
		case a.State == stWorking:
			working = append(working, a)
		default:
			done = append(done, a)
		}
	}
	sort.SliceStable(needs, func(i, j int) bool { return needs[i].Updated.Before(needs[j].Updated) })
	sort.SliceStable(working, func(i, j int) bool { return working[i].Started.After(working[j].Started) })
	sort.SliceStable(done, func(i, j int) bool { return done[i].Updated.After(done[j].Updated) })
	return
}

func runActivity(args []string) error {
	s, done, err := openScreen("")
	if err != nil {
		return err
	}
	defer done()
	host, _ := os.Hostname()
	v := &activityView{host: strings.Split(host, ".")[0]}
	v.reload()

	poll := time.NewTicker(400 * time.Millisecond)
	defer poll.Stop()
	slow := time.NewTicker(15 * time.Second)
	defer slow.Stop()
	stamp := activityStamp()
	msgUntil := time.Time{}

	draw := func() {
		s.begin()
		s.background()
		img := s.canvas(s.heightPt())
		v.draw(s.renderer(), img, s.widthPt(), s.heightPt())
		key := fmt.Sprint(s.ws, s.pal, v.drawKey())
		s.show("activity", key, 1, s.ws.rows, 0, func() *image.RGBA { return img })
		s.end()
	}
	say := func(m string) { v.msg, msgUntil = m, time.Now().Add(3*time.Second) }
	act := func(action string) {
		kind, id, _ := strings.Cut(action, ":")
		switch kind {
		case "tab":
			var n int
			fmt.Sscan(id, &n)
			if avTab(n) == v.tab {
				n = int(tabAll)
			}
			v.tab, v.scroll = avTab(n), 0
		case "allow", "always", "deny":
			if p := v.pending[id]; p != nil {
				if !p.armed() {
					return
				}
				p.answer(kind)
				delete(v.pending, id)
				say(map[string]string{"allow": "Allowed", "always": "Allowed from now on", "deny": "Denied"}[kind])
			}
		case "open":
			for _, a := range v.agents {
				if a.SessionID != id {
					continue
				}
				if a.RexSession == "" || a.RexBlock == "" {
					say("That session isn't in Rex")
				} else if err := focusBlock("", a.RexSession, a.RexBlock); err != nil {
					say("Couldn't find its pane")
				}
			}
		}
	}
	pruneAgents(loadAgents(), time.Now())
	draw()

	for {
		select {
		case <-s.quit:
			return nil
		case <-s.winch:
			s.resize()
			draw()
		case <-s.snapSig:
			s.saveSnapshot("activity")
		case <-poll.C:
			s.recheck()
			changed := false
			if st := activityStamp(); st != stamp {
				stamp = st
				v.reload()
				changed = true
			}
			if v.msg != "" && time.Now().After(msgUntil) {
				v.msg = ""
				changed = true
			}
			// Buttons arm a moment after a request arrives.
			for _, p := range v.pending {
				if !p.armed() && time.Since(p.opened) > armDelay-400*time.Millisecond {
					changed = true
				}
			}
			if changed {
				draw()
			}
		case <-slow.C:
			markGone(v.agents)
			v.reload()
			draw() // the ages move on
		case ev, open := <-s.events:
			if !open {
				return nil
			}
			if s.event(ev) {
				draw()
				continue
			}
			if ev.click != nil {
				x, y := s.clickPt(ev.click)
				for i := len(v.hits) - 1; i >= 0; i-- {
					h := v.hits[i]
					if x >= h.x0 && x < h.x1 && y >= h.y0 && y < h.y1 {
						act(h.action)
						draw()
						break
					}
				}
				continue
			}
			if ev.scroll != 0 {
				step := 48.0
				switch ev.scroll {
				case pageDown:
					v.scroll += s.heightPt() * 0.8
				case pageUp:
					v.scroll -= s.heightPt() * 0.8
				case scrollBottom:
					v.scroll = 1e9
				case scrollTop:
					v.scroll = 0
				case wheelDown, lineDown:
					v.scroll += step
				default:
					v.scroll -= step
				}
				draw()
				continue
			}
			k := ev.key
			switch {
			case k.is('q', 0), k.is(keyEscape, 0), k.is('c', modCtrl):
				return nil
			case k.is('j', 0), k.is(keyDown, 0):
				v.scroll += 48
			case k.is('k', 0), k.is(keyUp, 0):
				v.scroll -= 48
			case k.is('g', 0), k.is(keyHome, 0):
				v.scroll = 0
			case k.is('1', 0), k.is('2', 0), k.is('3', 0), k.is('4', 0):
				v.tab, v.scroll = avTab(k.code-'1'), 0
			case k.is('y', 0), k.is('a', 0), k.is('n', 0):
				// The first request in the list.
				needs, _, _ := v.sections()
				for _, a := range needs {
					if p := v.pending[a.SessionID]; p != nil && p.req.Tool != "AskUserQuestion" {
						act(map[rune]string{'y': "allow", 'a': "always", 'n': "deny"}[k.code] + ":" + a.SessionID)
						break
					}
				}
			case k.is(keyEnter, 0), k.is('o', 0):
				needs, working, done := v.sections()
				if all := append(append(needs, working...), done...); len(all) > 0 {
					act("open:" + all[0].SessionID)
				}
			default:
				continue
			}
			draw()
		}
	}
}

// activityStamp changes whenever an agent's record, a request or the
// usage does.
func activityStamp() string {
	var b strings.Builder
	b.WriteString(stampDir())
	for _, pat := range []string{filepath.Join(requestsDir(), "*.json"), filepath.Join(usageDir(), "*.json"), planPath()} {
		paths, _ := filepath.Glob(pat)
		for _, p := range paths {
			if fi, err := os.Stat(p); err == nil {
				fmt.Fprintf(&b, "%s%d;", filepath.Base(p), fi.ModTime().UnixNano())
			}
		}
	}
	return b.String()
}

func (v *activityView) drawKey() string {
	var b strings.Builder
	fmt.Fprint(&b, v.tab, v.scroll, v.msg, time.Now().Truncate(30*time.Second).Unix(), agentsKey(v.agents))
	for id, p := range v.pending {
		fmt.Fprint(&b, id, p.armed())
	}
	return b.String()
}

// --- drawing ---------------------------------------------------------------

// avPaint draws on img, or only measures when img is nil.
type avPaint struct {
	r   *renderer
	img *image.RGBA
}

func (p avPaint) on() bool { return p.img != nil }

func (p avPaint) text(s string, x, y, size float64, bold bool, col color.RGBA, maxW float64) float64 {
	if p.img == nil {
		return p.r.width(p.r.fit(p.r.clean(s, bold), size, bold, maxW), size, bold)
	}
	return p.r.text(p.img, s, x, y, size, bold, col, maxW)
}

func (p avPaint) right(s string, x, y, size float64, bold bool, col color.RGBA) float64 {
	if p.img == nil {
		return p.r.width(p.r.clean(s, bold), size, bold)
	}
	return p.r.textRight(p.img, s, x, y, size, bold, col)
}

func (p avPaint) box(x0, y0, x1, y1, rad float64, col color.RGBA) {
	if p.img != nil {
		p.r.fillRoundRect(p.img, x0, y0, x1, y1, rad, col)
	}
}

func (p avPaint) rect(x0, y0, x1, y1 float64, col color.RGBA) {
	if p.img != nil {
		p.r.fillRect(p.img, x0, y0, x1, y1, col)
	}
}

func (p avPaint) dot(cx, cy, rad float64, col color.RGBA) {
	if p.img != nil {
		p.r.fillCircle(p.img, cx, cy, rad, col)
	}
}

// outline is a rounded box's border, drawn as a ring.
func (p avPaint) outline(x0, y0, x1, y1, rad float64, col color.RGBA) {
	if p.img == nil {
		return
	}
	pb := p.r.path(p.img)
	pb.poly(roundRect(x0, y0, x1, y1, rad))
	pb.poly(reversed(roundRect(x0+1, y0+1, x1-1, y1-1, rad-1)))
	pb.fill(col)
}

// colours the view uses, all from the pane's theme.
type avColors struct {
	card, cardLine, well, track, needs, needsFill, needsInk, needsLine, work, good, warn color.RGBA
}

func (v *activityView) colors(r *renderer) avColors {
	amber := r.amber()
	ink := r.themed(rgb(255, 255, 255), rgb(26, 20, 6))
	return avColors{
		card:      r.tinted(r.pal.text, 0.05),
		cardLine:  r.tinted(r.pal.text, 0.10),
		well:      r.tinted(r.pal.text, 0.06),
		track:     r.tinted(r.pal.text, 0.12),
		needs:     amber,
		needsFill: r.themed(rgb(161, 98, 0), amber),
		needsInk:  ink,
		needsLine: r.tinted(amber, 0.45),
		work:      r.pal.accent,
		good:      r.green(),
		warn:      r.themed(rgb(196, 90, 20), rgb(242, 145, 90)),
	}
}

func (v *activityView) draw(r *renderer, img *image.RGBA, w, h float64) {
	v.hits = v.hits[:0]
	colW := min(w-24, avMaxW)
	x0 := (w - colW) / 2
	x1 := x0 + colW
	c := v.colors(r)

	// The list, scrolled, under the pinned header.
	needs, working, done := v.sections()
	measure := v.drawList(avPaint{r: r}, c, needs, working, done, x0, x1, 0)
	v.content = measure
	v.scroll = max(0, min(v.scroll, measure-(h-avHeadH)+16))
	v.drawList(avPaint{r: r, img: img}, c, needs, working, done, x0, x1, avHeadH-v.scroll)
	// Nothing of the list shows behind the header, and its buttons there
	// don't count.
	draw.Draw(img, image.Rect(0, 0, img.Bounds().Dx(), int(avHeadH*r.scale)), image.Transparent, image.Point{}, draw.Src)
	if !r.clear {
		r.fillRect(img, 0, 0, w, avHeadH, r.pal.bg)
	}
	kept := v.hits[:0]
	for _, ht := range v.hits {
		if ht.y0 >= avHeadH-1 {
			kept = append(kept, ht)
		}
	}
	v.hits = kept

	v.drawHeader(avPaint{r: r, img: img}, c, len(needs), len(working), len(done), x0, x1, w)
}

func (v *activityView) drawHeader(p avPaint, c avColors, nNeeds, nWorking, nDone int, x0, x1, w float64) {
	r := p.r
	hosts := map[string]bool{}
	for _, a := range v.agents {
		hosts[v.hostOf(a)] = true
	}
	sub := fmt.Sprintf("%d agent%s", len(v.agents), plural(len(v.agents)))
	if len(hosts) > 1 {
		sub = fmt.Sprintf("%d hosts · ", len(hosts)) + sub
	}
	if v.msg != "" {
		sub = v.msg
	}
	p.text(sub, x0+2, 20, 12, false, r.pal.gray, 0)
	p.text("Activity", x0, 46, 24, true, r.pal.text, 0)

	// Tabs.
	ty0, ty1 := 58.0, 92.0
	p.box(x0, ty0, x1, ty1, 10, c.card)
	tabs := []struct {
		label string
		n     int
		col   color.RGBA
	}{
		{"All", 0, c.work},
		{"Needs you", nNeeds, c.needsFill},
		{"Working", nWorking, c.work},
		{"Done", nDone, c.work},
	}
	tw := (x1 - x0 - 8) / float64(len(tabs))
	for i, t := range tabs {
		bx0 := x0 + 4 + float64(i)*tw
		bx1 := bx0 + tw
		label := t.label
		if i > 0 && t.n > 0 {
			label = fmt.Sprintf("%s %d", t.label, t.n)
		}
		size := 13.0
		if tw < 84 {
			size = 11.5
		}
		sel := avTab(i) == v.tab
		col := r.pal.gray
		if sel {
			p.box(bx0, ty0+4, bx1, ty1-4, 8, t.col)
			col = c.needsInk
			if t.col != c.needsFill {
				col = rgb(255, 255, 255)
			}
		} else if i == int(tabNeeds) && t.n > 0 {
			col = c.needs
		}
		lw := p.r.width(p.r.fit(p.r.clean(label, sel), size, sel, tw-8), size, sel)
		p.text(label, (bx0+bx1)/2-lw/2, (ty0+ty1)/2+size*0.36, size, sel || (i == int(tabNeeds) && t.n > 0), col, tw-8)
		v.hits = append(v.hits, avHit{bx0, ty0, bx1, ty1, fmt.Sprintf("tab:%d", i)})
	}
}

// drawList draws the plan and the sections from y and returns how tall
// they are.
func (v *activityView) drawList(p avPaint, c avColors, needs, working, done []Agent, x0, x1, y float64) float64 {
	start := y
	y += 10
	r := p.r
	if v.tab == tabAll && v.hasPlan {
		y = v.drawPlan(p, c, x0, x1, y) + 18
	}
	show := func(t avTab) bool { return v.tab == tabAll || v.tab == t }
	empty := true
	if show(tabNeeds) && len(needs) > 0 {
		empty = false
		y = v.section(p, "NEEDS YOU", c.needs, x0, y)
		for _, a := range needs {
			y = v.needsCard(p, c, a, x0, x1, y) + avGap
		}
		y += 8
	}
	if show(tabWorking) && len(working) > 0 {
		empty = false
		y = v.section(p, "WORKING", c.work, x0, y)
		y = v.rows(p, c, working, x0, x1, y, false) + 18
	}
	if show(tabDone) && len(done) > 0 {
		empty = false
		y = v.section(p, "DONE", c.good, x0, y)
		y = v.rows(p, c, done, x0, x1, y, true) + 18
	}
	if empty {
		msg := map[avTab]string{
			tabAll:     "No agents running",
			tabNeeds:   "Nothing needs you",
			tabWorking: "Nothing working",
			tabDone:    "Nothing finished yet",
		}[v.tab]
		y += 30
		p.text(msg, x0+2, y, 15, true, r.pal.gray, 0)
		if v.tab == tabAll {
			y += 22
			p.text("Start claude in a Rex pane and it shows up here.", x0+2, y, 12.5, false, r.pal.faint, x1-x0)
		}
		y += 20
	}
	return y - start
}

func (v *activityView) section(p avPaint, label string, col color.RGBA, x0, y float64) float64 {
	p.dot(x0+5, y+10, 4, col)
	p.text(label, x0+16, y+14.5, 11.5, true, col, 0)
	return y + 26
}

func (v *activityView) drawPlan(p avPaint, c avColors, x0, x1, y float64) float64 {
	r := p.r
	type row struct {
		label string
		w     *planWindow
	}
	var rows []row
	if v.plan.FiveHour != nil {
		rows = append(rows, row{"Claude · 5-hour", v.plan.FiveHour})
	}
	if v.plan.Weekly != nil {
		rows = append(rows, row{"Claude · weekly", v.plan.Weekly})
	}
	h := avPad*2 + 18 + float64(len(rows))*38
	p.box(x0, y, x1, y+h, avRadius, c.card)
	p.outline(x0, y, x1, y+h, avRadius, c.cardLine)
	ix0, ix1 := x0+avPad, x1-avPad
	ty := y + avPad + 12
	p.text("Plan usage", ix0, ty, 13, true, r.pal.text, 0)
	p.right("shared by every agent", ix1, ty, 11.5, false, r.pal.faint)
	ry := ty + 10
	for _, rw := range rows {
		ry += 18
		p.text(rw.label, ix0, ry, 12.5, false, r.pal.text, 0)
		note := fmt.Sprintf("%.0f%%", rw.w.Used)
		if !rw.w.ResetAt.IsZero() {
			note += " · resets " + resetText(r.now, rw.w.ResetAt)
		}
		col := r.pal.gray
		bar := c.work
		if rw.w.Used >= 80 {
			col, bar = c.warn, c.warn
		}
		p.right(note, ix1, ry, 12, false, col)
		by := ry + 8
		p.box(ix0, by, ix1, by+6, 3, c.track)
		p.box(ix0, by, ix0+max(6, (ix1-ix0)*min(1, rw.w.Used/100)), by+6, 3, bar)
		ry += 20
	}
	return y + h
}

func resetText(now, t time.Time) string {
	if t.Before(now) {
		return "now"
	}
	if t.Sub(now) < 20*time.Hour {
		return strings.ToLower(t.Format("3:04 pm"))
	}
	return t.Format("Mon")
}

func (v *activityView) hostOf(a Agent) string {
	if a.Host == "" || strings.EqualFold(strings.Split(a.Host, ".")[0], v.host) {
		return "this Mac"
	}
	return a.Host
}

func (v *activityView) meta(a Agent) string {
	return "Claude Code · " + a.Project() + " · " + v.hostOf(a)
}

func taskTitle(a Agent) string {
	if a.Prompt != "" {
		return oneLine(a.Prompt, 120)
	}
	return a.Project()
}

func (v *activityView) needsCard(p avPaint, c avColors, a Agent, x0, x1, y float64) float64 {
	r := p.r
	if p.on() {
		h := v.needsCard(avPaint{r: r}, c, a, x0, x1, y) - y
		p.box(x0, y, x1, y+h, avRadius, c.card)
		p.outline(x0, y, x1, y+h, avRadius, c.needsLine)
		// The whole card opens the agent's pane; its buttons come later and
		// win.
		v.hits = append([]avHit{{x0, y, x1, y + h, "open:" + a.SessionID}}, v.hits...)
	}
	ix0, ix1 := x0+avPad, x1-avPad
	pend := v.pending[a.SessionID]

	// The badge, the task and where it runs.
	p.box(ix0, y+avPad, ix0+32, y+avPad+32, 9, r.tinted(c.needs, 0.18))
	p.text("Cl", ix0+8, y+avPad+21, 12.5, true, c.needs, 0)
	tx := ix0 + 44
	age := agoText(r.now, a.Updated)
	if pend != nil {
		age = agoText(r.now, pend.opened)
	}
	aw := p.right(age, ix1, y+avPad+13, 11.5, false, r.pal.faint)
	p.text(taskTitle(a), tx, y+avPad+13, 15, true, r.pal.text, ix1-tx-aw-10)
	p.text(v.meta(a), tx, y+avPad+31, 12, false, r.pal.gray, ix1-tx)
	cy := y + avPad + 32 + 12

	// What it's waiting on.
	label, what := "Needs you", orDefault(a.Activity, "permission")
	question := false
	plain := false // prose rather than a command or a path
	if pend != nil {
		label, what, plain = requestText(pend.req)
		question = pend.req.Tool == "AskUserQuestion"
	} else if asksYou(a.Activity) {
		question = true
	}
	mono := r.mono()
	lines := mono.wrap(what, 12.5, false, ix1-ix0-24)
	if question || plain {
		lines = r.wrap(what, 13.5, false, ix1-ix0-24)
	}
	if len(lines) > 3 {
		lines = append(lines[:3:3], "…")
	}
	wellH := 26 + float64(len(lines))*18
	p.box(ix0, cy, ix1, cy+wellH, 10, c.well)
	p.text(label, ix0+12, cy+17, 11.5, false, r.pal.gray, 0)
	for i, l := range lines {
		ly := cy + 36 + float64(i)*18
		if p.on() {
			if question || plain {
				r.drawRaw(p.img, l, ix0+12, ly, 13.5, false, r.pal.text)
			} else {
				mono.drawRaw(p.img, l, ix0+12, ly, 12.5, false, r.pal.text)
			}
		}
	}
	cy += wellH + 12

	// The buttons.
	type button struct {
		label, action string
		primary       bool
		weight        float64
	}
	var bs []button
	if pend != nil && !question {
		bs = []button{
			{"Deny", "deny:" + a.SessionID, false, 1},
			{"Always", "always:" + a.SessionID, false, 1},
			{"Allow", "allow:" + a.SessionID, true, 1.3},
		}
	} else {
		bs = []button{{"Open tab", "open:" + a.SessionID, true, 1}}
		if question {
			bs[0].label = "Answer in its tab"
		}
	}
	armed := pend == nil || pend.armed()
	total := 0.0
	for _, b := range bs {
		total += b.weight
	}
	gap := 8.0
	unit := (ix1 - ix0 - gap*float64(len(bs)-1)) / total
	bx := ix0
	for _, b := range bs {
		bw := unit * b.weight
		ink := r.pal.text
		if b.primary {
			fill := c.needsFill
			ink = c.needsInk
			if !armed {
				fill = r.tinted(c.needsFill, 0.45)
			}
			p.box(bx, cy, bx+bw, cy+avBtnH, 10, fill)
		} else {
			p.outline(bx, cy, bx+bw, cy+avBtnH, 10, c.cardLine)
			if !armed {
				ink = r.pal.faint
			}
		}
		lw := r.width(r.clean(b.label, b.primary), 14, b.primary)
		p.text(b.label, bx+bw/2-lw/2, cy+avBtnH/2+5, 14, b.primary, ink, 0)
		if p.on() {
			v.hits = append(v.hits, avHit{bx, cy, bx + bw, cy + avBtnH, b.action})
		}
		bx += bw + gap
	}
	cy += avBtnH + 12

	if use, ok := v.contextOf(a); ok {
		cy = v.meter(p, c, use, ix0, ix1, cy+4, "Context") + 4
	}
	return cy + avPad - 8
}

// meter draws a context bar with its label and numbers, baseline at y.
func (v *activityView) meter(p avPaint, c avColors, use contextUse, x0, x1, y float64, label string) float64 {
	r := p.r
	frac := use.frac()
	note := fmt.Sprintf("%s / %s", kTokens(use.Tokens), kTokens(use.Window))
	col, bar := r.pal.gray, c.good
	if frac >= 0.85 {
		note = fmt.Sprintf("%.0f%% · compacts soon", frac*100)
		col, bar = c.warn, c.warn
	}
	lx := x0
	if label != "" {
		lx += p.text(label, x0, y, 11.5, false, r.pal.gray, 0) + 10
	}
	nw := p.right(note, x1, y, 11.5, false, col)
	bx0, bx1 := lx, x1-nw-10
	if bx1-bx0 > 24 {
		p.box(bx0, y-6, bx1, y-2, 2, c.track)
		p.box(bx0, y-6, bx0+max(4, (bx1-bx0)*frac), y-2, 2, bar)
	}
	return y + 6
}

func kTokens(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprint(n)
}

// rows draws agents as a grouped list in one card.
func (v *activityView) rows(p avPaint, c avColors, agents []Agent, x0, x1, y float64, finished bool) float64 {
	r := p.r
	rowH := 58.0
	if finished {
		rowH = 52
	}
	h := rowH * float64(len(agents))
	p.box(x0, y, x1, y+h, avRadius, c.card)
	p.outline(x0, y, x1, y+h, avRadius, c.cardLine)
	ix0, ix1 := x0+avPad, x1-avPad
	for i, a := range agents {
		ry := y + float64(i)*rowH
		if i > 0 {
			p.rect(ix0, ry, ix1, ry+1, c.cardLine)
		}
		if p.on() {
			v.hits = append(v.hits, avHit{x0, ry, x1, ry + rowH, "open:" + a.SessionID})
		}
		tx := ix0
		if finished {
			if p.on() {
				r.check(p.img, ix0+7, ry+rowH/2, 12, c.good)
			}
			tx = ix0 + 24
			age := "done " + agoText(r.now, a.Updated)
			if strings.HasSuffix(age, "now") {
				age = "just done"
			} else if !strings.HasSuffix(age, "Jan 2") {
				age += " ago"
			}
			p.text(taskTitle(a), tx, ry+22, 14.5, true, r.pal.text, ix1-tx)
			p.text(v.meta(a)+" · "+age, tx, ry+39, 12, false, r.pal.gray, ix1-tx)
			continue
		}
		age := agoText(r.now, a.Started)
		aw := p.right(age, ix1, ry+22, 11.5, false, r.pal.faint)
		p.text(taskTitle(a), tx, ry+22, 14.5, true, r.pal.text, ix1-tx-aw-10)
		meta := v.meta(a)
		if a.Activity != "" {
			meta = a.Activity
		}
		if use, ok := v.contextOf(a); ok {
			mw := min(p.text(meta, tx, ry+42, 12, false, r.pal.gray, (ix1-tx)*0.5), (ix1-tx)*0.5)
			v.meter(p, c, use, tx+mw+10, ix1, ry+42, "")
		} else {
			p.text(meta, tx, ry+42, 12, false, r.pal.gray, ix1-tx)
		}
	}
	return y + h
}

// requestText is what a request asks, for its card: a label, the text, and
// whether the text is prose (else it's a command or a path, in mono).
func requestText(req approvalRequest) (label, text string, plain bool) {
	var in map[string]any
	json.Unmarshal(req.Input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	rel := func(p string) string {
		if r, err := filepath.Rel(req.Cwd, p); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return p
	}
	switch req.Tool {
	case "Bash":
		return "Wants to run", str("command"), false
	case "Edit", "MultiEdit":
		return "Wants to edit", rel(str("file_path")), false
	case "Write":
		return "Wants to write", rel(str("file_path")), false
	case "NotebookEdit":
		return "Wants to edit", rel(str("notebook_path")), false
	case "WebFetch":
		return "Wants to fetch", str("url"), false
	case "AskUserQuestion":
		if qs, ok := in["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				t, _ := q["question"].(string)
				if len(qs) > 1 {
					t += fmt.Sprintf(" (and %d more)", len(qs)-1)
				}
				return "Asks you", t, true
			}
		}
		return "Asks you", "A question", true
	case "ExitPlanMode":
		return "Wants to start on", "its plan", true
	}
	return "Wants to", describeTool(req.Tool, req.Input, req.Cwd), true
}
