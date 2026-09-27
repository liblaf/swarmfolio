package metainfo

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestInfoHashHashesExactInfoDictionary(t *testing.T) {
	t.Parallel()
	info := []byte("d6:lengthi1e4:name1:xe")
	data := append([]byte("d4:info"), info...)
	data = append(data, 'e')
	wantSum := sha1.Sum(info)
	want := hex.EncodeToString(wantSum[:])
	got, err := InfoHash(data)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("InfoHash() = %q, want %q", got, want)
	}
}

func TestInspectReportsQBitTorrentIdentityAndPayloadSize(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		info []byte
		size int64
		v2   bool
	}{
		{"v1 single file", []byte("d6:lengthi3e4:name1:xe"), 3, false},
		{"v1 multi file", []byte("d5:filesld6:lengthi2eed6:lengthi3eee4:name1:xe"), 5, false},
		{"v1 padding file", []byte("d5:filesld4:attr1:p6:lengthi3eee4:name1:xe"), 0, false},
		{"v2 single file", []byte("d9:file treed4:filed0:d6:lengthi3eeee12:meta versioni2e4:name1:x12:piece lengthi16384ee"), 3, true},
		{"hybrid", []byte("d9:file treed4:filed0:d6:lengthi3eeee6:lengthi3e12:meta versioni2e4:name1:x12:piece lengthi16384ee"), 3, true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			data := append(append([]byte("d4:info"), test.info...), 'e')
			got, err := Inspect(data)
			if err != nil {
				t.Fatal(err)
			}
			if got.Size != test.size {
				t.Fatalf("size = %d, want %d", got.Size, test.size)
			}
			if test.v2 {
				sum := sha256.Sum256(test.info)
				want := hex.EncodeToString(sum[:20])
				if got.Hash != want {
					t.Fatalf("hash = %q, want %q", got.Hash, want)
				}
			}
		})
	}
}

func TestInfoHashRejectsMalformedData(t *testing.T) {
	t.Parallel()
	for _, data := range [][]byte{
		nil,
		[]byte("l4:infoe"),
		[]byte("d4:name1:xe"),
		[]byte("d4:infod1:x1:ye"),
		[]byte("d4:infod1:x1:yeed4:infodee"),
		[]byte("d4:info1:xe"),
		[]byte("d4:infod6:lengthi-1e4:name1:xee"),
		[]byte("d4:infod12:meta versioni3e4:name1:xee"),
		[]byte("d4:info" + strings.Repeat("le", maxNodes+1) + "e"),
	} {
		if _, err := InfoHash(data); err == nil {
			t.Fatalf("InfoHash(%q) succeeded", data)
		}
	}
}
