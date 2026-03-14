# Kafig Host–Runtime Protocol

This document describes the complete protocol between the Go host (`kafig-go`) and the
WebAssembly runtime (`kafig-runtime`). It covers the module lifecycle, every function that
crosses the boundary in either direction, the memory ownership rules for each call, and the
full execution flow for evaluating a script and dispatching events.

---

## Layers

```
┌──────────────────────────────────────┐
│  Go host  (kafig-go)                 │  orchestrates execution, owns event loop
├──────────────────────────────────────┤
│  wazero   (WebAssembly host runtime) │  executes the WASM module, enforces sandbox
├──────────────────────────────────────┤
│  WASM module  (kafig-runtime)        │  Rust + rquickjs; owns the JS heap
│    └─ QuickJS                        │  evaluates user JS, drives microtask queue
└──────────────────────────────────────┘
```

All function calls across the boundary are **synchronous** from the caller's perspective.
There is no shared threading — the WASM module is single-threaded. Asynchrony is achieved
by the Go host running its own goroutines while the WASM module is idle.

---

## Module Lifecycle

### Compilation (once per process)

```
Runtime.New()
  └─ wazero.CompileModule(RuntimeWasm)   // parse + validate; no instantiation yet
```

The compiled module (`wazero.CompiledModule`) is a read-only artifact shared across all
contexts. Compilation is expensive; it happens once.

### Instantiation (once per Context)

```
Runtime.Context()
  └─ newContext()
       ├─ register host module "env" exporting host_rpc + host_set_result
       └─ wazero.InstantiateModule(compiledModule)
            └─ WASM linear memory is created fresh for this instance
```

The compiled WASM binary was pre-initialized with [Wizer](https://github.com/bytecodealliance/wizer)
at build time. Wizer called `wizer.initialize` (see below) and snapshotted the resulting
linear memory into the binary. Instantiating the module restores that snapshot directly,
so the QuickJS runtime and the JS prelude are already initialized — no startup cost at
runtime.

Each `Context` has its own WASM instance with its own linear memory and its own QuickJS
heap. Contexts are fully isolated from each other.

### Wizer Pre-initialization (build time only)

`wizer.initialize` is exported from the WASM module and called by Wizer during the build:

1. Allocates a QuickJS `Runtime` with memory limit (32 MB) and stack limit (512 KB).
2. Installs an interrupt handler that reads `INTERRUPT_FLAG`.
3. Creates a full JS `Context`.
4. Registers the native bridge functions `__host_rpc_native` and `__host_set_result_native`
   as QuickJS globals.
5. Evaluates the prelude (see below).
6. Drains the microtask queue (`execute_pending_job` until empty).
7. Stores the `JsRuntime` in a `OnceLock<JsRuntime>` static.

After Wizer snapshots this state, the static is already populated in every future instance.

---

## Linear Memory & Allocation

The WASM module owns its linear memory. The Go host may read from and write to it using
wazero's `api.Memory`, but must do so only within the bounds of allocations made by the
WASM allocator.

### Exports

| Export name | Signature (WASM i32)        | Description                                                                                                                 |
| ----------- | --------------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| `alloc`     | `(len: i32) → ptr: i32`     | Allocates `len` bytes; returns pointer. Uses `std::alloc` with alignment 1.                                                 |
| `dealloc`   | `(ptr: i32, len: i32) → ()` | Frees a previous allocation. **Both ptr and len are required** — the Rust allocator uses `Layout::from_size_align(len, 1)`. |

### Ownership rules

**Go → WASM data (inputs)**

1. Go calls `alloc(len)` to get a pointer inside WASM linear memory.
2. Go writes the data via `mem.Write(ptr, data)`.
3. Go calls the WASM function passing `(ptr, len)`.
4. WASM reads the data. Once the WASM call returns, Go calls `dealloc(ptr, len)`.
5. **Go owns the allocation** from `alloc` until `dealloc`. Go is responsible for freeing it
   even if the WASM call returns an error.

**WASM → Go data (outputs / callbacks)**

When WASM calls a host import (`host_rpc`, `host_set_result`) it passes `(ptr, len)` pointing
into WASM linear memory. The pointers are **only valid for the duration of the host
function call**. Go must copy the bytes out (e.g. via `mem.Read`) before returning from
the host function — it must not retain the pointers.

WASM retains ownership of these buffers; Go never frees them.

---

## WASM Exports (Go → WASM)

These are the functions the Go host calls on the WASM module.

### `eval(ptr: i32, len: i32) → ()`

Evaluates a UTF-8 JavaScript source string.

**Preconditions:** `ptr` is a valid WASM allocation of at least `len` bytes, written by Go.

**What happens inside:**

1. Reads source from linear memory as a UTF-8 string.
2. Wraps the source in an async IIFE:
   ```js
   (async function __kafig_main() {
       try {
           const __result = await (async function() { <source> })();
           __host_set_result_native(JSON.stringify(__result ?? null), false);
       } catch(e) {
           __host_set_result_native(JSON.stringify({
               error: e?.message ?? String(e),
               errorType: __classifyError(e),
               stack: e?.stack ?? null,
           }), true);
       }
   })();
   ```
3. Calls `ctx.eval_promise(wrapped)` — this schedules the async IIFE as a microtask and
   returns immediately (the promise is not yet settled).
4. Drains the microtask queue with `execute_pending_job` until empty.

At step 4 the microtask queue may be empty because the script hit an `await` for a host
RPC (a pending promise). In that case `eval` returns to Go with work still in progress —
Go continues servicing RPCs until `host_set_result` fires (see Full Execution Flow).

If `eval_promise` itself throws synchronously (e.g. a parse error), WASM calls
`host_set_result` directly with `is_error=1` before returning.

**Memory:** Go allocates before calling, Go frees after `eval` returns.

**Completion signal:** `host_set_result` (a host import) is called by the JS wrapper when
the async IIFE settles — either successfully or with an error. This may happen synchronously
inside `eval` (if the script has no `await`s) or inside a later `resolve_rpc`/`reject_rpc`
call.

### `dispatch_event(name_ptr: i32, name_len: i32, params_ptr: i32, params_len: i32) → ()`

Invokes a named event handler previously registered by the JS script via `host.on(name, fn)`.

**Preconditions:**
- `eval` has already been called and its execution has fully completed (i.e.
  `host_set_result` has fired for the eval).
- A handler for `name` has been registered by the script.

**What happens inside:**

1. Reads `name` and `params` JSON from linear memory.
2. Looks up the handler in `__eventHandlers`. If not found, calls `host_set_result` with a
   `runtime_error` and returns.
3. Invokes the handler wrapped in an async IIFE identical in structure to `eval`'s wrapper:
   ```js
   (async function __kafig_event() {
       try {
           const __result = await __eventHandlers.get(name)(JSON.parse(paramsJson));
           __host_set_result_native(JSON.stringify(__result ?? null), false);
       } catch(e) {
           __host_set_result_native(JSON.stringify({
               error: e?.message ?? String(e),
               errorType: __classifyError(e),
               stack: e?.stack ?? null,
           }), true);
       }
   })();
   ```
4. Drains the microtask queue until empty.

The handler may itself call host APIs via `await host.*`. In that case `dispatch_event`
returns to Go with work in progress, and Go continues the same RPC service loop until
`host_set_result` fires — exactly as with `eval`.

**Memory:** Go allocates both name and params before calling, Go frees both after
`dispatch_event` returns.

**Completion signal:** Same as `eval` — `host_set_result` fires when the handler settles.

### `resolve_rpc(promise_id: i32, ptr: i32, len: i32) → ()`

Resolves a pending JS promise with a JSON-encoded success value.

**What happens inside:**

1. Reads the payload string from `(ptr, len)`.
2. Calls `__resolve_rpc(promise_id, payloadStr)` in JS.
3. `__resolve_rpc` looks up the promise by id in `__pendingRpcs`, removes it, and calls
   `entry.resolve(JSON.parse(payloadStr))`.
4. Drains the microtask queue — this resumes the `await` continuation and may trigger
   further host RPCs or ultimately call `__host_set_result_native`.

**Memory:** Go allocates before calling, Go frees after `resolve_rpc` returns.

### `reject_rpc(promise_id: i32, ptr: i32, len: i32) → ()`

Rejects a pending JS promise with a JSON-encoded error value.

Same flow as `resolve_rpc` but calls `__reject_rpc` → `entry.reject(new Error(msg))`.

**Memory:** Same as `resolve_rpc`.

### `set_interrupt() → ()`

Sets `INTERRUPT_FLAG = true`. The QuickJS interrupt handler checks this flag periodically
during JS execution. When true, QuickJS throws an `InternalError: interrupted`, which the
JS wrapper catches and maps to `errorType: "cpu_limit_exceeded"`.

### `clear_interrupt() → ()`

Clears `INTERRUPT_FLAG = false`. Must be called before starting any new execution to
ensure a stale flag from a previous timeout doesn't immediately kill the new script.

---

## Host Imports (WASM → Go)

These are the functions the WASM module calls into Go. They are registered under the
import module name `"env"`.

### `host_rpc(method_ptr, method_len, params_ptr, params_len, promise_id: i32) → ()`

Called by JS (via `__host_rpc_native`) whenever user code calls a host API:

```js
host.fetch(url);          // method="fetch",  params={"url":...}
host.log(msg);            // method="log",    params={"msg":...}
host.kv.get(key);         // method="kv.get", params={"key":...}
host.kv.set(key, val);    // method="kv.set", params={"key":...,"value":...}
host.kv.delete(key);      // method="kv.del", params={"key":...}
```

Parameters:

- `method_ptr/len` — UTF-8 method name in WASM memory.
- `params_ptr/len` — UTF-8 JSON params string in WASM memory.
- `promise_id` — integer ID matching a pending entry in `__pendingRpcs`.

**Memory:** All pointers are into WASM linear memory, owned by WASM. Go must copy the
bytes before returning. Go must not free these buffers.

**Control flow:** This function is called synchronously _from inside_ `eval`,
`dispatch_event`, or `resolve_rpc`. Go must **not** call back into WASM while inside this
function — doing so would re-enter wazero on the same goroutine and deadlock. Go should
record the pending RPC (e.g. in a channel or map) and return immediately. The actual work
happens after the originating WASM call returns.

### `host_set_result(result_ptr, result_len: i32, is_error: i32) → ()`

Called by the JS wrapper (`__host_set_result_native`) when an async IIFE settles —
either the `eval` wrapper or the `dispatch_event` wrapper.

- `result_ptr/len` — UTF-8 JSON string in WASM memory.
  - On success: the JSON-serialized return value of the script or handler (or `null`).
  - On error: `{"error": "...", "errorType": "...", "stack": "..."}`.
- `is_error` — `0` for success, `1` for error.

This is the definitive completion signal. Go uses it to know that the current execution
unit (eval or event dispatch) has fully finished. Go tracks pending RPC count internally —
when `host_set_result` fires there will always be zero pending RPCs.

**Memory:** Same ownership rules as `host_rpc` — copy before returning, do not free.

**Control flow:** Called from inside `eval`, `dispatch_event`, or `resolve_rpc`/`reject_rpc`.
Same re-entry rule applies: Go must not call back into WASM from within this function.

---

## JS Prelude

The prelude is evaluated at Wizer pre-initialization time and is part of every instance's
initial state. It installs the following globals:

| Global                                        | Type                          | Description                                                                                           |
| --------------------------------------------- | ----------------------------- | ----------------------------------------------------------------------------------------------------- |
| `__pendingRpcs`                               | `Map<int, {resolve, reject}>` | In-flight RPC promises keyed by id.                                                                   |
| `__nextRpcId`                                 | `int`                         | Auto-incrementing promise id counter.                                                                 |
| `__eventHandlers`                             | `Map<string, function>`       | Named event handlers registered by the script via `host.on`.                                         |
| `__make_rpc_promise(method, params)`          | function                      | Creates a promise, registers it in `__pendingRpcs`, calls `__host_rpc_native`.                        |
| `__resolve_rpc(id, resultJson)`               | function                      | Settles a pending promise with success.                                                               |
| `__reject_rpc(id, errorJson)`                 | function                      | Settles a pending promise with rejection.                                                             |
| `__host_rpc_native(method, paramsJson, id)`   | native fn                     | Thin bridge to the `host_rpc` import. Passes JS strings as raw pointers into WASM linear memory.     |
| `__host_set_result_native(resultJson, isErr)` | native fn                     | Thin bridge to the `host_set_result` import.                                                          |
| `__classifyError(err)`                        | function                      | Maps QuickJS exception types to error type strings.                                                   |
| `host`                                        | object                        | Public API surface: `host.log`, `host.fetch`, `host.kv.*`, `host.on(name, handler)`.                 |

### `host.on(name, handler)`

Registers an async-capable event handler function under the given name. Multiple calls
with the same name replace the previous handler. Handlers must be registered during the
top-level `eval` execution before any `dispatch_event` calls.

```js
host.on('message', async ({ topic, payload }) => {
    const response = await host.fetch(`https://api.example.com/${topic}`, { body: payload });
    return response;
});
```

---

## Error Classification

The JS wrapper classifies exceptions via `__classifyError`:

| `errorType` value       | Condition                                                                |
| ----------------------- | ------------------------------------------------------------------------ |
| `cpu_limit_exceeded`    | `InternalError` with message `"interrupted"` — the interrupt flag fired. |
| `memory_limit_exceeded` | `RangeError` with message containing `"out of memory"`.                  |
| `stack_overflow`        | `RangeError` with message containing `"stack"`.                          |
| `runtime_error`         | Everything else.                                                         |

---

## Full Execution Flow

### Phase 1: Eval

```
Go                              WASM / QuickJS
──────────────────────────────────────────────────────────────────────
clear_interrupt()
alloc(len(source)) → ptr
mem.Write(ptr, source)
eval(ptr, len)
  │                             wrap source in async IIFE
  │                             eval_promise(wrapped)  ← schedules IIFE as microtask
  │                             execute_pending_job()  ← runs IIFE synchronously until:
  │                               ...user code hits `await host.fetch(url)`
  │                               __make_rpc_promise("fetch", params)
  │                                 registers promise id=1 in __pendingRpcs
  │                                 __host_rpc_native("fetch", paramsJson, 1)
  │◄────────────────────────── host_rpc("fetch", paramsJson, id=1)
  │  (copy method+params, store pending {id:1}, return)
  │                             (promise unresolved; microtask queue empty)
  │                             eval returns
dealloc(ptr, len)

// Go performs the actual fetch in a goroutine:
// http.Get(url) → responseJson

alloc(len(responseJson)) → ptr2
mem.Write(ptr2, responseJson)
resolve_rpc(1, ptr2, len(responseJson))
  │                             __resolve_rpc(1, responseJson)
  │                               entry = __pendingRpcs.get(1); delete
  │                               entry.resolve(JSON.parse(responseJson))
  │                             execute_pending_job()  ← resumes the awaiting code
  │                               ...user code continues after `await`
  │                               ...script registers event handlers via host.on(...)
  │                               ...script returns a value or throws
  │                               __host_set_result_native(resultJson, isError)
  │◄────────────────────────── host_set_result(resultJson, isError)
  │  (copy result, signal completion, return)
  │                             execute_pending_job()  ← drain remaining microtasks
  │                             resolve_rpc returns
dealloc(ptr2, len)
// eval phase complete; event handlers are now registered
```

### Phase 2: Event Dispatch

After eval completes, Go may dispatch events by name. Each dispatch follows the same
RPC service loop as eval:

```
Go                              WASM / QuickJS
──────────────────────────────────────────────────────────────────────
alloc(len(name)) → namePtr
alloc(len(params)) → paramsPtr
mem.Write(namePtr, "message")
mem.Write(paramsPtr, paramsJson)
dispatch_event(namePtr, nameLen, paramsPtr, paramsLen)
  │                             look up __eventHandlers.get("message")
  │                             wrap handler call in async IIFE
  │                             eval_promise(wrapped)
  │                             execute_pending_job()  ← runs handler until:
  │                               ...handler hits `await host.fetch(...)`
  │◄────────────────────────── host_rpc("fetch", paramsJson, id=2)
  │  (copy, store pending {id:2}, return)
  │                             dispatch_event returns
dealloc(namePtr); dealloc(paramsPtr)

alloc(len(responseJson)) → ptr3
mem.Write(ptr3, responseJson)
resolve_rpc(2, ptr3, len(responseJson))
  │                             entry.resolve(JSON.parse(responseJson))
  │                             execute_pending_job()  ← handler resumes, returns value
  │                               __host_set_result_native(resultJson, false)
  │◄────────────────────────── host_set_result(resultJson, 0)
  │  (copy result, signal completion, return)
  │                             resolve_rpc returns
dealloc(ptr3, len)
// event dispatch complete
```

Each event dispatch is fully serial. Go must not call `dispatch_event` again until
`host_set_result` has fired for the current dispatch.

---

## CPU Limiting

Go sets a timer goroutine before calling `eval` or `dispatch_event`. If the budget expires:

```
Go (timer goroutine)         WASM / QuickJS
────────────────────────────────────────────
set_interrupt()
                             (QuickJS interrupt handler fires on next opcode)
                             throws InternalError("interrupted")
                             caught by __kafig_main / __kafig_event try/catch
                             __host_set_result_native({"errorType":"cpu_limit_exceeded",...}, true)
◄─────────────────────────── host_set_result(...)
```

Time spent blocked inside Go (while WASM is idle awaiting RPC results) does not consume
JS CPU budget. Only actual QuickJS execution time counts. This makes it straightforward to
track CPU time per event: start a timer before each `eval`/`dispatch_event`, accumulate
only the intervals when WASM is actually executing, and stop on `host_set_result`.

After any execution — success, error, or timeout — call `clear_interrupt()` before the
next `eval` or `dispatch_event` to reset the flag.

---

## Invariants & Constraints

1. **No re-entrant WASM calls.** `host_rpc` and `host_set_result` are called from within
   an active wazero call stack. Go must not call any WASM export from within these callbacks.
   All WASM calls (`resolve_rpc`, `reject_rpc`, `dispatch_event`) must happen after the
   originating `eval` or `resolve_rpc` has returned.

2. **Single-threaded WASM.** Only one goroutine may call into a given `Context` at a time.
   Go is responsible for serializing access.

3. **Pointer lifetimes.** Pointers passed from WASM to Go (in `host_rpc`/`host_set_result`)
   are only valid until the host function returns. Pointers passed from Go to WASM must
   remain valid until the WASM export returns — use `defer dealloc(ptr, len)` after each
   call.

4. **JSON as the wire format.** All data crossing the boundary (params, results, errors) is
   UTF-8 JSON. Neither side sends binary data or structured types directly.

5. **Execution is serial per Context.** Only one execution unit (eval or event dispatch)
   runs at a time within a `Context`. Go must not call `dispatch_event` while another
   dispatch or an eval is in progress. Multiple concurrent events must be queued by Go and
   fed to the Context one at a time.

6. **Event handlers are registered during eval.** `host.on` calls are only meaningful
   during the top-level `eval` execution. Calling `host.on` from within an event handler
   replaces the handler for future dispatches but does not affect any in-progress dispatch.

7. **CPU time is attributable per execution unit.** Because execution is serial and WASM
   is idle while Go services RPCs, the wall-clock time the WASM module is actively executing
   maps directly to one eval or one event dispatch. Each unit gets its own budget and its
   own `host_set_result` callback.
