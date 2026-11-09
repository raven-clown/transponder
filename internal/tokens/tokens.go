// Package tokens estimates token counts when a provider reports none.
package tokens

// Estimate counts ASCII at four characters a token and every other character as a token of its
// own. Thai, CJK and other scripts take far more tokens per character than English, so a flat
// characters-divided-by-four rule would undercount them; this errs on the high side instead.
func Estimate(s string) int64 {
	var ascii, other int64
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}
