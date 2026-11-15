package theme

import (
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// substrate-gke's blues and blue-grays, which this theme replaced with
// Amazon orange and neutral grays.
var gkeBlues = []string{"#38BDF8", "#818CF8", "#E2E8F0", "#1E293B", "#94A3B8", "#475569"}

var hexRe = regexp.MustCompile(`#[0-9A-Fa-f]{6}\b`)

func channels(hex string) (r, g, b float64) {
	v, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	return float64(v >> 16 & 0xFF), float64(v >> 8 & 0xFF), float64(v & 0xFF)
}

// hsv is the color's hue in degrees and its saturation, 0 to 1.
func hsv(hex string) (hue, sat float64) {
	r, g, b := channels(hex)
	hi, lo := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	if hi == lo {
		return 0, 0
	}
	d := hi - lo
	switch hi {
	case r:
		hue = math.Mod((g-b)/d, 6)
	case g:
		hue = (b-r)/d + 2
	default:
		hue = (r-g)/d + 4
	}
	return math.Mod(hue*60+360, 360), d / hi
}

// blueish is a cyan, blue or indigo hue that is more than a trace away
// from gray: slate #475569 is blue-gray, #525252 is neutral.
func blueish(hex string) bool {
	h, s := hsv(hex)
	return h >= 170 && h <= 280 && s >= 0.05
}

// blueHexes lists the hex colors in src that are substrate-gke's or blueish.
func blueHexes(src string) []string {
	var out []string
	for _, h := range hexRe.FindAllString(src, -1) {
		if blueish(h) || slices.Contains(gkeBlues, strings.ToUpper(h)) {
			out = append(out, h)
		}
	}
	return out
}

func lab(hex string) [3]float64 {
	lin := func(c float64) float64 {
		c /= 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	r, g, b := channels(hex)
	r, g, b = lin(r), lin(g), lin(b)
	x := (0.4124*r + 0.3576*g + 0.1805*b) / 0.95047
	y := 0.2126*r + 0.7152*g + 0.0722*b
	z := (0.0193*r + 0.1192*g + 0.9505*b) / 1.08883
	f := func(t float64) float64 {
		if t > 0.008856 {
			return math.Cbrt(t)
		}
		return 7.787*t + 16.0/116
	}
	return [3]float64{116*f(y) - 16, 500 * (f(x) - f(y)), 200 * (f(y) - f(z))}
}

// near reports two colors a reader could take for each other: closer than
// 30 in CIE76 delta E, or less than 10 degrees of hue apart. The old warning
// amber #FBBF24 is 23 from #FF9900 and 7 degrees of hue.
func near(a, b string) bool {
	la, lb := lab(a), lab(b)
	dE := math.Sqrt(math.Pow(la[0]-lb[0], 2) + math.Pow(la[1]-lb[1], 2) + math.Pow(la[2]-lb[2], 2))
	ha, _ := hsv(a)
	hb, _ := hsv(b)
	dh := math.Abs(ha - hb)
	return dE < 30 || math.Min(dh, 360-dh) < 10
}

func fg(t *testing.T, s lipgloss.Style) string {
	t.Helper()
	c, ok := s.GetForeground().(lipgloss.Color)
	if !ok {
		t.Fatalf("foreground %v is not a lipgloss.Color", s.GetForeground())
	}
	return string(c)
}

// TestThemeHasNoBlue: no color in this file is one of substrate-gke's blues
// or any other blue, cyan or indigo.
func TestThemeHasNoBlue(t *testing.T) {
	src, err := os.ReadFile("theme.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(hexRe.FindAllString(string(src), -1)) == 0 {
		t.Fatal("theme.go names no hex color: the scan would pass on anything")
	}
	if got := blueHexes(string(src)); len(got) != 0 {
		t.Errorf("theme.go still has blue colors: %v", got)
	}
}

// TestWarningStandsApartFromTheAccents: the warning color is neither equal
// nor near the primary or secondary accent.
func TestWarningStandsApartFromTheAccents(t *testing.T) {
	warn := fg(t, Warning)
	for name, s := range map[string]lipgloss.Style{"Title (primary)": Title, "Accent (secondary)": Accent} {
		if c := fg(t, s); near(warn, c) {
			t.Errorf("Warning %s is too near %s %s", warn, name, c)
		}
	}
}

// The checks above, proved both ways.
func TestColorChecks(t *testing.T) {
	for _, src := range []string{
		`Sky = lipgloss.Color("#38BDF8")`, // substrate-gke's primary
		`x = "#e2e8f0"`,                   // substrate-gke's selection text, lower case
		`x = "#3B82F6"`,                   // some other blue
		`x = "#64748B"`,                   // a blue-gray
	} {
		if len(blueHexes(src)) != 1 {
			t.Errorf("blueHexes(%s) found no blue", src)
		}
	}
	for _, src := range []string{
		`Orange = lipgloss.Color("#FF9900")`, `x = "#525252"`, `x = "#34D399"`, `x = "#F43F5E"`, `x = "#5C3300"`,
	} {
		if got := blueHexes(src); len(got) != 0 {
			t.Errorf("blueHexes(%s) = %v, want none", src, got)
		}
	}
	for _, p := range [][2]string{{"#FF9900", "#FF9900"}, {"#FBBF24", "#FF9900"}, {"#FFB84D", "#FF9900"}} {
		if !near(p[0], p[1]) {
			t.Errorf("near(%s, %s) = false, want true", p[0], p[1])
		}
	}
	for _, p := range [][2]string{{"#FFD60A", "#FF9900"}, {"#FFD60A", "#EC7211"}, {"#34D399", "#FF9900"}} {
		if near(p[0], p[1]) {
			t.Errorf("near(%s, %s) = true, want false", p[0], p[1])
		}
	}
}
