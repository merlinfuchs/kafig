package kafig

import (
	"log/slog"

	"github.com/tetratelabs/wazero"
)

// runtimeOptions holds configuration for a Runtime and its Instances.
type runtimeOptions struct {
	module             []byte // custom WASM binary; nil = use embedded default
	prelude            string
	preludeBytecode    []byte // compiled by New(), used by each Instance
	logger             *slog.Logger
	closeOnContextDone bool
	compilationCache   wazero.CompilationCache
}

// RuntimeOption configures a Runtime created with New.
type RuntimeOption func(*runtimeOptions)

// WithModule sets a custom WASM binary to use instead of the embedded default.
// The binary must export the same functions as the kafig-runtime module (alloc,
// dealloc, eval, eval_compiled, dispatch_event, etc.) and import the "env"
// host functions (host_rpc, host_set_result, host_rpc_sync, host_should_interrupt,
// host_promise_rejection).
func WithModule(data []byte) RuntimeOption {
	return func(o *runtimeOptions) { o.module = data }
}

// WithPrelude sets a JavaScript source string that is evaluated synchronously
// on every new Instance before any user Eval calls. Use this to define global
// helper functions or constants that user scripts can rely on.
func WithPrelude(source string) RuntimeOption {
	return func(o *runtimeOptions) { o.prelude = source }
}

// WithLogger sets a structured logger for internal debug logging. When set,
// the runtime logs instance creation, RPC dispatch, and errors.
func WithLogger(logger *slog.Logger) RuntimeOption {
	return func(o *runtimeOptions) { o.logger = logger }
}

// WithCloseOnContextDone makes each Instance watch the context passed to
// Instance() and automatically call SetInterrupt + Close when it is cancelled.
func WithCloseOnContextDone(closeOnContextDone bool) RuntimeOption {
	return func(o *runtimeOptions) { o.closeOnContextDone = closeOnContextDone }
}

// WithCompilationCache provides a custom wazero compilation cache. When set,
// the Runtime uses this cache instead of creating its own. This allows sharing
// the compiled WASM module across multiple Runtime instances, avoiding
// redundant compilation. The caller is responsible for closing the cache.
func WithCompilationCache(cache wazero.CompilationCache) RuntimeOption {
	return func(o *runtimeOptions) { o.compilationCache = cache }
}

// instanceOptions holds configuration for a single Instance.
type instanceOptions struct {
	router                  *RPCRouter
	logger                  *slog.Logger
	closeOnContextDone      bool
	preludeBytecode         []byte
	interruptCallback       func(opcodes uint64, cpuTimeUs uint64) bool
	promiseRejectionHandler func(*JsError) bool
	jsMemoryLimit        uint32 // 0 = use default (32MB)
	wasmMemoryLimitPages uint32 // 0 = unlimited (1 page = 64KB)
}

// InstanceOption configures a Instance created with Instance.
type InstanceOption func(*instanceOptions)

// WithRouter sets an RPCRouter for the Instance. Sync RPC calls
// (host.rpcSync in JS) are dispatched to handlers registered via
// RPCRouter.WithSync, and async RPC calls (host.rpc in JS) go to
// RPCRouter.WithAsync handlers (with fallback to sync handlers).
func WithRouter(router *RPCRouter) InstanceOption {
	return func(o *instanceOptions) { o.router = router }
}

// WithInterruptCallback sets a callback invoked every ~10,000 branch/loop
// opcodes with the current opcode count and CPU time in microseconds. Return
// true to interrupt execution.
func WithInterruptCallback(fn func(opcodes uint64, cpuTimeUs uint64) bool) InstanceOption {
	return func(o *instanceOptions) { o.interruptCallback = fn }
}

// WithPromiseRejectionHandler sets a callback invoked whenever an unhandled
// promise rejection occurs. The callback receives the JsError describing the
// rejection and returns true to interrupt execution or false to continue.
func WithPromiseRejectionHandler(fn func(*JsError) bool) InstanceOption {
	return func(o *instanceOptions) { o.promiseRejectionHandler = fn }
}

// WithJSMemoryLimit sets the QuickJS heap memory limit in bytes.
// Default: 32MB (set in the WASM runtime). Set to 0 to use the default.
func WithJSMemoryLimit(bytes uint32) InstanceOption {
	return func(o *instanceOptions) { o.jsMemoryLimit = bytes }
}

// WithWASMMemoryLimitPages sets the maximum WASM linear memory in pages
// (1 page = 64KB). This limits the total memory available to the WASM
// module including the QuickJS heap, stack, and Rust allocator overhead.
// Default: 0 (unlimited).
func WithWASMMemoryLimitPages(pages uint32) InstanceOption {
	return func(o *instanceOptions) { o.wasmMemoryLimitPages = pages }
}

// evalOptions holds per-call configuration for Eval, EvalCompiled, and
// DispatchEvent.
type evalOptions struct {
	async_ bool
}

// EvalOption configures a single Eval, EvalCompiled, or DispatchEvent call.
type EvalOption func(*evalOptions)

// WithAsync enables async evaluation. For Eval, this sets JS_EVAL_FLAG_ASYNC so
// that top-level await is supported and the last expression result is returned
// as a resolved Promise value. For Compile, it bakes the async flag into the
// bytecode. For all call types, it drains the QuickJS job queue and services
// RPC calls after execution.
func WithAsync() EvalOption {
	return func(o *evalOptions) { o.async_ = true }
}
