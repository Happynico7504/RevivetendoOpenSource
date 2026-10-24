package main

// Light/dark/system theme for every relay-admin page.
//
// The pages carry their own hand-written light CSS, so instead of keeping a
// second dark stylesheet per page, themeMiddleware rewrites every colour in a
// page's <style> blocks and style="" attributes (and in served .css files) to
// var(--t<role>-<hex>, <original>). Light mode is therefore the page exactly as
// written; the dark values are derived from the originals and only defined
// under :root[data-theme=dark] and, for "system", prefers-color-scheme: dark.
// The choice is stored per browser (localStorage "rvt-theme") and applied by a
// small script before the page renders.
//
// A page opts out with <meta name="rvt-theme" content="off"> (the OBS overlay,
// Mii TV, the dark-only Discord page). Console browsers are never touched: the
// Wii U and 3DS browsers don't know CSS variables.

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const themeOptOut = `<meta name="rvt-theme" content="off">`

type themeWriter struct {
	http.ResponseWriter
	r       *http.Request
	decided bool
	buffer  bool
	css     bool
	status  int
	buf     bytes.Buffer
}

func themeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || strings.Contains(r.UserAgent(), "Nintendo") {
			next.ServeHTTP(w, r)
			return
		}
		tw := &themeWriter{ResponseWriter: w, r: r, status: http.StatusOK}
		next.ServeHTTP(tw, r)
		tw.finish()
	})
}

func (tw *themeWriter) decide(first []byte) {
	if tw.decided {
		return
	}
	tw.decided = true
	ct := tw.Header().Get("Content-Type")
	if ct == "" && first != nil {
		ct = http.DetectContentType(first)
	}
	ct = strings.ToLower(ct)
	tw.css = strings.HasPrefix(ct, "text/css")
	tw.buffer = tw.status == http.StatusOK && (strings.HasPrefix(ct, "text/html") || tw.css)
	if !tw.buffer {
		tw.ResponseWriter.WriteHeader(tw.status)
	}
}

func (tw *themeWriter) WriteHeader(code int) {
	if tw.decided {
		return
	}
	tw.status = code
	if code != http.StatusOK {
		tw.decide(nil)
	}
}

func (tw *themeWriter) Write(b []byte) (int, error) {
	tw.decide(b)
	if tw.buffer {
		return tw.buf.Write(b)
	}
	return tw.ResponseWriter.Write(b)
}

func (tw *themeWriter) Unwrap() http.ResponseWriter { return tw.ResponseWriter }

func (tw *themeWriter) finish() {
	if !tw.decided {
		tw.decide(nil) // handler wrote nothing
		return
	}
	if !tw.buffer {
		return
	}
	var out string
	if tw.css {
		out = themeCSS(tw.buf.String())
	} else {
		out = themeHTML(tw.buf.String())
	}
	tw.Header().Del("Content-Length")
	tw.ResponseWriter.WriteHeader(tw.status)
	tw.ResponseWriter.Write([]byte(out))
}

// --- rewriting ---

var (
	themeStyleBlockRe = regexp.MustCompile(`(?is)(<style[^>]*>)(.*?)(</style>)`)
	themeStyleAttrRe  = regexp.MustCompile(`(\sstyle=)("[^"]*"|'[^']*')`)
	// Declarations whose colours are themed. Shadows are left alone (black
	// shadows and coloured focus rings work on both backgrounds).
	themeDeclRe     = regexp.MustCompile(`(?i)(^|[{;\s])(background(?:-color|-image)?|color|caret-color|text-decoration(?:-color)?|fill|stroke|accent-color|border(?:-(?:top|right|bottom|left|block|inline))?(?:-(?:start|end))?(?:-color)?|outline(?:-color)?|column-rule(?:-color)?)(\s*:\s*)([^;{}]*)`)
	themeColorRe    = regexp.MustCompile(`(?i)#[0-9a-f]{3,8}\b|rgba?\([^)]*\)|\b(?:white|black|gray|grey|silver|red|green|blue|orange|yellow|purple|pink|navy|teal|maroon)\b`)
	themeURLRe      = regexp.MustCompile(`(?i)url\([^)]*\)`)
	themeHeadOpenRe = regexp.MustCompile(`(?i)<head(?:\s[^>]*)?>`)
)

var themeNamedColors = map[string]string{
	"white": "#ffffff", "black": "#000000", "gray": "#808080", "grey": "#808080",
	"silver": "#c0c0c0", "red": "#ff0000", "green": "#008000", "blue": "#0000ff",
	"orange": "#ffa500", "yellow": "#ffff00", "purple": "#800080", "pink": "#ffc0cb",
	"navy": "#000080", "teal": "#008080", "maroon": "#800000",
}

// themePalette collects the dark value of every variable a page uses.
type themePalette map[string]string

func themeHTML(page string) string {
	if strings.Contains(page, themeOptOut) {
		return page
	}
	pal := themePalette{}
	page = themeStyleBlockRe.ReplaceAllStringFunc(page, func(m string) string {
		p := themeStyleBlockRe.FindStringSubmatch(m)
		return p[1] + pal.rewriteCSS(p[2]) + p[3]
	})
	page = themeStyleAttrRe.ReplaceAllStringFunc(page, func(m string) string {
		p := themeStyleAttrRe.FindStringSubmatch(m)
		q := p[2][:1]
		return p[1] + q + pal.rewriteCSS(p[2][1:len(p[2])-1]) + q
	})
	loc := themeHeadOpenRe.FindStringIndex(page)
	if loc == nil {
		// A fragment fetched into an already themed page: it only needs the
		// variables for its own colours.
		if len(pal) == 0 {
			return page
		}
		return `<style>` + pal.css() + `</style>` + page
	}
	head := `<style>` + themeBaseCSS + pal.css() + `</style>` + themeScript
	return page[:loc[1]] + head + page[loc[1]:]
}

func themeCSS(css string) string {
	pal := themePalette{}
	css = pal.rewriteCSS(css)
	return css + "\n" + pal.css()
}

func (pal themePalette) rewriteCSS(css string) string {
	return themeDeclRe.ReplaceAllStringFunc(css, func(m string) string {
		p := themeDeclRe.FindStringSubmatch(m)
		role := themeRole(strings.ToLower(p[2]))
		return p[1] + p[2] + p[3] + pal.rewriteValue(role, p[4])
	})
}

func themeRole(prop string) byte {
	switch {
	case strings.HasPrefix(prop, "background"):
		return 'b'
	case strings.HasPrefix(prop, "border"), strings.HasPrefix(prop, "outline"), strings.HasPrefix(prop, "column-rule"):
		return 'l'
	}
	return 'f'
}

// rewriteValue replaces the colours in one declaration value, leaving url()s
// (data: SVGs carry %23 hex colours of their own) as they are.
func (pal themePalette) rewriteValue(role byte, v string) string {
	var out strings.Builder
	last := 0
	for _, u := range themeURLRe.FindAllStringIndex(v, -1) {
		out.WriteString(pal.rewriteColors(role, v[last:u[0]]))
		out.WriteString(v[u[0]:u[1]])
		last = u[1]
	}
	out.WriteString(pal.rewriteColors(role, v[last:]))
	return out.String()
}

func (pal themePalette) rewriteColors(role byte, v string) string {
	return themeColorRe.ReplaceAllStringFunc(v, func(lit string) string {
		c, ok := parseThemeColor(lit)
		if !ok {
			return lit
		}
		name := fmt.Sprintf("--t%c-%s", role, c.key())
		pal[name] = themeDark(role, c).css()
		return "var(" + name + "," + lit + ")"
	})
}

func (pal themePalette) css() string {
	if len(pal) == 0 {
		return ""
	}
	names := make([]string, 0, len(pal))
	for n := range pal {
		names = append(names, n)
	}
	sort.Strings(names)
	var vars strings.Builder
	for _, n := range names {
		vars.WriteString(n + ":" + pal[n] + ";")
	}
	return `:root[data-theme=dark]{` + vars.String() + `}` +
		`@media (prefers-color-scheme:dark){:root:not([data-theme=light]){` + vars.String() + `}}`
}

// --- colours ---

type themeColor struct{ r, g, b, a float64 } // 0..1

func parseThemeColor(lit string) (themeColor, bool) {
	lit = strings.ToLower(lit)
	if hex, ok := themeNamedColors[lit]; ok {
		lit = hex
	}
	if strings.HasPrefix(lit, "#") {
		h := lit[1:]
		if len(h) == 3 || len(h) == 4 {
			var e strings.Builder
			for _, ch := range h {
				e.WriteRune(ch)
				e.WriteRune(ch)
			}
			h = e.String()
		}
		if len(h) != 6 && len(h) != 8 {
			return themeColor{}, false
		}
		n, err := strconv.ParseUint(h, 16, 32)
		if err != nil {
			return themeColor{}, false
		}
		if len(h) == 6 {
			n = n<<8 | 0xff
		}
		return themeColor{float64(n>>24&0xff) / 255, float64(n>>16&0xff) / 255, float64(n>>8&0xff) / 255, float64(n&0xff) / 255}, true
	}
	open := strings.IndexByte(lit, '(')
	if open < 0 || !strings.HasSuffix(lit, ")") {
		return themeColor{}, false
	}
	parts := strings.FieldsFunc(lit[open+1:len(lit)-1], func(r rune) bool { return r == ',' || r == ' ' || r == '/' })
	if len(parts) != 3 && len(parts) != 4 {
		return themeColor{}, false
	}
	var v [4]float64
	v[3] = 1
	for i, s := range parts {
		pct := strings.HasSuffix(s, "%")
		f, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil {
			return themeColor{}, false
		}
		switch {
		case pct:
			f /= 100
		case i < 3:
			f /= 255
		}
		v[i] = math.Max(0, math.Min(1, f))
	}
	return themeColor{v[0], v[1], v[2], v[3]}, true
}

func (c themeColor) key() string {
	k := fmt.Sprintf("%02x%02x%02x", int(math.Round(c.r*255)), int(math.Round(c.g*255)), int(math.Round(c.b*255)))
	if a := int(math.Round(c.a * 255)); a != 255 {
		k += fmt.Sprintf("%02x", a)
	}
	return k
}

func (c themeColor) css() string {
	r, g, b := int(math.Round(c.r*255)), int(math.Round(c.g*255)), int(math.Round(c.b*255))
	if c.a < 1 {
		return fmt.Sprintf("rgba(%d,%d,%d,%s)", r, g, b, strconv.FormatFloat(math.Round(c.a*1000)/1000, 'f', -1, 64))
	}
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

// themeDark maps a light-theme colour to its dark counterpart by its role:
// light backgrounds and borders turn dark (pale tints keep a muted hue), dark
// text turns light. Colours that already sit on the "dark" side - code blocks,
// saturated buttons, white text on them - stay as they are, so islands that
// were designed dark keep working.
func themeDark(role byte, c themeColor) themeColor {
	h, s, l := rgbToHSL(c.r, c.g, c.b)
	switch role {
	case 'b':
		if l > 0.5 {
			l, s = 0.07+(1-l)*0.6, s*0.75
		} else {
			l *= 0.85
		}
	case 'l':
		if l > 0.5 {
			l, s = 0.17+(1-l)*0.6, s*0.75
		}
	default:
		if l < 0.88 {
			l = 0.35 + 0.57*(1-l)
		}
	}
	r, g, b := hslToRGB(h, s, l)
	return themeColor{r, g, b, c.a}
}

func rgbToHSL(r, g, b float64) (h, s, l float64) {
	max, min := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l = (max + min) / 2
	if max == min {
		return 0, 0, l
	}
	d := max - min
	if l > 0.5 {
		s = d / (2 - max - min)
	} else {
		s = d / (max + min)
	}
	switch max {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h / 6, s, l
}

func hslToRGB(h, s, l float64) (r, g, b float64) {
	if s == 0 {
		return l, l, l
	}
	q := l * (1 + s)
	if l >= 0.5 {
		q = l + s - l*s
	}
	p := 2*l - q
	hue := func(t float64) float64 {
		switch {
		case t < 0:
			t++
		case t > 1:
			t--
		}
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 0.5:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	return hue(h + 1.0/3), hue(h), hue(h - 1.0/3)
}

// --- page chrome ---

var themeBaseCSS = func() string {
	dark := `color-scheme:dark;--t-page:` + themeDark('b', themeColor{1, 1, 1, 1}).css() +
		`;--t-text:` + themeDark('f', themeColor{0, 0, 0, 1}).css() + `;`
	return `html{background:var(--t-page,#fff);color:var(--t-text,#000)}` +
		`:root[data-theme=dark]{` + dark + `}` +
		`@media (prefers-color-scheme:dark){:root:not([data-theme=light]){` + dark + `}}` +
		`:where(:root[data-theme=dark] a:link){color:#8ab4f8}:where(:root[data-theme=dark] a:visited){color:#c4a5f5}` +
		`@media (prefers-color-scheme:dark){:where(:root:not([data-theme=light]) a:link){color:#8ab4f8}:where(:root:not([data-theme=light]) a:visited){color:#c4a5f5}}` +
		`.rvt-theme{position:fixed;right:12px;bottom:12px;z-index:9999;display:inline-flex;gap:2px;padding:3px;border-radius:999px;background:rgba(255,255,255,.92);border:1px solid rgba(0,0,0,.12);box-shadow:0 2px 8px rgba(0,0,0,.12);font:13px/1 system-ui,sans-serif}` +
		`.rvt-theme.in-slot{position:static;box-shadow:none;vertical-align:middle}` +
		`.rvt-theme button{all:unset;cursor:pointer;width:26px;height:26px;display:grid;place-items:center;border-radius:999px;color:#52525b}` +
		`.rvt-theme button:hover{background:rgba(0,0,0,.06)}` +
		`.rvt-theme button[aria-pressed=true]{background:#2563eb;color:#fff}` +
		`.rvt-theme button:focus-visible{outline:2px solid #2563eb;outline-offset:1px}` +
		`.rvt-theme svg{width:15px;height:15px;fill:none;stroke:currentColor;stroke-width:2;stroke-linecap:round;stroke-linejoin:round}` +
		`:root[data-theme=dark] .rvt-theme{background:rgba(30,30,34,.92);border-color:rgba(255,255,255,.14)}:root[data-theme=dark] .rvt-theme button{color:#d4d4d8}:root[data-theme=dark] .rvt-theme button:hover{background:rgba(255,255,255,.08)}` +
		`@media (prefers-color-scheme:dark){:root:not([data-theme=light]) .rvt-theme{background:rgba(30,30,34,.92);border-color:rgba(255,255,255,.14)}:root:not([data-theme=light]) .rvt-theme button{color:#d4d4d8}:root:not([data-theme=light]) .rvt-theme button:hover{background:rgba(255,255,255,.08)}}` +
		`@media print{.rvt-theme{display:none}}`
}()

// themeScript applies the stored choice before the body renders and adds the
// light/system/dark switch: into the page's [data-lang-slot] nav when it has
// one, floating in the bottom corner otherwise.
const themeScript = `<script>(function(){
var KEY='rvt-theme',root=document.documentElement;
function get(){try{var v=localStorage.getItem(KEY);return v==='light'||v==='dark'?v:'system'}catch(e){return 'system'}}
function apply(v){if(v==='system')root.removeAttribute('data-theme');else root.setAttribute('data-theme',v)}
apply(get());
var ICONS={light:'<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
system:'<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>',
dark:'<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/>'};
var LABELS={light:'Light',system:'System',dark:'Dark'};
function build(){
if(document.querySelector('.rvt-theme'))return;
var box=document.createElement('div');box.className='rvt-theme';box.setAttribute('role','group');box.setAttribute('aria-label','Theme');
['light','system','dark'].forEach(function(v){
var b=document.createElement('button');b.type='button';b.title=LABELS[v];b.setAttribute('aria-label',LABELS[v]);b.dataset.v=v;
b.innerHTML='<svg viewBox="0 0 24 24" aria-hidden="true">'+ICONS[v]+'</svg>';
b.addEventListener('click',function(){try{if(v==='system')localStorage.removeItem(KEY);else localStorage.setItem(KEY,v)}catch(e){}apply(v);sync()});
box.appendChild(b)});
function sync(){var cur=root.getAttribute('data-theme')||'system';
box.querySelectorAll('button').forEach(function(b){b.setAttribute('aria-pressed',b.dataset.v===cur?'true':'false')})}
sync();
var slot=document.querySelector('[data-lang-slot]');
if(slot){box.className+=' in-slot';slot.appendChild(box)}else document.body.appendChild(box)}
if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',build);else build();
})();</script>`
