package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Shared layout, in points.
const (
	padX      = 16.0 // content's left and right edge
	selInset  = 6.0  // a selection reaches this far from the pane's edges
	selRadius = 10.0
)

type palette struct {
	bg, text, gray, faint, divider, accent, selBg color.RGBA
}

var lightPalette = palette{
	bg:      rgb(255, 255, 255),
	text:    rgb(18, 20, 23),
	gray:    rgb(104, 108, 115),
	faint:   rgb(150, 153, 158),
	divider: rgb(230, 232, 236),
	accent:  rgb(49, 95, 189),
	selBg:   rgb(232, 238, 249),
}

var darkPalette = palette{
	bg:      rgb(30, 31, 34),
	text:    rgb(226, 228, 231),
	gray:    rgb(152, 155, 161),
	faint:   rgb(118, 121, 127),
	divider: rgb(58, 60, 64),
	accent:  rgb(84, 136, 238),
	selBg:   rgb(37, 49, 72),
}

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{r, g, b, 255} }

// grayMix is how far from the foreground toward the background each gray sits.
func grayMix(bg color.RGBA) (gray, faint, divider float64) {
	if luminance(bg) < 0.18 {
		return 0.38, 0.52, 0.85
	}
	return 0.45, 0.58, 0.9
}

// themedPalette derives the colors from a terminal theme.
func themedPalette(fg, bg, accent color.RGBA) palette {
	g, f, d := grayMix(bg)
	return palette{
		bg: bg, text: fg, accent: accent,
		gray:    mix(fg, bg, g),
		faint:   mix(fg, bg, f),
		divider: mix(fg, bg, d),
		selBg:   mix(accent, bg, 0.84),
	}
}

// seeThrough is the palette for drawing with no background: each gray is
// the foreground at the opacity that gives that gray over the background,
// so it blends with whatever the pane shows underneath.
func (p palette) seeThrough() palette {
	g, f, d := grayMix(p.bg)
	q := p
	q.gray = premul(p.text, 1-g)
	q.faint = premul(p.text, 1-f)
	q.divider = premul(p.text, 1-d)
	q.selBg = premul(p.accent, 0.16)
	return q
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	l := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 255}
}

// premul is c (opaque) at opacity a, premultiplied as image.RGBA wants.
func premul(c color.RGBA, a float64) color.RGBA {
	a = math.Max(0, math.Min(1, a))
	f := func(v uint8) uint8 { return uint8(math.Round(float64(v) * a)) }
	return color.RGBA{f(c.R), f(c.G), f(c.B), uint8(math.Round(255 * a))}
}

func luminance(c color.RGBA) float64 {
	lin := func(v uint8) float64 {
		f := float64(v) / 255
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

func parseHex(s string) (color.RGBA, bool) {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return color.RGBA{}, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.RGBA{}, false
	}
	return rgb(uint8(v>>16), uint8(v>>8), uint8(v)), true
}

type renderer struct {
	fonts     *fontSet
	scale     float64
	pal       palette
	clear     bool // leave the background transparent
	now       time.Time
	monoFonts *fontSet
}

// mono is r drawing in the monospaced face, for commands and code.
func (r *renderer) mono() *renderer {
	m := *r
	m.fonts = r.monoFonts
	return &m
}

// tinted is c at opacity a over the background: see-through on a clear
// image, mixed on a painted one. The two look the same.
func (r *renderer) tinted(c color.RGBA, a float64) color.RGBA {
	if r.clear {
		return premul(c, a)
	}
	return mix(c, r.pal.bg, 1-a)
}

func (r *renderer) paint(img *image.RGBA) {
	if !r.clear {
		draw.Draw(img, img.Bounds(), image.NewUniform(r.pal.bg), image.Point{}, draw.Src)
	}
}

// green and red for diffs, readable on the theme's background.
func (r *renderer) green() color.RGBA { return r.themed(rgb(46, 160, 67), rgb(87, 199, 110)) }
func (r *renderer) red() color.RGBA   { return r.themed(rgb(207, 34, 46), rgb(255, 110, 110)) }
func (r *renderer) amber() color.RGBA { return r.themed(rgb(191, 120, 0), rgb(240, 180, 60)) }

func (r *renderer) themed(light, dark color.RGBA) color.RGBA {
	if luminance(r.pal.bg) < 0.18 {
		return dark
	}
	return light
}

// --- text ---------------------------------------------------------------

func (r *renderer) face(size float64, bold bool) font.Face { return r.fonts.face(bold, size*r.scale) }

// clean drops what the font can't draw and folds whitespace to single spaces.
func (r *renderer) clean(s string, bold bool) string {
	var b strings.Builder
	space := false
	for _, c := range s {
		if unicode.IsSpace(c) {
			space = b.Len() > 0
			continue
		}
		if !r.fonts.has(bold, c) {
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(c)
	}
	return b.String()
}

// width is how wide s (already clean) draws, in points.
func (r *renderer) width(s string, size float64, bold bool) float64 {
	face := r.face(size, bold)
	var w fixed.Int26_6
	prev := rune(-1)
	for _, c := range s {
		if prev >= 0 {
			w += face.Kern(prev, c)
		}
		adv, _ := face.GlyphAdvance(c)
		w += adv
		prev = c
	}
	return float64(w) / 64 / r.scale
}

// fit cuts s (already clean) with an ellipsis to fit maxW points.
func (r *renderer) fit(s string, size float64, bold bool, maxW float64) string {
	if maxW <= 0 || r.width(s, size, bold) <= maxW {
		return s
	}
	const ell = "…"
	for len(s) > 0 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
		if r.width(strings.TrimRight(s, " ,.;:-")+ell, size, bold) <= maxW {
			break
		}
	}
	return strings.TrimRight(s, " ,.;:-") + ell
}

// text draws s with its baseline at (x, y), cut to maxW when maxW > 0, and
// returns how wide it drew.
func (r *renderer) text(img *image.RGBA, s string, x, y, size float64, bold bool, col color.RGBA, maxW float64) float64 {
	s = r.fit(r.clean(s, bold), size, bold, maxW)
	return r.drawRaw(img, s, x, y, size, bold, col)
}

// textRight draws s ending at x.
func (r *renderer) textRight(img *image.RGBA, s string, x, y, size float64, bold bool, col color.RGBA) float64 {
	s = r.clean(s, bold)
	w := r.width(s, size, bold)
	r.drawRaw(img, s, x-w, y, size, bold, col)
	return w
}

func (r *renderer) drawRaw(img *image.RGBA, s string, x, y, size float64, bold bool, col color.RGBA) float64 {
	face := r.face(size, bold)
	src := image.NewUniform(col)
	dot := fixed.Point26_6{X: fixed.Int26_6(x * r.scale * 64), Y: fixed.Int26_6(math.Round(y*r.scale) * 64)}
	start := dot.X
	prev := rune(-1)
	for _, c := range s {
		if prev >= 0 {
			dot.X += face.Kern(prev, c)
		}
		dr, mask, mp, adv, ok := face.Glyph(dot, c)
		if ok {
			draw.DrawMask(img, dr, src, image.Point{}, mask, mp, draw.Over)
		}
		dot.X += adv
		prev = c
	}
	return float64(dot.X-start) / 64 / r.scale
}

// wrap breaks s into lines that fit maxW points. "\n" in s is a hard break;
// a word too long for a line (a URL, say) breaks between characters.
func (r *renderer) wrap(s string, size float64, bold bool, maxW float64) []string {
	var lines []string
	for _, hard := range strings.Split(s, "\n") {
		words := strings.Fields(r.clean(hard, bold))
		if len(words) == 0 {
			continue
		}
		line := ""
		for _, word := range words {
			try := word
			if line != "" {
				try = line + " " + word
			}
			if r.width(try, size, bold) <= maxW {
				line = try
				continue
			}
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			for r.width(word, size, bold) > maxW {
				cut := len(word)
				for cut > 1 && r.width(word[:cut], size, bold) > maxW {
					_, n := utf8.DecodeLastRuneInString(word[:cut])
					cut -= n
				}
				if cut <= 0 {
					break
				}
				lines = append(lines, word[:cut])
				word = word[cut:]
			}
			line = word
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// --- shapes -------------------------------------------------------------

// pathBuilder collects polygons in points and fills them over only their
// bounding box.
type pathBuilder struct {
	r     *renderer
	img   *image.RGBA
	polys [][][2]float32
}

func (r *renderer) path(img *image.RGBA) *pathBuilder { return &pathBuilder{r: r, img: img} }

func (p *pathBuilder) poly(pts [][2]float64) {
	px := make([][2]float32, len(pts))
	for i, pt := range pts {
		px[i] = [2]float32{float32(pt[0] * p.r.scale), float32(pt[1] * p.r.scale)}
	}
	p.polys = append(p.polys, px)
}

func (p *pathBuilder) fill(col color.RGBA) {
	if len(p.polys) == 0 {
		return
	}
	minX, minY := float32(math.Inf(1)), float32(math.Inf(1))
	maxX, maxY := float32(math.Inf(-1)), float32(math.Inf(-1))
	for _, poly := range p.polys {
		for _, pt := range poly {
			minX, maxX = min(minX, pt[0]), max(maxX, pt[0])
			minY, maxY = min(minY, pt[1]), max(maxY, pt[1])
		}
	}
	box := image.Rect(int(math.Floor(float64(minX))), int(math.Floor(float64(minY))),
		int(math.Ceil(float64(maxX))), int(math.Ceil(float64(maxY)))).Intersect(p.img.Bounds())
	if box.Empty() {
		return
	}
	ras := vector.NewRasterizer(box.Dx(), box.Dy())
	ox, oy := float32(box.Min.X), float32(box.Min.Y)
	for _, poly := range p.polys {
		ras.MoveTo(poly[0][0]-ox, poly[0][1]-oy)
		for _, pt := range poly[1:] {
			ras.LineTo(pt[0]-ox, pt[1]-oy)
		}
		ras.ClosePath()
	}
	ras.Draw(p.img, box, image.NewUniform(col), image.Point{})
}

func arc(pts [][2]float64, cx, cy, rad, a0, a1 float64) [][2]float64 {
	n := int(math.Max(4, math.Ceil(math.Abs(a1-a0)*rad*4)))
	for i := 0; i <= n; i++ {
		a := a0 + (a1-a0)*float64(i)/float64(n)
		pts = append(pts, [2]float64{cx + rad*math.Cos(a), cy + rad*math.Sin(a)})
	}
	return pts
}

func roundRect(x0, y0, x1, y1, rad float64) [][2]float64 {
	rad = math.Max(0, math.Min(rad, math.Min(x1-x0, y1-y0)/2))
	var pts [][2]float64
	pts = arc(pts, x1-rad, y0+rad, rad, -math.Pi/2, 0)
	pts = arc(pts, x1-rad, y1-rad, rad, 0, math.Pi/2)
	pts = arc(pts, x0+rad, y1-rad, rad, math.Pi/2, math.Pi)
	pts = arc(pts, x0+rad, y0+rad, rad, math.Pi, 3*math.Pi/2)
	return pts
}

func (r *renderer) fillRoundRect(img *image.RGBA, x0, y0, x1, y1, rad float64, col color.RGBA) {
	p := r.path(img)
	p.poly(roundRect(x0, y0, x1, y1, rad))
	p.fill(col)
}

func (r *renderer) fillRect(img *image.RGBA, x0, y0, x1, y1 float64, col color.RGBA) {
	p := r.path(img)
	p.poly([][2]float64{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}})
	p.fill(col)
}

func (r *renderer) fillCircle(img *image.RGBA, cx, cy, rad float64, col color.RGBA) {
	p := r.path(img)
	p.poly(arc(nil, cx, cy, rad, 0, 2*math.Pi))
	p.fill(col)
}

func reversed(pts [][2]float64) [][2]float64 {
	out := make([][2]float64, len(pts))
	for i, p := range pts {
		out[len(pts)-1-i] = p
	}
	return out
}

// check strokes a checkmark size points wide, centered at (cx, cy).
func (r *renderer) check(img *image.RGBA, cx, cy, size float64, col color.RGBA) {
	pts := [][2]float64{{cx - size*0.55, cy + size*0.02}, {cx - size*0.15, cy + size*0.42}, {cx + size*0.6, cy - size*0.38}}
	const w = 1.0
	p := r.path(img)
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		dx, dy := b[0]-a[0], b[1]-a[1]
		l := math.Hypot(dx, dy)
		nx, ny := -dy/l*w, dx/l*w
		ex, ey := dx/l*w*0.6, dy/l*w*0.6
		p.poly([][2]float64{{a[0] - nx - ex, a[1] - ny - ey}, {b[0] - nx + ex, b[1] - ny + ey},
			{b[0] + nx + ex, b[1] + ny + ey}, {a[0] + nx - ex, a[1] + ny - ey}})
	}
	p.fill(col)
}
