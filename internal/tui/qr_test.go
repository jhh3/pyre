package tui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testdata/qr_ref.json holds matrices from the reference python-qrcode
// library (byte mode, level L, every mask); the encoder must match them
// module for module.
func TestQREncoderMatchesReference(t *testing.T) {
	b, err := os.ReadFile("testdata/qr_ref.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs []struct {
		Text    string   `json:"text"`
		Mask    int      `json:"mask"`
		Version int      `json:"version"`
		Rows    []string `json:"rows"`
	}
	if err := json.Unmarshal(b, &refs); err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		got := qrEncodeMask(r.Text, r.Mask)
		if len(got) != len(r.Rows) {
			t.Fatalf("%q mask %d: size %d, want %d (version %d)", r.Text, r.Mask, len(got), len(r.Rows), r.Version)
		}
		for y, row := range got {
			var sb strings.Builder
			for _, d := range row {
				if d {
					sb.WriteByte('1')
				} else {
					sb.WriteByte('0')
				}
			}
			if sb.String() != r.Rows[y] {
				t.Fatalf("%q mask %d row %d:\n got %s\nwant %s", r.Text, r.Mask, y, sb.String(), r.Rows[y])
			}
		}
	}
	if qrEncode(strings.Repeat("x", 200)) != nil {
		t.Fatal("too-long text should not encode")
	}
	if l := qrLines(qrEncode("https://auth.openai.com/codex/device"), true); len(l) != 17 { // (29+4)/2 rounded up
		t.Fatalf("rendered %d lines", len(l))
	}
}
