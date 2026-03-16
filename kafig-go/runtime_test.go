package kafig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
)

var testCache wazero.CompilationCache

func TestMain(m *testing.M) {
	testCache = wazero.NewCompilationCache()
	code := m.Run()
	testCache.Close(context.Background())
	os.Exit(code)
}

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt, err := New(context.Background(), WithCompilationCache(testCache))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return rt
}

func noopRouter() *RPCRouter {
	return NewRPCRouter()
}

func newTestInstance(t *testing.T, router *RPCRouter) *Instance {
	t.Helper()
	rt := newTestRuntime(t)
	inst, err := rt.Instance(context.Background(), WithRouter(router))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	t.Cleanup(func() { inst.Close(context.Background()) })
	return inst
}

// ─── Basic eval ──────────────────────────────────────────────────────────────

func TestEvalNoError(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	_, err := inst.Eval(context.Background(), `const x = 1 + 2;`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
}

func TestEvalWithResult(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	result, err := inst.Eval(context.Background(), `1 + 2`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != "3" {
		t.Errorf("got %s, want 3", result)
	}
}

func TestEvalResultString(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	result, err := inst.Eval(context.Background(), `"hello world"`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `"hello world"` {
		t.Errorf("got %s, want %q", result, "hello world")
	}
}

func TestEvalResultObject(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	result, err := inst.Eval(context.Background(), `({a: 1, b: "two"})`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	var obj map[string]any
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["a"] != float64(1) || obj["b"] != "two" {
		t.Errorf("got %v", obj)
	}
}

func TestEvalResultNull(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	result, err := inst.Eval(context.Background(), `undefined`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != "null" {
		t.Errorf("got %s, want null", result)
	}
}

func TestEvalNoResult(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	// const declarations have no completion value, so the result is
	// JSON-serialized undefined → "null".
	result, err := inst.Eval(context.Background(), `const x = 42;`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != "null" {
		t.Errorf("got %s, want null (const declaration has no completion value)", result)
	}
}

// ─── Script errors ───────────────────────────────────────────────────────────

func TestEvalRuntimeError(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	_, err := inst.Eval(context.Background(), `throw new Error("boom");`)
	if err == nil {
		t.Fatal("expected error")
	}
	var scriptErr *ScriptError
	if !errors.As(err, &scriptErr) {
		t.Fatalf("expected ScriptError, got %T: %v", err, err)
	}
	if scriptErr.ErrorType != ErrorTypeRuntimeError {
		t.Errorf("errorType = %s, want runtime_error", scriptErr.ErrorType)
	}
	if scriptErr.Message != "boom" {
		t.Errorf("message = %q, want %q", scriptErr.Message, "boom")
	}
}

func TestEvalSyntaxError(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	_, err := inst.Eval(context.Background(), `this is not valid javascript !!!`)
	if err == nil {
		t.Fatal("expected error")
	}
	var scriptErr *ScriptError
	if !errors.As(err, &scriptErr) {
		t.Fatalf("expected ScriptError, got %T: %v", err, err)
	}
}

func TestEvalReferenceError(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	_, err := inst.Eval(context.Background(), `undeclaredVariable.property;`)
	if err == nil {
		t.Fatal("expected error")
	}
}

// ─── RPC calls ───────────────────────────────────────────────────────────────

func TestEvalWithSingleRPC(t *testing.T) {
	var gotParams json.RawMessage
	router := NewRPCRouter().
		WithAsync("myMethod", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
			gotParams = params
			return json.RawMessage(`"ok"`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		const val = await host.rpc("myMethod", {key: "value"});
		val`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `"ok"` {
		t.Errorf("got %s, want %q", result, "ok")
	}
	var p map[string]string
	json.Unmarshal(gotParams, &p)
	if p["key"] != "value" {
		t.Errorf("params = %s, want key=value", gotParams)
	}
}

func TestEvalWithMultipleRPCs(t *testing.T) {
	callCount := 0
	router := NewRPCRouter().
		WithAsync("set", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			callCount++
			return json.RawMessage("null"), nil
		}).
		WithAsync("get", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			callCount++
			return json.RawMessage(`"stored_value"`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		await host.rpc("set", {key: "a"});
		await host.rpc("set", {key: "b"});
		const val = await host.rpc("get", {key: "a"});
		val`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `"stored_value"` {
		t.Errorf("got %s, want %q", result, "stored_value")
	}
	if callCount != 3 {
		t.Errorf("callCount = %d, want 3", callCount)
	}
}

func TestEvalRPCError(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("doSomething", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return nil, fmt.Errorf("network timeout")
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		try {
			await host.rpc("doSomething", {});
			"should not reach";
		} catch(e) {
			e.message;
		}`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `"network timeout"` {
		t.Errorf("got %s, want %q", result, "network timeout")
	}
}

// ─── Event dispatch ──────────────────────────────────────────────────────────

func TestDispatchEventSync(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	// Register a sync handler.
	_, err := inst.Eval(context.Background(), `
		host.on("add", (params) => params.a + params.b);
	`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	// Dispatch without WithAsync().
	result, err := inst.DispatchEvent(context.Background(), "add", json.RawMessage(`{"a":10,"b":20}`))
	if err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}
	if string(result) != "30" {
		t.Errorf("got %s, want 30", result)
	}
}

func TestDispatchEventAsync(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("getData", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`42`), nil
		})

	inst := newTestInstance(t, router)

	// Register an async handler that uses await.
	_, err := inst.Eval(context.Background(), `
		host.on("fetch", async (params) => {
			const val = await host.rpc("getData", params);
			return val;
		});
	`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	// Dispatch with WithAsync() — handler uses await.
	result, err := inst.DispatchEvent(context.Background(), "fetch", json.RawMessage(`{}`), WithAsync())
	if err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}
	if string(result) != "42" {
		t.Errorf("got %s, want 42", result)
	}
}

func TestDispatchEventWithRPC(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("getData", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"status":200,"data":"fetched"}`), nil
		})

	inst := newTestInstance(t, router)

	_, err := inst.Eval(context.Background(), `
		host.on("process", async (params) => {
			const resp = await host.rpc("getData", {url: params.url});
			return { input: params.url, output: resp };
		});
	`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	result, err := inst.DispatchEvent(context.Background(), "process", json.RawMessage(`{"url":"https://api.test"}`), WithAsync())
	if err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}

	var res map[string]any
	json.Unmarshal(result, &res)
	if res["input"] != "https://api.test" {
		t.Errorf("input = %v", res["input"])
	}
}

func TestDispatchUnregisteredEvent(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	_, err := inst.Eval(context.Background(), `const x = 1;`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	_, err = inst.DispatchEvent(context.Background(), "nonexistent", json.RawMessage("null"))
	if err == nil {
		t.Fatal("expected error for unregistered event")
	}
	var scriptErr *ScriptError
	if !errors.As(err, &scriptErr) {
		t.Fatalf("expected ScriptError, got %T: %v", err, err)
	}
	if scriptErr.ErrorType != ErrorTypeRuntimeError {
		t.Errorf("errorType = %s, want runtime_error", scriptErr.ErrorType)
	}
}

func TestDispatchMultipleEvents(t *testing.T) {
	inst := newTestInstance(t, noopRouter())

	_, err := inst.Eval(context.Background(), `
		host.on("ping", (params) => "pong " + params.n);
	`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	for i := 0; i < 3; i++ {
		params, _ := json.Marshal(map[string]int{"n": i})
		result, err := inst.DispatchEvent(context.Background(), "ping", params)
		if err != nil {
			t.Fatalf("DispatchEvent %d: %v", i, err)
		}
		want := fmt.Sprintf(`"pong %d"`, i)
		if string(result) != want {
			t.Errorf("event %d: got %s, want %s", i, result, want)
		}
	}
}

// ─── Multiple instances ──────────────────────────────────────────────────────

func TestMultipleInstances(t *testing.T) {
	rt := newTestRuntime(t)
	router := noopRouter()

	inst1, err := rt.Instance(context.Background(), WithRouter(router))
	if err != nil {
		t.Fatalf("Instance 1: %v", err)
	}
	defer inst1.Close(context.Background())

	inst2, err := rt.Instance(context.Background(), WithRouter(router))
	if err != nil {
		t.Fatalf("Instance 2: %v", err)
	}
	defer inst2.Close(context.Background())

	// Set state in instance 1.
	_, err = inst1.Eval(context.Background(), `globalThis.myVar = "instance1"`)
	if err != nil {
		t.Fatalf("Eval inst1: %v", err)
	}

	// Instance 2 should not see instance 1's state.
	result, err := inst2.Eval(context.Background(), `globalThis.myVar ?? "undefined"`)
	if err != nil {
		t.Fatalf("Eval inst2: %v", err)
	}
	if string(result) != `"undefined"` {
		t.Errorf("inst2 got %s, want %q (instances should be isolated)", result, "undefined")
	}
}

// ─── CPU interrupt ───────────────────────────────────────────────────────────

func TestSetInterruptStopsExecution(t *testing.T) {
	inst := newTestInstance(t, noopRouter())
	ctx := context.Background()

	// Set interrupt from a timer goroutine.
	go func() {
		time.Sleep(50 * time.Millisecond)
		inst.Interrupt()
	}()

	_, err := inst.Eval(ctx, `while(true) {}`)
	if err == nil {
		t.Fatal("expected error from interrupt")
	}
	var scriptErr *ScriptError
	if !errors.As(err, &scriptErr) {
		t.Fatalf("expected ScriptError, got %T: %v", err, err)
	}
	if scriptErr.Message != "interrupted" {
		t.Errorf("message = %q, want %q", scriptErr.Message, "interrupted")
	}
	if scriptErr.ErrorType != ErrorTypeCPULimitExceeded {
		t.Errorf("errorType = %s, want cpu_limit_exceeded", scriptErr.ErrorType)
	}
}

func TestInterruptClearedBetweenEvals(t *testing.T) {
	inst := newTestInstance(t, noopRouter())
	ctx := context.Background()

	// Trigger an interrupt.
	go func() {
		time.Sleep(50 * time.Millisecond)
		inst.Interrupt()
	}()
	_, err := inst.Eval(ctx, `while(true) {}`)
	if err == nil {
		t.Fatal("first eval should have been interrupted")
	}

	// Next eval should work fine — clear_interrupt is called automatically.
	result, err := inst.Eval(ctx, `42`)
	if err != nil {
		t.Fatalf("second eval should succeed, got: %v", err)
	}
	if string(result) != "42" {
		t.Errorf("got %s, want 42", result)
	}
}

// ─── Async behavior ─────────────────────────────────────────────────────────

func TestEvalWithAsyncAwait(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("getData", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"data"`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		async function fetchData() {
			const a = await host.rpc("getData", {id: 1});
			const b = await host.rpc("getData", {id: 2});
			return [a, b];
		}
		await fetchData()`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `["data","data"]` {
		t.Errorf("got %s", result)
	}
}

func TestEvalPromiseChain(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("getNumber", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`10`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		const val = await host.rpc("getNumber");
		const doubled = val * 2;
		doubled`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != "20" {
		t.Errorf("got %s, want 20", result)
	}
}

// ─── Concurrent RPC execution ────────────────────────────────────────────────

func TestPromiseAllRunsConcurrently(t *testing.T) {
	// Each RPC sleeps 100ms. If run sequentially, 3 RPCs would take ≥300ms.
	// With concurrent execution they should complete in ~100ms.
	router := NewRPCRouter().
		WithAsync("slowOp", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
			var p struct{ ID string }
			json.Unmarshal(params, &p)
			time.Sleep(100 * time.Millisecond)
			result, _ := json.Marshal(fmt.Sprintf("result_%s", p.ID))
			return result, nil
		})

	inst := newTestInstance(t, router)

	start := time.Now()
	result, err := inst.Eval(context.Background(), `
		const [a, b, c] = await Promise.all([
			host.rpc("slowOp", {id: "1"}),
			host.rpc("slowOp", {id: "2"}),
			host.rpc("slowOp", {id: "3"}),
		]);
		[a, b, c]`, WithAsync())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	var results []string
	json.Unmarshal(result, &results)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, want := range []string{"result_1", "result_2", "result_3"} {
		if results[i] != want {
			t.Errorf("results[%d] = %q, want %q", i, results[i], want)
		}
	}

	// With concurrency, 3x100ms sleeps should take ~100-200ms, not ~300ms+.
	if elapsed >= 280*time.Millisecond {
		t.Errorf("took %v — RPCs likely ran sequentially instead of concurrently", elapsed)
	}
	t.Logf("Promise.all with 3x100ms RPCs completed in %v", elapsed)
}

func TestPromiseAllWithMixedMethods(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("alpha", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"from_alpha"`), nil
		}).
		WithAsync("beta", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"from_beta"`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		const [a, b] = await Promise.all([
			host.rpc("alpha"),
			host.rpc("beta"),
		]);
		({ a, b })`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	var res map[string]string
	json.Unmarshal(result, &res)
	if res["a"] != "from_alpha" {
		t.Errorf("a = %q", res["a"])
	}
	if res["b"] != "from_beta" {
		t.Errorf("b = %q", res["b"])
	}
}

func TestPromiseAllWithPartialError(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("op", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
			var p struct{ ID string }
			json.Unmarshal(params, &p)
			if p.ID == "fail" {
				return nil, fmt.Errorf("request failed")
			}
			return json.RawMessage(`"ok"`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		try {
			await Promise.all([
				host.rpc("op", {id: "good"}),
				host.rpc("op", {id: "fail"}),
				host.rpc("op", {id: "good2"}),
			]);
			"should not reach";
		} catch(e) {
			e.message;
		}`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `"request failed"` {
		t.Errorf("got %s, want %q", result, "request failed")
	}
}

// ─── Prelude and options ─────────────────────────────────────────────────────

func TestPreludeDefinesGlobals(t *testing.T) {
	rt, err := New(context.Background(), WithPrelude(`
		function greet(name) { return "hello " + name; }
		const VERSION = 42;
	`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inst, err := rt.Instance(context.Background(), WithRouter(noopRouter()))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	defer inst.Close(context.Background())

	result, err := inst.Eval(context.Background(), `greet("world") + " v" + VERSION`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `"hello world v42"` {
		t.Errorf("got %s, want %q", result, "hello world v42")
	}
}

func TestPreludeSharedAcrossInstances(t *testing.T) {
	rt, err := New(context.Background(), WithPrelude(`
		function add(a, b) { return a + b; }
	`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 3; i++ {
		inst, err := rt.Instance(context.Background(), WithRouter(noopRouter()))
		if err != nil {
			t.Fatalf("Instance %d: %v", i, err)
		}

		result, err := inst.Eval(context.Background(), `add(10, 20)`)
		if err != nil {
			t.Fatalf("Eval %d: %v", i, err)
		}
		if string(result) != "30" {
			t.Errorf("instance %d: got %s, want 30", i, result)
		}
		inst.Close(context.Background())
	}
}

func TestPreludeSyntaxError(t *testing.T) {
	_, err := New(context.Background(), WithPrelude(`this is not valid javascript !!!`))
	if err == nil {
		t.Fatal("expected error from bad prelude")
	}
	t.Logf("got expected error: %v", err)
}

func TestCloseOnContextDone(t *testing.T) {
	rt, err := New(context.Background(), WithCloseOnContextDone(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	inst, err := rt.Instance(ctx, WithRouter(noopRouter()))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}

	// Verify the instance works before cancellation.
	result, err := inst.Eval(context.Background(), `1 + 1`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != "2" {
		t.Errorf("got %s, want 2", result)
	}

	// Cancel the context — the instance should auto-close.
	cancel()
	time.Sleep(50 * time.Millisecond)

	// After close, Eval should fail.
	_, err = inst.Eval(context.Background(), `1`)
	if err == nil {
		t.Fatal("expected error after context cancellation closed the instance")
	}
	t.Logf("got expected error after close: %v", err)
}

// ─── Bytecode compilation ────────────────────────────────────────────────────

func TestCompileThenEval(t *testing.T) {
	rt := newTestRuntime(t)
	defer rt.Close(context.Background())

	bytecode, err := rt.Compile(context.Background(), `10 * 5`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(bytecode) == 0 {
		t.Fatal("expected non-empty bytecode")
	}

	inst, err := rt.Instance(context.Background(), WithRouter(noopRouter()))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	defer inst.Close(context.Background())

	result, err := inst.EvalCompiled(context.Background(), bytecode)
	if err != nil {
		t.Fatalf("EvalCompiled: %v", err)
	}
	if string(result) != "50" {
		t.Errorf("got %s, want 50", result)
	}
}

func TestCompileSyntaxError(t *testing.T) {
	rt := newTestRuntime(t)
	defer rt.Close(context.Background())

	_, err := rt.Compile(context.Background(), `this is not valid javascript !!!`)
	if err == nil {
		t.Fatal("expected error from bad source")
	}
	t.Logf("got expected error: %v", err)
}

func TestCompileBytecodeReusedAcrossInstances(t *testing.T) {
	rt := newTestRuntime(t)
	defer rt.Close(context.Background())

	bytecode, err := rt.Compile(context.Background(), `7 * 6`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	for i := 0; i < 3; i++ {
		inst, err := rt.Instance(context.Background(), WithRouter(noopRouter()))
		if err != nil {
			t.Fatalf("Instance %d: %v", i, err)
		}

		result, err := inst.EvalCompiled(context.Background(), bytecode)
		if err != nil {
			t.Fatalf("EvalCompiled %d: %v", i, err)
		}
		if string(result) != "42" {
			t.Errorf("instance %d: got %s, want 42", i, result)
		}
		inst.Close(context.Background())
	}
}

// ─── Sync RPC ────────────────────────────────────────────────────────────────

func TestSyncRPC(t *testing.T) {
	router := NewRPCRouter().
		WithSync("getTime", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`1234567890`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `host.rpcSync("getTime")`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != "1234567890" {
		t.Errorf("got %s, want 1234567890", result)
	}
}

func TestSyncRPCWithParams(t *testing.T) {
	router := NewRPCRouter().
		WithSync("add", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
			var p struct{ A, B int }
			json.Unmarshal(params, &p)
			result, _ := json.Marshal(p.A + p.B)
			return result, nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `host.rpcSync("add", {A: 3, B: 7})`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != "10" {
		t.Errorf("got %s, want 10", result)
	}
}

func TestSyncRPCError(t *testing.T) {
	router := NewRPCRouter().
		WithSync("fail", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return nil, fmt.Errorf("something went wrong")
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		try {
			host.rpcSync("fail");
			"should not reach";
		} catch(e) {
			e.message;
		}
	`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `"something went wrong"` {
		t.Errorf("got %s, want %q", result, "something went wrong")
	}
}

func TestSyncRPCMethodNotFound(t *testing.T) {
	router := NewRPCRouter().
		WithSync("exists", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`true`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		try {
			host.rpcSync("doesNotExist");
			"should not reach";
		} catch(e) {
			"caught";
		}
	`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `"caught"` {
		t.Errorf("got %s, want %q", result, "caught")
	}
}

func TestMixedSyncAndAsyncRPC(t *testing.T) {
	router := NewRPCRouter().
		WithSync("getTime", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`1000`), nil
		}).
		WithAsync("fetchData", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"fetched"`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		const t = host.rpcSync("getTime");
		const d = await host.rpc("fetchData");
		({ time: t, data: d })`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	var res map[string]any
	json.Unmarshal(result, &res)
	if res["time"] != float64(1000) {
		t.Errorf("time = %v, want 1000", res["time"])
	}
	if res["data"] != "fetched" {
		t.Errorf("data = %v, want fetched", res["data"])
	}
}

func TestSyncHandlerViaAsyncPath(t *testing.T) {
	// A sync-registered handler should be callable via host.rpc() (async path)
	// as a fallback.
	router := NewRPCRouter().
		WithSync("getTime", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`999`), nil
		})

	inst := newTestInstance(t, router)
	result, err := inst.Eval(context.Background(), `
		const val = await host.rpc("getTime");
		val`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != "999" {
		t.Errorf("got %s, want 999", result)
	}
}

// ─── Instance reuse ──────────────────────────────────────────────────────────

func TestReuseAfterError(t *testing.T) {
	inst := newTestInstance(t, noopRouter())
	ctx := context.Background()

	// First call throws.
	_, err := inst.Eval(ctx, `throw new Error("boom");`)
	if err == nil {
		t.Fatal("expected error")
	}

	// Second call should succeed — state is clean.
	result, err := inst.Eval(ctx, `42`)
	if err != nil {
		t.Fatalf("second eval should succeed, got: %v", err)
	}
	if string(result) != "42" {
		t.Errorf("got %s, want 42", result)
	}
}

func TestReuseAfterInterrupt(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("getData", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"ok"`), nil
		})

	inst := newTestInstance(t, router)
	ctx := context.Background()

	// Interrupt an infinite loop.
	go func() {
		time.Sleep(50 * time.Millisecond)
		inst.Interrupt()
	}()
	_, err := inst.Eval(ctx, `while(true) {}`)
	if err == nil {
		t.Fatal("expected interrupt error")
	}

	// Subsequent async eval with RPC should succeed.
	result, err := inst.Eval(ctx, `
		const val = await host.rpc("getData");
		val`, WithAsync())
	if err != nil {
		t.Fatalf("second eval should succeed, got: %v", err)
	}
	if string(result) != `"ok"` {
		t.Errorf("got %s, want %q", result, "ok")
	}
}

func TestReuseAfterRPCError(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("failOp", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return nil, fmt.Errorf("handler error")
		}).
		WithAsync("okOp", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"success"`), nil
		})

	inst := newTestInstance(t, router)
	ctx := context.Background()

	// First call: RPC handler returns error, JS catches it.
	result, err := inst.Eval(ctx, `
		try {
			await host.rpc("failOp");
			"should not reach";
		} catch(e) {
			"caught: " + e.message;
		}`, WithAsync())
	if err != nil {
		t.Fatalf("first eval: %v", err)
	}
	if string(result) != `"caught: handler error"` {
		t.Errorf("first eval: got %s", result)
	}

	// Second call: should succeed cleanly with a different RPC.
	result, err = inst.Eval(ctx, `
		const val = await host.rpc("okOp");
		val`, WithAsync())
	if err != nil {
		t.Fatalf("second eval should succeed, got: %v", err)
	}
	if string(result) != `"success"` {
		t.Errorf("got %s, want %q", result, "success")
	}
}

func TestReuseAfterPanicInHandler(t *testing.T) {
	callCount := 0
	router := NewRPCRouter().
		WithAsync("panicOp", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			panic("handler blew up")
		}).
		WithAsync("safeOp", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			callCount++
			return json.RawMessage(`"safe"`), nil
		})

	inst := newTestInstance(t, router)
	ctx := context.Background()

	// First call: handler panics — should be recovered as an error.
	result, err := inst.Eval(ctx, `
		try {
			await host.rpc("panicOp");
			"should not reach";
		} catch(e) {
			e.message;
		}`, WithAsync())
	if err != nil {
		t.Fatalf("eval should succeed (panic caught as rejection), got: %v", err)
	}
	if string(result) != `"handler panic: handler blew up"` {
		t.Errorf("got %s", result)
	}

	// Second call: should succeed cleanly.
	result, err = inst.Eval(ctx, `
		const val = await host.rpc("safeOp");
		val`, WithAsync())
	if err != nil {
		t.Fatalf("second eval should succeed, got: %v", err)
	}
	if string(result) != `"safe"` {
		t.Errorf("got %s, want %q", result, "safe")
	}
	if callCount != 1 {
		t.Errorf("callCount = %d, want 1", callCount)
	}
}

func TestDispatchEventReuseManyTimes(t *testing.T) {
	inst := newTestInstance(t, noopRouter())
	ctx := context.Background()

	_, err := inst.Eval(ctx, `
		host.on("double", (params) => params.n * 2);
	`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}

	for i := 0; i < 100; i++ {
		params, _ := json.Marshal(map[string]int{"n": i})
		result, err := inst.DispatchEvent(ctx, "double", params)
		if err != nil {
			t.Fatalf("DispatchEvent %d: %v", i, err)
		}
		want := fmt.Sprintf("%d", i*2)
		if string(result) != want {
			t.Errorf("event %d: got %s, want %s", i, result, want)
		}
	}
}

func TestReuseAfterEarlyServiceLoopExit(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("failOp", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return nil, fmt.Errorf("fail")
		}).
		WithAsync("slowOp", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			time.Sleep(100 * time.Millisecond)
			return json.RawMessage(`"slow_result"`), nil
		}).
		WithAsync("okOp", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"ok"`), nil
		})

	inst := newTestInstance(t, router)
	ctx := context.Background()

	// Promise.all with a failing and a slow RPC. The failing one rejects
	// Promise.all, JS catches it. Both RPCs are launched concurrently.
	result, err := inst.Eval(ctx, `
		try {
			await Promise.all([
				host.rpc("failOp"),
				host.rpc("slowOp"),
			]);
			"should not reach";
		} catch(e) {
			"caught: " + e.message;
		}`, WithAsync())
	if err != nil {
		t.Fatalf("first eval: %v", err)
	}
	if string(result) != `"caught: fail"` {
		t.Errorf("first eval: got %s", result)
	}

	// Wait for the slow goroutine to finish.
	time.Sleep(150 * time.Millisecond)

	// Next call should succeed — no dangling state from the slow RPC.
	result, err = inst.Eval(ctx, `
		const val = await host.rpc("okOp");
		val`, WithAsync())
	if err != nil {
		t.Fatalf("second eval should succeed, got: %v", err)
	}
	if string(result) != `"ok"` {
		t.Errorf("got %s, want %q", result, "ok")
	}
}

// ─── WithInterruptCallback ──────────────────────────────────────────────────

func newTestInstanceWithInterruptCallback(t *testing.T, router *RPCRouter, cb func(uint64, uint64) bool) *Instance {
	t.Helper()
	rt := newTestRuntime(t)
	inst, err := rt.Instance(context.Background(), WithRouter(router), WithInterruptCallback(cb))
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	t.Cleanup(func() { inst.Close(context.Background()) })
	return inst
}

func TestInterruptCallbackStopsExecution(t *testing.T) {
	inst := newTestInstanceWithInterruptCallback(t, noopRouter(), func(instructions, _ uint64) bool {
		return instructions > 10_000
	})

	_, err := inst.Eval(context.Background(), `while(true) {}`)
	if err == nil {
		t.Fatal("expected interrupt error")
	}
	var scriptErr *ScriptError
	if !errors.As(err, &scriptErr) {
		t.Fatalf("expected ScriptError, got %T: %v", err, err)
	}
	if scriptErr.ErrorType != ErrorTypeCPULimitExceeded {
		t.Errorf("errorType = %s, want cpu_limit_exceeded", scriptErr.ErrorType)
	}
}

func TestInterruptCallbackNotTriggeredOnShortScript(t *testing.T) {
	inst := newTestInstanceWithInterruptCallback(t, noopRouter(), func(instructions, _ uint64) bool {
		return instructions > 1_000_000
	})

	result, err := inst.Eval(context.Background(), `42`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != "42" {
		t.Errorf("got %s, want 42", result)
	}
}

func TestInterruptCallbackReceivesStats(t *testing.T) {
	var lastInstructions, lastCPUTimeUs uint64
	inst := newTestInstanceWithInterruptCallback(t, noopRouter(), func(instructions, cpuTimeUs uint64) bool {
		lastInstructions = instructions
		lastCPUTimeUs = cpuTimeUs
		return instructions > 50_000
	})

	_, err := inst.Eval(context.Background(), `
		let sum = 0;
		for (let i = 0; i < 100000; i++) { sum += i; }
	`)
	// May or may not be interrupted depending on loop cost.
	_ = err

	if lastInstructions == 0 {
		t.Error("callback never received a non-zero instruction count")
	}
	// CPU time should be non-zero since we did real work.
	if lastCPUTimeUs == 0 {
		t.Error("callback never received a non-zero CPU time")
	}
	t.Logf("last callback: instructions=%d, cpuTimeUs=%d", lastInstructions, lastCPUTimeUs)
}

func TestInterruptCallbackWithAsyncRPC(t *testing.T) {
	router := NewRPCRouter().
		WithAsync("getData", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"data"`), nil
		})

	inst := newTestInstanceWithInterruptCallback(t, router, func(instructions, _ uint64) bool {
		return instructions > 1_000_000 // High threshold — shouldn't trigger.
	})

	result, err := inst.Eval(context.Background(), `
		const val = await host.rpc("getData");
		val`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if string(result) != `"data"` {
		t.Errorf("got %s, want %q", result, "data")
	}
}

func TestInterruptCallbackCPUTimeDuringRPC(t *testing.T) {
	var maxCPUTimeUs uint64
	router := NewRPCRouter().
		WithAsync("noop", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`null`), nil
		})

	inst := newTestInstanceWithInterruptCallback(t, router, func(_, cpuTimeUs uint64) bool {
		if cpuTimeUs > maxCPUTimeUs {
			maxCPUTimeUs = cpuTimeUs
		}
		return false // Never interrupt.
	})

	// Do heavy computation after an RPC settles to verify CPU time tracks
	// during the post-RPC JS continuation.
	_, err := inst.Eval(context.Background(), `
		await host.rpc("noop");
		let sum = 0;
		for (let i = 0; i < 100000; i++) { sum += i; }
		sum`, WithAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if maxCPUTimeUs == 0 {
		t.Error("CPU time was never tracked during post-RPC JS execution")
	}
	t.Logf("maxCPUTimeUs observed = %d", maxCPUTimeUs)
}

func TestInterruptCallbackCumulativeAcrossDispatches(t *testing.T) {
	var instructionsAfterSecond uint64
	callCount := 0

	inst := newTestInstanceWithInterruptCallback(t, noopRouter(), func(instructions, _ uint64) bool {
		callCount++
		instructionsAfterSecond = instructions
		return false // Never interrupt.
	})

	ctx := context.Background()

	// First eval — does some work.
	_, err := inst.Eval(ctx, `
		let sum = 0;
		for (let i = 0; i < 100000; i++) { sum += i; }
	`)
	if err != nil {
		t.Fatalf("first eval: %v", err)
	}
	firstInstructions := instructionsAfterSecond

	// Reset callback tracking for second eval.
	callCount = 0

	// Second eval — instruction count should continue from where the first left off.
	_, err = inst.Eval(ctx, `
		let sum2 = 0;
		for (let i = 0; i < 100000; i++) { sum2 += i; }
	`)
	if err != nil {
		t.Fatalf("second eval: %v", err)
	}

	if instructionsAfterSecond <= firstInstructions {
		t.Errorf("instruction count did not accumulate: first=%d, second=%d",
			firstInstructions, instructionsAfterSecond)
	}
	t.Logf("instructions: after first=%d, after second=%d", firstInstructions, instructionsAfterSecond)
}

