// byte slice, avoiding an extra import of bytes.Reader-adjacent packages
// spread across files (kept isolated for clarity/testability).
package main

import "io"

// newBytesReader returns an io.Reader over b.
func newBytesReader(b []byte) io.Reader {
	return &sliceReader{data: b}
}

type sliceReader struct {
	data []byte
	pos  int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.pos:])
	s.pos += n
	return n, nil
}
