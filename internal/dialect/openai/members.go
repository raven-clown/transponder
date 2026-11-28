package openai

import (
	"bytes"
	"encoding/json"
)

// member is one key and value of a JSON object, found in the body without decoding it.
type member struct {
	// key is the decoded key: a slice of the body, or a copy when the key holds escapes.
	key []byte
	// start and end bound the whole member, from its key's opening quote to the end of its value.
	start, end int
	value      []byte
}

// objectMembers appends the members of the JSON object in body to buf. body must be valid JSON,
// as json.Valid reports; it returns false when body is not an object.
func objectMembers(body []byte, buf []member) ([]member, bool) {
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return buf, false
	}
	i++
	for {
		i = skipSpace(body, i)
		if body[i] == '}' {
			return buf, true
		}
		start := i
		i = skipString(body, i)
		key := body[start+1 : i-1]
		if bytes.IndexByte(key, '\\') >= 0 {
			var s string
			_ = json.Unmarshal(body[start:i], &s)
			key = []byte(s)
		}
		i = skipSpace(body, i) + 1 // past the colon
		i = skipSpace(body, i)
		v := i
		i = skipValue(body, i)
		buf = append(buf, member{key: key, start: start, end: i, value: body[v:i]})
		i = skipSpace(body, i)
		if body[i] == ',' {
			i++
		}
	}
}

// lastMember returns the last member named key, which is the one encoding/json would keep.
func lastMember(ms []member, key string) (member, bool) {
	for i := len(ms) - 1; i >= 0; i-- {
		if string(ms[i].key) == key {
			return ms[i], true
		}
	}
	return member{}, false
}

// duplicateKeys reports whether two members share a key.
func duplicateKeys(ms []member) bool {
	for i := range ms {
		for j := i + 1; j < len(ms); j++ {
			if bytes.Equal(ms[i].key, ms[j].key) {
				return true
			}
		}
	}
	return false
}

func skipSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipString returns the index just past the string that starts at b[i].
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}

// skipValue returns the index just past the value that starts at b[i].
func skipValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for i < len(b) {
		switch b[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return i
		}
		i++
	}
	return i
}
