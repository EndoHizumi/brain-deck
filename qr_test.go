package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- QR コード ----------

// qrTexts は、GUI と比べる QR コードの文字列。
var qrTexts = []string{"https://example.com/", "Hello, world!", "日本語のテキスト：Brain", "WIFI:S:brain;T:WPA;P:pass1234;;",
	"https://github.com/endohizumi/brain-deck/blob/main/README.md#brain-deck",
	strings.Repeat("0123456789abcdefghijklmnopqrstuvwxyz", 8), "a\tb\nc"}

func qrTable() []map[string]any {
	var out []map[string]any
	for _, s := range qrTexts {
		q, err := EncodeQR(s)
		if err != nil {
			panic(err)
		}
		var rows []string
		for y := 0; y < q.Size; y++ {
			var b strings.Builder
			for x := 0; x < q.Size; x++ {
				if q.Dark(x, y) {
					b.WriteByte('1')
				} else {
					b.WriteByte('0')
				}
			}
			rows = append(rows, b.String())
		}
		out = append(out, map[string]any{"text": s, "version": q.Version, "ecl": q.ECL, "mask": q.Mask, "rows": rows})
	}
	return out
}

// TestQRCodesTable は、QR コードの模様を gui/test/fixtures/qrcodes.json（Project Nayuki の Python 版と同じことを
// 確かめたもの）と比べる。GUI の qr.ts も同じ表と比べる。
// 変えたら LEFTHAND_UPDATE_QRCODES=1 go test -run QRCodesTable で書き直す。
func TestQRCodesTable(t *testing.T) {
	path := filepath.Join("gui", "test", "fixtures", "qrcodes.json")
	got, _ := json.MarshalIndent(qrTable(), "", " ")
	got = append(got, '\n')
	if os.Getenv("LEFTHAND_UPDATE_QRCODES") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Errorf("%s differs; regenerate with LEFTHAND_UPDATE_QRCODES=1 go test -run QRCodesTable", path)
	}
}
