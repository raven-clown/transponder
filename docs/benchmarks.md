# Benchmarks

How fast transponder is, how that was measured, and how to measure it again. Every number here comes from `bench/` and the `*_bench_test.go` files in this repository, run on the machine below.

## In short

v0.3.0 does the same work per call as v0.2.4 in about the same time, and under load it does far more of it:

- At max throughput (64 callers, a provider that answers at once) v0.3.0 serves **11,300 chat calls a second, 2.6 times v0.2.4's 4,340**, and the p99 latency it adds falls from 64 ms to 14 ms. Streamed calls go from 1,630 to 2,110 a second (+29%).
- **No errors under load.** v0.2.4 answers 502 for about 1 streamed call in 7 at max throughput, and can stop with `fatal error: thread exhaustion`: it keeps two idle connections to a provider, so it runs out of local ports. v0.3.0 had 0 errors in every round.
- At 500 calls a second, the median latency v0.3.0 adds is lower on every path: 0.45 ms for chat (v0.2.4: 0.75 ms), 1.30 ms for a stream (2.19 ms), 0.78 ms for a stream translated from OpenAI to Anthropic (2.20 ms), 0.48 ms for embeddings (1.06 ms).
- The code that runs on every call, decoding, translating, routing, the SSE reader, token counting and limits, is as fast as in v0.2.4 within noise. The API key lookup is twice as fast and a usage write five times as fast when calls finish together.
- It uses a little more memory: 3 MiB more working set at rest and up to 10 MiB more at moderate load, though a lower peak under full load (59 MiB against 65), and 0.2 to 5% more bytes and 2 to 8% more allocations per call, for the request id, the caller's identity on the request, content parts that can carry images, and three more columns per API key. Metrics, on by default, cost about 0.4 µs a call; tracing, off by default, about 11 µs.

Why: v0.2.4 and an untuned v0.3.0 both spent most of their time on SQLite and on connections, not on their own work. Four changes fixed that: idle connections to providers are kept (#148), SQLite read connections are kept and the key lookup is prepared once (#149), usage records of calls that finish together go in one transaction (#150), and the router reads its settings without a lock (#152). #151 removes six allocations per call from the request id.

## Machine

| | |
|---|---|
| CPU | Intel Core i7-14700, 20 cores, 28 threads |
| Memory | 32 GB |
| OS | Windows 11 Pro |
| Go | 1.27.2, windows/amd64 |
| Store | SQLite, the default, in a temp directory on an NVMe disk |

The load tool, the fake provider and the gateway share the machine, so the numbers are a floor for what one box does, not a ceiling for a server with the gateway on its own. Windows timers and scheduling make the p99 at a fixed rate noisy: compare p50 and p95 first, and the direct floor row says how much of a p99 is the machine.

This CPU mixes performance and efficiency cores, and the same benchmark can differ by 30% or more from one hour to the next. Two versions are therefore always measured in turns, one run of each after the other, never one version now and the other later. Measured apart, the dialect code looked 20 to 60% faster in v0.3.0; measured in turns it is the same.

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

The hot paths each have a Go benchmark next to their code: decoding and encoding chat requests and responses, translating requests and streams between dialects, router resolve and plan, the SSE reader, token counting, limits admit and settle, usage writes and key lookups in SQLite, key authentication, and `BenchmarkGateway`, one whole call through the handler for each request path with the provider answering in process. These files only use APIs that v0.2.4 has as well, so the same files are copied into a v0.2.4 checkout and run there. Each package's test binary is built for both versions and the two run in turns, six times each (eight or ten where a result was close); `benchstat` reports the median and whether a change is significant (`~` means it is not).

## How to run it

The load suite against the current code, and against v0.2.4 from a second worktree:

```shell
go build -o transponder-next ./cmd/transponder
git worktree add ../transponder-v0.2.4 v0.2.4
(cd ../transponder-v0.2.4 && go build -o ../transponder/transponder-v0.2.4 ./cmd/transponder)
go build -o bench-tool ./bench
for i in 1 2 3; do
  ./bench-tool run -bin ./transponder-v0.2.4 -label v0.2.4 -out v0.2.4-$i.json
  ./bench-tool run -bin ./transponder-next -label next -out next-$i.json
done
./bench-tool merge v0.2.4.json v0.2.4-1.json v0.2.4-2.json v0.2.4-3.json
./bench-tool merge next.json next-1.json next-2.json next-3.json
./bench-tool compare v0.2.4.json next.json
```

Runs that follow each other share the machine's local ports: a version that runs out of them leaves sockets waiting to close for a few minutes, which can cost the next run errors. `-log file` keeps a run's gateway log, stderr included, to check.

The Go benchmarks on both, with the benchmark files copied into the old tree and the two test binaries of a package taking turns:

```shell
cp --parents $(git ls-files '*_bench_test.go' | grep -v -e telemetry -e observe -e stream_bench) ../transponder-v0.2.4/
go test -c -o server-next.test ./internal/server
(cd ../transponder-v0.2.4 && go test -c -o ../transponder/server-v0.2.4.test ./internal/server)
for i in 1 2 3 4 5 6; do
  ./server-v0.2.4.test -test.run '^$' -test.bench . -test.benchmem >> v0.2.4.txt
  ./server-next.test -test.run '^$' -test.bench . -test.benchmem >> next.txt
done
go run golang.org/x/perf/cmd/benchstat@latest v0.2.4.txt next.txt
```

Do the same for each package with a `*_bench_test.go`.

Profiles under load: build with `-tags pprof`, which serves `net/http/pprof` on `TRANSPONDER_PPROF_ADDR`, and let the suite collect CPU, heap, allocation, block and mutex profiles during one scenario:

```shell
go build -tags pprof -o transponder-pprof ./cmd/transponder
go run ./bench run -bin ./transponder-pprof -only "chat, max" -pprof 127.0.0.1:6060 -profile "chat, max throughput" -profile-dir prof
go tool pprof -top transponder-pprof prof/cpu.pprof
```

`go run ./bench upstream` and `go run ./bench load` run the fake provider and the load tool on their own, against any gateway.

## v0.3.0 against v0.2.4

v0.3.0 here is `next/v0.3.0` at 728b549 with #148, #149, #150 and #151 merged in. The two versions took turns, three rounds each, and each number is the median of the three. #152 (the router) came after the load runs; its effect is in the Go benchmarks below and is too small to show under load.

### Load

| Scenario | req/s v0.2.4 | req/s v0.3.0 | p50 added v0.2.4 | p50 added v0.3.0 | p95 added v0.2.4 | p95 added v0.3.0 | p99 added v0.2.4 | p99 added v0.3.0 | Errors v0.2.4 | Errors v0.3.0 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| direct chat (floor) | 499 | 499 | 0.27 ms | 0.27 ms | 0.50 ms | 0.56 ms | 0.63 ms | 3.01 ms | 0 | 0 |
| chat | 499 | 499 | 0.75 ms | 0.45 ms | 1.57 ms | 3.32 ms | 2.09 ms | 43.47 ms | 0 | 0 |
| chat stream | 499 | 499 | 2.19 ms | 1.30 ms | 3.23 ms | 2.49 ms | 4.12 ms | 3.13 ms | 0 | 0 |
| messages stream, translated | 499 | 499 | 2.20 ms | 0.78 ms | 3.31 ms | 1.67 ms | 4.03 ms | 2.38 ms | 0 | 0 |
| embeddings | 499 | 499 | 1.06 ms | 0.48 ms | 1.92 ms | 1.94 ms | 2.45 ms | 2.33 ms | 0 | 0 |
| chat, max throughput | 4,343 | **11,308** | 10.59 ms | 5.25 ms | 41.90 ms | 10.37 ms | 63.71 ms | 13.69 ms | 0 | 0 |
| chat stream, max throughput | 1,629 | **2,105** | 30.83 ms | 29.04 ms | 79.68 ms | 51.69 ms | 118.23 ms | 62.95 ms | 8,421 | 0 |

The fixed-rate rows run at the rate asked for on both, so their calls per second are equal by design; the latency columns are what differs. In the max throughput rows every call is the gateway's own work, so the p50 there is the whole call.

The v0.3.0 chat p95 and p99 at 500/s come from two rounds that each had a burst of slow calls; the direct floor had one in the same minutes (p99 55 ms in round two). Run again on its own, three rounds in one process, v0.3.0's chat had p95 0.57 to 0.88 ms and p99 0.99 to 1.36 ms added. In the same check v0.2.4 stopped after its first round's streamed max throughput scenario with `fatal error: thread exhaustion`.

Working set of the gateway process:

| | v0.2.4 | v0.3.0 |
|---|---:|---:|
| idle, after start | 17 MiB | 20 MiB |
| after the fixed-rate scenarios | 33 MiB | 43 MiB |
| after the max throughput scenarios | 71 MiB | 56 MiB |
| peak | 65 MiB | 59 MiB |

### Go benchmarks

Time, bytes and allocations per operation, the two versions taking turns, i7-14700:

| Benchmark | Time v0.2.4 | Time v0.3.0 | Change | B/op v0.2.4 | B/op v0.3.0 | allocs v0.2.4 | allocs v0.3.0 |
|---|---:|---:|---:|---:|---:|---:|---:|
| `Gateway/memory/chat` | 55.1 µs | 55.2 µs | ~ | 30.3 KiB | 31.0 KiB | 259 | 266 |
| `Gateway/memory/chat_stream` | 191 µs | 191 µs | ~ | 94.4 KiB | 95.2 KiB | 680 | 703 |
| `Gateway/memory/chat_stream_from_anthropic` | 224 µs | 237 µs | ~ | 108.7 KiB | 108.8 KiB | 819 | 838 |
| `Gateway/memory/messages_stream` | 174 µs | 177 µs | ~ | 94.6 KiB | 94.8 KiB | 724 | 746 |
| `Gateway/memory/messages_stream_from_openai` | 174 µs | 172 µs | ~ (10 runs) | 116.4 KiB | 116.2 KiB | 925 | 942 |
| `Gateway/memory/embeddings` | 103 µs | 101 µs | ~ (10 runs) | 70.7 KiB | 70.0 KiB | 178 | 185 |
| `Gateway/sqlite/chat` | 266 µs | 234 µs | -12% | 34.6 KiB | 36.2 KiB | 356 | 384 |
| `AuthenticateKey` (parallel) | 22.8 µs | 9.6 µs | -58% | 1.8 KiB | 2.2 KiB | 59 | 69 |
| `KeyByHash` (parallel) | 19.8 µs | 10.4 µs | -47% | 1.7 KiB | 2.1 KiB | 57 | 68 |
| `RecordUsage/serial` | 66.8 µs | 53.4 µs | -20% | 2.3 KiB | 2.8 KiB | 39 | 45 |
| `RecordUsage/parallel` | 77.2 µs | 13.6 µs | -82% | 2.4 KiB | 2.3 KiB | 42 | 33 |
| `AdmitSettle` | 2.14 µs | 1.96 µs | ~ | 1.40 KiB | 1.35 KiB | 25 | 22 |
| `Plan/team-0-model` (#152) | 126 ns | 134 ns | +5.9% | 224 B | 240 B | 2 | 2 |
| `Plan/team-19-model` (#152) | 559 ns | 579 ns | ~ | 224 B | 240 B | 2 | 2 |
| `DecodeChatRequest` | 9.64 µs | 10.17 µs | ~ | 7.1 KiB | 7.3 KiB | 32 | 36 |
| `EncodeChatRequest` | 5.11 µs | 5.35 µs | ~ | 3.1 KiB | 3.1 KiB | 20 | 20 |
| `PeekAndRewrite` | 15.2 µs | 15.2 µs | ~ | 5.2 KiB | 5.2 KiB | 45 | 45 |
| `StreamDecode` (OpenAI) | 59.4 µs | 56.3 µs | ~ | 7.5 KiB | 7.5 KiB | 149 | 149 |
| `StreamEncode` (OpenAI) | 54.5 µs | 51.7 µs | -5% | 25.1 KiB | 25.1 KiB | 253 | 253 |
| `TranslateRequest/anthropic_to_openai` | 13.1 µs | 13.5 µs | ~ | 6.3 KiB | 6.6 KiB | 49 | 53 |
| `TranslateStream/anthropic_to_openai` | 89.3 µs | 98.9 µs | ~ | 34.0 KiB | 34.0 KiB | 372 | 373 |
| `TranslateStream/openai_to_anthropic` | 51.9 µs | 52.4 µs | ~ | 30.2 KiB | 30.2 KiB | 440 | 440 |
| `Reader` (SSE, 32 events) | 9.60 µs | 9.54 µs | ~ | 17.7 KiB | 17.7 KiB | 104 | 104 |
| `Estimate/english_1KB` | 506 ns | 493 ns | ~ | 0 | 0 | 0 | 0 |

Memory per call through the whole gateway is 70 to 116 KiB on both versions, depending on the path.

### What is slower than v0.2.4, and why

- **`Plan/team-0-model`, +5.9% (8 ns, 16 B).** Access per route: each plan now carries the route's `allow` list and, per target, whether the model name came from the caller. That is about 0.005% of a call. The rest of `Plan` and `Resolve` is as fast as v0.2.4 since #152.
- **Allocations per call, +2 to +8% (7 to 28 more).** The request id: generated, stored on the request context and sent to the provider (four allocations after #151). The caller's identity on the request context (two). Chat messages decoded into content parts that can carry images (four per request). Three more columns per API key: `version`, `source`, `updated_at` (eleven per key lookup, which is still twice as fast as v0.2.4's). The time per call is unchanged within noise.
- **3 to 10 MiB more working set below full load.** The metrics registry, the admin API and its OpenAPI document, and the SQLite read connections now kept open, up to one per CPU, each with its page cache. Under full load v0.3.0 peaks lower, since it no longer opens a connection per call.
- **Metrics and tracing.** With the echo provider and nothing else (`BenchmarkChat`), metrics add 0.4 µs, 0.4 KiB and 5 allocations a call (20.7 µs off, 21.2 µs on, within noise). Tracing with an exporter adds 11 µs, 3 KiB and 9 allocations (32.1 µs); it is off unless `otel.endpoint` is set.

Nothing else is more than 5% slower.

## Baseline: v0.2.4 and v0.3.0 before tuning

Recorded before any change made for speed, on `next/v0.3.0` at e8c035e. These runs did not yet take turns: each version ran on its own, so the Go benchmark numbers in this section carry the drift described under Machine.

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

The same runs showed the dialect code 20 to 64% faster in v0.3.0. Measured in turns it is the same as in v0.2.4: that difference was the machine drifting between the two runs, which is why every result above was measured in turns. The key lookup, the request id and the router were slower in turns too, and are fixed in #149, #151 and #152.
