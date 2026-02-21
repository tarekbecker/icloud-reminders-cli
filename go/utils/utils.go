// Package utils provides helper functions for iCloud Reminders.
package utils

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// EncodeVarint encodes a uint64 as a protobuf varint.
func EncodeVarint(v uint64) []byte {
	var buf []byte
	for v > 127 {
		buf = append(buf, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

// EncodeField encodes a protobuf field.
// wireType 0 = varint (data must be uint64)
// wireType 2 = length-delimited (data must be []byte)
func EncodeField(fieldNum int, wireType int, data interface{}) []byte {
	tag := byte((fieldNum << 3) | wireType)
	switch wireType {
	case 0:
		v := data.(uint64)
		return append([]byte{tag}, EncodeVarint(v)...)
	case 2:
		b := data.([]byte)
		hdr := append([]byte{tag}, EncodeVarint(uint64(len(b)))...)
		return append(hdr, b...)
	}
	return nil
}

// position encodes a CRDT position message {field1: replica, field2: offset}.
// If offset == -1, use the sentinel encoding (0xFFFFFFFF as unsigned varint).
func position(replica uint64, offset int64) []byte {
	replicaField := EncodeField(1, 0, replica)
	if offset == -1 {
		// Sentinel: field 2, wire type 0, value 0xFFFFFFFF = bytes [0xff,0xff,0xff,0xff,0x0f]
		sentinelTag := byte((2 << 3) | 0)
		offsetField := append([]byte{sentinelTag}, EncodeVarint(0xFFFFFFFF)...)
		return append(replicaField, offsetField...)
	}
	return append(replicaField, EncodeField(2, 0, uint64(offset))...)
}

// EncodeTitle encodes a title string as Apple's CRDT TitleDocument protobuf,
// gzip-compressed and base64-encoded.
//
// IMPORTANT: Apple uses CHARACTER length (not byte length) for CRDT metadata.
func EncodeTitle(title string) (string, error) {
	titleBytes := []byte(title)
	charLen := uint64(utf8.RuneCountInString(title)) // CHARACTER count for CRDT

	// Generate a random UUID for the document
	docUUID := generateUUID()

	// CRDT Op #1: Initial position
	op1 := make([]byte, 0, 64)
	op1 = append(op1, EncodeField(1, 2, position(0, 0))...)
	op1 = append(op1, EncodeField(2, 0, uint64(0))...)
	op1 = append(op1, EncodeField(3, 2, position(0, 0))...)
	op1 = append(op1, EncodeField(5, 0, uint64(1))...)

	// CRDT Op #2: Content insert
	op2 := make([]byte, 0, 64)
	op2 = append(op2, EncodeField(1, 2, position(1, 0))...)
	op2 = append(op2, EncodeField(2, 0, charLen)...)
	op2 = append(op2, EncodeField(3, 2, position(1, 0))...)
	op2 = append(op2, EncodeField(5, 0, uint64(2))...)

	// CRDT Op #3: Sentinel/end marker (no field 5)
	op3 := make([]byte, 0, 64)
	op3 = append(op3, EncodeField(1, 2, position(0, -1))...)
	op3 = append(op3, EncodeField(2, 0, uint64(0))...)
	op3 = append(op3, EncodeField(3, 2, position(0, -1))...)

	// Metadata (field 4): uuid_entry only
	// uuid_entry: uuid (f1) + clock (f2) + replica (f2)
	clockPayload := EncodeField(1, 0, charLen)
	replicaPayload := EncodeField(1, 0, uint64(1))
	uuidEntry := make([]byte, 0, 64)
	uuidEntry = append(uuidEntry, EncodeField(1, 2, docUUID)...)
	uuidEntry = append(uuidEntry, EncodeField(2, 2, clockPayload)...)
	uuidEntry = append(uuidEntry, EncodeField(2, 2, replicaPayload)...)
	metadata := EncodeField(1, 2, uuidEntry)

	// AttributeRun (field 5): length = charLen
	attrRun := EncodeField(1, 0, charLen)

	// Note content (field 3 in Document)
	note := make([]byte, 0, 256)
	note = append(note, EncodeField(2, 2, titleBytes)...)
	note = append(note, EncodeField(3, 2, op1)...)
	note = append(note, EncodeField(3, 2, op2)...)
	note = append(note, EncodeField(3, 2, op3)...)
	note = append(note, EncodeField(4, 2, metadata)...)
	note = append(note, EncodeField(5, 2, attrRun)...)

	// Document (field 2 in outer)
	document := make([]byte, 0, 256)
	document = append(document, EncodeField(1, 0, uint64(0))...)
	document = append(document, EncodeField(2, 0, uint64(0))...)
	document = append(document, EncodeField(3, 2, note)...)

	// Outer wrapper
	outer := make([]byte, 0, 256)
	outer = append(outer, EncodeField(1, 0, uint64(0))...)
	outer = append(outer, EncodeField(2, 2, document)...)

	// Gzip compress
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(outer); err != nil {
		return "", fmt.Errorf("gzip write: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("gzip close: %w", err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// printableRe matches printable runs of at least 2 characters.
var printableRe = regexp.MustCompile(`[^\x00-\x1f\x7f-\x9f]{2,}`)

// ExtractTitle extracts the title text from a base64-encoded gzip CRDT TitleDocument.
func ExtractTitle(tdB64 string) string {
	if tdB64 == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(tdB64)
	if err != nil {
		return ""
	}

	var text string
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		// Gzip compressed
		r, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return ""
		}
		dec, err := io.ReadAll(r)
		if err != nil {
			return ""
		}
		text = string(dec)
	} else {
		// Try plain UTF-8
		text = strings.TrimSpace(string(raw))
		if text != "" {
			return text
		}
		return ""
	}

	// Extract printable runs
	matches := printableRe.FindAllString(text, -1)
	for _, s := range matches {
		s = strings.TrimSpace(s)
		if len([]rune(s)) >= 2 {
			return s
		}
	}
	return ""
}

// TsToStr converts a millisecond timestamp to YYYY-MM-DD string.
// Returns empty string if tsMs is 0.
func TsToStr(tsMs int64) string {
	if tsMs == 0 {
		return ""
	}
	t := time.UnixMilli(tsMs).UTC()
	return t.Format("2006-01-02")
}

// StrToTs converts a YYYY-MM-DD string to milliseconds timestamp.
func StrToTs(dateStr string) (int64, error) {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return 0, err
	}
	return t.UTC().UnixMilli(), nil
}

// generateUUID generates a random 16-byte UUID (v4).
func generateUUID() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use time-based
		now := time.Now().UnixNano()
		for i := 0; i < 16; i++ {
			b[i] = byte(now >> (i % 8 * 8))
		}
	}
	// Set version 4 bits
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return b
}

// NewUUIDString generates a new uppercase UUID string.
func NewUUIDString() string {
	b := generateUUID()
	return fmt.Sprintf("%08X-%04X-%04X-%04X-%012X",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
