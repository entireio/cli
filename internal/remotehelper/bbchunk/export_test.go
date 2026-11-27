package bbchunk

import "testing"

// SampleChunked is sampleChunked for the external tests of the encoder in
// package bbchunktest.
var SampleChunked = sampleChunked

// FetchEntry is a valid fetch vector with its parsed recipe.
type FetchEntry struct {
	Name   string
	Hex    string
	Size   int64
	Recipe *ChunkedPayload
}

// ValidFetchEntries parses and checks every valid fetch vector.
func ValidFetchEntries(t *testing.T) []FetchEntry {
	t.Helper()
	v := loadFetchVectors(t)
	out := make([]FetchEntry, 0, len(v.Entries))
	for _, e := range v.Entries {
		out = append(out, FetchEntry{Name: e.Name, Hex: e.Hex, Size: e.Size, Recipe: checkFetchEntry(t, e)})
	}
	return out
}
