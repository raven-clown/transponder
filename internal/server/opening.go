package server

// maxOpening caps the events held back before a stream's first output.
const maxOpening = 64

// open reads the opening of a stream: the events that carry no output yet, such as a role
// chunk, message_start, a ping or response.created. They are held back until the first event
// that carries output, so a stream that ends or sends an error before then fails like one
// that never began, and the next target can answer in full. failure returns the error an
// event stands for, or nil. The returned next yields the held events, then reads on.
func open[T any](read func() (T, error), opening func(T) bool, failure func(T) error) (next func() (T, error), err error) {
	var held []T
	for {
		ev, err := read()
		if err != nil {
			return nil, err
		}
		if err := failure(ev); err != nil {
			return nil, err
		}
		held = append(held, ev)
		if !opening(ev) || len(held) == maxOpening {
			break
		}
	}
	return func() (T, error) {
		if len(held) > 0 {
			ev := held[0]
			held = held[1:]
			return ev, nil
		}
		return read()
	}, nil
}

func noFailure[T any](T) error { return nil }
