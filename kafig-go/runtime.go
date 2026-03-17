package kafig

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/merlinfuchs/kafig/kafig-go/internal/wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Runtime holds a compilation cache so that the WASM module is parsed and
// compiled once, then reused across all Instances. Each Instance gets its own
// wazero.Runtime (required for per-instance host function bindings) but the
// expensive compilation step is shared via the cache.
type Runtime struct {
	module   []byte // WASM binary (custom or embedded default)
	cache    wazero.CompilationCache
	ownCache bool // true if we created the cache and should close it
	opts     runtimeOptions
}

// New pre-compiles the embedded WASM module. The returned Runtime can create
// any number of Instances via [Runtime.Instance].
//
// If WithPrelude is set, the prelude source is compiled to QuickJS bytecode
// at this point. Each Instance then evaluates the bytecode (skipping parsing).
func New(ctx context.Context, options ...RuntimeOption) (*Runtime, error) {
	opts := runtimeOptions{
		logger: slog.Default(),
	}
	for _, o := range options {
		o(&opts)
	}

	// Resolve the WASM binary: use the custom module if provided, otherwise
	// fall back to the embedded default.
	module := opts.module
	if module == nil {
		module = wasm.RuntimeWasm
	}

	cache := opts.compilationCache
	ownCache := cache == nil
	if ownCache {
		cache = wazero.NewCompilationCache()
	}

	// Compile once into the cache by creating a temporary runtime.
	r := wazero.NewRuntimeWithConfig(
		ctx,
		wazero.NewRuntimeConfig().
			WithCloseOnContextDone(opts.closeOnContextDone).
			WithCompilationCache(cache),
	)
	if _, err := r.CompileModule(ctx, module); err != nil {
		return nil, fmt.Errorf("kafig: compile module: %w", err)
	}
	r.Close(ctx)

	rt := &Runtime{module: module, cache: cache, ownCache: ownCache, opts: opts}

	// If a prelude is configured, compile it to bytecode now so each
	// Instance can skip parsing.
	if opts.prelude != "" {
		bytecode, err := rt.compileBytecode(ctx, opts.prelude, false)
		if err != nil {
			cache.Close(ctx)
			return nil, fmt.Errorf("kafig: compile prelude: %w", err)
		}
		rt.opts.preludeBytecode = bytecode
	}

	return rt, nil
}

// Instance creates a new isolated WASM instance. Each instance has its own
// wazero runtime, linear memory, QuickJS heap, and host function bindings.
func (r *Runtime) Instance(ctx context.Context, options ...InstanceOption) (*Instance, error) {
	opts := instanceOptions{
		logger:             r.opts.logger,
		closeOnContextDone: r.opts.closeOnContextDone,
		preludeBytecode:    r.opts.preludeBytecode,
	}
	for _, o := range options {
		o(&opts)
	}

	if opts.router == nil {
		return nil, fmt.Errorf("kafig: RPCRouter is required (use WithRouter)")
	}

	return newInstance(ctx, r.cache, r.module, opts)
}

// Compile compiles JavaScript source to QuickJS bytecode. The bytecode can be
// passed to [Instance.EvalCompiled] to skip parsing on every call. Bytecode is
// portable across all Instances from the same Runtime (same QuickJS version).
//
// Use WithAsync() if the source uses top-level await — this bakes
// JS_EVAL_FLAG_ASYNC into the bytecode so that EvalCompiled returns a Promise
// whose resolved value becomes the result.
func (r *Runtime) Compile(ctx context.Context, source string, options ...EvalOption) ([]byte, error) {
	var opts evalOptions
	for _, o := range options {
		o(&opts)
	}
	return r.compileBytecode(ctx, source, opts.async_)
}

// Close releases the compilation cache if it was created by this Runtime.
// If a custom cache was provided via WithCompilationCache, it is not closed.
func (r *Runtime) Close(ctx context.Context) error {
	if r.ownCache {
		return r.cache.Close(ctx)
	}
	return nil
}

// compileBytecode creates a temporary WASM instance, compiles the given source
// to QuickJS bytecode via the compile() export, and returns the bytecode bytes.
func (r *Runtime) compileBytecode(ctx context.Context, source string, isAsync bool) ([]byte, error) {
	// Create a temporary wazero runtime with the shared compilation cache.
	wzr := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(r.cache))
	defer wzr.Close(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, wzr)

	// Stub host imports — the WASM module declares them, but compile()
	// doesn't call host_rpc. host_set_result may be called on compile error
	// via send_error, but we detect errors from the packed return value.
	if _, err := wzr.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _, _, _, _, _ uint32) {}).Export("host_rpc").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _, _, _ uint32) {}).Export("host_set_result").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _, _, _, _ uint32) uint64 { return 0 }).Export("host_rpc_sync").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _, _ uint64) int32 { return 0 }).Export("host_should_interrupt").
		Instantiate(ctx); err != nil {
		return nil, fmt.Errorf("register host module: %w", err)
	}

	compiled, err := wzr.CompileModule(ctx, r.module)
	if err != nil {
		return nil, fmt.Errorf("compile module: %w", err)
	}

	module, err := wzr.InstantiateModule(ctx, compiled, wazero.NewModuleConfig())
	if err != nil {
		return nil, fmt.Errorf("instantiate module: %w", err)
	}

	fnCompile := module.ExportedFunction("compile")
	fnAlloc := module.ExportedFunction("alloc")
	fnDealloc := module.ExportedFunction("dealloc")

	// Allocate and write source into WASM memory.
	sourceBytes := []byte(source)
	allocRes, err := fnAlloc.Call(ctx, uint64(len(sourceBytes)))
	if err != nil {
		return nil, fmt.Errorf("alloc: %w", err)
	}
	ptr := uint32(allocRes[0])
	if ptr == 0 {
		return nil, fmt.Errorf("alloc returned null")
	}
	module.Memory().Write(ptr, sourceBytes)

	// Call compile(ptr, len, is_async) → packed u64
	isAsyncArg := uint64(0)
	if isAsync {
		isAsyncArg = 1
	}
	results, err := fnCompile.Call(ctx, uint64(ptr), uint64(len(sourceBytes)), isAsyncArg)
	if err != nil {
		return nil, fmt.Errorf("compile call: %w", err)
	}
	fnDealloc.Call(ctx, uint64(ptr), uint64(len(sourceBytes))) //nolint:errcheck

	packed := results[0]
	if packed == 0 {
		return nil, fmt.Errorf("compilation failed (syntax error or invalid source)")
	}

	// Unpack: high 32 bits = pointer, low 32 bits = length
	bcPtr := uint32(packed >> 32)
	bcLen := uint32(packed & 0xFFFFFFFF)

	// Read bytecode from WASM memory and copy to Go-owned slice.
	bcData, ok := module.Memory().Read(bcPtr, bcLen)
	if !ok {
		return nil, fmt.Errorf("failed to read bytecode from WASM memory")
	}
	bytecode := make([]byte, len(bcData))
	copy(bytecode, bcData)

	// Dealloc the WASM-side buffer.
	fnDealloc.Call(ctx, uint64(bcPtr), uint64(bcLen)) //nolint:errcheck

	return bytecode, nil
}
