package utils

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

// Expected lengths are independent of the text decoder. Apple's published
// topotext schema specifies UTF-16 units for Substring/AttributeRun lengths.
func TestTitleDocumentUsesNativeUTF16Lengths(t *testing.T) {
	for _, tc := range []struct {
		text  string
		units uint64
	}{
		{"ASCII", 5}, {"ÄÖÜ", 3}, {"❤️", 2}, {"🧪 Test", 7},
		{"🏠 Objekt & Ausstattung", 23}, {"🥰", 2}, {"👩‍💻", 5}, {"e\u0301", 2},
	} {
		t.Run(tc.text, func(t *testing.T) {
			encoded, err := EncodeTitle(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := base64.StdEncoding.DecodeString(encoded)
			zip, _ := gzip.NewReader(bytes.NewReader(raw))
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(zip)
			_ = zip.Close()
			doc, _ := protobufBytes(buf.Bytes(), 2)
			note, _ := protobufBytes(doc, 3)
			run, _ := protobufBytes(note, 5)
			if len(run) < 2 || run[0] != 8 {
				t.Fatal("attribute run missing")
			}
			units, n := binary.Uvarint(run[1:])
			if n <= 0 || units != tc.units {
				t.Fatalf("native attribute length = %d, want %d", units, tc.units)
			}
			// Content-insert length and its vector clock must use the same units.
			if !bytes.Contains(note, append(EncodeField(1, 2, position(1, 0)), EncodeField(2, 0, tc.units)...)) {
				t.Fatal("substring length is inconsistent")
			}
			metadata, _ := protobufBytes(note, 4)
			clock, _ := protobufBytes(metadata, 1)
			if !bytes.Contains(clock, EncodeField(2, 2, EncodeField(1, 0, tc.units))) {
				t.Fatal("vector clock length is inconsistent")
			}
		})
	}
	if _, err := EncodeTitle(string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 was encoded")
	}
}

func TestTitleDocumentExactRoundTrip(t *testing.T) {
	titles := []string{"", "X", "❤️", "ÄÖÜ äöü ß", "Rollläden im Wohnzimmer getrennt steuerbar",
		"Nur noch schöne Dinge machen ❤️", "Glücklich sein und sich lieben! 🥰",
		"3,43 % Sparkasse Dortmund, 10 Jahre Zinsbindung", "6 Monate bereitstellungsfreie Zeit",
		"Arbeitsverträge von Wera an Herrn Moser senden", "Sondertilgung im November immer machen",
		"Vertragsvorlage ohne unsere Namen von einem anderen Objekt",
		"Alina fragen, ob sie eine Risikolebensversicherung abgeschlossen hat",
		"Unterlagen für die Bank zusammenstellen", "Bank anrufen und erfragen, welche Unterlagen benötigt werden",
		"!#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", "  preserve whitespace\nsecond line\t ", strings.Repeat("😀", 300)}
	// Cover every one-byte printable length, and multi-byte varint lengths.
	for n := 1; n <= 4096; n++ {
		titles = append(titles, strings.Repeat("x", n))
	}
	for _, title := range titles {
		encoded, err := EncodeTitle(title)
		if err != nil {
			t.Fatal(err)
		}
		if got := ExtractTitle(encoded); got != title {
			t.Fatalf("text differs at length %d", len(title))
		}
	}
}

func compressed(raw []byte) string {
	var out bytes.Buffer
	zip := gzip.NewWriter(&out)
	_, _ = zip.Write(raw)
	_ = zip.Close()
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

func TestDocumentDecoderRejectsMalformedFramingAndMetadata(t *testing.T) {
	for _, raw := range [][]byte{
		{0x12, 0xff}, {0x12, 0x7f, 'x'}, {0x00}, {0x0a, 0x03, 'f', 'o', 'o'},
		append(EncodeField(2, 2, []byte{0x1a, 0x02, 0x12, 0x00}), EncodeField(2, 2, []byte{})...),
		[]byte(strings.Repeat("x", (1<<20)+1)),
	} {
		if got := ExtractTitle(compressed(raw)); got != "" {
			t.Fatal("malformed/metadata text escaped")
		}
	}
	if got := ExtractTitle("not-base64"); got != "" {
		t.Fatal("invalid base64 escaped")
	}
	text := "  plain legacy notes\nwith whitespace  "
	if got := ExtractTitle(base64.StdEncoding.EncodeToString([]byte(text))); got != text {
		t.Fatal("legacy text changed")
	}
}
