package kafig

import (
	"log/slog"

	"github.com/tetratelabs/wazero"
)

// runtimeOptions holds configuration for a Runtime and its Instances.
type runtimeOptions struct {
	prelude            string
	preludeBytecode    []byte // compiled by New(), used by each Instance
	logger             *slog.Logger
	closeOnContextDone bool
	compilationCache   wazero.CompilationCache
}

// RuntimeOption configures a Runtime created with New.
type RuntimeOption func(*runtimeOptions)

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
	router             *RPCRouter
	logger             *slog.Logger
	closeOnContextDone bool
	preludeBytecode    []byte
	interruptCallback  func(opcodes uint64, cpuTimeUs uint64) bool
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

// evalOptions holds per-call configuration for Eval, EvalCompiled, and
// DispatchEvent.
type evalOptions struct {
	async_ bool
}

// EvalOption configures a single Eval, EvalCompiled, or DispatchEvent call.
type EvalOption func(*evalOptions)

// WithAsync enables async evaluation. When set, the source is wrapped in an
// async IIFE, allowing top-level await and RPC calls. Without this option,
// evaluation is synchronous.
func WithAsync() EvalOption {
	return func(o *evalOptions) { o.async_ = true }
}
