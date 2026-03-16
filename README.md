<div align="center">
  <img src="./kafig.webp" alt="Kafig" width="200" />
</div>

# {JS} Kafig

Kafig (from German _Käfig_, meaning cage) is a sandboxed JavaScript runtime for Go. It lets you execute untrusted JavaScript with strict CPU and memory limits, full async/await support, and a bidirectional RPC bridge between JS and Go.

The runtime compiles [QuickJS](https://bellard.org/quickjs/) to WebAssembly via Rust, then executes it inside [wazero](https://wazero.io/) (a pure-Go WASM runtime). Each instance gets its own isolated linear memory and JS heap with no shared state.

## Architecture

```
Go application
  └─ kafig-go          ← Go API: Runtime, Instance, RPCRouter
       └─ wazero       ← pure-Go WASM host, enforces sandbox
            └─ kafig-runtime.wasm
                 └─ QuickJS (compiled from Rust via rquickjs)
```

**Key properties:**

- Each Instance has its own WASM linear memory and QuickJS heap (32 MB limit, 512 KB stack)
- CPU time is tracked per-execution and excludes time spent waiting for RPC results
- JS can call Go functions via async (`host.rpc`) or sync (`host.rpcSync`) RPC
- Go can call into JS via named event handlers registered with `host.on`
- The WASM binary is pre-initialized with [Wizer](https://github.com/bytecodealliance/wizer). QuickJS and the JS prelude are snapshotted at build time, so instantiation has near-zero startup cost
- JavaScript can be pre-compiled to QuickJS bytecode, skipping parse overhead on repeated evaluations

## Usage

```shell
go get github.com/merlinfuchs/kafig/kafig-go
```

### Create a Runtime and Instance

```go
rt, err := kafig.New(ctx,
    kafig.WithPrelude(`const VERSION = "1.0";`),
)
defer rt.Close(ctx)

router := kafig.NewRPCRouter().
    WithAsync("fetch", func(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
        // runs in a goroutine, suitable for I/O
        var req struct{ URL string }
        json.Unmarshal(params, &req)
        resp, _ := http.Get(req.URL)
        // ...
        return json.Marshal(result)
    }).
    WithSync("now", func(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
        // runs inline, no goroutine. Suitable for fast, pure-compute calls
        return json.Marshal(map[string]int64{"ts": time.Now().Unix()})
    })

inst, err := rt.Instance(ctx,
    kafig.WithRouter(router),
    kafig.WithInterruptCallback(func(opcodes, cpuTimeUs uint64) bool {
        return cpuTimeUs > 5_000_000 // 5 second CPU limit
    }),
)
defer inst.Close(ctx)
```

### Evaluate JavaScript

```go
// Synchronous (no promises, no RPC)
result, err := inst.Eval(ctx, `1 + 2`)

// Async (enables top-level await, promise resolution and RPC processing)
result, err := inst.Eval(ctx, `
    const data = await host.rpc("fetch", {url: "https://api.example.com"});
    data
`, kafig.WithAsync())
```

### Event Handlers

Scripts can register persistent event handlers that Go dispatches later:

```go
// During eval, JS registers handlers:
inst.Eval(ctx, `
    host.on("process", async (params) => {
        const result = await host.rpc("fetch", {url: params.url});
        return result;
    });
`, kafig.WithAsync())

// Later, Go dispatches events:
result, err := inst.DispatchEvent(ctx, "process",
    json.RawMessage(`{"url": "https://example.com"}`),
    kafig.WithAsync(),
)
```

### Pre-compiled Bytecode

```go
bytecode, err := rt.Compile(ctx, `42`)

// Reuse across instances, skips parsing
result, err := inst.EvalCompiled(ctx, bytecode, kafig.WithAsync())
```

### Execution Stats

```go
stats, _ := inst.GetExecutionStats(ctx)
fmt.Printf("opcodes: %d, cpu: %d us\n", stats.Opcodes, stats.CPUTimeUs)
inst.ResetExecutionStats(ctx)
```

## JavaScript API

Inside the sandbox, scripts have access to the `host` object:

| Function                       | Description                                                     |
| ------------------------------ | --------------------------------------------------------------- |
| `host.rpc(method, params)`     | Async RPC call to Go. Returns a Promise.                        |
| `host.rpcSync(method, params)` | Sync RPC call to Go. Blocks until the handler returns.          |
| `host.on(name, fn)`            | Register an event handler callable from Go via `DispatchEvent`. |

The result of an `Eval` call is the value of the last expression in the script (like a REPL). For `DispatchEvent`, it's the return value of the handler function. In async mode, if the result is a Promise, it's automatically awaited before being returned to Go.

## Sync vs Async Execution

By default, `Eval`, `EvalCompiled`, and `DispatchEvent` run synchronously: the JS source executes and returns immediately with no promise resolution or RPC processing. This is the fast path for simple expressions and pure computation. The return value is the result of the last expression.

To enable promise resolution and RPC processing, pass `kafig.WithAsync()`. This enables top-level `await` (via QuickJS's `JS_EVAL_FLAG_ASYNC`) and tells kafig to drain the microtask queue and service RPC calls in a loop until the script settles. If the last expression evaluates to a Promise, it's automatically awaited before the result is returned to Go.

```go
// Sync: no promises, no RPC. Fast and simple.
result, _ := inst.Eval(ctx, `1 + 2`)

// Async: enables top-level await, promise resolution and RPC processing.
result, _ := inst.Eval(ctx, `
    const data = await host.rpc("fetch", {url: "https://example.com"});
    data
`, kafig.WithAsync())
```

`host.rpcSync()` is the exception: it works in both modes because it calls the Go handler inline during WASM execution and returns the result directly, without promises.

RPC handlers registered via `RPCRouter.WithAsync()` run in goroutines, so multiple concurrent `host.rpc()` calls (e.g. from `Promise.all`) execute in parallel on the Go side. Handlers registered via `RPCRouter.WithSync()` run inline with no goroutine overhead.

## CLI

The `kafig-cli` tool provides an interactive REPL and file execution mode:

```shell
go install github.com/merlinfuchs/kafig/kafig-cli@latest
```

```bash
# Interactive REPL
kafig-cli

# Execute a file
kafig-cli script.js

# With CPU limits
kafig-cli -max-cpu-ms 1000 -max-opcodes 100000 script.js

# JSON mode (stdin/stdout)
echo '{"eval": "1 + 2"}' | kafig-cli
```

In the REPL, use `.dispatch <name> <json>` to dispatch events, `.reset` to reset the instance, and `.stats` to view execution statistics.

## Building from Source

See [kafig-runtime/README.md](kafig-runtime/README.md) for toolchain setup (Rust, wasi-sdk, Wizer).

```bash
# Build the WASM runtime and install into kafig-go
cd kafig-runtime && make install

# Run tests
cd kafig-go && go test ./...
```

## Design Decisions

**JSON as the wire format.** All data crossing the WASM boundary is UTF-8 JSON. This is simple, debuggable, and avoids the complexity of shared-memory serialization formats. The tradeoff is serialization overhead on large payloads, but for typical RPC parameters and results, JSON is fast enough and keeps the protocol straightforward.

**Minimal JS API.** The guest-side API is intentionally small: `host.rpc`, `host.rpcSync`, and `host.on`. There is no way to expose Go functions directly into the JS global scope or call JS functions from Go by name. Everything goes through RPC calls or event handlers. This keeps the core simple and gives library users full control over the API surface they expose to scripts. You build your own JS API (helper functions, domain-specific abstractions) on top of these primitives, either via the prelude or using a bundle step for user scripts.

**No module system.** There is no `import`, `require`, or ES module support. Scripts run as top-level code in global scope. This is deliberate: module resolution adds complexity, filesystem access requirements, and attack surface. If you need to compose scripts, use a bundler like [esbuild](https://esbuild.github.io/) to bundle them into a single file.

**Opcode-based CPU limiting.** CPU budgets are enforced via QuickJS's interrupt handler, which fires every ~10,000 opcodes. This means the budget is measured in actual JS execution time (microseconds), excluding time spent waiting for RPC results. A script that makes a slow HTTP call via `host.rpc` only burns CPU budget while JS is actively running.

**Pre-initialization with Wizer.** The WASM binary includes a full snapshot of an initialized QuickJS runtime. This means instantiation restores memory from the snapshot instead of re-parsing and evaluating the prelude. The cost is a larger binary; the benefit is near-zero cold start.

**Single-threaded, serial execution.** Each Instance is single-threaded. Only one eval or event dispatch runs at a time. The caller must serialize access. This avoids the need for internal locking and makes the execution model easy to reason about.

**WASM as the isolation boundary.** Rather than relying on QuickJS's own sandboxing (which has had CVEs), kafig runs the entire JS engine inside a WASM sandbox. WASM provides hardware-enforced memory isolation: guest code physically cannot access host memory. This is a defense-in-depth approach: even if QuickJS has a memory safety bug, the WASM sandbox contains it.

**QuickJS over V8.** Kafig uses QuickJS, not V8 or SpiderMonkey. QuickJS is small (~200 KB compiled), deterministic, and compiles cleanly to WASM. It lacks JIT compilation, so raw throughput is lower than V8, but it's far more predictable, embeddable, and doesn't require platform-specific assembly. For sandboxed scripting workloads (config evaluation, event handlers, data transformation), parsing and I/O dominate, and QuickJS is more than fast enough.

## When Kafig Might Not Be Right

- **High-throughput number crunching.** QuickJS is an interpreter. If your workload is CPU-bound computation (matrix math, image processing, cryptography), you'll see 10-100x slower execution compared to V8's JIT. Consider running those workloads natively.
- **Full Node.js compatibility.** There's no `require`, no `fs`, no `Buffer`, no Node.js standard library. If your scripts depend on the Node.js ecosystem, kafig won't work without significant adaptation.
- **ES modules.** If you need `import`/`export` syntax or dynamic `import()`, kafig doesn't support it. You can use a bundler like [esbuild](https://esbuild.github.io/) to get around this.
- **Large JSON payloads.** Every RPC call serializes parameters and results as JSON across the WASM boundary. If you're passing multi-megabyte blobs back and forth, the serialization overhead may matter.

## License

MIT
