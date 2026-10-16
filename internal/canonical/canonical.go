// Package canonical writes JSON in the canonical form of RFC 8785 (the JSON
// Canonicalization Scheme). Two requests that carry the same data, with keys in
// a different order or with different spacing, produce the same bytes. The
// engine hashes those bytes to tell "the same request again" from "the same
// idempotency key with a different request" (ADR 005).
//
// The rules (RFC 8785, section 3.2):
//   - no spaces or newlines between tokens
//   - object keys sorted by their UTF-16 code units
//   - strings escaped only where JSON requires it
//   - numbers written the way JavaScript writes a double (ECMAScript Number::toString)
package canonical

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Marshal returns the canonical form of the JSON text in b.
// It fails on text that is not valid JSON, is not valid UTF-8, or holds a number
// that a double cannot represent (for example 1e999).
// If an object repeats a key, the last value wins, which is also what Postgres
// jsonb stores, so the hash matches the stored value.
func Marshal(b []byte) ([]byte, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep numbers as text until we format them ourselves
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF { // anything after the value is an error
		return nil, errors.New("unexpected data after the JSON value")
	}
	var out bytes.Buffer
	if err := write(&out, v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func write(out *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(x))
	case json.Number:
		s, err := formatNumber(string(x))
		if err != nil {
			return err
		}
		out.WriteString(s)
	case string:
		writeString(out, x)
	case []any:
		out.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := write(out, e); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// Sort by UTF-16 code units, not by UTF-8 bytes. They differ for
		// characters above U+FFFF (an emoji sorts before U+FB33 in UTF-16).
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		out.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			writeString(out, k)
			out.WriteByte(':')
			if err := write(out, x[k]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unexpected type %T", v)
	}
	return nil
}

func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// writeString escapes like JavaScript's JSON.stringify: the quote, the backslash
// and the control characters. Everything else is written as is (including < > &
// and non-ASCII), unlike Go's encoding/json.
func writeString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

// formatNumber writes a number the way ECMAScript Number::toString does.
func formatNumber(text string) (string, error) {
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", fmt.Errorf("number %q does not fit in a double", text)
	}
	if f == 0 {
		return "0", nil // also covers -0
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// 'e' with precision -1 gives the shortest digits that read back as the same double:
	// "d.ddde+XX". From it: the digits, and the exponent.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expText, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	k := len(digits)
	n := exp + 1 // the value is 0.<digits> x 10^n

	switch {
	case k <= n && n <= 21: // whole number: digits then zeros
		return sign + digits + strings.Repeat("0", n-k), nil
	case 0 < n && n <= 21: // decimal point inside the digits
		return sign + digits[:n] + "." + digits[n:], nil
	case -6 < n && n <= 0: // small number: 0.000ddd
		return sign + "0." + strings.Repeat("0", -n) + digits, nil
	}
	// Exponent form.
	expSign := "+"
	if n-1 < 0 {
		expSign = "-"
	}
	absExp := n - 1
	if absExp < 0 {
		absExp = -absExp
	}
	if k == 1 {
		return sign + digits + "e" + expSign + strconv.Itoa(absExp), nil
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + expSign + strconv.Itoa(absExp), nil
}
