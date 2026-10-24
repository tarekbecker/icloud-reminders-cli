package utils

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"unicode/utf16"
	"unicode/utf8"
)

type wireField struct {
	number, kind, integer uint64
	bytes, raw            []byte
}

func wireFields(b []byte) ([]wireField, error) {
	fields := []wireField{}
	for len(b) > 0 {
		original := b
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 {
			return nil, fmt.Errorf("invalid document framing")
		}
		b = b[n:]
		f := wireField{number: tag >> 3, kind: tag & 7}
		switch f.kind {
		case 0:
			f.integer, n = binary.Uvarint(b)
			if n <= 0 {
				return nil, fmt.Errorf("invalid document integer")
			}
			b = b[n:]
		case 2:
			length, n := binary.Uvarint(b)
			if n <= 0 || length > uint64(len(b)-n) {
				return nil, fmt.Errorf("invalid document length")
			}
			f.bytes = b[n : n+int(length)]
			b = b[n+int(length):]
		case 1:
			if len(b) < 8 {
				return nil, fmt.Errorf("invalid document fixed64")
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return nil, fmt.Errorf("invalid document fixed32")
			}
			b = b[4:]
		default:
			return nil, fmt.Errorf("unsupported document wire type")
		}
		f.raw = original[:len(original)-len(b)]
		fields = append(fields, f)
	}
	return fields, nil
}
func wireBlob(b []byte, number uint64) ([]byte, error) {
	fields, err := wireFields(b)
	if err != nil {
		return nil, err
	}
	var out []byte
	found := false
	for _, f := range fields {
		if f.number == number {
			if found || f.kind != 2 {
				return nil, fmt.Errorf("unsupported document field")
			}
			out = f.bytes
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("missing document field")
	}
	return out, nil
}
func wireInteger(b []byte, number uint64) (uint64, error) {
	fields, err := wireFields(b)
	if err != nil {
		return 0, err
	}
	var out uint64
	found := false
	for _, f := range fields {
		if f.number == number {
			if found || f.kind != 0 {
				return 0, fmt.Errorf("unsupported document integer field")
			}
			out = f.integer
			found = true
		}
	}
	return out, nil
}
func replaceWire(b []byte, number uint64, replacement []byte) ([]byte, error) {
	fields, err := wireFields(b)
	if err != nil {
		return nil, err
	}
	out := []byte{}
	done := false
	for _, f := range fields {
		if f.number == number {
			if !done {
				out = append(out, replacement...)
				done = true
			}
		} else {
			out = append(out, f.raw...)
		}
	}
	if !done {
		out = append(out, replacement...)
	}
	return out, nil
}
func compressedDocument(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid encoded document")
	}
	if isCompressedDocument(raw) {
		var gz io.ReadCloser
		var err error
		if raw[0] == 31 && raw[1] == 139 {
			gz, err = gzip.NewReader(bytes.NewReader(raw))
		} else {
			gz, err = zlib.NewReader(bytes.NewReader(raw))
		}
		if err != nil {
			return nil, fmt.Errorf("invalid compressed document")
		}
		defer gz.Close()
		raw, err = io.ReadAll(io.LimitReader(gz, (1<<20)+1))
		if err != nil {
			return nil, fmt.Errorf("incomplete compressed document")
		}
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("document exceeds bounds")
	}
	return raw, nil
}

func isCompressedDocument(raw []byte) bool {
	return len(raw) >= 2 && (raw[0] == 31 && raw[1] == 139 || raw[0]&15 == 8 && raw[0]>>4 <= 7 && (uint16(raw[0])<<8|uint16(raw[1]))%31 == 0)
}
func encodeDocument(raw []byte) (string, error) {
	if len(raw) > 1<<20 {
		return "", fmt.Errorf("document exceeds bounds")
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write(raw); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.Bytes()), nil
}

// UpdateTitleDocument replaces the visible text while retaining its CRDT history.
// Old character IDs become tombstones; a distinct replica contributes the new
// text and a later style clock. Replacing the archive with a fresh document
// loses deletion history and lets offline Apple clients append the old text.
func UpdateTitleDocument(encoded, text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("text is not valid UTF-8")
	}
	raw, err := compressedDocument(encoded)
	if err != nil {
		return "", err
	}
	document, err := wireBlob(raw, 2)
	if err != nil {
		return "", err
	}
	outerVersion, err := wireInteger(raw, 1)
	if err != nil || outerVersion != 0 {
		return "", fmt.Errorf("unsupported outer serialization version")
	}
	version, err := wireInteger(document, 1)
	if err != nil || version != 0 {
		return "", fmt.Errorf("unsupported serialization version")
	}
	minimum, err := wireInteger(document, 2)
	if err != nil || minimum != 0 {
		return "", fmt.Errorf("unsupported minimum version")
	}
	note, err := wireBlob(document, 3)
	if err != nil {
		return "", err
	}
	oldText, err := wireBlob(note, 2)
	if err != nil || !utf8.Valid(oldText) {
		return "", fmt.Errorf("invalid document text")
	}
	fields, err := wireFields(note)
	if err != nil {
		return "", err
	}
	nodes := [][]byte{}
	for _, f := range fields {
		if f.number == 3 {
			if f.kind != 2 {
				return "", fmt.Errorf("unsupported substring")
			}
			nodes = append(nodes, f.bytes)
		}
	}
	if len(nodes) < 2 || len(nodes) > 10000 {
		return "", fmt.Errorf("unsupported substring history")
	}
	metadata, err := wireBlob(note, 4)
	if err != nil {
		return "", err
	}
	clockFields, err := wireFields(metadata)
	if err != nil {
		return "", err
	}
	type clock struct{ uuid, data []byte }
	clocks := []clock{}
	for _, f := range clockFields {
		if f.number == 1 {
			if f.kind != 2 {
				return "", fmt.Errorf("unsupported vector clock")
			}
			uuid, err := wireBlob(f.bytes, 1)
			if err != nil || len(uuid) != 16 || bytes.Equal(uuid, make([]byte, 16)) {
				return "", fmt.Errorf("invalid replica")
			}
			clocks = append(clocks, clock{uuid, f.bytes})
		}
	}
	if len(clocks) == 0 || len(clocks) > 10000 {
		return "", fmt.Errorf("unsupported vector clock")
	}
	sort.Slice(clocks, func(i, j int) bool { return bytes.Compare(clocks[i].uuid, clocks[j].uuid) < 0 })
	for i := 1; i < len(clocks); i++ {
		if bytes.Equal(clocks[i-1].uuid, clocks[i].uuid) {
			return "", fmt.Errorf("duplicate replica")
		}
	}
	liveUnits := uint64(0)
	maxStyle := uint64(0)
	children := make([][]uint64, len(nodes))
	rootIndex := -1
	endIndex := -1
	for i, node := range nodes {
		id, err := wireBlob(node, 1)
		if err != nil {
			return "", err
		}
		replica, err := wireInteger(id, 1)
		if err != nil || replica > uint64(len(clocks)) {
			return "", fmt.Errorf("invalid character replica")
		}
		offset, err := wireInteger(id, 2)
		if err != nil || offset > 0xffffffff {
			return "", fmt.Errorf("invalid character clock")
		}
		length, err := wireInteger(node, 2)
		if err != nil || length > 0xffffffff {
			return "", fmt.Errorf("invalid substring length")
		}
		style, err := wireBlob(node, 3)
		if err != nil {
			return "", err
		}
		sr, err := wireInteger(style, 1)
		if err != nil || sr > uint64(len(clocks)) {
			return "", fmt.Errorf("invalid style replica")
		}
		sc, err := wireInteger(style, 2)
		if err != nil {
			return "", err
		}
		if sr != 0 && sc > maxStyle {
			maxStyle = sc
		}
		tombstone, err := wireInteger(node, 4)
		if err != nil || tombstone > 1 {
			return "", fmt.Errorf("invalid tombstone")
		}
		if tombstone == 0 {
			liveUnits += length
		}
		if replica == 0 {
			if length != 0 {
				return "", fmt.Errorf("invalid sentinel extent")
			}
			if offset == 0 {
				if rootIndex != -1 {
					return "", fmt.Errorf("duplicate root")
				}
				rootIndex = i
			} else if offset == 0xffffffff {
				if endIndex != -1 {
					return "", fmt.Errorf("duplicate sentinel")
				}
				endIndex = i
			} else {
				return "", fmt.Errorf("unsupported sentinel")
			}
		}
		nodeFields, err := wireFields(node)
		if err != nil {
			return "", err
		}
		for _, f := range nodeFields {
			if f.number != 5 {
				continue
			}
			values := []uint64{}
			if f.kind == 0 {
				values = append(values, f.integer)
			} else if f.kind == 2 {
				b := f.bytes
				for len(b) > 0 {
					v, n := binary.Uvarint(b)
					if n <= 0 {
						return "", fmt.Errorf("invalid child index")
					}
					values = append(values, v)
					b = b[n:]
				}
			} else {
				return "", fmt.Errorf("unsupported child field")
			}
			for _, v := range values {
				if v >= uint64(len(nodes)) {
					return "", fmt.Errorf("child index outside document")
				}
				children[i] = append(children[i], v)
			}
		}
	}
	if rootIndex != 0 || endIndex != len(nodes)-1 || liveUnits != uint64(len(utf16.Encode([]rune(string(oldText))))) {
		return "", fmt.Errorf("unsupported document layout")
	}
	visited := make([]bool, len(nodes))
	visiting := make([]bool, len(nodes))
	var visit func(int) error
	visit = func(i int) error {
		if visiting[i] {
			return fmt.Errorf("invalid substring graph")
		}
		if visited[i] {
			return nil
		}
		visiting[i] = true
		for _, v := range children[i] {
			if err := visit(int(v)); err != nil {
				return err
			}
		}
		visiting[i] = false
		visited[i] = true
		return nil
	}
	if err := visit(0); err != nil {
		return "", err
	}
	for _, v := range visited {
		if !v {
			return "", fmt.Errorf("unreachable substring")
		}
	}
	if string(oldText) == text {
		return encoded, nil
	}
	// Style timestamps are Lamport counters. Advance past every observed style
	// timestamp, including all clocks retained for already deleted substrings.
	for _, c := range clocks {
		fs, err := wireFields(c.data)
		if err != nil {
			return "", err
		}
		dim := 0
		for _, f := range fs {
			if f.number == 2 {
				if f.kind != 2 {
					return "", fmt.Errorf("unsupported clock dimension")
				}
				if dim == 1 {
					v, err := wireInteger(f.bytes, 1)
					if err != nil {
						return "", err
					}
					if v > maxStyle {
						maxStyle = v
					}
				}
				dim++
			}
		}
		if dim != 2 {
			return "", fmt.Errorf("unsupported clock dimensions")
		}
	}
	if maxStyle >= 0xfffffffd {
		return "", fmt.Errorf("style clock exhausted")
	}
	styleClock := maxStyle + 1
	units := uint64(len(utf16.Encode([]rune(text))))
	if units > 0xfffffffe {
		return "", fmt.Errorf("text exceeds clock bounds")
	}
	uuid := generateUUID()
	for _, c := range clocks {
		if bytes.Equal(uuid, c.uuid) {
			return "", fmt.Errorf("replica collision")
		}
	}
	all := append(append([]clock{}, clocks...), clock{uuid, nil})
	sort.Slice(all, func(i, j int) bool { return bytes.Compare(all[i].uuid, all[j].uuid) < 0 })
	replicas := map[string]uint64{}
	for i, c := range all {
		replicas[string(c.uuid)] = uint64(i + 1)
	}
	newReplica := replicas[string(uuid)]
	remap := func(pos []byte) ([]byte, error) {
		r, err := wireInteger(pos, 1)
		if err != nil {
			return nil, err
		}
		if r > 0 {
			pos, err = replaceWire(pos, 1, EncodeField(1, 0, replicas[string(clocks[r-1].uuid)]))
		}
		return pos, err
	}
	updated := [][]byte{}
	for i, node := range nodes {
		id, _ := wireBlob(node, 1)
		id, err = remap(id)
		if err != nil {
			return "", err
		}
		node, err = replaceWire(node, 1, EncodeField(1, 2, id))
		if err != nil {
			return "", err
		}
		style, _ := wireBlob(node, 3)
		style, err = remap(style)
		if err != nil {
			return "", err
		}
		length, _ := wireInteger(node, 2)
		if length > 0 {
			node, err = replaceWire(node, 4, EncodeField(4, 0, uint64(1)))
			if err != nil {
				return "", err
			}
			style = position(newReplica, int64(styleClock))
		}
		node, err = replaceWire(node, 3, EncodeField(3, 2, style))
		if err != nil {
			return "", err
		}
		links := []byte{}
		if i == 0 {
			links = append(links, EncodeField(5, 0, uint64(1))...)
		}
		for _, v := range children[i] {
			if v > 0 {
				v++
			}
			links = append(links, EncodeField(5, 0, v)...)
		}
		node, err = replaceWire(node, 5, links)
		if err != nil {
			return "", err
		}
		updated = append(updated, node)
	}
	inserted := []byte{}
	inserted = append(inserted, EncodeField(1, 2, position(newReplica, 0))...)
	inserted = append(inserted, EncodeField(2, 0, units)...)
	inserted = append(inserted, EncodeField(3, 2, position(newReplica, int64(styleClock)))...)
	for _, child := range children[0] {
		if child > 0 {
			child++
		}
		inserted = append(inserted, EncodeField(5, 0, child)...)
	}
	substrings := EncodeField(3, 2, updated[0])
	substrings = append(substrings, EncodeField(3, 2, inserted)...)
	for _, node := range updated[1:] {
		substrings = append(substrings, EncodeField(3, 2, node)...)
	}
	clockData := []byte{}
	for _, c := range all {
		data := c.data
		if bytes.Equal(c.uuid, uuid) {
			data = EncodeField(1, 2, uuid)
			data = append(data, EncodeField(2, 2, EncodeField(1, 0, units))...)
			data = append(data, EncodeField(2, 2, EncodeField(1, 0, styleClock+1))...)
		}
		clockData = append(clockData, EncodeField(1, 2, data)...)
	}
	metadata, err = replaceWire(metadata, 1, clockData)
	if err != nil {
		return "", err
	}
	note, err = replaceWire(note, 2, EncodeField(2, 2, []byte(text)))
	if err != nil {
		return "", err
	}
	note, err = replaceWire(note, 3, substrings)
	if err != nil {
		return "", err
	}
	note, err = replaceWire(note, 4, EncodeField(4, 2, metadata))
	if err != nil {
		return "", err
	}
	note, err = replaceWire(note, 5, EncodeField(5, 2, EncodeField(1, 0, units)))
	if err != nil {
		return "", err
	}
	document, err = replaceWire(document, 3, EncodeField(3, 2, note))
	if err != nil {
		return "", err
	}
	raw, err = replaceWire(raw, 2, EncodeField(2, 2, document))
	if err != nil {
		return "", err
	}
	return encodeDocument(raw)
}
