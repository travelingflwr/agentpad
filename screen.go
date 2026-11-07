package main

// The full-screen kitty viewer both panes share: raw mode, the theme's
// colors, and images placed on the cell grid.

import (
	"bufio"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	enterScreen = "\x1b[?1049h\x1b[?25l\x1b[H\x1b[2J\x1b[?2031h\x1b[?1000h\x1b[?1006h\x1b[?1016h\x1b[>1u"
	leaveScreen = "\x1b_Ga=d,d=A,q=2\x1b\\\x1b[<u\x1b[?1016l\x1b[?1006l\x1b[?1000l\x1b[?2031l\x1b[?25h\x1b[?1049l"
)

type screen struct {
	w      *bufio.Writer
	ws     winSize
	scale  float64
	theme  string
	colors termColors
	pal    palette
	clear  bool
	fonts  *fontSet
	mono   *fontSet
	events chan termEvent
	winch  chan os.Signal
	quit   chan os.Signal

	nextID  int
	placed  map[string]int // what's on screen: image ID by name
	keys    map[string]string
	sizes   map[string]image.Point
	rowOf   map[string]int
	last    map[string]*image.RGBA // the latest of each, for snapshots
	snapSig chan os.Signal
}

func openScreen(theme string) (*screen, func(), error) {
	if theme == "" {
		theme = os.Getenv("AGENTPAD_THEME")
	}
	if theme == "" {
		theme = "terminal"
	}
	fonts, err := loadFonts()
	if err != nil {
		return nil, nil, err
	}
	mono, err := loadMono()
	if err != nil {
		return nil, nil, err
	}
	in := int(os.Stdin.Fd())
	var saved *term.State
	if term.IsTerminal(in) {
		saved, _ = term.MakeRaw(in)
	}
	s := &screen{
		w: bufio.NewWriterSize(os.Stdout, 1<<16), theme: theme, fonts: fonts, mono: mono,
		events: make(chan termEvent, 256), winch: make(chan os.Signal, 1), quit: make(chan os.Signal, 1),
		nextID: 1, placed: map[string]int{}, keys: map[string]string{}, sizes: map[string]image.Point{}, last: map[string]*image.RGBA{}, rowOf: map[string]int{}, snapSig: make(chan os.Signal, 1),
	}
	go (&ttyReader{}).run(s.events)
	signal.Notify(s.winch, syscall.SIGWINCH)
	signal.Notify(s.quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	signal.Notify(s.snapSig, syscall.SIGUSR1)
	fmt.Fprint(s.w, enterScreen)
	s.w.Flush()
	done := func() {
		fmt.Fprint(s.w, leaveScreen)
		s.w.Flush()
		if saved != nil {
			term.Restore(in, saved)
		}
	}

	// Wait briefly for the theme so the first frame has the right colors.
	queryColors(s.w)
	s.w.Flush()
	if theme == "terminal" {
		// A pane that's just opened can miss the first query: ask again.
		deadline := time.After(3 * time.Second)
		retry := time.NewTicker(200 * time.Millisecond)
	wait:
		for !s.colors.complete() {
			select {
			case ev, open := <-s.events:
				if !open {
					break wait
				}
				s.colors.apply(ev.osc)
			case <-retry.C:
				queryColors(s.w)
				s.w.Flush()
			case <-deadline:
				break wait
			}
		}
		retry.Stop()
	}
	s.resize()
	return s, done, nil
}

func (tc termColors) complete() bool { return tc.hasFg && tc.hasBg && tc.hasBlue }

// recheck asks for the theme again while it's still unknown; the replies
// arrive as events and redraw.
func (s *screen) recheck() {
	if s.theme == "terminal" && !s.colors.hasBg {
		queryColors(s.w)
		s.w.Flush()
	}
}

func (s *screen) resize() {
	s.ws = getWinSize(int(os.Stdout.Fd()))
	s.scale = pickScale(0, s.ws)
	s.pal = s.colors.palette(s.theme)
	s.clear = s.theme == "terminal"
	s.keys = map[string]string{} // everything redraws
}

// event handles what every viewer handles the same way (theme replies and
// changes) and reports whether the caller should redraw.
func (s *screen) event(ev termEvent) bool {
	if ev.osc != "" {
		before := s.pal
		s.colors.apply(ev.osc)
		s.pal = s.colors.palette(s.theme)
		if s.pal != before {
			s.keys = map[string]string{}
			return true
		}
		return false
	}
	if ev.scheme {
		queryColors(s.w)
		s.w.Flush()
	}
	return false
}

func (s *screen) renderer() *renderer {
	pal := s.pal
	if s.clear {
		pal = pal.seeThrough()
	}
	return &renderer{fonts: s.fonts, monoFonts: s.mono, scale: s.scale, pal: pal, clear: s.clear, now: time.Now()}
}

// Sizes in points.
func (s *screen) widthPt() float64  { return float64(s.ws.xpix) / s.scale }
func (s *screen) heightPt() float64 { return float64(s.ws.ypix) / s.scale }
func (s *screen) cellPt() float64   { return float64(s.ws.ypix) / float64(s.ws.rows) / s.scale }

// snap rounds pt up to whole cells, in points.
func (s *screen) snap(pt float64) float64 {
	c := s.cellPt()
	return math.Max(1, math.Ceil(pt/c-0.01)) * c
}

// cellRow is the 1-based cell row a point offset falls in.
func (s *screen) cellRow(pt float64) int { return int(math.Round(pt/s.cellPt())) + 1 }

func (s *screen) canvas(heightPt float64) *image.RGBA {
	return image.NewRGBA(image.Rect(0, 0, s.ws.xpix, int(math.Round(heightPt*s.scale))))
}

// show puts img on screen as name, top-left at cell row, spanning rows
// cells (the whole width), at z-index z. key says what it shows; showing the
// same key again only moves it.
func (s *screen) show(name, key string, row, rows, z int, draw func() *image.RGBA) {
	s.rowOf[name] = row
	id, ok := s.placed[name]
	if !ok || s.keys[name] != key {
		img := draw()
		s.last[name] = img
		if ok {
			kittyDelete(s.w, id)
		}
		id = s.nextID
		s.nextID++
		kittyUpload(s.w, img, id)
		s.placed[name], s.keys[name], s.sizes[name] = id, key, img.Bounds().Size()
		kittyPlace(s.w, id, row, 1, 0, img.Bounds().Dx(), img.Bounds().Dy(), s.ws.cols, rows, z)
		return
	}
	b := s.sizes[name]
	kittyPlace(s.w, id, row, 1, 0, b.X, b.Y, s.ws.cols, rows, z)
}

func (s *screen) hide(name string) {
	if id, ok := s.placed[name]; ok {
		kittyDelete(s.w, id)
		delete(s.placed, name)
		delete(s.keys, name)
	}
}

// background paints the pane when a fixed theme is chosen; with the
// terminal's theme the pane's own background shows through.
func (s *screen) background() {
	if s.clear {
		s.hide("bg")
		return
	}
	s.show("bg", fmt.Sprint(s.pal.bg, s.ws), 1, s.ws.rows, -3, func() *image.RGBA {
		img := s.canvas(s.heightPt())
		r := s.renderer()
		r.clear = false
		r.paint(img)
		return img
	})
}

func (s *screen) begin() { fmt.Fprint(s.w, "\x1b[?2026h") }
func (s *screen) end() {
	fmt.Fprint(s.w, "\x1b[?2026l")
	s.w.Flush()
}

// clickPt is where a click landed, in points from the pane's top-left.
func (s *screen) clickPt(c *mouseClick) (x, y float64) {
	// Mode 1016 reports pixels, 1-based.
	return float64(c.x-1) / s.scale, float64(c.y-1) / s.scale
}

// saveSnapshot writes what's on screen (the selection under the main image)
// to ~/.local/state/agentpad/snapshot-PID.png: kill -USR1 a viewer to check
// what it shows.
func (s *screen) saveSnapshot(main string, under ...string) {
	m, ok := s.last[main]
	if !ok {
		return
	}
	out := image.NewRGBA(m.Bounds())
	if !s.clear {
		draw.Draw(out, out.Bounds(), image.NewUniform(s.pal.bg), image.Point{}, draw.Src)
	} else {
		draw.Draw(out, out.Bounds(), image.NewUniform(s.colors.bg), image.Point{}, draw.Src)
	}
	cell := float64(s.ws.ypix) / float64(s.ws.rows)
	for _, name := range under {
		if img, ok := s.last[name]; ok {
			y := int(float64(s.rowOf[name]-1) * cell)
			draw.Draw(out, img.Bounds().Add(image.Pt(0, y)), img, image.Point{}, draw.Over)
		}
	}
	draw.Draw(out, out.Bounds(), m, image.Point{}, draw.Over)
	f, err := os.Create(filepath.Join(stateDir(), fmt.Sprintf("snapshot-%d.png", os.Getpid())))
	if err != nil {
		return
	}
	defer f.Close()
	png.Encode(f, out)
}
