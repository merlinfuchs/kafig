package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// rpcRequestMsg is the wire format for an RPC call sent to the external caller.
type rpcRequestMsg struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// rpcResponseMsg is the wire format for an RPC response from the external caller.
type rpcResponseMsg struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// rpcForwarder forwards unhandled RPC calls to an external process over a
// writer (STDOUT) and waits for responses routed via Deliver.
type rpcForwarder struct {
	writeMu sync.Mutex
	writer  io.Writer

	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan rpcResponseMsg
}

func newRPCForwarder(w io.Writer) *rpcForwarder {
	return &rpcForwarder{
		writer:  w,
		pending: make(map[uint64]chan rpcResponseMsg),
	}
}

// Forward is an RPCFallbackCallback. It writes the RPC request to the writer
// and blocks until a matching response is delivered or the context is cancelled.
func (f *rpcForwarder) Forward(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := f.nextID.Add(1)

	ch := make(chan rpcResponseMsg, 1)
	f.mu.Lock()
	f.pending[id] = ch
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.pending, id)
		f.mu.Unlock()
	}()

	// Write the RPC request as a JSON line.
	line, _ := json.Marshal(struct {
		RPC rpcRequestMsg `json:"rpc"`
	}{
		RPC: rpcRequestMsg{ID: id, Method: method, Params: params},
	})

	f.writeMu.Lock()
	_, err := fmt.Fprintln(f.writer, string(line))
	f.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("failed to write RPC request: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Deliver routes an incoming rpc_response to the waiting Forward call.
func (f *rpcForwarder) Deliver(resp rpcResponseMsg) {
	f.mu.Lock()
	ch, ok := f.pending[resp.ID]
	f.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// interactiveRPCForwarder handles RPC forwarding in interactive mode by
// printing the RPC call and reading a JSON response line from stdin.
type interactiveRPCForwarder struct {
	nextID atomic.Uint64
}

func newInteractiveRPCForwarder() *interactiveRPCForwarder {
	return &interactiveRPCForwarder{}
}

// Forward prints the RPC call and reads a JSON response from stdin.
// This works during go-prompt's executor callback where the prompt is paused.
func (f *interactiveRPCForwarder) Forward(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := f.nextID.Add(1)

	// Print RPC request to stderr so it doesn't mix with result output.
	req := struct {
		RPC rpcRequestMsg `json:"rpc"`
	}{
		RPC: rpcRequestMsg{ID: id, Method: method, Params: params},
	}
	line, _ := json.Marshal(req)
	fmt.Fprintf(os.Stderr, "\n%s\n", string(line))
	fmt.Fprintf(os.Stderr, "rpc response> ")

	// Read a single line from stdin.
	reader := bufio.NewReader(os.Stdin)
	respLine, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read RPC response: %w", err)
	}

	var resp rpcResponseMsg
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		return nil, fmt.Errorf("invalid RPC response JSON: %w", err)
	}

	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}
