package server

// SetMaxUpstreamBytes lowers the upstream body limit for a test and returns a function that restores it.
func SetMaxUpstreamBytes(n int64) func() {
	old := maxUpstreamBytes
	maxUpstreamBytes = n
	return func() { maxUpstreamBytes = old }
}
