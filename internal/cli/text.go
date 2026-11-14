package cli

// textPieceLen returns the number of text bytes in a buffer that was filled with 0xFF before the
// driver wrote a NUL-terminated piece into it: the position of the last NUL before the 0xFF tail.
// Embedded NULs in the text are kept, since only the final terminator is followed by 0xFF bytes.
func textPieceLen(b []byte) int {
	i := len(b) - 1
	for i >= 0 && b[i] == 0xFF {
		i--
	}
	if i >= 0 && b[i] == 0 {
		return i
	}
	return len(b) // no terminator found: the driver filled the whole buffer
}

// NormalizeDecimal turns TimesTen's decimal text into the conventional form by adding the leading
// zero TimesTen leaves out: ".25" → "0.25", "-.5" → "-0.5", ".5E-3" → "0.5E-3". Anything else is
// returned unchanged, so the function is safe to apply to every NUMBER/DECIMAL value.
func NormalizeDecimal(s string) string {
	switch {
	case len(s) > 0 && s[0] == '.':
		return "0" + s
	case len(s) > 1 && (s[0] == '-' || s[0] == '+') && s[1] == '.':
		return s[:1] + "0" + s[1:]
	}
	return s
}
