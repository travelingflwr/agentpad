package main

// `agentpad chat` is Claude Code drawn as a Rex surface. The real Claude
// Code runs in a tab of its own (the engine, on your subscription, with all
// its tools, settings and slash commands); the chat pane draws the
// conversation from the session's transcript and types what you write into
// the engine. Permission prompts and questions arrive as agentpad's sheets.
// Ctrl+O shows the engine's own terminal, which keeps working throughout.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// --- the conversation, from the transcript ------------------------------

type itemKind int

const (
	itUser itemKind = iota
	itText          // Claude's words
	itTool          // a tool call and, once it's back, its result
	itNote          // a slash command, its output, an interruption
)

type chatItem struct {
	kind   itemKind
	text   string
	tool   string
	input  json.RawMessage
	result string
	isErr  bool
	denied bool // you said no
	done   bool

	ver  int // bumps when the item changes, to redo its layout
	h    float64
	hw   float64 // the width h was measured at
	hver int
}

type chatLog struct {
	path   string
	offset int64
	cwd    string
	items  []chatItem
	byID   map[string]int // tool_use ID to its item
	users  int            // user messages read so far
}

var (
	commandName = regexp.MustCompile(`<command-name>(.*?)</command-name>`)
	commandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	localOutput = regexp.MustCompile(`(?s)<local-command-std(?:out|err)>(.*?)</local-command-std(?:out|err)>`)
	ansiCodes   = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
)

// read takes in what's been added to the transcript and reports whether
// anything changed.
func (c *chatLog) read() bool {
	if c.path == "" {
		return false
	}
	f, err := os.Open(c.path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return false
	}
	r := bufio.NewReaderSize(f, 1<<16)
	changed := false
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial last line is read again next time
		}
		c.offset += int64(len(line))
		if c.take(line) {
			changed = true
		}
	}
	return changed
}

func (c *chatLog) add(it chatItem) {
	c.items = append(c.items, it)
}

func (c *chatLog) take(line []byte) bool {
	var l struct {
		Type      string `json:"type"`
		IsMeta    bool   `json:"isMeta"`
		Sidechain bool   `json:"isSidechain"`
		Cwd       string `json:"cwd"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.Sidechain || (l.Type != "user" && l.Type != "assistant") {
		return false
	}
	if l.Cwd != "" {
		c.cwd = l.Cwd
	}
	content := l.Message.Content
	if len(content) == 0 {
		return false
	}
	if content[0] == '"' {
		var s string
		json.Unmarshal(content, &s)
		if l.IsMeta || l.Type != "user" {
			return false
		}
		return c.userText(s)
	}
	var parts []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return false
	}
	changed := false
	for _, p := range parts {
		switch {
		case p.Type == "text" && l.Type == "assistant":
			if t := strings.TrimSpace(p.Text); t != "" {
				c.add(chatItem{kind: itText, text: t})
				changed = true
			}
		case p.Type == "text" && !l.IsMeta:
			if c.userText(p.Text) {
				changed = true
			}
		case p.Type == "tool_use":
			c.byID[p.ID] = len(c.items)
			c.add(chatItem{kind: itTool, tool: p.Name, input: p.Input})
			changed = true
		case p.Type == "tool_result":
			i, ok := c.byID[p.ToolUseID]
			if !ok {
				continue
			}
			it := &c.items[i]
			it.result, it.isErr, it.done = resultText(p.Content), p.IsError, true
			if it.isErr && strings.Contains(it.result, "doesn't want to proceed") {
				it.result, it.denied = "", true
			}
			it.ver++
			changed = true
		}
	}
	return changed
}

// userText adds what you typed, or the note a slash command leaves.
func (c *chatLog) userText(s string) bool {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return false
	case strings.HasPrefix(s, "[Request interrupted"):
		c.add(chatItem{kind: itNote, text: "Interrupted"})
		return true
	case strings.Contains(s, "<command-name>"):
		name := ""
		if m := commandName.FindStringSubmatch(s); m != nil {
			name = m[1]
		}
		if m := commandArgs.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
			name += " " + strings.TrimSpace(m[1])
		}
		c.add(chatItem{kind: itNote, text: name})
		c.users++
		return true
	case strings.Contains(s, "<local-command-std"):
		if m := localOutput.FindStringSubmatch(s); m != nil {
			if out := strings.TrimSpace(ansiCodes.ReplaceAllString(m[1], "")); out != "" {
				c.add(chatItem{kind: itNote, text: out})
				return true
			}
		}
		return false
	case strings.HasPrefix(s, "<"):
		return false // reminders and other machinery
	}
	c.add(chatItem{kind: itUser, text: s})
	c.users++
	return true
}

// resultText is a tool result's text, whether it came as a string or parts.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
			b.WriteByte('\n')
		} else if p.Type == "image" {
			b.WriteString("[image]\n")
		}
	}
	return b.String()
}

// --- the engine ---------------------------------------------------------

// startEngine opens Claude Code in a Rex session of its own (so it can name
// that session, as Rex asks agents to, without renaming yours) and returns
// the session and its pane. The Rex app stays where it is.
func startEngine(dir string, claudeArgs []string) (session, block string, err error) {
	label := "claude-" + safeName(filepath.Base(dir))
	argv := []string{"new", label, "--window", "Claude Code", "--json", "-c", dir, "--", "claude"}
	argv = append(argv, claudeArgs...)
	out, err := exec.Command("rex", argv...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", "", fmt.Errorf("rex new: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", "", err
	}
	session = string(regexp.MustCompile(`session:[0-9a-f-]+`).Find(out))
	block = string(regexp.MustCompile(`block:[0-9a-f-]+`).Find(out))
	if session == "" || block == "" {
		return "", "", fmt.Errorf("rex new didn't say which pane it made: %s", strings.TrimSpace(string(out)))
	}
	return session, block, nil
}

// engineAgent is the session record agentpad's hooks keep for the engine.
func engineAgent(block string) (Agent, bool) {
	var best Agent
	found := false
	for _, a := range loadAgents() {
		if a.RexBlock == block && a.State != stEnded && (!found || a.Updated.After(best.Updated)) {
			best, found = a, true
		}
	}
	return best, found
}

// sendPrompt types text into the engine and presses Enter.
func sendPrompt(session, block, text string) error {
	if strings.Contains(text, "\n") {
		text = "\x1b[200~" + text + "\x1b[201~" // a paste, so the lines stay in one message
	}
	if out, err := exec.Command("rex", "send", "-s", session, "-b", block, text).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	time.Sleep(120 * time.Millisecond)
	return exec.Command("rex", "send-key", "-s", session, "-b", block, "enter").Run()
}

func sendKey(session, block, key string) {
	exec.Command("rex", "send-key", "-s", session, "-b", block, key).Run()
}

// --- the pane -----------------------------------------------------------

func runChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	attach := fs.String("block", "", "use the Claude Code already running in this pane instead of starting one")
	cont := fs.Bool("continue", false, "continue the folder's most recent conversation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := needRex(); err != nil {
		return err
	}
	// agentpad chat [DIR] [-- CLAUDE FLAGS]
	rest := fs.Args()
	var extra []string
	for i, a := range rest {
		if a == "--" {
			rest, extra = rest[:i], rest[i+1:]
			break
		}
	}
	dir, _ := os.Getwd()
	if len(rest) > 0 {
		dir, _ = filepath.Abs(rest[0])
	}
	session, engine := os.Getenv("REX_SESSION"), *attach // the engine's session and pane
	if engine != "" {
		a, ok := engineAgent(engine)
		if !ok || a.RexSession == "" {
			return fmt.Errorf("no Claude Code that agentpad knows of is running in %s", engine)
		}
		session = a.RexSession
		if a.Cwd != "" && len(rest) == 0 {
			dir = a.Cwd
		}
	} else {
		claudeArgs := extra
		if *cont {
			claudeArgs = append(claudeArgs, "--continue")
		}
		var err error
		if session, engine, err = startEngine(dir, claudeArgs); err != nil {
			return err
		}
	}

	// Tell the engine's hooks that this pane is its face, so its sheets
	// float here.
	sp := surfacePath(session)
	os.MkdirAll(filepath.Dir(sp), 0o700)
	surf, _ := json.Marshal(map[string]any{"session": os.Getenv("REX_SESSION"), "block": os.Getenv("REX_BLOCK"), "pid": os.Getpid()})
	writeAtomic(sp, surf)
	defer os.Remove(sp)

	s, done, err := openScreen("")
	if err != nil {
		return err
	}
	defer done()

	c := &chatLog{byID: map[string]int{}, cwd: dir}
	ch := &chatView{s: s, log: c, project: filepath.Base(dir)}
	started := time.Now()
	var agent Agent
	haveAgent := false
	lastStamp := ""
	sent := 0 // your messages sent from here, waiting to show in the transcript
	var history []string
	histAt := -1
	sendErr := make(chan error, 4)
	var act func(string)
	var held []string // messages waiting for Claude Code's prompt to close
	ch.cmds = loadCommands(dir)
	ch.in.goalX = -1
	trustAsk := false // Claude Code is asking whether to trust a new folder
	lastLook := time.Time{}

	refresh := func() bool {
		changed := false
		if st := stampDir(); st != lastStamp {
			lastStamp = st
			a, ok := engineAgent(engine)
			if ok != haveAgent || a.Updated != agent.Updated || a.State != agent.State || a.Activity != agent.Activity {
				changed = true
			}
			agent, haveAgent = a, ok
			if ok && a.Transcript != "" && a.Transcript != c.path {
				*c = chatLog{path: a.Transcript, byID: map[string]int{}, cwd: dir}
				changed = true
			}
		}
		// Until the hooks hear from it, look at the engine's screen for the
		// question Claude Code asks in a folder it hasn't seen.
		if !haveAgent && time.Since(lastLook) > time.Second {
			lastLook = time.Now()
			out, _ := exec.Command("rex", "capture", "-s", session, "-b", engine).Output()
			ask := strings.Contains(string(out), "trust this folder")
			if ask != trustAsk {
				trustAsk, changed = ask, true
			}
		} else if haveAgent && trustAsk {
			trustAsk, changed = false, true
		}
		before := c.users
		if c.read() {
			changed = true
		}
		if c.users > before {
			sent = max(0, sent-(c.users-before))
		}
		return changed
	}

	status := func() (string, statusKind) {
		switch {
		case trustAsk:
			return "Claude Code hasn't worked in this folder before. Trust it? Ctrl+Y trusts it · Ctrl+O to look first", skWarn
		case !haveAgent && time.Since(started) > 12*time.Second:
			return "Claude Code hasn't started here. It may be asking something: Ctrl+O shows it", skWarn
		case !haveAgent:
			return "Starting Claude Code…", skBusy
		case agent.State == stWaiting:
			return "Needs you: " + orDefault(agent.Activity, "a permission"), skWarn
		case agent.State == stWorking || sent > 0:
			what := orDefault(agent.Activity, "Thinking")
			el := time.Since(agent.Started)
			if sent > 0 && agent.State != stWorking {
				what, el = "Sending", 0
			}
			t := what
			if el > 2*time.Second {
				t += fmt.Sprintf(" · %s", shortDur(el))
			}
			return t + " · Esc interrupts", skBusy
		}
		return "", skIdle
	}

	draw := func(all bool) {
		st, kind := status()
		s.begin()
		s.background()
		ch.place(all, st, kind)
		s.end()
	}
	refresh()
	draw(true)

	// submit sends what's in the box, or a slash command. A command that
	// opens its own screen in Claude Code shows the terminal too.
	submit := func(raw string, held2 bool) {
		text := strings.TrimSpace(raw)
		if text == "" {
			return
		}
		if !haveAgent {
			ch.flash = "Claude Code isn't ready yet"
			return
		}
		ch.in.set("")
		ch.scroll, ch.flash, histAt = 0, "", -1
		if !held2 {
			history = append(history, text)
		}
		if agent.State == stWaiting || trustAsk {
			// Keys typed now would land in Claude Code's prompt, where
			// Enter picks its highlighted answer. Hold the message.
			held = append(held, text)
			ch.flash = "Claude Code is asking you something: this sends once it's answered"
			return
		}
		sent++
		showTerminal := false
		if strings.HasPrefix(text, "/") {
			name := strings.Fields(text)[0]
			for _, c := range ch.cmds {
				if c.name == name && c.terminal {
					showTerminal = true
				}
			}
		}
		go func() {
			err := sendPrompt(session, engine, text)
			sendErr <- err
			if err == nil && showTerminal {
				time.Sleep(300 * time.Millisecond)
				focusBlock("", session, engine)
			}
		}()
	}
	// act does what a key or a button asks for.
	act = func(a string) {
		switch {
		case a == "send":
			submit(ch.in.String(), false)
		case a == "stop":
			if agent.State == stWorking || agent.State == stWaiting || sent > 0 {
				go sendKey(session, engine, "escape")
				ch.flash = "Interrupting…"
			}
		case a == "terminal":
			focusBlock("", session, engine)
		case a == "trust" && trustAsk:
			go func() {
				sendKey(session, engine, "down")
				time.Sleep(150 * time.Millisecond)
				sendKey(session, engine, "enter")
			}()
			trustAsk = false
			ch.flash = "Trusted. Starting Claude Code…"
		case strings.HasPrefix(a, "menu:"):
			var i int
			fmt.Sscan(strings.TrimPrefix(a, "menu:"), &i)
			if menu := ch.menuItems(); i < len(menu) {
				ch.in.set(menu[i].name)
				submit(menu[i].name, false)
			}
		}
	}

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.quit:
			return nil
		case <-s.winch:
			s.resize()
			draw(true)
		case <-s.snapSig:
			ch.snapshot()
		case err := <-sendErr:
			if err != nil {
				ch.flash = "Couldn't reach Claude Code: " + err.Error()
				sent = 0
				draw(false)
			}
		case <-tick.C:
			s.recheck()
			changed := refresh()
			if len(held) > 0 && haveAgent && agent.State != stWaiting && !trustAsk {
				text := held[0]
				held = held[1:]
				submit(text, true)
				changed = true
			}
			_, kind := status()
			if changed {
				draw(true)
			} else if kind == skBusy || ch.flash != "" {
				ch.spin++
				draw(false)
			}
		case ev, open := <-s.events:
			if !open {
				return nil
			}
			if s.event(ev) {
				draw(true)
				continue
			}
			if ev.scroll != 0 {
				step := 60.0
				if ev.scroll == pageUp || ev.scroll == pageDown {
					step = ch.logH * 0.8
				}
				switch ev.scroll {
				case wheelUp, lineUp, pageUp:
					ch.scroll += step
				case scrollTop:
					ch.scroll = math.Inf(1)
				case scrollBottom:
					ch.scroll = 0
				default:
					ch.scroll -= step
				}
				draw(true)
				continue
			}
			if ev.click != nil {
				x, y := s.clickPt(ev.click)
				if a := ch.hit(x, y-ch.logH); a != "" {
					act(a)
					draw(false)
				}
				continue
			}
			if ev.paste != "" {
				ch.in.insert([]rune(strings.ReplaceAll(ev.paste, "\r\n", "\n")))
				ch.flash = ""
				draw(false)
				continue
			}
			k := ev.key
			lines := ch.in.layout(s.renderer(), inputSize, ch.inputW(s.widthPt()))
			r := s.renderer()
			// The slash menu takes the arrows, Tab, Enter and Esc while it's up.
			if menu := ch.menuItems(); len(menu) > 0 {
				n := len(menu)
				handled := true
				switch {
				case k.is(keyUp, 0), k.is(keyTab, modShift):
					ch.menuSel = (ch.menuSel + n - 1) % n
				case k.is(keyDown, 0):
					ch.menuSel = (ch.menuSel + 1) % n
				case k.is(keyTab, 0):
					ch.in.set(menu[ch.menuSel].name + " ")
				case k.is(keyEnter, 0):
					act(fmt.Sprintf("menu:%d", ch.menuSel))
				case k.is(keyEscape, 0):
					ch.menuOff = true
				default:
					handled = false
				}
				if handled {
					draw(false)
					continue
				}
			}
			typed := []rune(nil)
			switch {
			case k.is('d', modCtrl) && ch.in.empty(), k.is('c', modCtrl) && ch.in.empty():
				done()
				os.Remove(sp)
				fmt.Println("Claude Code keeps running in its own Rex session.")
				os.Exit(0)
			case k.is('d', modCtrl), k.is(keyDelete, 0):
				ch.in.deleteForward()
			case k.is('c', modCtrl):
				ch.in.set("")
			case k.is('u', modCtrl):
				ls := ch.in.lineStart()
				ch.in.text = append(ch.in.text[:ls], ch.in.text[ch.in.cur:]...)
				ch.in.move(ls)
			case k.is('k', modCtrl):
				le := ch.in.lineEnd()
				ch.in.text = append(ch.in.text[:ch.in.cur], ch.in.text[le:]...)
			case k.is('y', modCtrl) && trustAsk:
				act("trust")
			case k.is('o', modCtrl):
				act("terminal")
				continue
			case k.is(keyEscape, 0):
				act("stop")
			case k.is(keyEnter, 0):
				act("send")
			case k.is(keyEnter, modShift), k.is(keyEnter, modAlt), k.is('j', modCtrl):
				typed = []rune{'\n'}
			case k.is(keyBackspace, 0):
				ch.in.backspace()
			case k.is(keyBackspace, modAlt), k.is('w', modCtrl):
				ch.in.deleteWord()
			case k.is(keyBackspace, modSuper):
				ls := ch.in.lineStart()
				ch.in.text = append(ch.in.text[:ls], ch.in.text[ch.in.cur:]...)
				ch.in.move(ls)
			case k.is(keyLeft, 0), k.is('b', modCtrl):
				ch.in.move(ch.in.cur - 1)
			case k.is(keyRight, 0), k.is('f', modCtrl):
				ch.in.move(ch.in.cur + 1)
			case k.is(keyLeft, modAlt), k.is('b', modAlt):
				ch.in.move(ch.in.wordStart())
			case k.is(keyRight, modAlt), k.is('f', modAlt):
				ch.in.move(ch.in.wordEnd())
			case k.is(keyLeft, modSuper), k.is(keyHome, 0), k.is('a', modCtrl):
				ch.in.move(ch.in.lineStart())
			case k.is(keyRight, modSuper), k.is(keyEnd, 0), k.is('e', modCtrl):
				ch.in.move(ch.in.lineEnd())
			case k.is(keyUp, modSuper):
				ch.in.move(0)
			case k.is(keyDown, modSuper):
				ch.in.move(len(ch.in.text))
			case k.is(keyUp, 0):
				// Up on the first line (or in an empty box) goes back
				// through what you've sent.
				if histAt >= 0 || ch.in.empty() || !ch.in.vertical(r, lines, inputSize, -1) {
					if len(history) > 0 && (histAt >= 0 || ch.in.empty()) {
						if histAt < 0 {
							histAt = len(history)
						}
						histAt = max(0, histAt-1)
						ch.in.set(history[histAt])
					} else {
						ch.in.move(0)
					}
				}
			case k.is(keyDown, 0):
				if histAt >= 0 {
					histAt++
					if histAt >= len(history) {
						histAt = -1
						ch.in.set("")
					} else {
						ch.in.set(history[histAt])
					}
				} else if !ch.in.vertical(r, lines, inputSize, 1) {
					ch.in.move(len(ch.in.text))
				}
			case k.is(keyPageUp, 0):
				ch.scroll += ch.logH * 0.8
				draw(true)
				continue
			case k.is(keyPageDown, 0):
				ch.scroll -= ch.logH * 0.8
				draw(true)
				continue
			case k.code >= 0x20 && (k.mods == 0 || k.mods == modShift):
				typed = []rune{k.code}
			case k.is(keyTab, 0):
				typed = []rune{' ', ' '}
			default:
				continue
			}
			if typed != nil {
				ch.in.insert(typed)
				ch.flash = ""
			}
			draw(false)
		}
	}
}

func shortDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

type statusKind int

const (
	skIdle statusKind = iota
	skBusy
	skWarn
)

// chatView draws the conversation (the "log" image, which only changes with
// the transcript or the scroll) above the composer (which changes as you
// type), so a key press redraws only the composer.
type chatView struct {
	s       *screen
	log     *chatLog
	project string
	in      composer
	cmds    []slashCommand
	menuSel int
	menuOff bool   // Esc closed the menu for what's typed
	menuFor string // what the menu was made for
	buttons []chatButton
	busy    bool    // there's something to stop
	scroll  float64 // points scrolled up from the bottom
	flash   string
	spin    int
	logH    float64
	logRows int
}

// Layout, in points.
const (
	chatPad    = 16.0
	chatGap    = 14.0
	proseSize  = 13.5
	proseLH    = 20.0
	inputSize  = 13.5
	inputLH    = 19.0
	maxInputLn = 8
)

func (ch *chatView) place(all bool, status string, kind statusKind) {
	s := ch.s
	r := s.renderer()
	w := s.widthPt()
	ch.busy = kind == skBusy
	lines := ch.in.layout(r, inputSize, ch.inputW(w))
	compPt := ch.composerHeight(lines)
	compRows := min(s.ws.rows-2, int(math.Ceil(compPt/s.cellPt())))
	logRows := s.ws.rows - compRows
	if logRows != ch.logRows {
		all = true
	}
	ch.logRows = logRows
	ch.logH = float64(logRows) * s.cellPt()

	if all {
		content := ch.layout(r, w)
		maxScroll := math.Max(0, content-ch.logH)
		ch.scroll = math.Max(0, math.Min(ch.scroll, maxScroll))
		key := fmt.Sprint(s.ws, s.pal, logRows, ch.scroll, len(ch.log.items), ch.itemsVer())
		s.show("log", key, 1, logRows, 0, func() *image.RGBA {
			img := s.canvas(ch.logH)
			ch.drawLog(s.renderer(), img, w, content)
			return img
		})
	}
	key := fmt.Sprint(s.ws, s.pal, compRows, ch.in.String(), ch.in.cur, ch.menuSel, ch.menuOff, status, kind, ch.flash, ch.spin%8)
	h := float64(compRows) * s.cellPt()
	s.show("composer", key, logRows+1, compRows, 0, func() *image.RGBA {
		img := s.canvas(h)
		ch.drawComposer(s.renderer(), img, w, h, lines, status, kind)
		return img
	})
}

func (ch *chatView) itemsVer() int {
	v := 0
	for _, it := range ch.log.items {
		v += it.ver
	}
	return v
}

// layout measures every item at width w and returns the content's height.
func (ch *chatView) layout(r *renderer, w float64) float64 {
	total := chatPad
	for i := range ch.log.items {
		it := &ch.log.items[i]
		if it.hw != w || it.hver != it.ver || it.h == 0 {
			it.h = ch.drawItem(r, nil, it, 0, w)
			it.hw, it.hver = w, it.ver
		}
		total += it.h + chatGap
	}
	return total
}

func (ch *chatView) drawLog(r *renderer, img *image.RGBA, w, content float64) {
	top := math.Max(0, content-ch.logH-ch.scroll) // the content's y at the image's top
	y := chatPad
	if content < ch.logH {
		y += 0 // short conversations start at the top
	}
	if len(ch.log.items) == 0 {
		c0, c1 := columnOf(w)
		r.text(img, "Claude Code · "+ch.project, c0, chatPad+40, 20, true, r.pal.text, c1-c0)
		r.text(img, "Ask for anything. It runs in a Claude Code of its own, with your tools and settings: Ctrl+O shows it.", c0, chatPad+66, 13, false, r.pal.gray, c1-c0)
		return
	}
	for i := range ch.log.items {
		it := &ch.log.items[i]
		if y+it.h >= top && y <= top+ch.logH {
			ch.drawItem(r, img, it, y-top, w)
		}
		y += it.h + chatGap
	}
	if ch.scroll > 1 {
		label := "↓ Newer messages · End or scroll down"
		tw := r.width(r.clean(label, true), 11.5, true) + 24
		x0 := w/2 - tw/2
		y0 := ch.logH - 34
		r.fillRoundRect(img, x0, y0, x0+tw, y0+24, 12, r.tinted(r.pal.accent, 0.9))
		r.text(img, label, x0+12, y0+16, 11.5, true, rgb(255, 255, 255), 0)
	}
}

// drawItem draws it with its top at y (or only measures, when img is nil)
// and returns its height.
func (ch *chatView) drawItem(r *renderer, img *image.RGBA, it *chatItem, y, w float64) float64 {
	p := painter{r: r, img: img}
	x0, x1 := columnOf(w)
	switch it.kind {
	case itUser:
		n := len(wrapRich(r, it.text, proseSize, x1-x0-28))
		h := float64(n)*proseLH + 18
		p.roundRect(x0, y, x1, y+h, 10, r.tinted(r.pal.accent, 0.13))
		p.fillRect(x0, y+8, x0+3, y+h-8, r.pal.accent)
		ch.richText(p, it.text, x0+16, y+9, x1-x0-28, proseSize, proseLH, r.pal.text)
		return h

	case itNote:
		lines := strings.Split(it.text, "\n")
		if len(lines) > 12 {
			lines = append(lines[:11], fmt.Sprintf("… %d more lines", len(lines)-11))
		}
		m := r.mono()
		h := 4.0
		for _, l := range lines {
			h += 16
			pm := painter{r: m, img: img}
			pm.text(l, x0+12, y+h-4, 11.5, false, r.pal.gray, x1-x0-12)
		}
		p.fillRect(x0, y+2, x0+2, y+h+2, r.pal.divider)
		return h + 6

	case itText:
		return ch.drawProse(p, it.text, x0, x1, y)

	case itTool:
		return ch.drawTool(p, it, x0, x1, y)
	}
	return 0
}

// drawProse draws Claude's markdown: paragraphs, headings, lists, quotes and
// code blocks, with inline marks dropped.
func (ch *chatView) drawProse(p painter, md string, x0, x1, y float64) float64 {
	r := p.r
	start := y
	lines := strings.Split(md, "\n")
	para := []string{}
	flush := func() {
		if len(para) == 0 {
			return
		}
		y = ch.richText(p, strings.Join(para, " "), x0, y, x1-x0, proseSize, proseLH, r.pal.text) + 6
		para = para[:0]
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "```"):
			flush()
			var code []codeLine
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, codeLine{lnPlain, lines[i]})
			}
			y = ch.drawCode(p, code, x0, x1, y+2, 40) + 8
		case t == "":
			flush()
		case strings.HasPrefix(t, "#"):
			flush()
			h := strings.TrimSpace(strings.TrimLeft(t, "#"))
			for _, hl := range r.wrap(stripMarkdown(h), 15, true, x1-x0) {
				y += 22
				p.raw(hl, x0, y-5, 15, true, r.pal.text)
			}
			y += 6
		case strings.HasPrefix(t, "|"):
			flush()
			var rows []codeLine
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				row := strings.TrimSpace(lines[i])
				if strings.Trim(row, "|-: ") == "" {
					continue
				}
				rows = append(rows, codeLine{lnPlain, stripMarkdown(row)})
			}
			i--
			y = ch.drawCode(p, rows, x0, x1, y+2, 30) + 8
		case bulletRe.MatchString(l):
			flush()
			m := bulletRe.FindStringSubmatch(l)
			indent := float64(len(m[1])/2) * 16
			mark := "•"
			if m[2] != "-" && m[2] != "*" && m[2] != "+" {
				mark = m[2]
			}
			body := m[3]
			// Following indented lines continue the item.
			for i+1 < len(lines) && strings.HasPrefix(lines[i+1], strings.Repeat(" ", len(m[1])+2)) && !bulletRe.MatchString(lines[i+1]) && strings.TrimSpace(lines[i+1]) != "" {
				i++
				body += " " + strings.TrimSpace(lines[i])
			}
			mw := r.width(mark, proseSize, false)
			tx := x0 + indent + math.Max(14, mw+6)
			p.raw(mark, x0+indent, y+proseLH-5, proseSize, false, r.pal.gray)
			y = ch.richText(p, body, tx, y, x1-tx, proseSize, proseLH, r.pal.text) + 3
		case strings.HasPrefix(t, ">"):
			flush()
			q := strings.TrimSpace(strings.TrimPrefix(t, ">"))
			top := y
			y = ch.richText(p, q, x0+14, y, x1-x0-14, proseSize, proseLH, r.pal.gray)
			p.fillRect(x0, top+4, x0+3, y+1, r.pal.divider)
			y += 4
		default:
			para = append(para, t)
		}
	}
	flush()
	return y - start - 6
}

var bulletRe = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(.*)$`)

// drawCode draws a box of code lines (at most maxLines) and returns where
// it ends.
func (ch *chatView) drawCode(p painter, code []codeLine, x0, x1, y float64, maxLines int) float64 {
	if len(code) == 0 {
		return y
	}
	r := p.r
	m := r.mono()
	pm := painter{r: m, img: p.img}
	cols := max(10, int((x1-x0-2*codePad-14)/m.width("M", codeSize, false)))
	lines := wrapCode(code, cols)
	if len(lines) > maxLines {
		more := len(lines) - (maxLines - 1)
		lines = append(lines[:maxLines-1], codeLine{lnNote, fmt.Sprintf("… %d more line%s", more, plural(more))})
	}
	boxH := float64(len(lines))*codeLineH + 2*codePad - 4
	p.roundRect(x0, y, x1, y+boxH, 8, r.tinted(r.pal.text, 0.06))
	ly := y + codePad - 2
	for _, l := range lines {
		col, mark := r.pal.text, ""
		switch l.kind {
		case lnAdd:
			p.fillRect(x0, ly, x1, ly+codeLineH, r.tinted(r.green(), 0.14))
			col, mark = r.green(), "+"
		case lnDel:
			p.fillRect(x0, ly, x1, ly+codeLineH, r.tinted(r.red(), 0.14))
			col, mark = r.red(), "−"
		case lnNote:
			col = r.pal.faint
		}
		base := ly + codeLineH - 4.5
		if mark != "" {
			pm.raw(mark, x0+codePad-2, base, codeSize, true, col)
			col = mix(col, r.pal.text, 0.35)
			if r.clear {
				col = r.pal.text
			}
		}
		pm.raw(codeText(m, l.text), x0+codePad+12, base, codeSize, false, col)
		ly += codeLineH
	}
	return y + boxH
}

// drawTool draws a tool call as one line saying what it does, with the
// command, diff, or a few lines of output below.
func (ch *chatView) drawTool(p painter, it *chatItem, x0, x1, y float64) float64 {
	r := p.r
	start := y
	var in map[string]any
	json.Unmarshal(it.input, &in)
	str := func(k string) string { v, _ := in[k].(string); return v }

	dot := r.amber()
	if it.done {
		dot = r.green()
	}
	if it.isErr {
		dot = r.red()
	}
	title := describeTool(it.tool, it.input, ch.log.cwd)
	if it.done {
		title = pastTense(title)
	}
	if it.denied {
		title += " · denied"
		dot = r.pal.faint
	}
	y += 15
	p.circle(x0+5, y-4.5, 4, dot)
	p.text(title, x0+18, y, 13, true, r.pal.text, x1-x0-18)
	y += 6

	var code []codeLine
	plain := func(s string, n int, fromEnd bool) {
		ls := strings.Split(strings.TrimRight(ansiCodes.ReplaceAllString(s, ""), "\n"), "\n")
		if len(ls) > n {
			if fromEnd {
				ls = append([]string{fmt.Sprintf("… %d earlier lines", len(ls)-n)}, ls[len(ls)-n:]...)
			} else {
				ls = append(ls[:n], fmt.Sprintf("… %d more lines", len(ls)-n))
			}
		}
		for _, l := range ls {
			code = append(code, codeLine{lnPlain, l})
		}
	}
	maxLines := 10
	switch it.tool {
	case "Bash":
		plain("$ "+str("command"), 4, false)
		if it.done && strings.TrimSpace(it.result) != "" {
			code = append(code, codeLine{lnNote, ""})
			before := len(code)
			plain(it.result, 5, true)
			for i := before; i < len(code); i++ {
				code[i].kind = lnNote
			}
		}
	case "Edit":
		code = diffLines(str("old_string"), str("new_string"))
	case "MultiEdit":
		edits, _ := in["edits"].([]any)
		for i, e := range edits {
			m, _ := e.(map[string]any)
			o, _ := m["old_string"].(string)
			n, _ := m["new_string"].(string)
			if i > 0 {
				code = append(code, codeLine{lnNote, "⋯"})
			}
			code = append(code, diffLines(o, n)...)
		}
	case "Write":
		ls := strings.Split(strings.TrimRight(str("content"), "\n"), "\n")
		for i, l := range ls {
			if i == 6 {
				code = append(code, codeLine{lnNote, fmt.Sprintf("… %d lines in all", len(ls))})
				break
			}
			code = append(code, codeLine{lnAdd, l})
		}
	case "TodoWrite", "TaskCreate", "TaskUpdate":
		// The board shows plans; keep the chat to the line above.
	default:
		if it.isErr {
			plain(it.result, 3, false)
		}
	}
	if it.isErr && it.tool != "Bash" && len(code) > 0 && it.result != "" {
		code = append(code, codeLine{lnNote, oneLine(it.result, 200)})
	}
	if len(code) > 0 {
		y = ch.drawCode(p, code, x0+18, x1, y+2, maxLines)
	}
	return y - start + 2
}

// pastTense turns "Editing x" into "Edited x" once the call is back.
func pastTense(s string) string {
	for _, pair := range [][2]string{
		{"Editing ", "Edited "}, {"Writing ", "Wrote "}, {"Reading ", "Read "}, {"Running ", "Ran "},
		{"Searching ", "Searched "}, {"Loading tools", "Loaded tools"}, {"Using the ", "Used the "},
		{"Asking you", "Asked you"}, {"Presenting", "Presented"}, {"Running a subagent", "Ran a subagent"},
	} {
		if strings.HasPrefix(s, pair[0]) {
			return pair[1] + s[len(pair[0]):]
		}
	}
	return s
}

// columnOf is the reading column in a pane w points wide: the whole width,
// up to a comfortable measure, centered. A phone-narrow pane gets slimmer
// margins.
func columnOf(w float64) (x0, x1 float64) {
	const widest = 820.0
	pad := chatPad
	if narrow(w) {
		pad = 10
	}
	if w-2*pad <= widest {
		return pad, w - pad
	}
	x0 = math.Round((w - widest) / 2)
	return x0, x0 + widest
}

// narrow is a pane about as wide as a phone.
func narrow(w float64) bool { return w < 520 }

const (
	menuRowH  = 34.0
	menuShown = 6
	sendSize  = 28.0
)

type chatButton struct {
	action         string
	x0, y0, x1, y1 float64
}

// hit is the action under a click at (x, y) in the composer, if any.
func (ch *chatView) hit(x, y float64) string {
	for _, b := range ch.buttons {
		if x >= b.x0 && x <= b.x1 && y >= b.y0 && y <= b.y1 {
			return b.action
		}
	}
	return ""
}

// inputW is how wide the composer's text runs.
func (ch *chatView) inputW(w float64) float64 {
	c0, c1 := columnOf(w)
	return c1 - c0 - 28 - sendSize - 6
}

// menuItems is the slash menu for what's typed: a "/" and a name, no space
// yet.
func (ch *chatView) menuItems() []slashCommand {
	text := ch.in.String()
	if text != ch.menuFor {
		ch.menuFor, ch.menuSel, ch.menuOff = text, 0, false
	}
	if ch.menuOff || !strings.HasPrefix(text, "/") || strings.ContainsAny(text, " \n") {
		return nil
	}
	return matchCommands(ch.cmds, text)
}

func (ch *chatView) composerHeight(lines []inLine) float64 {
	h := 8 + 26.0 // the status row
	if n := len(ch.menuItems()); n > 0 {
		h += float64(min(n, menuShown))*menuRowH + 12
	}
	h += 2*11 + float64(min(len(lines), maxInputLn))*inputLH // the box
	return h + 24                                            // the hint
}

func (ch *chatView) drawComposer(r *renderer, img *image.RGBA, w, h float64, lines []inLine, status string, kind statusKind) {
	x0, x1 := columnOf(w)
	small := narrow(w)
	ch.buttons = ch.buttons[:0]
	y := 4.0

	// The slash menu, above everything else.
	if menu := ch.menuItems(); len(menu) > 0 {
		n := min(len(menu), menuShown)
		first := max(0, min(ch.menuSel-n/2, len(menu)-n))
		mh := float64(n)*menuRowH + 8
		r.fillRoundRect(img, x0, y, x1, y+mh, 10, r.tinted(r.pal.text, 0.07))
		m := r.mono()
		for i := first; i < first+n; i++ {
			c := menu[i]
			ry := y + 4 + float64(i-first)*menuRowH
			if i == ch.menuSel {
				r.fillRoundRect(img, x0+4, ry, x1-4, ry+menuRowH, 7, r.tinted(r.pal.accent, 0.2))
			}
			nameW := m.width(codeText(m, c.name), 12.5, true)
			m.drawRaw(img, codeText(m, c.name), x0+14, ry+21.5, 12.5, true, r.pal.text)
			desc := c.desc
			if c.terminal {
				desc = strings.TrimSpace(desc + " · opens the terminal")
			}
			if !small || nameW < (x1-x0)*0.45 {
				dx := x0 + 14 + math.Max(nameW+14, math.Min(200, (x1-x0)*0.35))
				r.text(img, desc, dx, ry+21.5, 12, false, r.pal.gray, x1-dx-12)
			}
			ch.buttons = append(ch.buttons, chatButton{fmt.Sprintf("menu:%d", i), x0, ry, x1, ry + menuRowH})
		}
		y += mh + 4
	}

	// The status row, with buttons for what keys would do.
	sy := y + 18
	bx := x1
	pill := func(label, action string, primary bool) {
		lw := r.width(label, 11.5, true)
		bw := lw + 22
		bx -= bw
		bg, fg := r.tinted(r.pal.text, 0.09), r.pal.text
		if primary {
			bg, fg = r.tinted(r.red(), 0.16), r.red()
		}
		r.fillRoundRect(img, bx, sy-15, bx+bw, sy+7, 11, bg)
		r.drawRaw(img, label, bx+11, sy, 11.5, true, fg)
		ch.buttons = append(ch.buttons, chatButton{action, bx, sy - 15, bx + bw, sy + 7})
		bx -= 6
	}
	pill("Terminal", "terminal", false)
	if kind == skBusy {
		pill("Stop", "stop", true)
	}
	textW := bx - x0 - 8
	switch {
	case ch.flash != "":
		r.text(img, ch.flash, x0, sy, 11.5, false, r.amber(), textW)
	case kind == skBusy:
		// Three dots that take turns.
		for i := 0; i < 3; i++ {
			a := 0.3
			if (ch.spin/2)%3 == i {
				a = 1
			}
			r.fillCircle(img, x0+4+float64(i)*9, sy-4, 3, r.tinted(r.pal.accent, a))
		}
		if small {
			status = strings.TrimSuffix(status, " · Esc interrupts")
		}
		r.text(img, status, x0+32, sy, 11.5, true, r.pal.gray, textW-32)
	case kind == skWarn:
		r.fillCircle(img, x0+4, sy-4, 4, r.amber())
		lines := r.wrap(status, 11.5, true, textW-14)
		if len(lines) > 1 && strings.Contains(status, "Ctrl+Y") {
			// The trust question gets a button of its own.
			pill("Trust", "trust", false)
			status = "Trust this new folder?"
			textW = bx - x0 - 8
		}
		r.text(img, status, x0+14, sy, 11.5, true, r.amber(), textW-14)
	default:
		r.text(img, "Claude Code · "+ch.project, x0, sy, 11.5, true, r.pal.faint, textW)
	}

	// The box, showing the lines around the cursor.
	by0 := y + 26 + 8
	shown := min(len(lines), maxInputLn)
	by1 := by0 + 2*11 + float64(shown)*inputLH
	r.fillRoundRect(img, x0, by0, x1, by1, 12, r.tinted(r.pal.text, 0.07))
	r.fillRoundRect(img, x0+0.5, by0+0.5, x0+3, by1-0.5, 2, r.tinted(r.pal.accent, 0.8))
	tx := x0 + 14
	cl, cx := ch.in.caret(r, lines, inputSize)
	top := max(0, min(cl-shown+1, len(lines)-shown))
	if cl < top {
		top = cl
	}
	ly := by0 + 11
	if ch.in.empty() {
		ph := "Message Claude…  / for commands"
		if small {
			ph = "Message Claude…"
		}
		r.text(img, ph, tx, ly+inputLH-5, inputSize, false, r.pal.faint, ch.inputW(w))
	}
	for i := top; i < top+shown; i++ {
		l := lines[i]
		r.drawRaw(img, string(ch.in.text[l.start:l.end]), tx, ly+inputLH-5, inputSize, false, r.pal.text)
		if i == cl {
			r.fillRect(img, tx+cx, ly+3, tx+cx+1.5, ly+inputLH-1, r.pal.accent)
		}
		ly += inputLH
	}
	// Send, in the box's corner.
	scx, scy := x1-8-sendSize/2, by1-8-sendSize/2
	on := strings.TrimSpace(ch.in.String()) != ""
	bg := r.tinted(r.pal.text, 0.12)
	if on {
		bg = r.pal.accent
	}
	r.fillCircle(img, scx, scy, sendSize/2, bg)
	fg := rgb(255, 255, 255)
	if luminance(r.pal.accent) > 0.3 && on {
		fg = rgb(20, 20, 20)
	}
	r.arrowUp(img, scx, scy, 11, fg)
	ch.buttons = append(ch.buttons, chatButton{"send", scx - sendSize/2 - 4, scy - sendSize/2 - 4, scx + sendSize/2 + 4, scy + sendSize/2 + 4})

	hint := "Enter sends · Shift+Enter new line · / commands · Ctrl+O terminal · Ctrl+D close"
	switch {
	case ch.scroll > 1:
		hint = "Scrolled up · End or Page Down to come back"
	case len(ch.menuItems()) > 0:
		hint = "↑↓ choose · Tab completes · Enter runs · Esc closes"
	case small:
		hint = ""
	}
	r.text(img, hint, x0, by1+16, 10.5, false, r.pal.faint, x1-x0)
}

// arrowUp draws the send arrow centered at (cx, cy).
func (r *renderer) arrowUp(img *image.RGBA, cx, cy, size float64, col color.RGBA) {
	t := size * 0.16
	p := r.path(img)
	// The shaft.
	p.poly([][2]float64{{cx - t/2, cy - size*0.3}, {cx + t/2, cy - size*0.3}, {cx + t/2, cy + size/2}, {cx - t/2, cy + size/2}})
	// The head.
	p.poly([][2]float64{{cx, cy - size/2}, {cx + size*0.42, cy - size*0.06}, {cx + size*0.42 - t*1.1, cy - size*0.06 + t*1.1}, {cx, cy - size/2 + t*1.6}, {cx - size*0.42 + t*1.1, cy - size*0.06 + t*1.1}, {cx - size*0.42, cy - size*0.06}})
	p.fill(col)
}

// still draws the whole pane into img, for `agentpad render chat`.
func (ch *chatView) still(r *renderer, img *image.RGBA) {
	s := ch.s
	w := s.widthPt()
	lines := ch.in.layout(r, inputSize, ch.inputW(w))
	compRows := min(s.ws.rows-2, int(math.Ceil(ch.composerHeight(lines)/s.cellPt())))
	ch.logRows = s.ws.rows - compRows
	ch.logH = float64(ch.logRows) * s.cellPt()
	logImg := s.canvas(ch.logH)
	ch.drawLog(r, logImg, w, ch.layout(r, w))
	comp := s.canvas(float64(compRows) * s.cellPt())
	ch.drawComposer(r, comp, w, float64(compRows)*s.cellPt(), lines, "", skIdle)
	overImage(img, logImg, 0)
	overImage(img, comp, logImg.Bounds().Dy())
}

// snapshot writes the pane as it looks to ~/.local/state/agentpad/snapshot-PID.png.
func (ch *chatView) snapshot() {
	s := ch.s
	logImg, comp := s.last["log"], s.last["composer"]
	if logImg == nil || comp == nil {
		return
	}
	out := image.NewRGBA(image.Rect(0, 0, s.ws.xpix, s.ws.ypix))
	bg := s.pal.bg
	if s.clear {
		bg = s.colors.bg
	}
	fillImage(out, bg)
	overImage(out, logImg, 0)
	cell := float64(s.ws.ypix) / float64(s.ws.rows)
	overImage(out, comp, int(float64(ch.logRows)*cell))
	f, err := os.Create(filepath.Join(stateDir(), fmt.Sprintf("snapshot-%d.png", os.Getpid())))
	if err != nil {
		return
	}
	defer f.Close()
	png.Encode(f, out)
}

// painter draws through a renderer, or only measures when img is nil.
type painter struct {
	r   *renderer
	img *image.RGBA
}

func (p painter) raw(s string, x, y, size float64, bold bool, col color.RGBA) {
	if p.img != nil {
		p.r.drawRaw(p.img, s, x, y, size, bold, col)
	}
}

func (p painter) text(s string, x, y, size float64, bold bool, col color.RGBA, maxW float64) {
	if p.img != nil {
		p.r.text(p.img, s, x, y, size, bold, col, maxW)
	}
}

func (p painter) roundRect(x0, y0, x1, y1, rad float64, col color.RGBA) {
	if p.img != nil {
		p.r.fillRoundRect(p.img, x0, y0, x1, y1, rad, col)
	}
}

func (p painter) fillRect(x0, y0, x1, y1 float64, col color.RGBA) {
	if p.img != nil {
		p.r.fillRect(p.img, x0, y0, x1, y1, col)
	}
}

func (p painter) circle(cx, cy, rad float64, col color.RGBA) {
	if p.img != nil {
		p.r.fillCircle(p.img, cx, cy, rad, col)
	}
}

func fillImage(img *image.RGBA, c color.RGBA) {
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
}

func overImage(dst, src *image.RGBA, y int) {
	draw.Draw(dst, src.Bounds().Add(image.Pt(0, y)), src, image.Point{}, draw.Over)
}

// --- inline marks -------------------------------------------------------

// span is a run of text in one style, placed on a line.
type span struct {
	text       string
	code, bold bool
	x, w       float64
}

// richWords splits markdown into words that carry their style: `code` and
// **bold**. Other marks are dropped.
func richWords(md string) []span {
	var out []span
	code, bold := false, false
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, span{text: cur.String(), code: code, bold: bold})
			cur.Reset()
		}
	}
	rs := []rune(md)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '`':
			flush()
			code = !code
		case !code && c == '*' && i+1 < len(rs) && rs[i+1] == '*':
			flush()
			bold = !bold
			i++
		case unicode.IsSpace(c) && !code:
			flush()
			out = append(out, span{text: " "})
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

// wrapRich lays out words in lines that fit maxW, joining a word to the
// one before it when no space came between them.
func wrapRich(r *renderer, md string, size, maxW float64) [][]span {
	words := richWords(md)
	m := r.mono()
	measure := func(s span) float64 {
		if s.code {
			return m.width(codeText(m, s.text), size-1, false) + 6
		}
		return r.width(r.clean(s.text, s.bold), size, s.bold)
	}
	space := r.width(" ", size, false)
	var lines [][]span
	var line []span
	x := 0.0
	gap := false
	for _, w := range words {
		if w.text == " " {
			gap = len(line) > 0
			continue
		}
		w.w = measure(w)
		lead := 0.0
		if gap {
			lead = space
		}
		if len(line) > 0 && x+lead+w.w > maxW {
			lines = append(lines, line)
			line, x, lead = nil, 0, 0
		}
		w.x = x + lead
		x = w.x + w.w
		line = append(line, w)
		gap = false
	}
	if len(line) > 0 {
		lines = append(lines, line)
	}
	return lines
}

// richText draws md wrapped from (x, y), a line every lh, and returns the y
// after it.
func (ch *chatView) richText(p painter, md string, x, y, maxW, size, lh float64, col color.RGBA) float64 {
	r := p.r
	m := r.mono()
	pm := painter{r: m, img: p.img}
	for _, line := range wrapRich(r, md, size, maxW) {
		y += lh
		for _, s := range line {
			switch {
			case s.code:
				p.roundRect(x+s.x, y-lh+3.5, x+s.x+s.w, y-1.5, 4, r.tinted(r.pal.text, 0.09))
				pm.raw(codeText(m, s.text), x+s.x+3, y-5, size-1, false, col)
			default:
				p.raw(r.clean(s.text, s.bold), x+s.x, y-5, size, s.bold, col)
			}
		}
	}
	return y
}
