package kafig

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// ─── Instance lifecycle ──────────────────────────────────────────────────────

func BenchmarkInstanceCreation(b *testing.B) {
	b.ReportAllocs()
	rt, err := New(context.Background(), WithCompilationCache(testCache))
	if err != nil {
		b.Fatal(err)
	}
	router := noopRouter()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		inst, err := rt.Instance(ctx, WithRouter(router))
		if err != nil {
			b.Fatal(err)
		}
		inst.Close(ctx)
	}
}

// ─── Eval paths ──────────────────────────────────────────────────────────────

func BenchmarkEvalSimple(b *testing.B) {
	b.ReportAllocs()
	inst := benchInstance(b, noopRouter())
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := inst.Eval(ctx, `1+1`); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEvalWithResult(b *testing.B) {
	b.ReportAllocs()
	inst := benchInstance(b, noopRouter())
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := inst.Eval(ctx, `42`)
		if err != nil {
			b.Fatal(err)
		}
		if string(result) != "42" {
			b.Fatalf("unexpected result: %s", result)
		}
	}
}

func BenchmarkEvalCompiled(b *testing.B) {
	b.ReportAllocs()
	rt, err := New(context.Background(), WithCompilationCache(testCache))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()

	bytecode, err := rt.Compile(ctx, `42`)
	if err != nil {
		b.Fatal(err)
	}

	inst, err := rt.Instance(ctx, WithRouter(noopRouter()))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { inst.Close(ctx) })

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := inst.EvalCompiled(ctx, bytecode)
		if err != nil {
			b.Fatal(err)
		}
		if string(result) != "42" {
			b.Fatalf("unexpected result: %s", result)
		}
	}
}

func BenchmarkEvalSourceVsCompiled(b *testing.B) {
	const script = `{ let sum = 0; for (let i = 0; i < 100; i++) { sum += i; } sum; }`

	b.Run("Source", func(b *testing.B) {
		b.ReportAllocs()
		inst := benchInstance(b, noopRouter())
		ctx := context.Background()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := inst.Eval(ctx, script); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Compiled", func(b *testing.B) {
		b.ReportAllocs()
		rt, err := New(context.Background(), WithCompilationCache(testCache))
		if err != nil {
			b.Fatal(err)
		}
		ctx := context.Background()

		bytecode, err := rt.Compile(ctx, script)
		if err != nil {
			b.Fatal(err)
		}
		inst, err := rt.Instance(ctx, WithRouter(noopRouter()))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { inst.Close(ctx) })

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := inst.EvalCompiled(ctx, bytecode); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ─── RPC ─────────────────────────────────────────────────────────────────────

func BenchmarkSyncRPC(b *testing.B) {
	b.ReportAllocs()
	router := NewRPCRouter().WithSync("echo", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
		return params, nil
	})
	inst := benchInstance(b, router)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := inst.Eval(ctx, `host.rpcSync("echo", {v:1})`)
		if err != nil {
			b.Fatal(err)
		}
		if len(result) == 0 {
			b.Fatal("empty result")
		}
	}
}

func BenchmarkAsyncRPC(b *testing.B) {
	b.ReportAllocs()
	router := NewRPCRouter().WithAsync("echo", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
		return params, nil
	})
	inst := benchInstance(b, router)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := inst.Eval(ctx, `
			var r = await host.rpc("echo", {v:1});
			r`, WithAsync())
		if err != nil {
			b.Fatal(err)
		}
		if len(result) == 0 {
			b.Fatal("empty result")
		}
	}
}

func BenchmarkConcurrentRPCs(b *testing.B) {
	for _, n := range []int{1, 5, 10} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			router := NewRPCRouter().WithAsync("echo", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
				return params, nil
			})
			inst := benchInstance(b, router)
			ctx := context.Background()

			// Build JS that does Promise.all with n calls.
			calls := make([]string, n)
			for i := range calls {
				calls[i] = fmt.Sprintf(`host.rpc("echo", {i:%d})`, i)
			}
			script := fmt.Sprintf(`
				var results = await Promise.all([%s]);
				results.length`, strings.Join(calls, ","))

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := inst.Eval(ctx, script, WithAsync())
				if err != nil {
					b.Fatal(err)
				}
				if string(result) != fmt.Sprintf("%d", n) {
					b.Fatalf("expected %d results, got %s", n, result)
				}
			}
		})
	}
}

func BenchmarkRPCLargePayload(b *testing.B) {
	b.ReportAllocs()
	router := NewRPCRouter().WithAsync("echo", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
		return params, nil
	})
	inst := benchInstance(b, router)
	ctx := context.Background()

	// Build a ~10KB JSON payload in JS.
	script := `
		var big = { data: "x".repeat(10000) };
		var r = await host.rpc("echo", big);
		r.data.length`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := inst.Eval(ctx, script, WithAsync())
		if err != nil {
			b.Fatal(err)
		}
		if string(result) != "10000" {
			b.Fatalf("unexpected result: %s", result)
		}
	}
}

// ─── Event dispatch ──────────────────────────────────────────────────────────

func BenchmarkDispatchEvent(b *testing.B) {
	b.ReportAllocs()
	inst := benchInstance(b, noopRouter())
	ctx := context.Background()

	// Register event handler.
	_, err := inst.Eval(ctx, `
		host.on("greet", (params) => "hello " + params.name);
	`)
	if err != nil {
		b.Fatal(err)
	}

	params := json.RawMessage(`{"name":"world"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := inst.DispatchEvent(ctx, "greet", params)
		if err != nil {
			b.Fatal(err)
		}
		if string(result) != `"hello world"` {
			b.Fatalf("unexpected result: %s", result)
		}
	}
}

// ─── Memory ─────────────────────────────────────────────────────────────────

// heapAlloc returns current Go heap allocation after forcing GC.
func heapAlloc() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func BenchmarkMemoryRuntime(b *testing.B) {
	ctx := context.Background()

	var goBytes uint64
	for i := 0; i < b.N; i++ {
		before := heapAlloc()
		rt, err := New(ctx)
		if err != nil {
			b.Fatal(err)
		}
		after := heapAlloc()
		goBytes = after - before
		rt.Close(ctx)
	}
	b.ReportMetric(float64(goBytes), "go-bytes")
	b.ReportMetric(float64(goBytes)/(1024*1024), "go-MB")
}

func BenchmarkMemoryInstance(b *testing.B) {
	ctx := context.Background()
	rt, err := New(ctx, WithCompilationCache(testCache))
	if err != nil {
		b.Fatal(err)
	}

	// heapAlloc includes the WASM linear memory (wazero backs it with a Go
	// []byte), so go-overhead = heap delta - wasm size.
	var heapDelta uint64
	var wasmSize uint32
	for i := 0; i < b.N; i++ {
		before := heapAlloc()
		inst, err := rt.Instance(ctx, WithRouter(noopRouter()))
		if err != nil {
			b.Fatal(err)
		}
		after := heapAlloc()
		heapDelta = after - before
		wasmSize = inst.module.Memory().Size()
		inst.Close(ctx)
	}
	b.ReportMetric(float64(wasmSize), "wasm-bytes")
	b.ReportMetric(float64(heapDelta-uint64(wasmSize)), "go-bytes")
	b.ReportMetric(float64(heapDelta)/(1024*1024), "total-MB")
}

func BenchmarkMemoryInstanceAfterEval(b *testing.B) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"Empty", `undefined`},
		{"SmallAlloc", `var a = "x".repeat(1024); a.length`},
		{"LargeAlloc", `var a = "x".repeat(1024*1024); a.length`},
		{"ManyObjects", `var arr = []; for (var i = 0; i < 10000; i++) arr.push({i:i}); arr.length`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ctx := context.Background()
			rt, err := New(ctx, WithCompilationCache(testCache))
			if err != nil {
				b.Fatal(err)
			}

			var heapDelta uint64
			var wasmSize uint32
			for i := 0; i < b.N; i++ {
				before := heapAlloc()
				inst, err := rt.Instance(ctx, WithRouter(noopRouter()))
				if err != nil {
					b.Fatal(err)
				}
				if _, err := inst.Eval(ctx, tc.script); err != nil {
					b.Fatal(err)
				}
				after := heapAlloc()
				heapDelta = after - before
				wasmSize = inst.module.Memory().Size()
				inst.Close(ctx)
			}
			b.ReportMetric(float64(wasmSize), "wasm-bytes")
			b.ReportMetric(float64(heapDelta-uint64(wasmSize)), "go-bytes")
			b.ReportMetric(float64(heapDelta)/(1024*1024), "total-MB")
		})
	}
}

func BenchmarkMemoryMultipleInstances(b *testing.B) {
	for _, n := range []int{1, 10, 50} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			ctx := context.Background()
			rt, err := New(ctx, WithCompilationCache(testCache))
			if err != nil {
				b.Fatal(err)
			}

			var heapPerInst float64
			var wasmPerInst float64
			for i := 0; i < b.N; i++ {
				before := heapAlloc()
				instances := make([]*Instance, n)
				for j := 0; j < n; j++ {
					inst, err := rt.Instance(ctx, WithRouter(noopRouter()))
					if err != nil {
						b.Fatal(err)
					}
					instances[j] = inst
				}
				after := heapAlloc()
				heapPerInst = float64(after-before) / float64(n)
				wasmPerInst = float64(instances[0].module.Memory().Size())
				for _, inst := range instances {
					inst.Close(ctx)
				}
			}
			b.ReportMetric(wasmPerInst, "wasm-bytes/inst")
			b.ReportMetric(heapPerInst-wasmPerInst, "go-bytes/inst")
			b.ReportMetric(heapPerInst/(1024*1024), "total-MB/inst")
		})
	}
}

// ─── JS computation ─────────────────────────────────────────────────────────

func BenchmarkJSComputation(b *testing.B) {
	b.ReportAllocs()
	inst := benchInstance(b, noopRouter())
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := inst.Eval(ctx, `{
			let sum = 0;
			for (let i = 0; i < 10000; i++) { sum += i; }
			sum;
		}`)
		if err != nil {
			b.Fatal(err)
		}
		if string(result) != "49995000" {
			b.Fatalf("unexpected result: %s", result)
		}
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func benchInstance(b *testing.B, router *RPCRouter) *Instance {
	b.Helper()
	rt, err := New(context.Background(), WithCompilationCache(testCache))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	inst, err := rt.Instance(ctx, WithRouter(router))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { inst.Close(ctx) })
	return inst
}
