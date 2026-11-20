# Benchmarks

How fast transponder is, how that was measured, and how to measure it again. Every number here comes from `bench/` and the `*_bench_test.go` files in this repository, run on the machine below.

## Machine

| | |
|---|---|
| CPU | Intel Core i7-14700, 20 cores, 28 threads |
| Memory | 32 GB |
| OS | Windows 11 Pro |
| Go | 1.27.2, windows/amd64 |
| Store | SQLite, the default, in a temp directory on an NVMe disk |

The load tool, the fake provider and the gateway share the machine, so the numbers are a floor for what one box does, not a ceiling for a server with the gateway on its own. Windows timers and scheduling make the p99 at a fixed rate noisy: compare p50 and p95 first, and the direct floor row says how much of a p99 is the machine.

## Method

### Load

`go run ./bench run -bin <binary>` starts the binary with a generated config, two fake providers and a fresh SQLite file, reads the admin key it prints, and runs each scenario for a warmup and then a measured window:

- The fake provider (`bench/upstream.go`) speaks the OpenAI API. Every chat answer is 1 KB of text with fixed usage, streamed as 32 content chunks plus the role, finish and usage chunks; every embedding has 256 dimensions. The `slow` provider waits 20 ms before the first byte, like a fast real provider; the `quick` one answers at once, so a max throughput scenario measures the gateway and nothing else.
- Every caller has a limits profile (request rate, token rate, daily cost) high enough never to refuse, so each call is admitted and settled, and usage is written to SQLite as in production. Logs go to a pipe the tool throws away.
- Fixed-rate scenarios send 500 calls a second for 10 s, each call measured from when it was sent to its last byte. The latency a gateway adds is that time minus the 20 ms the provider waits. The `direct chat (floor)` row sends the same calls straight to the provider, so it shows what the client, loopback and the machine add with no gateway at all.
- Max throughput scenarios run 64 callers back to back for 10 s against the `quick` provider.
- `-count 3` runs the whole suite three times; each number is the median of the three.

Memory is the gateway process's working set after each scenario (resident set on Linux), and its peak since start in the first round.

### Go benchmarks

The hot paths each have a Go benchmark next to their code: decoding and encoding chat requests and responses, translating requests and streams between dialects, router resolve and plan, the SSE reader, token counting, limits admit and settle, usage writes and key lookups in SQLite, key authentication, and `BenchmarkGateway`, one whole call through the handler for each request path with the provider answering in process. These files only use APIs that v0.2.4 has as well, so the same files are copied into a v0.2.4 checkout and run there. Both runs use `-count 6`; `benchstat` reports the median and whether a change is significant (`~` means it is not).

## How to run it

The load suite against the current code, and against v0.2.4 from a second worktree:

```shell
go build -o transponder-next ./cmd/transponder
git worktree add ../transponder-v0.2.4 v0.2.4
(cd ../transponder-v0.2.4 && go build -o ../transponder/transponder-v0.2.4 ./cmd/transponder)
go run ./bench run -bin ./transponder-v0.2.4 -label v0.2.4 -count 3 -out v0.2.4.json
go run ./bench run -bin ./transponder-next -label next -count 3 -out next.json
go run ./bench compare v0.2.4.json next.json
```

The Go benchmarks on both, with the benchmark files copied into the old tree:

```shell
go test -run '^$' -bench . -benchmem -count 6 ./internal/... > next.txt
cp --parents $(git ls-files '*_bench_test.go' | grep -v -e telemetry -e observe -e stream_bench) ../transponder-v0.2.4/
(cd ../transponder-v0.2.4 && go test -run '^$' -bench . -benchmem -count 6 ./internal/... > ../transponder/v0.2.4.txt)
go run golang.org/x/perf/cmd/benchstat@latest v0.2.4.txt next.txt
```

Profiles under load: build with `-tags pprof`, which serves `net/http/pprof` on `TRANSPONDER_PPROF_ADDR`, and let the suite collect CPU, heap, allocation, block and mutex profiles during one scenario:

```shell
go build -tags pprof -o transponder-pprof ./cmd/transponder
go run ./bench run -bin ./transponder-pprof -only "chat, max" -pprof 127.0.0.1:6060 -profile "chat, max throughput" -profile-dir prof
go tool pprof -top transponder-pprof prof/cpu.pprof
```

`go run ./bench upstream` and `go run ./bench load` run the fake provider and the load tool on their own, against any gateway.

## Baseline: v0.2.4 and v0.3.0 before tuning

Recorded before any change made for speed, on `next/v0.3.0` at e8c035e.

### Load

| Scenario | req/s v0.2.4 | req/s v0.3.0 | p50 added v0.2.4 | p50 added v0.3.0 | p95 added v0.2.4 | p95 added v0.3.0 | Errors v0.2.4 | Errors v0.3.0 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| direct chat (floor) | 499 | 499 | 0.29 ms | 0.30 ms | 0.53 ms | 0.56 ms | 0 | 0 |
| chat | 499 | 499 | 0.81 ms | 1.26 ms | 2.79 ms | 12.96 ms | 0 | 0 |
| chat stream | 499 | 499 | 2.25 ms | 2.50 ms | 3.08 ms | 3.53 ms | 0 | 0 |
| messages stream, translated | 499 | 499 | 2.29 ms | 2.68 ms | 3.16 ms | 3.56 ms | 0 | 0 |
| embeddings | 499 | 499 | 1.01 ms | 1.39 ms | 1.91 ms | 2.00 ms | 0 | 0 |
| chat, max throughput | 5345 | 4813 | | | | | 0 | 0 |
| chat stream, max throughput | 1929 | 1816 | | | | | 4093 | 3737 |

The errors in the streamed max throughput run are 502s on both versions: the gateway ran out of local ports, because its client to the providers keeps two idle connections per host and opens a new one for every other call.

### Profiles

Under the chat max throughput load, the CPU profile of both versions is dominated by SQLite, not by the gateway's own work:

- 9% (v0.3.0) to 17% (v0.2.4) of CPU opens new SQLite connections: the read pool keeps two idle connections, so with 64 callers each key lookup opens and closes one.
- About 10% parses SQL again on every call, since no statement is prepared.
- 91% of the time requests spend blocked is waiting for the single SQLite writer, one usage insert at a time.

### Go benchmarks that were slower in v0.3.0

| Benchmark | v0.2.4 | v0.3.0 | Change | Why |
|---|---:|---:|---:|---|
| `KeyByHash` | 27.4 µs | 43.9 µs | +60% | each key row has three more columns (`version`, `source`, `updated_at`): 14 more allocations |
| `AuthenticateKey` | 31.1 µs | 44.4 µs | +43% | the same lookup |
| `Gateway/sqlite/chat` | 321 µs | 347 µs | +8% | the same lookup, plus the request id below |
| `Gateway/memory/embeddings` | 134 µs | 145 µs | +8% | 13 more allocations per call: the request id, generated and then copied on every read and write of the `X-Request-ID` key, which is not in Go's canonical form, and the identity on the request context |
| `Gateway/memory/messages_stream` | 201 µs | 217 µs | +8% | the same |
| `Gateway/memory/messages_stream_from_openai` | 299 µs | 315 µs | +5% | the same |

The dialect code is faster in v0.3.0: chat request encoding -64%, peek and rewrite -50%, stream decoding -35%, request translation -20% and stream translation -23% to -30%. Router, SSE reader, token counting and limits are the same within noise.
