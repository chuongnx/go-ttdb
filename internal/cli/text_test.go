package cli

import "testing"

func TestNormalizeDecimal(t *testing.T) {
	for in, want := range map[string]string{
		".25":     "0.25",
		"-.5":     "-0.5",
		"+.5":     "+0.5",
		".5E-3":   "0.5E-3",
		"-.5E+10": "-0.5E+10",
		"0.25":    "0.25",
		"-0.5":    "-0.5",
		"12.5":    "12.5",
		"-12":     "-12",
		"0":       "0",
		"1E-130":  "1E-130",
		"":        "",
		"-":       "-",
		".":       "0.",
	} {
		if got := NormalizeDecimal(in); got != want {
			t.Errorf("NormalizeDecimal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTextPieceLen(t *testing.T) {
	ff := func(s string, total int) []byte {
		b := []byte(s)
		for len(b) < total {
			b = append(b, 0xFF)
		}
		return b
	}
	for _, c := range []struct {
		name string
		buf  []byte
		want int
	}{
		{"full piece", []byte("abcd\x00"), 4},
		{"split 2-byte char", ff("aé\x00", 6), 3},     // driver stopped before a character
		{"split 4-byte char", ff("a\x00", 6), 1},      // 😀 did not fit
		{"embedded NUL kept", ff("a\x00b\x00", 7), 3}, // text "a\x00b"
		{"empty piece", ff("\x00", 4), 0},
		{"no terminator", []byte("abcd"), 4},
	} {
		if got := textPieceLen(c.buf); got != c.want {
			t.Errorf("%s: textPieceLen = %d, want %d", c.name, got, c.want)
		}
	}
}
