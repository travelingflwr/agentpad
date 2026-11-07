package main

// The question sheet: Claude's multiple-choice questions (AskUserQuestion)
// as option cards. Claude Code shows its own question only after the hook
// returns, so this sheet is the question until you answer it, or press Esc
// to have the terminal ask instead (where you can also type your own
// answer).

import (
	"encoding/json"
	"fmt"
	"image"
	"strings"
	"syscall"
	"time"
)

type question struct {
	Question string `json:"question"`
	Header   string `json:"header"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
	MultiSelect bool `json:"multiSelect"`
}

type questionSheet struct {
	req       approvalRequest
	questions []question
	at        int            // the question on screen
	cursor    int            // the highlighted option
	picked    []map[int]bool // what's chosen, per question
	cards     [][4]float64   // where each option drew: x0, y0, x1, y1
	done      [4]float64     // the Done button, for several choices
}

func runQuestion(req approvalRequest, resPath string) error {
	var in struct {
		Questions []question `json:"questions"`
	}
	if err := json.Unmarshal(req.Input, &in); err != nil || len(in.Questions) == 0 {
		return fmt.Errorf("no questions in the request")
	}
	q := &questionSheet{req: req, questions: in.Questions, picked: make([]map[int]bool, len(in.Questions))}
	for i := range q.picked {
		q.picked[i] = map[int]bool{}
	}
	answer := func(res approvalResult) {
		out, _ := json.Marshal(res)
		writeAtomic(resPath, out)
	}

	s, done, err := openScreen("")
	if err != nil {
		return err
	}
	defer done()

	opened := time.Now()
	armed := false
	gone := ""
	var closeIn time.Time
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()

	draw := func() {
		s.begin()
		s.background()
		key := fmt.Sprint(s.ws, s.pal, armed, gone, q.at, q.cursor, q.picked[q.at])
		s.show("sheet", key, 1, s.ws.rows, 0, func() *image.RGBA {
			img := s.canvas(s.heightPt())
			q.draw(s.renderer(), img, s.widthPt(), s.heightPt(), armed, gone)
			return img
		})
		s.end()
	}
	draw()
	fitLayer(s, q.height(s.renderer(), s.widthPt()))

	// finish moves on from the current question, and answers once the last
	// one is settled.
	finish := func() bool {
		if len(q.picked[q.at]) == 0 {
			return false
		}
		if q.at < len(q.questions)-1 {
			q.at++
			q.cursor = 0
			return false
		}
		answers := map[string]string{}
		for i, qu := range q.questions {
			var labels []string
			for j, o := range qu.Options {
				if q.picked[i][j] {
					labels = append(labels, o.Label)
				}
			}
			answers[qu.Question] = strings.Join(labels, ", ")
		}
		answer(approvalResult{Decision: "answer", Answers: answers})
		return true
	}
	choose := func(j int) bool {
		cur := q.questions[q.at]
		if j < 0 || j >= len(cur.Options) {
			return false
		}
		q.cursor = j
		if cur.MultiSelect {
			q.picked[q.at][j] = !q.picked[q.at][j]
			if !q.picked[q.at][j] {
				delete(q.picked[q.at], j)
			}
			return false
		}
		q.picked[q.at] = map[int]bool{j: true}
		return finish()
	}

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
			case req.HookPID > 0 && syscall.Kill(req.HookPID, 0) != nil:
				gone = "Claude moved on"
			case now.After(req.Deadline):
				gone = "Timed out: the terminal will ask"
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
				answer(approvalResult{Decision: "terminal"})
				return nil
			}
			if !armed {
				continue
			}
			cur := q.questions[q.at]
			n := len(cur.Options)
			finished := false
			if ev.click != nil {
				x, y := s.clickPt(ev.click)
				for j, c := range q.cards {
					if x >= c[0] && x <= c[2] && y >= c[1] && y <= c[3] {
						finished = choose(j)
					}
				}
				d := q.done
				if cur.MultiSelect && x >= d[0] && x <= d[2] && y >= d[1] && y <= d[3] {
					finished = finish()
				}
			} else {
				k := ev.key
				switch {
				case k.mods&^modShift == 0 && k.code >= '1' && k.code <= '9':
					finished = choose(int(k.code - '1'))
				case k.is('j', 0), k.is(keyDown, 0), k.is(keyTab, 0):
					q.cursor = (q.cursor + 1) % n
				case k.is('k', 0), k.is(keyUp, 0), k.is(keyTab, modShift):
					q.cursor = (q.cursor + n - 1) % n
				case k.is(' ', 0):
					finished = choose(q.cursor)
				case k.is(keyEnter, 0):
					if cur.MultiSelect {
						if len(q.picked[q.at]) == 0 {
							choose(q.cursor)
						}
						finished = finish()
					} else {
						finished = choose(q.cursor)
					}
				case k.is(keyBackspace, 0), k.is(keyLeft, 0), k.is('h', 0):
					if q.at > 0 {
						q.at--
						q.cursor = 0
					}
				default:
					continue
				}
			}
			if finished {
				return nil
			}
			draw()
		}
	}
}

// Question layout, in points.
const (
	optPad    = 10.0
	optGap    = 6.0
	optLabel  = 13.5
	optDesc   = 12.0
	optDescLH = 16.0
	qSize     = 16.0
	qLineH    = 21.0
)

func (q *questionSheet) optionHeight(r *renderer, qu question, j int, textW float64) float64 {
	h := optPad + 17 + optPad - 4
	if d := qu.Options[j].Description; d != "" {
		h += optDescLH * float64(min(2, len(r.wrap(d, optDesc, false, textW))))
	}
	return h
}

// height is the tallest the sheet gets over all its questions, in points.
func (q *questionSheet) height(r *renderer, w float64) float64 {
	best := 0.0
	x0, x1 := sheetPad, w-sheetPad
	textW := x1 - x0 - 2*optPad - 30
	for _, qu := range q.questions {
		h := sheetPad + 4 + 12 + qLineH*float64(len(r.wrap(qu.Question, qSize, true, x1-x0))) + 12
		for j := range qu.Options {
			h += q.optionHeight(r, qu, j, textW) + optGap
		}
		h += 10 + buttonH + footerHint + sheetPad
		best = max(best, h)
	}
	return best
}

func (q *questionSheet) draw(r *renderer, img *image.RGBA, w, h float64, armed bool, gone string) {
	x0, x1 := sheetPad, w-sheetPad
	qu := q.questions[q.at]
	y := sheetPad + 4

	eyebrow := "Claude asks · " + q.req.Project()
	if q.req.Elsewhere != "" {
		eyebrow += " · " + q.req.Elsewhere
	}
	r.fillCircle(img, x0+4, y-4, 4, r.pal.accent)
	right := qu.Header
	if len(q.questions) > 1 {
		right = fmt.Sprintf("%d of %d", q.at+1, len(q.questions))
		if qu.Header != "" {
			right = qu.Header + " · " + right
		}
	}
	rw := 0.0
	if right != "" {
		rw = r.textRight(img, right, x1, y, 11.5, false, r.pal.faint) + 12
	}
	r.text(img, eyebrow, x0+14, y, 11.5, true, r.pal.gray, x1-x0-14-rw)
	y += 12
	for _, l := range r.wrap(qu.Question, qSize, true, x1-x0) {
		y += qLineH
		r.text(img, l, x0, y, qSize, true, r.pal.text, 0)
	}
	y += 12

	fade := 1.0
	if !armed {
		fade = 0.45
	}
	textX := x0 + optPad + 30
	textW := x1 - textX - optPad
	q.cards = q.cards[:0]
	m := r.mono()
	for j, o := range qu.Options {
		oh := q.optionHeight(r, qu, j, textW)
		picked := q.picked[q.at][j]
		bg := r.tinted(r.pal.text, 0.05*fade)
		if j == q.cursor && armed {
			bg = r.tinted(r.pal.accent, 0.18)
		}
		r.fillRoundRect(img, x0, y, x1, y+oh, 9, bg)
		q.cards = append(q.cards, [4]float64{x0, y, x1, y + oh})

		// The key: a number, or a check once picked.
		kx, ky := x0+optPad, y+optPad+1
		capBg, capFg := r.tinted(r.pal.text, 0.1*fade), premulOr(r, r.pal.text, fade)
		if picked {
			capBg, capFg = r.pal.accent, rgb(255, 255, 255)
			if luminance(r.pal.accent) > 0.3 {
				capFg = rgb(20, 20, 20)
			}
		}
		r.fillRoundRect(img, kx, ky, kx+20, ky+20, 5, capBg)
		if picked {
			r.check(img, kx+10, ky+10, 11, capFg)
		} else if j < 9 {
			k := fmt.Sprint(j + 1)
			kw := m.width(k, 11, true)
			m.drawRaw(img, k, kx+10-kw/2, ky+14.5, 11, true, capFg)
		}

		ly := y + optPad + 15
		r.text(img, o.Label, textX, ly, optLabel, true, premulOr(r, r.pal.text, fade), textW)
		if o.Description != "" {
			for i, l := range r.wrap(o.Description, optDesc, false, textW) {
				if i == 2 {
					break
				}
				ly += optDescLH
				if i == 1 && len(r.wrap(o.Description, optDesc, false, textW)) > 2 {
					l = r.fit(l+" …", optDesc, false, textW)
				}
				r.drawRaw(img, l, textX, ly, optDesc, false, r.pal.gray)
			}
		}
		y += oh + optGap
	}

	btnY0 := h - sheetPad - footerHint - buttonH
	if gone != "" {
		r.fillRoundRect(img, x0, btnY0, x1, btnY0+buttonH, buttonH/2, r.tinted(r.pal.text, 0.07))
		tw := r.width(r.clean(gone, true), 13, true)
		r.text(img, gone, (x0+x1)/2-tw/2, btnY0+buttonH/2+4.5, 13, true, r.pal.gray, 0)
		return
	}
	hint := fmt.Sprintf("1–%d or click to choose", min(9, len(qu.Options)))
	hintX := x0
	if qu.MultiSelect {
		hint = "Pick as many as you like, then Done"
		label := "Done"
		bw := r.width(label, 13, true) + 44
		white := rgb(255, 255, 255)
		if luminance(r.pal.accent) > 0.3 {
			white = rgb(20, 20, 20)
		}
		on := len(q.picked[q.at]) > 0 && armed
		bg, fg := r.tinted(r.pal.accent, 0.35), premul(white, 0.8)
		if on {
			bg, fg = r.pal.accent, white
		}
		r.fillRoundRect(img, x0, btnY0, x0+bw, btnY0+buttonH, buttonH/2, bg)
		lw := r.width(label, 13, true)
		r.drawRaw(img, label, x0+bw/2-lw/2, btnY0+buttonH/2+4.5, 13, true, fg)
		q.done = [4]float64{x0, btnY0, x0 + bw, btnY0 + buttonH}
		hintX = x0 + bw + 12
	}
	if !armed {
		hint = "Getting ready…"
	} else if q.at > 0 {
		hint += " · ← back"
	}
	r.text(img, hint, hintX, btnY0+buttonH/2+4, 12, false, r.pal.gray, x1-hintX)
	r.text(img, "Esc asks in the terminal instead, where you can type your own", x0, h-sheetPad-2, 11, false, r.pal.faint, x1-x0)
}
