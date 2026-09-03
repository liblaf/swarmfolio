package metainfo

import (
	"crypto/sha1"
	"encoding/hex"
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

func TestInfoHashRejectsMalformedData(t *testing.T) {
	t.Parallel()
	for _, data := range [][]byte{
		nil,
		[]byte("l4:infoe"),
		[]byte("d4:name1:xe"),
		[]byte("d4:infod1:x1:ye"),
		[]byte("d4:infod1:x1:yeed4:infodee"),
	} {
		if _, err := InfoHash(data); err == nil {
			t.Fatalf("InfoHash(%q) succeeded", data)
		}
	}
}
