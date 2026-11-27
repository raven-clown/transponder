// Package tokens estimates token counts when a provider reports none.
package tokens

// Image is the estimate for one image in a request. The real count depends on the picture's
// size and on the model, from under a hundred tokens to a few thousand, and an image at a URL
// cannot be measured without fetching it, so every image counts the same: about what OpenAI,
// Anthropic and Gemini charge for a picture of 1024 by 1024 pixels.
const Image = 1000

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
	return FromCounts(ascii, other)
}

// FromCounts is Estimate for a text already counted: ascii characters below 0x80 and other
// characters, an invalid UTF-8 byte counting as one other.
func FromCounts(ascii, other int64) int64 {
	return (ascii+3)/4 + (other*3+1)/2
}
