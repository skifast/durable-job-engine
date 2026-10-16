package api

import (
	"bytes"
	"encoding/json"
	"io"
)

func copyTo(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

// compactLen is the length of the JSON text with no extra whitespace. That is the
// size the 64 KB limit is measured on (docs/api.md, decision 3).
func compactLen(raw []byte) (int, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return 0, err
	}
	return buf.Len(), nil
}
