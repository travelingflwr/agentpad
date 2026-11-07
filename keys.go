package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Keys arrive in two encodings. Legacy terminals send bytes: control
// characters, UTF-8 text, and CSI sequences for arrows and such, with
// modifiers only on some keys. With the kitty keyboard protocol's
// "disambiguate" flag (CSI > 1 u), which the viewer turns on, every key
// with Cmd, Ctrl, or Opt held comes as CSI code ; mods u, so Cmd-A and
// Opt-Backspace can be told apart from plain keys. The decoder reads both.

// Special keys use negative codes; characters are their rune.
const (
	keyUp rune = -(iota + 1)
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyPageUp
	keyPageDown
	keyDelete // forward delete (Fn-Backspace)
	keyInsert
)

const (
	keyBackspace rune = 0x7f
	keyEnter     rune = '\r'
	keyEscape    rune = 0x1b
	keyTab       rune = '\t'
)

// Modifier bits, as the kitty protocol numbers them (its value minus one).
const (
	modShift = 1
	modAlt   = 2 // Option
	modCtrl  = 4
	modSuper = 8 // Command
)

type keyPress struct {
	code rune
	mods int
}

func (k keyPress) is(code rune, mods int) bool { return k.code == code && k.mods == mods }

// termEvent is one thing read from the terminal: a key press, pasted text,
// an OSC reply, a color-scheme change report (CSI ? 997 ; n n, mode 2031),
// or a mouse wheel tick.
type termEvent struct {
	key    keyPress // code 0 when it isn't a key
	paste  string
	osc    string
	scheme bool
	scroll scrollKind
	click  *mouseClick // a left-button press
}

// mouseClick is where a left-button press landed, 1-based, in pixels when
// the terminal reports SGR pixel positions (mode 1016) and in cells if not.
type mouseClick struct{ x, y int }

// csiClick reads a left-button press from an SGR mouse report (without the
// ESC [). Releases, drags and other buttons are ignored.
func csiClick(seq string) (mouseClick, bool) {
	if !strings.HasPrefix(seq, "<") || !strings.HasSuffix(seq, "M") {
		return mouseClick{}, false
	}
	parts := strings.Split(seq[1:len(seq)-1], ";")
	if len(parts) != 3 {
		return mouseClick{}, false
	}
	btn, err1 := strconv.Atoi(parts[0])
	x, err2 := strconv.Atoi(parts[1])
	y, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || btn&^(4|8|16) != 0 {
		return mouseClick{}, false
	}
	return mouseClick{x, y}, true
}

type scrollKind int

const (
	scrollNone scrollKind = iota
	wheelUp
	wheelDown
	lineUp
	lineDown
	pageUp
	pageDown
	scrollTop
	scrollBottom
)

// csiScroll maps an SGR mouse report (mode 1006, without the ESC [) to a
// wheel tick.
func csiScroll(seq string) scrollKind {
	if !strings.HasPrefix(seq, "<") || !strings.HasSuffix(seq, "M") {
		return scrollNone
	}
	btn, err := strconv.Atoi(strings.SplitN(seq[1:], ";", 2)[0])
	if err != nil {
		return scrollNone
	}
	switch btn &^ (4 | 8 | 16) { // ignore Shift, Alt, Ctrl
	case 64:
		return wheelUp
	case 65:
		return wheelDown
	}
	return scrollNone
}

const pasteEnd = "\x1b[201~"

type keyDecoder struct {
	state   int
	seq     []byte
	utf     []byte // a UTF-8 character still arriving
	inPaste bool
	paste   []byte
}

const (
	stNormal = iota
	stEsc
	stCSI
	stSS3
	stOSC
	stOSCEsc
)

// feed decodes buf, calling emit for each event. A sequence split across
// calls carries over to the next one.
func (d *keyDecoder) feed(buf []byte, emit func(termEvent)) {
	for _, c := range buf {
		if d.inPaste {
			d.paste = append(d.paste, c)
			if strings.HasSuffix(string(d.paste), pasteEnd) {
				emit(termEvent{paste: string(d.paste[:len(d.paste)-len(pasteEnd)])})
				d.inPaste, d.paste = false, nil
			}
			continue
		}
		switch d.state {
		case stNormal:
			if c == 0x1b {
				d.state = stEsc
				continue
			}
			if c >= 0x80 || len(d.utf) > 0 {
				d.utf = append(d.utf, c)
				if utf8.FullRune(d.utf) {
					r, _ := utf8.DecodeRune(d.utf)
					d.utf = d.utf[:0]
					if r != utf8.RuneError {
						emit(termEvent{key: keyPress{code: r}})
					}
				}
				continue
			}
			if k, ok := byteKey(c); ok {
				emit(termEvent{key: k})
			}
		case stEsc:
			d.seq = d.seq[:0]
			switch c {
			case '[':
				d.state = stCSI
			case ']':
				d.state = stOSC
			case 'O':
				d.state = stSS3
			case 0x1b:
				emit(termEvent{key: keyPress{code: keyEscape}}) // and maybe another sequence
			default:
				// ESC then a key is that key with Option, in legacy terms.
				if k, ok := byteKey(c); ok {
					k.mods |= modAlt
					emit(termEvent{key: k})
				}
				d.state = stNormal
			}
		case stCSI:
			d.seq = append(d.seq, c)
			if c >= 0x40 && c <= 0x7e {
				d.csi(string(d.seq), emit)
				d.state = stNormal
			}
		case stSS3:
			if k, ok := arrowKey(c); ok {
				emit(termEvent{key: keyPress{code: k}})
			}
			d.state = stNormal
		case stOSC:
			switch c {
			case 0x07:
				emit(termEvent{osc: string(d.seq)})
				d.state = stNormal
			case 0x1b:
				d.state = stOSCEsc
			default:
				d.seq = append(d.seq, c)
			}
		case stOSCEsc:
			if c == '\\' {
				emit(termEvent{osc: string(d.seq)})
				d.state = stNormal
			} else {
				d.seq = append(d.seq, 0x1b, c)
				d.state = stOSC
			}
		}
	}
}

// flush ends a read: an ESC left at the very end is the Escape key, since
// terminals send each escape sequence in one write.
func (d *keyDecoder) flush(emit func(termEvent)) {
	if d.state == stEsc && !d.inPaste {
		emit(termEvent{key: keyPress{code: keyEscape}})
		d.state = stNormal
	}
}

// byteKey is the key for one legacy byte: a control character or ASCII.
func byteKey(c byte) (keyPress, bool) {
	switch {
	case c == '\r' || c == '\n':
		return keyPress{code: keyEnter}, true
	case c == '\t':
		return keyPress{code: keyTab}, true
	case c == 0x7f || c == 0x08:
		return keyPress{code: keyBackspace}, true
	case c >= 1 && c <= 26:
		return keyPress{code: rune('a' + c - 1), mods: modCtrl}, true
	case c >= 0x20 && c < 0x7f:
		return keyPress{code: rune(c)}, true
	}
	return keyPress{}, false
}

func arrowKey(c byte) (rune, bool) {
	switch c {
	case 'A':
		return keyUp, true
	case 'B':
		return keyDown, true
	case 'C':
		return keyRight, true
	case 'D':
		return keyLeft, true
	case 'H':
		return keyHome, true
	case 'F':
		return keyEnd, true
	}
	return 0, false
}

// csi handles a finished CSI sequence (without the ESC [).
func (d *keyDecoder) csi(seq string, emit func(termEvent)) {
	final, params := seq[len(seq)-1], seq[:len(seq)-1]
	switch {
	case strings.HasPrefix(params, "?997;"):
		emit(termEvent{scheme: true})
		return
	case strings.HasPrefix(params, "<"):
		if k := csiScroll(seq); k != scrollNone {
			emit(termEvent{scroll: k})
		} else if c, ok := csiClick(seq); ok {
			emit(termEvent{click: &c})
		}
		return
	case strings.HasPrefix(params, "?") || strings.HasPrefix(params, ">"):
		return // replies, such as the keyboard protocol's flags
	}
	// code[:alternates] ; mods[:event] ; text
	fields := strings.Split(params, ";")
	num := func(i, def int) int {
		if i >= len(fields) {
			return def
		}
		v, err := strconv.Atoi(strings.SplitN(fields[i], ":", 2)[0])
		if err != nil {
			return def
		}
		return v
	}
	mods := 0
	if m := num(1, 1); m > 1 {
		mods = (m - 1) & (modShift | modAlt | modCtrl | modSuper)
	}
	if len(fields) > 1 {
		if ev := strings.SplitN(fields[1], ":", 2); len(ev) == 2 && ev[1] == "3" {
			return // a key release
		}
	}
	key := func(code rune) { emit(termEvent{key: keyPress{code: code, mods: mods}}) }
	switch final {
	case 'u':
		switch code := num(0, 0); {
		case code == 13:
			key(keyEnter)
		case code == 27:
			key(keyEscape)
		case code == 127 || code == 8:
			key(keyBackspace)
		case code == 9:
			key(keyTab)
		case code >= 0x20 && code < 57344: // above that are keypad and modifier keys
			key(rune(code))
		}
	case '~':
		switch num(0, 0) {
		case 200:
			d.inPaste, d.paste = true, nil
		case 2:
			key(keyInsert)
		case 3:
			key(keyDelete)
		case 5:
			key(keyPageUp)
		case 6:
			key(keyPageDown)
		case 1, 7:
			key(keyHome)
		case 4, 8:
			key(keyEnd)
		}
	case 'Z': // Shift-Tab, in legacy terms
		emit(termEvent{key: keyPress{code: keyTab, mods: mods | modShift}})
	default:
		if k, ok := arrowKey(final); ok {
			key(k)
		}
	}
}
