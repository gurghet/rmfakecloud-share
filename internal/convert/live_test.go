//go:build live

package convert

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

// Run with:
//
//	RMSHARE_LIVE_TEST=1 ANTHROPIC_API_KEY=... go test -tags live -run TestLive -v ./internal/convert/
//
// It sends one synthetic page (no real data) to the real Anthropic API and costs a few cents.

var glyphs = map[rune][7]string{
	'H': {"X...X", "X...X", "X...X", "XXXXX", "X...X", "X...X", "X...X"},
	'E': {"XXXXX", "X....", "X....", "XXXX.", "X....", "X....", "XXXXX"},
	'L': {"X....", "X....", "X....", "X....", "X....", "X....", "XXXXX"},
	'O': {".XXX.", "X...X", "X...X", "X...X", "X...X", "X...X", ".XXX."},
	'W': {"X...X", "X...X", "X...X", "X.X.X", "X.X.X", "XX.XX", "X...X"},
	'R': {"XXXX.", "X...X", "X...X", "XXXX.", "X.X..", "X..X.", "X...X"},
	'D': {"XXXX.", "X...X", "X...X", "X...X", "X...X", "X...X", "XXXX."},
}

// renderText draws upper-case text from the tiny built-in font, scaled up.
func renderText(text string, scale int) []byte {
	const margin = 4
	w := (len(text)*6 + 2*margin) * scale
	h := (7 + 2*margin) * scale
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for ci, r := range text {
		g, ok := glyphs[r]
		if !ok {
			continue
		}
		for y, row := range g {
			for x, c := range row {
				if c != 'X' {
					continue
				}
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						img.SetGray(((margin+ci*6+x)*scale)+dx, ((margin+y)*scale)+dy, color.Gray{0})
					}
				}
			}
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func TestLiveClaudeConversion(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if os.Getenv("RMSHARE_LIVE_TEST") != "1" || key == "" {
		t.Skip("set RMSHARE_LIVE_TEST=1 and ANTHROPIC_API_KEY to run against the real API")
	}
	c := NewClaude(ClaudeOptions{APIKey: key, Model: os.Getenv("RMSHARE_MODEL"), MaxRetries: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	note, err := c.Convert(ctx, [][]byte{renderText("HELLO WORLD", 12)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model %s returned:\n%s", c.Model(), note)
	low := strings.ToLower(note)
	if strings.TrimSpace(note) == "" || !strings.Contains(low, "hello") || !strings.Contains(low, "world") {
		t.Fatalf("transcription should contain the rendered words, got %q", note)
	}
}
