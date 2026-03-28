package kafig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// instanceContextKey is used to pass the active *Instance through
// context.Context so that shared host functions can dispatch to
// the correct instance.
type instanceContextKey struct{}

func instanceFromContext(ctx context.Context) *Instance {
	return ctx.Value(instanceContextKey{}).(*Instance)
}

// Instance is an isolated WASM+QuickJS execution environment. Each Instance has
// its own linear memory, JS heap, and pending RPC state.
//
// All methods must be called from a single goroutine — the WASM module is
// single-threaded and Instance does not synchronize access internally.
type Instance struct {
	module api.Module
	router *RPCRouter
	logger *slog.Logger

	// WASM export function handles, cached on creation for performance.
	fnAlloc               api.Function
	fnDealloc             api.Function
	fnEval                api.Function // eval(ptr, len, is_async)
	fnEvalCompiled        api.Function // eval_compiled(ptr, len, is_async)
	fnDispatchEvent       api.Function // dispatch_event(name_ptr, name_len, params_ptr, params_len, is_async)
	fnResolveRpc          api.Function
	fnRejectRpc           api.Function
	fnGetOpcodeCount      api.Function
	fnGetCPUTimeUs        api.Function
	fnResetExecutionStats api.Function
	fnSetMemoryLimit      api.Function // set_memory_limit(limit)

	// interruptCallback is called from the WASM interrupt handler every ~10,000
	// branch/loop opcodes with the current opcode count and CPU time in
	// microseconds. Return true to interrupt execution.
	interruptCallback func(instructions uint64, cpuTimeUs uint64) bool

	// promiseRejectionHandler is called whenever an unhandled promise rejection
	// occurs. Returns true to interrupt execution.
	promiseRejectionHandler func(*JsError) bool

	// interrupted is set by Interrupt() (possibly from another goroutine)
	// and checked by hostShouldInterrupt.
	interrupted atomic.Bool

	// pendingRPCs collects host_rpc calls made during a single WASM invocation.
	// They are drained after the WASM export returns (no re-entrant calls).
	pendingRPCs []rpcCall

	// scriptError is set by hostSetResult when is_error=1 (from Rust send_js_error/send_runtime_error).
	scriptError error
	hasError    bool

	// resultValue is set by hostSetResult when is_error=0 (from the JS completion value).
	resultValue json.RawMessage
	hasResult   bool
}

func newInstance(ctx context.Context, wzRuntime wazero.Runtime, compiled wazero.CompiledModule, closeOnContextDone bool, opts instanceOptions) (*Instance, error) {
	inst := &Instance{
		router:                  opts.router,
		logger:                  opts.logger,
		interruptCallback:       opts.interruptCallback,
		promiseRejectionHandler: opts.promiseRejectionHandler,
	}

	// Inject *Instance into context so shared host functions can find it.
	instCtx := context.WithValue(ctx, instanceContextKey{}, inst)

	// Instantiate an anonymous guest module (empty name allows multiple
	// instances in the same runtime).
	module, err := wzRuntime.InstantiateModule(instCtx, compiled,
		wazero.NewModuleConfig().WithName("").WithSysNanotime())
	if err != nil {
		return nil, fmt.Errorf("kafig: instantiate module: %w", err)
	}
	inst.module = module

	// Cache exported function handles.
	inst.fnAlloc = module.ExportedFunction("alloc")
	inst.fnDealloc = module.ExportedFunction("dealloc")
	inst.fnEval = module.ExportedFunction("eval")
	inst.fnEvalCompiled = module.ExportedFunction("eval_compiled")
	inst.fnDispatchEvent = module.ExportedFunction("dispatch_event")
	inst.fnResolveRpc = module.ExportedFunction("resolve_rpc")
	inst.fnRejectRpc = module.ExportedFunction("reject_rpc")
	inst.fnGetOpcodeCount = module.ExportedFunction("get_opcode_count")
	inst.fnGetCPUTimeUs = module.ExportedFunction("get_cpu_time_us")
	inst.fnResetExecutionStats = module.ExportedFunction("reset_execution_stats")
	inst.fnSetMemoryLimit = module.ExportedFunction("set_memory_limit")

	// Apply QuickJS memory limit if configured (before any JS evaluation).
	if opts.jsMemoryLimit > 0 {
		if _, err := inst.fnSetMemoryLimit.Call(instCtx, uint64(opts.jsMemoryLimit)); err != nil {
			module.Close(ctx)
			return nil, fmt.Errorf("kafig: set_memory_limit: %w", err)
		}
	}

	// Evaluate the prelude bytecode if configured.
	if len(opts.preludeBytecode) > 0 {
		if err := inst.evalPrelude(instCtx, opts.preludeBytecode); err != nil {
			module.Close(ctx)
			return nil, err
		}
		inst.logDebug("prelude evaluated")
	}

	// If configured, watch the creation context and auto-close on cancellation.
	if closeOnContextDone {
		go func() {
			<-ctx.Done()
			inst.Interrupt()
			inst.Close(context.Background())
		}()
	}

	inst.logDebug("instance created")
	return inst, nil
}

// withInstanceCtx returns a context carrying this Instance so shared host
// functions can dispatch to it.
func (inst *Instance) withInstanceCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, instanceContextKey{}, inst)
}

// evalPrelude evaluates compiled bytecode synchronously (no async wrapper).
func (inst *Instance) evalPrelude(ctx context.Context, bytecode []byte) error {
	ptr, err := inst.wasmAlloc(ctx, len(bytecode))
	if err != nil {
		return fmt.Errorf("kafig: alloc for prelude: %w", err)
	}
	inst.wasmWrite(ptr, bytecode)

	// eval_compiled(ptr, len, is_async=0)
	if _, err := inst.fnEvalCompiled.Call(ctx, uint64(ptr), uint64(len(bytecode)), 0); err != nil {
		inst.wasmDealloc(ctx, ptr, len(bytecode))
		return fmt.Errorf("kafig: eval_compiled (prelude): %w", err)
	}
	inst.wasmDealloc(ctx, ptr, len(bytecode))

	if inst.hasError {
		err := inst.scriptError
		inst.resetState()
		return fmt.Errorf("kafig: prelude error: %s", err.Error())
	}
	return nil
}

// Close releases the WASM module and its associated resources. Other
// instances sharing the same Runtime are unaffected.
func (inst *Instance) Close(ctx context.Context) error {
	return inst.module.Close(ctx)
}

// Eval evaluates a JavaScript source string globally. The result of the last
// expression is JSON-serialized and returned. Use WithAsync() to enable
// top-level await (JS_EVAL_FLAG_ASYNC) and drain the QuickJS job queue after
// evaluation, enabling promise resolution and RPC calls. When the last
// expression is a Promise, its resolved value is returned.
func (inst *Instance) Eval(ctx context.Context, source string, options ...EvalOption) (json.RawMessage, error) {
	var opts evalOptions
	for _, o := range options {
		o(&opts)
	}

	ctx = inst.withInstanceCtx(ctx)
	inst.interrupted.Store(false)
	inst.resetState()

	sourceBytes := []byte(source)
	ptr, err := inst.wasmAlloc(ctx, len(sourceBytes))
	if err != nil {
		return nil, fmt.Errorf("kafig: alloc for eval source: %w", err)
	}
	inst.wasmWrite(ptr, sourceBytes)

	isAsync := uint64(0)
	if opts.async_ {
		isAsync = 1
	}
	if _, err := inst.fnEval.Call(ctx, uint64(ptr), uint64(len(sourceBytes)), isAsync); err != nil {
		inst.wasmDealloc(ctx, ptr, len(sourceBytes))
		return nil, fmt.Errorf("kafig: eval: %w", err)
	}
	inst.wasmDealloc(ctx, ptr, len(sourceBytes))

	if opts.async_ {
		if err := inst.serviceRPCLoop(ctx); err != nil {
			return nil, err
		}
	}

	if inst.hasError {
		return nil, inst.scriptError
	}
	if inst.hasResult {
		return inst.resultValue, nil
	}
	return nil, nil
}

// EvalCompiled evaluates previously compiled bytecode. The bytecode must have
// been produced by [Runtime.Compile]. The result of the last expression is
// JSON-serialized and returned. Use WithAsync() if the compiled source uses
// promises or RPC calls. If the bytecode was compiled with WithAsync(), the
// result is the resolved value of the top-level Promise.
func (inst *Instance) EvalCompiled(ctx context.Context, bytecode []byte, options ...EvalOption) (json.RawMessage, error) {
	var opts evalOptions
	for _, o := range options {
		o(&opts)
	}

	ctx = inst.withInstanceCtx(ctx)
	inst.interrupted.Store(false)
	inst.resetState()

	ptr, err := inst.wasmAlloc(ctx, len(bytecode))
	if err != nil {
		return nil, fmt.Errorf("kafig: alloc for eval_compiled: %w", err)
	}
	inst.wasmWrite(ptr, bytecode)

	isAsync := uint64(0)
	if opts.async_ {
		isAsync = 1
	}
	if _, err := inst.fnEvalCompiled.Call(ctx, uint64(ptr), uint64(len(bytecode)), isAsync); err != nil {
		inst.wasmDealloc(ctx, ptr, len(bytecode))
		return nil, fmt.Errorf("kafig: eval_compiled: %w", err)
	}
	inst.wasmDealloc(ctx, ptr, len(bytecode))

	if opts.async_ {
		if err := inst.serviceRPCLoop(ctx); err != nil {
			return nil, err
		}
	}

	if inst.hasError {
		return nil, inst.scriptError
	}
	if inst.hasResult {
		return inst.resultValue, nil
	}
	return nil, nil
}

// DispatchEvent invokes a named event handler previously registered via
// host.on(name, fn) during Eval. The paramsJSON is passed to the handler.
// The handler's return value is JSON-serialized and returned. Use WithAsync()
// if the handler returns a Promise or uses RPC calls — the resolved value of
// the Promise is returned.
func (inst *Instance) DispatchEvent(ctx context.Context, name string, paramsJSON json.RawMessage, options ...EvalOption) (json.RawMessage, error) {
	var opts evalOptions
	for _, o := range options {
		o(&opts)
	}

	ctx = inst.withInstanceCtx(ctx)
	inst.interrupted.Store(false)
	inst.resetState()

	nameBytes := []byte(name)
	namePtr, err := inst.wasmAlloc(ctx, len(nameBytes))
	if err != nil {
		return nil, fmt.Errorf("kafig: alloc for event name: %w", err)
	}
	inst.wasmWrite(namePtr, nameBytes)

	paramsBytes := []byte(paramsJSON)
	paramsPtr, err := inst.wasmAlloc(ctx, len(paramsBytes))
	if err != nil {
		inst.wasmDealloc(ctx, namePtr, len(nameBytes))
		return nil, fmt.Errorf("kafig: alloc for event params: %w", err)
	}
	inst.wasmWrite(paramsPtr, paramsBytes)

	isAsync := uint64(0)
	if opts.async_ {
		isAsync = 1
	}
	if _, err := inst.fnDispatchEvent.Call(ctx,
		uint64(namePtr), uint64(len(nameBytes)),
		uint64(paramsPtr), uint64(len(paramsBytes)),
		isAsync,
	); err != nil {
		inst.wasmDealloc(ctx, namePtr, len(nameBytes))
		inst.wasmDealloc(ctx, paramsPtr, len(paramsBytes))
		return nil, fmt.Errorf("kafig: dispatch_event: %w", err)
	}
	inst.wasmDealloc(ctx, namePtr, len(nameBytes))
	inst.wasmDealloc(ctx, paramsPtr, len(paramsBytes))

	if opts.async_ {
		if err := inst.serviceRPCLoop(ctx); err != nil {
			return nil, err
		}
	}

	if inst.hasError {
		return nil, inst.scriptError
	}
	if inst.hasResult {
		return inst.resultValue, nil
	}
	return nil, nil
}

// Interrupt signals that JS execution should be interrupted. The interrupt
// takes effect within ~8192 opcodes when the next host_should_interrupt
// callback fires.
func (inst *Instance) Interrupt() {
	inst.interrupted.Store(true)
}

// ─── RPC service loop ────────────────────────────────────────────────────────

// serviceRPCLoop drains pending RPCs until all are resolved and no new ones
// are queued. RPC handlers run concurrently in goroutines so that independent
// host calls (e.g. from Promise.all) execute in parallel.
//
// Flow:
//  1. A WASM call returns. During it, host_rpc may have queued N rpcCalls.
//  2. If no pending RPCs, return immediately.
//  3. Launch all pending RPCs as goroutines writing to a channel.
//  4. Read results from the channel one by one, feeding each into WASM via
//     resolve_rpc / reject_rpc (which may queue more RPCs).
//  5. Any newly queued RPCs are also launched as goroutines.
//  6. Repeat until all RPCs are drained (inflight == 0).
//
// Note on early exit: when this loop returns early (e.g. due to hasError or a
// resolve/reject failure), the deferred rpcCancel() signals all in-flight
// goroutines to stop. However, handler goroutines that are already executing
// their handler function will run to completion before observing the
// cancellation — their side effects will occur but results are discarded.
// RPC handlers that need to abort promptly should check ctx.Done().
func (inst *Instance) serviceRPCLoop(ctx context.Context) error {
	if len(inst.pendingRPCs) == 0 {
		return nil
	}

	// Derive a cancellable context for RPC handlers. When serviceRPCLoop
	// returns (for any reason), the deferred cancel signals all in-flight
	// handler goroutines to abort. The goroutines select between sending
	// their result and observing cancellation, so they never block.
	rpcCtx, rpcCancel := context.WithCancel(ctx)
	defer rpcCancel()

	results := make(chan rpcResult, 16)
	inflight := 0

	// Launch initial batch of RPCs concurrently.
	inflight += inst.launchPendingRPCs(rpcCtx, results)

	for inflight > 0 {
		res := <-results
		inflight--

		// Feed the result back into WASM (single-threaded).
		if res.Err != nil {
			inst.logDebug("rpc rejected", "promise_id", res.PromiseID, "error", res.Err)
			if err := inst.rejectRPC(ctx, res.PromiseID, res.Err); err != nil {
				return fmt.Errorf("kafig: reject rpc (promise %d): %w", res.PromiseID, err)
			}
		} else {
			inst.logDebug("rpc resolved", "promise_id", res.PromiseID)
			if err := inst.resolveRPC(ctx, res.PromiseID, res.Value); err != nil {
				return fmt.Errorf("kafig: resolve rpc (promise %d): %w", res.PromiseID, err)
			}
		}

		// Early exit on error (e.g. unhandled exception during resolve/reject)
		if inst.hasError {
			return inst.scriptError
		}

		// The resolve/reject may have triggered more host_rpc calls.
		inflight += inst.launchPendingRPCs(rpcCtx, results)
	}

	return nil
}

// launchPendingRPCs starts a goroutine for each queued RPC and returns how
// many were launched. Each goroutine calls the handler and sends the
// result to the channel. If ctx is cancelled (e.g. serviceRPCLoop returned
// early), goroutines exit without blocking on the channel send.
func (inst *Instance) launchPendingRPCs(ctx context.Context, results chan<- rpcResult) int {
	batch := inst.pendingRPCs
	inst.pendingRPCs = inst.pendingRPCs[:0]

	for _, rpc := range batch {
		go func(r rpcCall) {
			defer func() {
				if p := recover(); p != nil {
					select {
					case results <- rpcResult{PromiseID: r.PromiseID, Err: fmt.Errorf("handler panic: %v", p)}:
					case <-ctx.Done():
					}
				}
			}()

			h, ok := inst.router.asyncHandlers[r.Method]
			if !ok {
				// Fallback: sync handlers can be called via the async path too.
				h, ok = inst.router.syncHandlers[r.Method]
			}
			if !ok {
				if inst.router.fallbackHandler != nil {
					value, err := inst.router.fallbackHandler(ctx, r.Method, r.Params)
					select {
					case results <- rpcResult{PromiseID: r.PromiseID, Value: value, Err: err}:
					case <-ctx.Done():
					}
					return
				}
				select {
				case results <- rpcResult{PromiseID: r.PromiseID, Err: fmt.Errorf("method %q not registered", r.Method)}:
				case <-ctx.Done():
				}
				return
			}

			value, err := h(ctx, r.Params)
			select {
			case results <- rpcResult{PromiseID: r.PromiseID, Value: value, Err: err}:
			case <-ctx.Done():
			}
		}(rpc)
	}

	return len(batch)
}

// resolveRPC writes a success response into WASM and calls resolve_rpc.
func (inst *Instance) resolveRPC(ctx context.Context, promiseID uint32, result json.RawMessage) error {
	if result == nil {
		result = json.RawMessage("null")
	}

	ptr, err := inst.wasmAlloc(ctx, len(result))
	if err != nil {
		return fmt.Errorf("alloc for resolve: %w", err)
	}
	inst.wasmWrite(ptr, result)

	if _, err := inst.fnResolveRpc.Call(ctx, uint64(promiseID), uint64(ptr), uint64(len(result))); err != nil {
		inst.wasmDealloc(ctx, ptr, len(result))
		return fmt.Errorf("resolve_rpc: %w", err)
	}
	inst.wasmDealloc(ctx, ptr, len(result))
	return nil
}

// rejectRPC writes an error response into WASM and calls reject_rpc.
func (inst *Instance) rejectRPC(ctx context.Context, promiseID uint32, rpcErr error) error {
	payload, _ := json.Marshal(map[string]string{"message": rpcErr.Error()})

	ptr, err := inst.wasmAlloc(ctx, len(payload))
	if err != nil {
		return fmt.Errorf("alloc for reject: %w", err)
	}
	inst.wasmWrite(ptr, payload)

	if _, err := inst.fnRejectRpc.Call(ctx, uint64(promiseID), uint64(ptr), uint64(len(payload))); err != nil {
		inst.wasmDealloc(ctx, ptr, len(payload))
		return fmt.Errorf("reject_rpc: %w", err)
	}
	inst.wasmDealloc(ctx, ptr, len(payload))
	return nil
}

// ─── Shared host import callbacks ───────────────────────────────────────────
// These are registered once on the shared wazero.Runtime and dispatch to the
// active *Instance via context. They are called synchronously from within WASM
// and must NOT call back into WASM.

func sharedHostRPC(ctx context.Context, methodPtr, methodLen, paramsPtr, paramsLen, promiseID uint32) {
	instanceFromContext(ctx).hostRPC(ctx, methodPtr, methodLen, paramsPtr, paramsLen, promiseID)
}

func sharedHostSetResult(ctx context.Context, resultPtr, resultLen, isError uint32) {
	instanceFromContext(ctx).hostSetResult(ctx, resultPtr, resultLen, isError)
}

func sharedHostRPCSync(ctx context.Context, methodPtr, methodLen, paramsPtr, paramsLen uint32) uint64 {
	return instanceFromContext(ctx).hostRPCSync(ctx, methodPtr, methodLen, paramsPtr, paramsLen)
}

func sharedHostShouldInterrupt(ctx context.Context, instructions, cpuTimeUs uint64) int32 {
	return instanceFromContext(ctx).hostShouldInterrupt(ctx, instructions, cpuTimeUs)
}

func sharedHostPromiseRejection(ctx context.Context, errorJsonPtr, errorJsonLen uint32) int32 {
	return instanceFromContext(ctx).hostPromiseRejection(ctx, errorJsonPtr, errorJsonLen)
}

// ─── Instance host import callbacks ─────────────────────────────────────────
// These are the actual implementations, called via the shared wrappers above.

// hostRPC is the host_rpc import. It copies the method and params from WASM
// memory and queues an rpcCall for later processing.
func (inst *Instance) hostRPC(_ context.Context, methodPtr, methodLen, paramsPtr, paramsLen, promiseID uint32) {
	method := inst.wasmReadString(methodPtr, methodLen)
	params := inst.wasmReadCopy(paramsPtr, paramsLen)

	inst.logDebug("rpc call queued", "method", method, "promise_id", promiseID)

	inst.pendingRPCs = append(inst.pendingRPCs, rpcCall{
		PromiseID: promiseID,
		Method:    method,
		Params:    json.RawMessage(params),
	})
}

// hostRPCSync is the host_rpc_sync import. It handles synchronous RPC calls
// from JS host.rpcSync(). The handler is called inline (no goroutine) and the
// result is written directly to WASM memory as a tagged buffer.
//
// Returns packed u64: (ptr << 32) | len
// At ptr: [tag_byte][json_bytes...] where tag 0=success, 1=error.
// Returns 0 on handler-not-found or allocation failure.
func (inst *Instance) hostRPCSync(ctx context.Context, methodPtr, methodLen, paramsPtr, paramsLen uint32) uint64 {
	method := inst.wasmReadString(methodPtr, methodLen)
	params := json.RawMessage(inst.wasmReadCopy(paramsPtr, paramsLen))

	inst.logDebug("sync rpc call", "method", method)

	// Look up sync handler, falling back to the catch-all fallback.
	h, ok := inst.router.syncHandlers[method]
	if !ok {
		if inst.router.fallbackHandler != nil {
			result, err := inst.router.fallbackHandler(ctx, method, params)
			if err != nil {
				inst.logDebug("sync rpc fallback error", "method", method, "error", err)
				return inst.writeSyncRPCResult(ctx, 1, []byte(err.Error()))
			}
			if result == nil {
				result = json.RawMessage("null")
			}
			inst.logDebug("sync rpc fallback resolved", "method", method)
			return inst.writeSyncRPCResult(ctx, 0, result)
		}
		return inst.writeSyncRPCResult(ctx, 1,
			[]byte(fmt.Sprintf("method %q not registered for sync RPC", method)))
	}

	// Call the handler synchronously — no goroutine, no channel.
	result, err := h(ctx, params)
	if err != nil {
		inst.logDebug("sync rpc error", "method", method, "error", err)
		return inst.writeSyncRPCResult(ctx, 1, []byte(err.Error()))
	}

	if result == nil {
		result = json.RawMessage("null")
	}
	inst.logDebug("sync rpc resolved", "method", method)
	return inst.writeSyncRPCResult(ctx, 0, result)
}

// writeSyncRPCResult allocates WASM memory, writes [tag][payload], and returns
// the packed u64. Returns 0 on allocation failure.
func (inst *Instance) writeSyncRPCResult(ctx context.Context, tag byte, payload []byte) uint64 {
	totalLen := 1 + len(payload)
	ptr, err := inst.wasmAlloc(ctx, totalLen)
	if err != nil {
		inst.logError("sync rpc alloc failed", "error", err)
		return 0
	}

	inst.module.Memory().WriteByte(ptr, tag)
	inst.module.Memory().Write(ptr+1, payload)

	return (uint64(ptr) << 32) | uint64(totalLen)
}

// hostSetResult is the host_set_result import. It handles both error reporting
// (is_error=1, from Rust send_js_error / send_runtime_error / promise_reject_cb)
// and result reporting (is_error=0, from Rust send_result_value / promise_resolve_cb).
func (inst *Instance) hostSetResult(_ context.Context, resultPtr, resultLen, isError uint32) {
	resultJSON := inst.wasmReadCopy(resultPtr, resultLen)

	if isError != 0 {
		inst.scriptError = parseErrorJSON(resultJSON)
		inst.hasError = true
	} else {
		inst.resultValue = json.RawMessage(resultJSON)
		inst.hasResult = true
	}
}

func (inst *Instance) resetState() {
	inst.scriptError = nil
	inst.hasError = false
	inst.resultValue = nil
	inst.hasResult = false
	inst.pendingRPCs = inst.pendingRPCs[:0]
}

// ─── WASM memory helpers ─────────────────────────────────────────────────────

func (inst *Instance) wasmAlloc(ctx context.Context, size int) (uint32, error) {
	if size == 0 {
		return 0, nil
	}
	res, err := inst.fnAlloc.Call(ctx, uint64(size))
	if err != nil {
		return 0, err
	}
	ptr := uint32(res[0])
	if ptr == 0 {
		return 0, fmt.Errorf("WASM alloc returned null (out of memory, requested %d bytes)", size)
	}
	return ptr, nil
}

func (inst *Instance) wasmDealloc(ctx context.Context, ptr uint32, size int) {
	inst.fnDealloc.Call(ctx, uint64(ptr), uint64(size)) //nolint:errcheck
}

func (inst *Instance) wasmWrite(ptr uint32, data []byte) {
	inst.module.Memory().Write(ptr, data)
}

// wasmReadCopy reads bytes from WASM memory and returns a Go-owned copy.
// The copy is necessary because WASM memory pointers are only valid during
// the host function call.
func (inst *Instance) wasmReadCopy(ptr, length uint32) []byte {
	data, ok := inst.module.Memory().Read(ptr, length)
	if !ok {
		return nil
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out
}

// wasmReadString reads bytes from WASM memory directly into a Go string,
// avoiding the intermediate []byte allocation that wasmReadCopy would need.
func (inst *Instance) wasmReadString(ptr, length uint32) string {
	data, ok := inst.module.Memory().Read(ptr, length)
	if !ok {
		return ""
	}
	return string(data)
}

// ─── Execution stats & interrupt handler ─────────────────────────────────────

// ExecutionStats holds opcode count and CPU time from JS execution.
type ExecutionStats struct {
	Opcodes   uint64
	CPUTimeUs uint64
}

// GetExecutionStats returns the opcode count and CPU time accumulated
// since the last ResetExecutionStats call (or instance creation).
func (inst *Instance) GetExecutionStats(ctx context.Context) (ExecutionStats, error) {
	ctx = inst.withInstanceCtx(ctx)
	instrRes, err := inst.fnGetOpcodeCount.Call(ctx)
	if err != nil {
		return ExecutionStats{}, fmt.Errorf("kafig: get_opcode_count: %w", err)
	}
	timeRes, err := inst.fnGetCPUTimeUs.Call(ctx)
	if err != nil {
		return ExecutionStats{}, fmt.Errorf("kafig: get_cpu_time_us: %w", err)
	}
	return ExecutionStats{Opcodes: instrRes[0], CPUTimeUs: timeRes[0]}, nil
}

// ResetExecutionStats zeros the instruction counter and CPU timer.
func (inst *Instance) ResetExecutionStats(ctx context.Context) error {
	ctx = inst.withInstanceCtx(ctx)
	_, err := inst.fnResetExecutionStats.Call(ctx)
	return err
}

// hostShouldInterrupt is called from the WASM interrupt handler every ~8192
// opcodes. It checks the interrupted flag and the user-provided callback.
func (inst *Instance) hostShouldInterrupt(_ context.Context, instructions, cpuTimeUs uint64) int32 {
	if inst.interrupted.Load() {
		return 1
	}
	if inst.interruptCallback != nil && inst.interruptCallback(instructions, cpuTimeUs) {
		return 1
	}
	return 0
}

// hostPromiseRejection is called from the WASM promise rejection tracker
// whenever an unhandled promise rejection occurs. It forwards the error to the
// user-provided handler and returns 1 to interrupt execution or 0 to continue.
func (inst *Instance) hostPromiseRejection(_ context.Context, errorJsonPtr, errorJsonLen uint32) int32 {
	if inst.promiseRejectionHandler == nil {
		return 0
	}
	errorJSON := inst.wasmReadCopy(errorJsonPtr, errorJsonLen)
	jsErr, ok := parseErrorJSON(errorJSON).(*JsError)
	if !ok {
		inst.logError("hostPromiseRejection: unexpected error type", "raw", string(errorJSON))
		return 0
	}
	if inst.promiseRejectionHandler(jsErr) {
		return 1
	}
	return 0
}

// ─── Logging helpers ─────────────────────────────────────────────────────────

func (inst *Instance) logDebug(msg string, args ...any) {
	if inst.logger != nil {
		inst.logger.Debug(msg, args...)
	}
}

func (inst *Instance) logError(msg string, args ...any) {
	if inst.logger != nil {
		inst.logger.Error(msg, args...)
	}
}
