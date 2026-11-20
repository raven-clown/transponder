package main

import "net/http"

// idleConnsPerProvider is how many idle connections to one provider host are kept for reuse.
const idleConnsPerProvider = 512

// providerClient is the client for calls to providers. Go's default keeps two idle connections
// per host, so with more calls than that in flight to one provider every other call dials
// again; under load the gateway then runs out of local ports and answers 502.
func providerClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 0 // no cap across hosts; each host has its own below
	t.MaxIdleConnsPerHost = idleConnsPerProvider
	return &http.Client{Transport: t}
}
