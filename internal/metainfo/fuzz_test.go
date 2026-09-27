package metainfo

import "testing"

// Torrent bytes come from a remote token URL. Arbitrary input must produce an
// inspection result or an error without panicking or recursing without a bound.
func FuzzInspect(f *testing.F) {
	for _, seed := range []string{
		"d4:infod6:lengthi1e4:name1:xee",
		"d4:infod5:filesld6:lengthi1e4:pathl1:aeed6:lengthi2e4:pathl1:beee4:name1:xee",
		"d4:infod9:file treed1:xd0:d6:lengthi1eee12:meta versioni2e4:name1:xee",
		"d4:infole",
		"",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Inspect(data)
	})
}
