// Package batch splits document IDs into export batches.
package batch

// Split cuts ids into batches of size while more than merge IDs remain; the
// remainder (at most merge IDs) becomes one final batch.
func Split(ids []uint64, size, merge int) [][]uint64 {
	var out [][]uint64
	rest := ids
	for len(rest) > merge && len(rest) > size {
		out = append(out, rest[:size])
		rest = rest[size:]
	}
	if len(rest) > 0 {
		out = append(out, rest)
	}
	return out
}
