# Benchmarks

How fast transponder is, how that was measured, and how to measure it again. Every number here comes from `bench/` and the `*_bench_test.go` files in this repository, run on the machine below, with both versions built by the same Go.

## In short

v0.3.0 is faster than v0.2.4 on the same machine, built with the same Go (1.26.11):

| What | v0.2.4 | v0.3.0 | Change | What made the difference |
|---|---:|---:|---:|---|
| Chat calls a second at max throughput | 5,836 | 12,235 | **2.1 times (+110%)** | usage writes batched (#150), SQLite read connections kept (#149) |
| p99 latency added at max throughput | 47.0 ms | 11.6 ms | **75% lower** | the same |
| p95 latency added at max throughput | 30.7 ms | 8.6 ms | 72% lower | the same |
| Streamed calls a second at max throughput | 1,019 | 1,982 | **+95%** | idle provider connections kept (#148) |
| Streamed calls failed at max throughput | 43,775 of 78,311 (56%) | 0 | **none** | the same: v0.2.4 runs out of local ports, and can stop with `fatal error: thread exhaustion` |
| CPU time of one streamed call relayed as is | 199 µs | 141 µs | **29% lower** | each chunk decoded once, SSE reader and frames allocate less (#154) |
| Allocations of one streamed call | 1,281 | 770 | **40% fewer** | the same |
| CPU time of one chat call | 54.4 µs | 48.2 µs | 11% lower | request body parsed fewer times (#155) |
| Allocations of one chat call | 327 | 264 | 19% fewer | #155, #151 |
| CPU time of one chat call with SQLite | 122 µs | 93 µs | 23% lower | #149, #150, #155 |
| Anthropic and translated streams (three paths) | 121 to 160 µs | 109 to 154 µs | 3 to 10% lower | #154 |
| API key check | 16.6 µs | 7.1 µs | 57% lower | #149 |
| Usage write, many calls at once | 46.4 µs | 9.2 µs | 80% lower | #150 |
| Bytes allocated per call | 70 to 127 KiB | 70 to 112 KiB | up to 23% fewer | #154, #155 |
| Working set, idle / after the max throughput runs | 17 / 93 MiB | 19 / 52 MiB | +2 MiB idle, 44% less under load | kept SQLite connections cost a little at rest; no connection churn under load |

At a steady 500 calls a second, the latency the gateway adds is the same as v0.2.4's within noise (p50 0.37 to 0.89 ms on both, depending on the path): at that rate both versions spend most of the time waiting on the provider.

The code that runs on every call and was not changed for speed (dialect decoding and encoding, routing, token counting, limits) is as fast as in v0.2.4 within noise. Two Go benchmarks remain more than 5% slower; both are explained under [What is slower](#what-is-slower-than-v024-and-why).

Why v0.3.0 is faster: profiles of both versions under load showed they spent most of their time on SQLite and on connections, not on their own work. v0.2.4 opened a SQLite connection for most key lookups, wrote each usage record in its own transaction behind a single writer, and dialed the provider again for almost every call. v0.3.0 keeps its connections, writes the usage of calls that finish together in one transaction, and on the stream path decodes and copies each event once.

## Machine

| | |
|---|---|
| CPU | Intel Core i7-14700, 20 cores (8 performance, 12 efficiency), 28 threads |
| Memory | 32 GB |
| OS | Windows 11 Pro |
| Go | 1.26.11 for both versions (`GOTOOLCHAIN=go1.26.11`), windows/amd64 |
| Store | SQLite, the default, in a temp directory on an NVMe disk |

Both binaries and all test binaries are built by the same Go, so the comparison is between the code and not the compilers. v0.2.4's release binaries were built with Go 1.26.0; v0.3.0 pins `toolchain go1.26.11` in `go.mod`.

The load tool, the fake provider and the gateway share the machine, so the numbers are a floor for what one box does, not a ceiling for a server with the gateway on its own. Windows timers and scheduling make the p99 at a fixed rate noisy: compare p50 and p95 first, and the direct floor row says how much of a p99 is the machine.

This CPU mixes performance and efficiency cores, and the same benchmark can differ by 30% or more from one hour to the next. Two versions are therefore always measured in turns, one run of each after the other, never one version now and the other later, and nothing else runs meanwhile.

## Method

### Load

`go run ./bench run -bin <binary>` starts the binary with a generated config, two fake providers and a fresh SQLite file, reads the admin key it prints, and runs each scenario for a warmup and then a measured window:

- The fake provider (`bench/upstream.go`) speaks the OpenAI API. Every chat answer is 1 KB of text with fixed usage, streamed as 32 content chunks plus the role, finish and usage chunks; every embedding has 256 dimensions. The `slow` provider waits 20 ms before the first byte, like a fast real provider; the `quick` one answers at once, so a max throughput scenario measures the gateway and nothing else.
- Every caller has a limits profile (request rate, token rate, daily cost) high enough never to refuse, so each call is admitted and settled, and usage is written to SQLite as in production. Logs go to a pipe the tool throws away.
- Fixed-rate scenarios send 500 calls a second for 10 s, each call measured from when it was sent to its last byte. The latency a gateway adds is that time minus the 20 ms the provider waits. The `direct chat (floor)` row sends the same calls straight to the provider, so it shows what the client, loopback and the machine add with no gateway at all.
- Max throughput scenarios run 64 callers back to back for 10 s against the `quick` provider. Calls a second counts answered calls only.
- Each version runs the whole suite once per round, the versions taking turns, three rounds; `bench merge` takes the median of each number.

Memory is the gateway process's working set after each scenario (resident set on Linux), and its peak since start.

### Go benchmarks

The hot paths each have a Go benchmark next to their code: decoding and encoding chat requests and responses, translating requests and streams between dialects, router resolve and plan, the SSE reader and frames, token counting, limits admit and settle, usage writes and key lookups in SQLite, key authentication, and `BenchmarkGateway`, one whole call through the handler for each request path with the provider answering in process. These files only use APIs that v0.2.4 has as well, so the same files are copied into a v0.2.4 checkout and run there. Each package's test binary is built for both versions and the two run in turns, six times each (ten where a result was close); `benchstat` reports the median and whether a change is significant (`~` means it is not).

## How to run it

Build both versions with the same Go:

```shell
export GOTOOLCHAIN=go1.26.11
go build -o transponder-next ./cmd/transponder
git worktree add ../transponder-v0.2.4 v0.2.4
(cd ../transponder-v0.2.4 && go build -o ../transponder/transponder-v0.2.4 ./cmd/transponder)
go build -o bench-tool ./bench
```

The load suite, taking turns:

```shell
for i in 1 2 3; do
  ./bench-tool run -bin ./transponder-v0.2.4 -label v0.2.4 -out v0.2.4-$i.json
  ./bench-tool run -bin ./transponder-next -label next -out next-$i.json
done
./bench-tool merge v0.2.4.json v0.2.4-1.json v0.2.4-2.json v0.2.4-3.json
./bench-tool merge next.json next-1.json next-2.json next-3.json
./bench-tool compare v0.2.4.json next.json
```

Runs that follow each other share the machine's local ports: a version that runs out of them leaves sockets waiting to close for a few minutes, which can cost the next run errors. `-log file` keeps a run's gateway log, stderr included, to check.

The Go benchmarks, with the benchmark files copied into the old tree and the two test binaries of a package taking turns:

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
./bench-tool run -bin ./transponder-pprof -only "chat, max" -pprof 127.0.0.1:6060 -profile "chat, max throughput" -profile-dir prof
go tool pprof -top transponder-pprof prof/cpu.pprof
```

`./bench-tool upstream` and `./bench-tool load` run the fake provider and the load tool on their own, against any gateway.

## v0.3.0 against v0.2.4

v0.3.0 here is `next/v0.3.0` at 728b549 with #148, #149, #150, #151, #152, #154 and #155 merged in. "v0.3.0 untuned" is 728b549 itself. All three built with Go 1.26.11, taking turns, three rounds, the median of each.

### Load

| Scenario | req/s v0.2.4 | req/s v0.3.0 | p50 added v0.2.4 | p50 added v0.3.0 | p95 added v0.2.4 | p95 added v0.3.0 | p99 added v0.2.4 | p99 added v0.3.0 | Errors v0.2.4 | Errors v0.3.0 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| direct chat (floor) | 499 | 499 | 0.35 ms | 0.30 ms | 0.50 ms | 0.52 ms | 0.55 ms | 17.22 ms | 0 | 0 |
| chat | 499 | 499 | 0.38 ms | 0.37 ms | 0.90 ms | 0.96 ms | 1.42 ms | 6.01 ms | 0 | 0 |
| chat stream | 499 | 499 | 0.86 ms | 0.82 ms | 1.67 ms | 1.64 ms | 2.38 ms | 2.53 ms | 0 | 0 |
| messages stream, translated | 499 | 499 | 0.85 ms | 0.89 ms | 1.80 ms | 1.72 ms | 2.64 ms | 2.57 ms | 0 | 0 |
| embeddings | 499 | 499 | 0.38 ms | 0.39 ms | 1.00 ms | 1.01 ms | 2.04 ms | 1.57 ms | 0 | 0 |
| chat, max throughput | 5,836 | **12,235** | 8.02 ms | 4.97 ms | 30.69 ms | 8.61 ms | 46.96 ms | 11.59 ms | 0 | 0 |
| chat stream, max throughput | 1,019 | **1,982** | 17.74 ms | 30.89 ms | 37.27 ms | 53.57 ms | 66.60 ms | 66.30 ms | 43,775 | 0 |

- The fixed-rate rows run at the rate asked for on both, so their calls per second are equal by design; at that rate both versions spend their time waiting on the provider, and the latency they add is the same within noise.
- The v0.3.0 p99 at 500/s in the first two rows comes from bursts of slow calls in two rounds that hit the direct floor too (17 ms there, with no gateway at all). Measured again on its own in an earlier session, three rounds in one process, v0.3.0's chat had p99 0.99 to 1.36 ms added.
- In the streamed max throughput row v0.2.4 answered 502 to 56% of calls, quickly, which is why its p50 for the calls it did answer is lower. In a separate run of three rounds in one process, v0.2.4 stopped after the first with `fatal error: thread exhaustion`: out of ports, each failed dial holds an OS thread in `closesocket`, and the runtime stops at 10,000.
- v0.3.0 untuned served 4,984 chat calls a second at max throughput and failed streamed calls the same way as v0.2.4. Running right after v0.2.4, it inherited the ports v0.2.4 left waiting to close and also failed calls at 500/s, so its other rows are not comparable and are left out.

Working set of the gateway process:

| | v0.2.4 | v0.3.0 |
|---|---:|---:|
| idle, after start | 17 MiB | 19 MiB |
| after the fixed-rate scenarios | 33 MiB | 38 MiB |
| after the max throughput scenarios | 93 MiB | 52 MiB |
| peak | 60 MiB | 57 MiB |

### Go benchmarks

Time, bytes and allocations per operation, the two versions taking turns, Go 1.26.11:

| Benchmark | Time v0.2.4 | Time v0.3.0 | Change | B/op v0.2.4 | B/op v0.3.0 | allocs v0.2.4 | allocs v0.3.0 |
|---|---:|---:|---:|---:|---:|---:|---:|
| `Gateway/memory/chat` | 54.4 µs | 48.2 µs | -11% | 33.1 KiB | 29.7 KiB | 327 | 264 |
| `Gateway/memory/chat_stream` | 199 µs | 141 µs | -29% | 120.2 KiB | 93.1 KiB | 1,281 | 770 |
| `Gateway/memory/chat_stream_from_anthropic` | 160 µs | 154 µs | -3% | 118.8 KiB | 112.0 KiB | 1,154 | 1,035 |
| `Gateway/memory/messages_stream` | 121 µs | 109 µs | -10% | 108.2 KiB | 90.2 KiB | 1,097 | 861 |
| `Gateway/memory/messages_stream_from_openai` | 155 µs | 144 µs | -7% | 127.3 KiB | 106.8 KiB | 1,170 | 967 |
| `Gateway/memory/embeddings` | 88.2 µs | 94.7 µs | +7.4% (10 runs) | 70.5 KiB | 70.6 KiB | 196 | 203 |
| `Gateway/sqlite/chat` | 122 µs | 93.3 µs | -23% | 37.4 KiB | 34.9 KiB | 425 | 383 |
| `AuthenticateKey` (parallel) | 16.6 µs | 7.1 µs | -57% | 2.0 KiB | 2.3 KiB | 62 | 72 |
| `KeyByHash` (parallel) | 14.5 µs | 7.3 µs | -50% | 1.9 KiB | 2.3 KiB | 60 | 71 |
| `RecordUsage/serial` | 42.7 µs | 35.2 µs | -18% | 2.3 KiB | 2.8 KiB | 38 | 44 |
| `RecordUsage/parallel` | 46.4 µs | 9.2 µs | -80% | 2.4 KiB | 2.3 KiB | 41 | 32 |
| `sse.Reader` (32 events) | 6.17 µs | 3.61 µs | -42% | 17.7 KiB | 10.7 KiB | 104 | 36 |
| `sse.Frame` | 90.4 ns | 45.4 ns | -50% | 264 B | 160 B | 4 | 1 |
| `TranslateStream/openai_to_anthropic` | 29.9 µs | 19.7 µs | -34% | 26.2 KiB | 13.9 KiB | 334 | 189 |
| `TranslateStream/anthropic_to_openai` | 77.9 µs | 78.1 µs | ~ | 42.8 KiB | 42.8 KiB | 668 | 669 |
| `TranslateRequest/anthropic_to_openai` | 17.1 µs | 17.1 µs | ~ | 7.6 KiB | 7.8 KiB | 65 | 69 |
| `DecodeChatRequest` | 10.5 µs | 10.3 µs | ~ | 6.6 KiB | 6.8 KiB | 53 | 57 |
| `PeekAndRewrite` | 18.4 µs | 19.3 µs | ~ | 6.6 KiB | 6.6 KiB | same | same |
| `ParseUsage` | 4.85 µs | 5.22 µs | +7.5% (10 runs) | 352 B | 352 B | 9 | 9 |
| `StreamDecode` (OpenAI) | 55.5 µs | 55.8 µs | ~ | same | same | same | same |
| `AdmitSettle` | 1.58 µs | 1.53 µs | -3% | 1.40 KiB | 1.35 KiB | 25 | 22 |
| `AdmitSettleParallel` | 2.90 µs | 2.89 µs | ~ | 1.41 KiB | 1.37 KiB | 25 | 22 |
| `Plan/team-0-model` | 133 ns | 136 ns | ~ | 224 B | 240 B | 2 | 2 |
| `Plan/unknown-model` | 753 ns | 734 ns | ~ | 224 B | 240 B | 2 | 2 |
| `Estimate/english_1KB` | 390 ns | 394 ns | ~ | 0 | 0 | 0 | 0 |

### What each change did

Each pull request was measured on its own against its parent, the two builds taking turns:

| Pull request | What it changed | Measured effect |
|---|---|---|
| #148 | keeps up to 512 idle connections per provider (Go's default keeps 2) | streamed max throughput errors from 2 to 3% of calls (up to 15%, or a crash) to 0 |
| #149 | SQLite read pool keeps its connections; key lookup prepared once | `KeyByHash` -78%, CPU per call under load -46% (1.73 to 0.93 ms) |
| #150 | usage of calls that finish together written in one transaction | chat max throughput 4,928 to 8,903 req/s (+81%), p99 added 55 to 17.6 ms; `RecordUsage/parallel` -82% |
| #151 | `X-Request-Id` read and written under Go's canonical key | 6 fewer allocations per call |
| #152 | router settings read without a lock, wildcards worked out once | `Plan` and `Resolve` -6 to -12%, back to v0.2.4's speed |
| #154 | each stream chunk decoded once; SSE reader stops copying lines; frames allocated once | native stream -27% time, -35% allocations; other streams -10 to -11%; `sse.Reader` -39%, `sse.Frame` -50% |
| #155 | a chat request's top-level fields parsed once; no tool-id pass without tool calls | chat -19% time, -21% allocations; chat stream -6% |

#148 to #152 were measured with Go 1.27.2, #154 and #155 with Go 1.26.11; each comparison used one Go for both sides.

Under max load the final build's block profile shows `sync.Mutex` at about 2% of blocked time and the limits code does not appear in the CPU profile, so the limits store's single lock was left as it is.

### What is slower than v0.2.4, and why

- **`Gateway/memory/embeddings`, +7.4% (6.5 µs) in 10 runs taking turns.** `internal/server/embed.go` is the same in both versions. The work v0.3.0 adds to every call is the request id (90 ns to generate, measured), the caller's identity on the request context, and 7 more allocations; together well under 1 µs. In a profiled pair of runs v0.3.0 was 2% faster on this path. The rest is within this path's spread (±6 to 10%).
- **`ParseUsage`, +7.5% (0.37 µs) in 10 runs.** The function, its wire type, its input and its allocations (9, 352 bytes) are the same in both versions; the difference comes from where the compiler places the code, not from the code.
- **Allocations on a few paths.** Chat messages are decoded into content parts that can carry images (+4 per request decode); each API key row has three more columns, `version`, `source` and `updated_at` (+11 per key lookup, which is still twice as fast as v0.2.4's); a lone usage write carries the batch's bookkeeping (+6). The times on those paths are lower than v0.2.4's.
- **2 to 5 MiB more working set below full load.** The metrics registry, the admin API and its OpenAPI document, and the SQLite read connections now kept open, up to one per CPU, each with its page cache. Under full load v0.3.0 uses 44% less.
- **Metrics and tracing.** With the echo provider and nothing else (`BenchmarkChat`), metrics, on by default, add 0.5 µs, 0.4 KiB and 5 allocations a call (20.8 µs off, 21.3 µs on, within noise). Tracing with an exporter adds 12 µs, 3.3 KiB and 14 allocations (33.0 µs); it is off unless `otel.endpoint` is set.

Nothing else is more than 5% slower.

## Baseline: v0.2.4 and v0.3.0 before tuning

Recorded before any change made for speed, on `next/v0.3.0` at e8c035e, with both versions built by Go 1.27.2 and each run on its own, not in turns. It is kept because the profiles below are what the changes above came from; its numbers are not comparable with the tables above.

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

### Profiles

Under the chat max throughput load, the CPU profile of both versions was dominated by SQLite, not by the gateway's own work:

- 9% (v0.3.0) to 17% (v0.2.4) of CPU opened new SQLite connections: the read pool kept two idle connections, so with 64 callers each key lookup opened and closed one.
- About 10% parsed SQL again on every call, since no statement was prepared.
- 91% of the time requests spent blocked was waiting for the single SQLite writer, one usage insert at a time.

On a single streamed call, a third of the CPU was the garbage collector, and 30% decoded every chunk twice; on a single chat call, the request body was parsed six times.

Measured apart like this, the dialect code looked 20 to 64% faster in v0.3.0; measured in turns it is the same as in v0.2.4. That is why every result above was measured in turns.
