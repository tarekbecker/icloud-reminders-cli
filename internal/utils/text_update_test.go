package utils

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"testing"
	"unicode/utf16"
)

func TestTextReplacementPreservesOldIDsAndTombstonesEveryPreviousInsert(t *testing.T) {
	encoded, err := EncodeTitle("Old 🥰")
	if err != nil {
		t.Fatal(err)
	}
	previous := encoded
	for _, text := range []string{"New 🧪", "Third ❤️", "Fourth 👩‍💻"} {
		encoded, err = UpdateTitleDocument(previous, text)
		if err != nil {
			t.Fatal(err)
		}
		if ExtractTitle(encoded) != text {
			t.Fatal("replacement returned old visible text")
		}
		raw, _ := compressedDocument(encoded)
		doc, _ := wireBlob(raw, 2)
		note, _ := wireBlob(doc, 3)
		fields, _ := wireFields(note)
		live := uint64(0)
		tombstones := 0
		oldIDs := map[string]bool{}
		oldRaw, _ := compressedDocument(previous)
		oldDoc, _ := wireBlob(oldRaw, 2)
		oldNote, _ := wireBlob(oldDoc, 3)
		oldFields, _ := wireFields(oldNote)
		idSet := func(note []byte, fs []wireField) map[string]bool {
			metadata, _ := wireBlob(note, 4)
			clocks, _ := wireFields(metadata)
			uuids := [][]byte{}
			for _, f := range clocks {
				if f.number == 1 {
					uuid, _ := wireBlob(f.bytes, 1)
					uuids = append(uuids, uuid)
				}
			}
			out := map[string]bool{}
			for _, f := range fs {
				if f.number != 3 {
					continue
				}
				id, _ := wireBlob(f.bytes, 1)
				replica, _ := wireInteger(id, 1)
				offset, _ := wireInteger(id, 2)
				if replica > 0 {
					out[string(uuids[replica-1])+string(EncodeVarint(offset))] = true
				}
			}
			return out
		}
		oldIDs = idSet(oldNote, oldFields)
		newIDs := idSet(note, fields)
		for id := range oldIDs {
			if !newIDs[id] {
				t.Fatal("replacement discarded old character identities")
			}
		}
		for _, f := range fields {
			if f.number != 3 {
				continue
			}
			length, _ := wireInteger(f.bytes, 2)
			deleted, _ := wireInteger(f.bytes, 4)
			if deleted == 0 {
				live += length
			} else {
				tombstones++
			}
		}
		if live != uint64(len(utf16.Encode([]rune(text)))) || tombstones < len(oldIDs) {
			t.Fatal("old inserts were not tombstoned")
		}
		previous = encoded
	}
	noOp, err := UpdateTitleDocument(encoded, "Fourth 👩‍💻")
	if err != nil || noOp != encoded {
		t.Fatal("no-op created another CRDT replica")
	}
}

func TestNativeZlibArchivesRemainReadableAndEditable(t *testing.T) {
	encoded, _ := EncodeTitle("old 🥰")
	raw, _ := compressedDocument(encoded)
	var b bytes.Buffer
	zip := zlib.NewWriter(&b)
	_, _ = zip.Write(raw)
	_ = zip.Close()
	encoded = base64.StdEncoding.EncodeToString(b.Bytes())
	if ExtractTitle(encoded) != "old 🥰" {
		t.Fatal("native deflate archive was not decoded")
	}
	updated, err := UpdateTitleDocument(encoded, "new 🧪")
	if err != nil || ExtractTitle(updated) != "new 🧪" {
		t.Fatal("native deflate update failed")
	}
}

func TestReplacementRetainsBothBranchesOfMergedNativeDocument(t *testing.T) {
	// Two independently inserted copies share one end sentinel. This represents
	// the native merge observed after replacing an archive with a fresh replica.
	node := func(replica, length uint64, end bool, children ...uint64) []byte {
		offset := int64(0)
		if end {
			offset = -1
		}
		out := EncodeField(1, 2, position(replica, offset))
		out = append(out, EncodeField(2, 0, length)...)
		out = append(out, EncodeField(3, 2, position(replica, offset))...)
		for _, child := range children {
			out = append(out, EncodeField(5, 0, child)...)
		}
		return out
	}
	metadata := []byte{}
	for _, replica := range []byte{0x11, 0x22} {
		clock := EncodeField(1, 2, bytes.Repeat([]byte{replica}, 16))
		clock = append(clock, EncodeField(2, 2, EncodeField(1, 0, uint64(3)))...)
		clock = append(clock, EncodeField(2, 2, EncodeField(1, 0, uint64(1)))...)
		metadata = append(metadata, EncodeField(1, 2, clock)...)
	}
	note := EncodeField(2, 2, []byte("oldold"))
	for _, data := range [][]byte{node(0, 0, false, 1, 2), node(1, 3, false, 3), node(2, 3, false, 3), node(0, 0, true)} {
		note = append(note, EncodeField(3, 2, data)...)
	}
	note = append(note, EncodeField(4, 2, metadata)...)
	note = append(note, EncodeField(5, 2, EncodeField(1, 0, uint64(6)))...)
	encoded, _ := encodeDocument(EncodeField(2, 2, EncodeField(3, 2, note)))
	updated, err := UpdateTitleDocument(encoded, "new 🥰")
	if err != nil || ExtractTitle(updated) != "new 🥰" {
		t.Fatal("merged native graph could not be updated")
	}
	updated, err = UpdateTitleDocument(updated, "last 🧪")
	if err != nil || ExtractTitle(updated) != "last 🧪" {
		t.Fatal("merged history could not be updated again")
	}
}

func TestTextReplacementPreservesExtensionsAndRejectsMalformedGraphs(t *testing.T) {
	encoded, _ := EncodeTitle("old")
	raw, _ := compressedDocument(encoded)
	doc, _ := wireBlob(raw, 2)
	note, _ := wireBlob(doc, 3)
	extension := EncodeField(77, 2, []byte("opaque extension"))
	note = append(note, extension...)
	doc, _ = replaceWire(doc, 3, EncodeField(3, 2, note))
	raw, _ = replaceWire(raw, 2, EncodeField(2, 2, doc))
	encoded, _ = encodeDocument(raw)
	updated, err := UpdateTitleDocument(encoded, "new 🥰")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := compressedDocument(updated)
	if !bytes.Contains(data, extension) {
		t.Fatal("unknown document property lost")
	}
	root := []byte{}
	root = append(root, EncodeField(1, 2, position(0, 0))...)
	root = append(root, EncodeField(2, 0, uint64(0))...)
	root = append(root, EncodeField(3, 2, position(0, 0))...)
	root = append(root, EncodeField(5, 0, uint64(0))...)
	note, _ = replaceWire(note, 3, EncodeField(3, 2, root))
	doc, _ = replaceWire(doc, 3, EncodeField(3, 2, note))
	raw, _ = replaceWire(raw, 2, EncodeField(2, 2, doc))
	bad, _ := encodeDocument(raw)
	if _, err := UpdateTitleDocument(bad, "new"); err == nil {
		t.Fatal("invalid graph was replaced without a guard")
	}
}
