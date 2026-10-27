package upstream

import (
	"io"
	"strings"
)

// newSlowReader returns a reader that yields one byte per Read call, to exercise
// chunk boundaries in the middle of a JSON payload.
func newSlowReader(raw string) (io.Reader, *slowWriter) {
	pr, pw := io.Pipe()
	return &slowReader{r: pr}, (*slowWriter)(pw)
}

type slowReader struct{ r io.Reader }

func (s *slowReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return s.r.Read(p[:1])
}

type slowWriter io.PipeWriter

func (w *slowWriter) Write(b []byte) (int, error) {
	n := 0
	for _, c := range b {
		m, err := (*io.PipeWriter)(w).Write([]byte{c})
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (w *slowWriter) Close() error { return (*io.PipeWriter)(w).Close() }

var _ = strings.TrimSpace
