// Package utils provides helper functions for iCloud Reminders.
package utils

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"time"
	"unicode/utf16"
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
// Apple's Substring and AttributeRun lengths use UTF-16 code units. A rune
// outside the BMP occupies two units; protobuf string lengths remain UTF-8 bytes.
func EncodeTitle(title string) (string, error) {
	if !utf8.ValidString(title) {
		return "", fmt.Errorf("text is not valid UTF-8")
	}
	titleBytes := []byte(title)
	charLen := uint64(len(utf16.Encode([]rune(title))))

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

// ExtractTitle decodes the text field, never protobuf framing or CRDT metadata.
// TitleDocument stores text at outer.document(2).note(3).text(2).
func ExtractTitle(tdB64 string) string {
	raw, err := base64.StdEncoding.DecodeString(tdB64)
	if err != nil || len(raw) == 0 {
		return ""
	}
	compressed := isCompressedDocument(raw)
	if compressed {
		raw, err = compressedDocument(tdB64)
		if err != nil {
			return ""
		}
	}
	text, ok := documentText(raw)
	if ok && utf8.Valid(text) {
		return string(text)
	}
	// Legacy uncompressed plain text remains supported, with its whitespace.
	if !compressed && utf8.Valid(raw) && len(raw) <= 1<<20 {
		for _, b := range raw {
			if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
				return ""
			}
		}
		return string(raw)
	}
	return ""
}

func documentText(raw []byte) ([]byte, bool) {
	for _, field := range []uint64{2, 3, 2} {
		value, ok := protobufBytes(raw, field)
		if !ok {
			return nil, false
		}
		raw = value
	}
	return raw, true
}

// protobufBytes skips unknown fields and validates the complete message.
// A duplicate target field or malformed framing fails closed.
func protobufBytes(raw []byte, field uint64) ([]byte, bool) {
	var value []byte
	found := false
	for len(raw) > 0 {
		tag, n := binary.Uvarint(raw)
		if n <= 0 || tag>>3 == 0 {
			return nil, false
		}
		raw = raw[n:]
		var payload []byte
		switch tag & 7 {
		case 0:
			_, n = binary.Uvarint(raw)
			if n <= 0 {
				return nil, false
			}
			raw = raw[n:]
		case 1:
			if len(raw) < 8 {
				return nil, false
			}
			raw = raw[8:]
		case 2:
			length, n := binary.Uvarint(raw)
			if n <= 0 || length > uint64(len(raw)-n) {
				return nil, false
			}
			raw = raw[n:]
			payload = raw[:int(length)]
			raw = raw[int(length):]
		case 5:
			if len(raw) < 4 {
				return nil, false
			}
			raw = raw[4:]
		default:
			return nil, false
		}
		if tag>>3 == field {
			if found || tag&7 != 2 {
				return nil, false
			}
			value, found = payload, true
		}
	}
	return value, found
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
