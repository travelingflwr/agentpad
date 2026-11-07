package main

// Terminal plumbing: pixel sizes, the kitty graphics protocol, theme color
// queries, and a stdin reader that can step aside while another program
// (an editor) uses the terminal. The kitty and color code is rexpad's.

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type winSize struct{ rows, cols, xpix, ypix int }

func getWinSize(fd int) winSize {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return winSize{rows: 24, cols: 80, xpix: 80 * 16, ypix: 24 * 34}
	}
	w := winSize{rows: int(ws.Row), cols: int(ws.Col), xpix: int(ws.Xpixel), ypix: int(ws.Ypixel)}
	if w.xpix == 0 || w.ypix == 0 {
		w.xpix, w.ypix = w.cols*16, w.rows*34 // assume a Retina cell
	}
	return w
}

// pickScale sizes the drawing to the terminal's own text: 34 px cells (a
// 13pt font on a Retina Mac) mean 2x.
func pickScale(flag float64, ws winSize) float64 {
	if flag > 0 {
		return flag
	}
	if v, err := strconv.ParseFloat(os.Getenv("AGENTPAD_SCALE"), 64); err == nil && v > 0 {
		return v
	}
	return max(1, math.Round(float64(ws.ypix)/float64(ws.rows)/17*2)/2)
}

// encoded is an image as the kitty protocol's raw format takes it: RGBA
// with straight alpha, zlib-compressed (f=32, o=z).
type encoded struct {
	data []byte
	w, h int
}

func encodeImage(img *image.RGBA) encoded {
	b := img.Bounds()
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	row := make([]byte, b.Dx()*4)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		src := img.Pix[img.PixOffset(b.Min.X, y):][:len(row)]
		for i := 0; i < len(row); i += 4 {
			switch a := src[i+3]; a {
			case 0, 255:
				copy(row[i:i+4], src[i:i+4])
			default: // image.RGBA is premultiplied; the protocol wants straight
				for c := 0; c < 3; c++ {
					row[i+c] = uint8(min(255, (int(src[i+c])*255+int(a)/2)/int(a)))
				}
				row[i+3] = a
			}
		}
		zw.Write(row)
	}
	zw.Close()
	return encoded{buf.Bytes(), b.Dx(), b.Dy()}
}

// kittyUpload transmits img as image id without placing it.
func kittyUpload(w *bufio.Writer, img *image.RGBA, id int) {
	e := encodeImage(img)
	data := base64.StdEncoding.EncodeToString(e.data)
	const chunk = 4096
	for i := 0; i < len(data) || i == 0; i += chunk {
		end := min(i+chunk, len(data))
		more := 0
		if end < len(data) {
			more = 1
		}
		if i == 0 {
			fmt.Fprintf(w, "\x1b_Ga=t,f=32,o=z,s=%d,v=%d,i=%d,q=2,m=%d;%s\x1b\\", e.w, e.h, id, more, data[i:end])
		} else {
			fmt.Fprintf(w, "\x1b_Gm=%d;%s\x1b\\", more, data[i:end])
		}
		if end >= len(data) {
			break
		}
	}
}

// kittyPlace shows the width x height pixel window of image id starting at
// pixel row y, with its top-left at cell (row, col), pinned to cols x rows
// cells, at z-index z. Placing again with the same placement ID moves it,
// which is how the viewer scrolls without re-sending pixels. The cursor is
// saved and put back, and C=1 keeps the screen from scrolling.
func kittyPlace(w *bufio.Writer, id, row, col, y, width, height, cols, rows, z int) {
	fmt.Fprintf(w, "\x1b7\x1b[%d;%dH\x1b_Ga=p,i=%d,p=1,x=0,y=%d,w=%d,h=%d,c=%d,r=%d,z=%d,C=1,q=2\x1b\\\x1b8",
		row, col, id, y, width, height, cols, rows, z)
}

func kittyDelete(w *bufio.Writer, id int) {
	fmt.Fprintf(w, "\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)
}

// --- theme colors -------------------------------------------------------

type termColors struct {
	fg, bg, blue          color.RGBA
	hasFg, hasBg, hasBlue bool
}

// queryColors asks for the foreground, background, and palette blue.
func queryColors(w *bufio.Writer) {
	fmt.Fprint(w, "\x1b]10;?\x07\x1b]11;?\x07\x1b]4;4;?\x07")
}

func (tc *termColors) apply(reply string) bool {
	i := strings.Index(reply, ";rgb:")
	if i < 0 {
		return false
	}
	c, ok := parseRGB(reply[i+5:])
	if !ok {
		return false
	}
	switch reply[:i] {
	case "10":
		tc.fg, tc.hasFg = c, true
	case "11":
		tc.bg, tc.hasBg = c, true
	case "4;4":
		tc.blue, tc.hasBlue = c, true
	default:
		return false
	}
	return true
}

func parseRGB(spec string) (color.RGBA, bool) {
	parts := strings.Split(spec, "/")
	if len(parts) != 3 {
		return color.RGBA{}, false
	}
	var out [3]uint8
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 32)
		if err != nil || len(p) == 0 || len(p) > 4 {
			return color.RGBA{}, false
		}
		mx := float64(uint64(1)<<(4*len(p)) - 1)
		out[i] = uint8(float64(v)/mx*255 + 0.5)
	}
	return color.RGBA{out[0], out[1], out[2], 255}, true
}

func (tc termColors) palette(theme string) palette {
	switch theme {
	case "light":
		return lightPalette
	case "dark":
		return darkPalette
	}
	if !tc.hasBg {
		return lightPalette
	}
	base := lightPalette
	if luminance(tc.bg) < 0.18 {
		base = darkPalette
	}
	fg, accent := base.text, base.accent
	if tc.hasFg {
		fg = tc.fg
	}
	if tc.hasBlue {
		accent = tc.blue
	}
	return themedPalette(fg, tc.bg, accent)
}

// --- input --------------------------------------------------------------

// ttyReader decodes stdin into events. pause makes it stop reading, so an
// editor run in the same terminal gets every key.
type ttyReader struct {
	mu     sync.Mutex
	paused atomic.Bool
}

func (t *ttyReader) run(events chan<- termEvent) {
	var d keyDecoder
	emit := func(ev termEvent) {
		select {
		case events <- ev:
		default: // the viewer is busy (an editor is up): drop it
		}
	}
	fd := int(os.Stdin.Fd())
	buf := make([]byte, 4096)
	for {
		if t.paused.Load() {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.mu.Lock()
		if t.paused.Load() {
			t.mu.Unlock()
			continue
		}
		// macOS can't poll a tty, but select works.
		var set unix.FdSet
		set.Set(fd)
		tv := unix.Timeval{Usec: 50000}
		n, err := unix.Select(fd+1, &set, nil, nil, &tv)
		if err != nil && err != unix.EINTR {
			t.mu.Unlock()
			close(events)
			return
		}
		if n > 0 {
			k, err := unix.Read(fd, buf)
			if err == unix.EINTR || err == unix.EAGAIN {
				t.mu.Unlock()
				continue
			}
			if err != nil || k == 0 {
				t.mu.Unlock()
				close(events)
				return
			}
			d.feed(buf[:k], emit)
			d.flush(emit)
		}
		t.mu.Unlock()
	}
}

// pause returns once the reader has stopped reading.
func (t *ttyReader) pause() {
	t.mu.Lock()
	t.paused.Store(true)
	t.mu.Unlock()
}

func (t *ttyReader) resume() { t.paused.Store(false) }
