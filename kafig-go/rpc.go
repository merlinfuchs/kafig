package kafig

import (
	"context"
	"encoding/json"
)

// RPCCallback is called when JavaScript code invokes an RPC method.
// Implementations must return a JSON-serializable result or an error.
//
// The context carries the deadline/cancellation of the parent Eval or
// DispatchEvent call.
type RPCCallback func(ctx context.Context, params json.RawMessage) (json.RawMessage, error)

// RPCRouter dispatches RPC calls to registered handlers. Sync handlers
// (registered via WithSync) are called inline during WASM execution with no
// goroutine overhead — ideal for fast, pure-compute calls like "get time".
// Async handlers (registered via WithAsync) run in goroutines with results
// fed back through the promise settlement loop — suitable for I/O-bound work.
//
// A method can be registered on both paths. When called via host.rpc() (async
// JS API), the router checks async handlers first, then falls back to sync
// handlers. When called via host.rpcSync() (sync JS API), only sync handlers
// are checked.
type RPCRouter struct {
	syncHandlers  map[string]RPCCallback
	asyncHandlers map[string]RPCCallback
}

// NewRPCRouter creates an empty RPCRouter.
func NewRPCRouter() *RPCRouter {
	return &RPCRouter{
		syncHandlers:  make(map[string]RPCCallback),
		asyncHandlers: make(map[string]RPCCallback),
	}
}

// WithSync registers a handler for synchronous dispatch (host.rpcSync in JS).
// The handler is called inline during WASM execution with no goroutine overhead.
func (r *RPCRouter) WithSync(method string, handler RPCCallback) *RPCRouter {
	r.syncHandlers[method] = handler
	return r
}

// WithAsync registers a handler for asynchronous dispatch (host.rpc in JS).
// The handler runs in a goroutine and the result is fed back via the promise
// settlement loop.
func (r *RPCRouter) WithAsync(method string, handler RPCCallback) *RPCRouter {
	r.asyncHandlers[method] = handler
	return r
}

// rpcCall is an in-flight RPC request from JavaScript to the Go host.
type rpcCall struct {
	PromiseID uint32
	Method    string
	Params    json.RawMessage
}

// rpcResult is the outcome of a single RPC handler invocation, sent back
// through the results channel so the service loop can feed it into WASM.
type rpcResult struct {
	PromiseID uint32
	Value     json.RawMessage // set on success
	Err       error           // set on failure
}
