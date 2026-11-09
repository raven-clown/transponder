// Package tokens estimates token counts when a provider reports none.
package tokens

// Estimate counts ASCII at four characters a token and every other character at one and a half
// tokens. Thai, CJK and other scripts take far more tokens per character than English, and some
// tokenizers spend more than one token on a single character, so this errs on the high side. It
// is a heuristic: a model with a small vocabulary can still use more.
func Estimate(s string) int64 {
	var ascii, other int64
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + (other*3+1)/2
}
