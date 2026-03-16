use rquickjs::qjs;
use rquickjs::{Context, Error, Function, Runtime, Value};
use std::ffi::{c_int, CString};
use std::sync::OnceLock;
use std::time::Instant;

// ─── Limits ───────────────────────────────────────────────────────────────────
const JS_MEMORY_LIMIT: usize = 32 * 1024 * 1024;
const JS_STACK_LIMIT: usize = 512 * 1024;

// ─── Execution tracking ──────────────────────────────────────────────────────

static mut OPCODE_COUNT: u64 = 0;
static mut EXEC_START: Option<Instant> = None;
static mut CPU_ELAPSED_US: u64 = 0;
static mut DRAINING: bool = false;
static mut INITIALIZED: bool = false;
static mut USED_RPC_CALLS: bool = false;

// Call host_should_interrupt on every QuickJS interrupt handler invocation.
// QuickJS fires the handler every ~10,000 branch/loop opcodes
// (controlled by JS_INTERRUPT_COUNTER_INIT = 10000). We increment
// OPCODE_COUNT by 10,000 per call to approximate real opcode counts.
const CHECK_INTERVAL: u64 = 1;
static mut NEXT_CHECK: u64 = CHECK_INTERVAL;

unsafe fn sample_elapsed_us() -> u64 {
    unsafe {
        match EXEC_START {
            Some(start) => {
                let e = start.elapsed().as_micros() as u64;
                CPU_ELAPSED_US = e;
                e
            }
            None => CPU_ELAPSED_US,
        }
    }
}

#[unsafe(export_name = "get_opcode_count")]
pub extern "C" fn get_opcode_count() -> u64 {
    unsafe { OPCODE_COUNT }
}

#[unsafe(export_name = "get_cpu_time_us")]
pub extern "C" fn get_cpu_time_us() -> u64 {
    unsafe { sample_elapsed_us() }
}

#[unsafe(export_name = "reset_execution_stats")]
pub extern "C" fn reset_execution_stats() {
    unsafe {
        OPCODE_COUNT = 0;
        CPU_ELAPSED_US = 0;
        EXEC_START = None;
        NEXT_CHECK = CHECK_INTERVAL;
    }
}

// ─── Stale state cleanup ──────────────────────────────────────────────────────
// Called at the start of each eval/dispatch to discard leftover pending jobs
// and orphaned pending RPC promises from a previous invocation. Since QuickJS
// has no API to drop pending jobs without executing them, we set a DRAINING
// flag so the interrupt handler returns true immediately — each stale job
// throws "interrupted" on its first opcode without executing meaningful code.
// Execution stats are saved and restored so the drain doesn't affect the
// current dispatch's tracking.

fn discard_stale_state() {
    unsafe {
        INITIALIZED = true;
        if !USED_RPC_CALLS {
            return; // Fast path: nothing to clean up
        }
        USED_RPC_CALLS = false;
    }

    let js = RUNTIME.get().expect("wizer_initialize was not called");
    unsafe {
        let saved = (OPCODE_COUNT, CPU_ELAPSED_US, EXEC_START, NEXT_CHECK);
        DRAINING = true;

        js.ctx.with(|ctx| {
            // Drain stale jobs — they throw "interrupted" on first opcode
            while ctx.execute_pending_job() {}

            // Clear any exception left by interrupted jobs.
            let _ = ctx.catch();
        });

        // Stop forcing interrupts before calling __reset, so the reset
        // function can execute normally.
        DRAINING = false;
        OPCODE_COUNT = saved.0;
        CPU_ELAPSED_US = saved.1;
        EXEC_START = saved.2;
        NEXT_CHECK = saved.3;
    }

    // Clear orphaned pending RPC promises. This runs with DRAINING=false
    // so the JS function executes normally without being interrupted.
    js.ctx.with(|ctx| {
        if let Ok(func) = ctx.globals().get::<_, Function>("__reset") {
            let _ = func.call::<_, ()>(());
        }
    });
}

// ─── Prelude ──────────────────────────────────────────────────────────────────

const PRELUDE_SCRIPT: &str = include_str!("prelude.js");

struct JsRuntime {
    _rt: Runtime,
    ctx: Context,
}

impl std::fmt::Debug for JsRuntime {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "JsRuntime {{ rt: , ctx: }}")
    }
}

unsafe impl Send for JsRuntime {}
unsafe impl Sync for JsRuntime {}

static RUNTIME: OnceLock<JsRuntime> = OnceLock::new();

#[unsafe(export_name = "wizer.initialize")]
pub extern "C" fn wizer_initialize() {
    let rt = Runtime::new().unwrap();
    rt.set_memory_limit(JS_MEMORY_LIMIT);
    rt.set_max_stack_size(JS_STACK_LIMIT);
    rt.set_interrupt_handler(Some(Box::new(|| unsafe {
        if DRAINING {
            return true; // Immediately interrupt stale jobs during cleanup
        }
        OPCODE_COUNT += 10_000;
        if !INITIALIZED || OPCODE_COUNT < NEXT_CHECK {
            return false;
        }
        NEXT_CHECK = OPCODE_COUNT + CHECK_INTERVAL;
        let elapsed = sample_elapsed_us();
        host_should_interrupt(OPCODE_COUNT, elapsed) != 0
    })));

    let ctx = Context::full(&rt).unwrap();
    ctx.with(|ctx| {
        let globals = ctx.globals();

        // ── Native bridge: __host_rpc_native ──────────────────────────────────
        globals.set(
            "__host_rpc_native",
            Function::new(
                ctx.clone(),
                |method: String, params_json: String, id: i32| unsafe {
                    USED_RPC_CALLS = true;
                    host_rpc(
                        method.as_ptr(),
                        method.len(),
                        params_json.as_ptr(),
                        params_json.len(),
                        id,
                    );
                },
            )?,
        )?;

        // ── Native bridge: __host_rpc_sync_native ──────────────────────────
        // Synchronous RPC: calls the Go host inline and returns the result
        // (or throws on error) without creating a Promise.
        globals.set(
            "__host_rpc_sync_native",
            Function::new(
                ctx.clone(),
                |ctx: rquickjs::Ctx<'_>,
                 method: String,
                 params_json: String|
                 -> rquickjs::Result<String> {
                    let packed = unsafe {
                        host_rpc_sync(
                            method.as_ptr(),
                            method.len(),
                            params_json.as_ptr(),
                            params_json.len(),
                        )
                    };

                    if packed == 0 {
                        return Err(ctx.throw(
                            rquickjs::Exception::from_message(
                                ctx.clone(),
                                "sync RPC: method not found or internal error",
                            )?
                            .into_value(),
                        ));
                    }

                    let ptr = (packed >> 32) as *const u8;
                    let len = (packed & 0xFFFFFFFF) as usize;

                    // Read the tagged result from our own WASM linear memory.
                    let data = unsafe { std::slice::from_raw_parts(ptr, len) };
                    let tag = data[0];
                    let json_str = unsafe { std::str::from_utf8_unchecked(&data[1..]) }.to_owned();

                    // Free the buffer that the Go host allocated via alloc().
                    dealloc(ptr as *mut u8, len);

                    if tag == 1 {
                        // Error path: throw a JS Error with the message.
                        return Err(ctx.throw(
                            rquickjs::Exception::from_message(ctx.clone(), &json_str)?.into_value(),
                        ));
                    }

                    // Success path: return JSON string for JS to JSON.parse.
                    Ok(json_str)
                },
            )?,
        )?;

        ctx.eval::<(), _>(PRELUDE_SCRIPT)?;
        while ctx.execute_pending_job() {}

        rquickjs::Result::Ok(())
    })
    .unwrap();

    RUNTIME.set(JsRuntime { _rt: rt, ctx }).unwrap();
}

// ─── Error classification (Rust side) ─────────────────────────────────────────

fn classify_error(msg: &str) -> &'static str {
    if msg == "interrupted" {
        "cpu_limit_exceeded"
    } else if msg.contains("out of memory") {
        "memory_limit_exceeded"
    } else if msg.contains("stack") {
        "stack_overflow"
    } else {
        "runtime_error"
    }
}

fn send_error(msg: &str) {
    let error_type = classify_error(msg);
    let json = format!(
        r#"{{"error":{},"errorType":"{}","stack":null}}"#,
        json_escape_string(msg),
        error_type
    );
    let b = json.as_bytes();
    unsafe { host_set_result(b.as_ptr(), b.len(), 1) };
}

/// Properly escape a string for embedding in JSON, including control characters.
fn json_escape_string(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => {
                out.push_str(&format!("\\u{:04x}", c as u32));
            }
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// Extract the error message from a QuickJS exception on the given context.
fn extract_exception_message(ctx: &rquickjs::Ctx<'_>) -> String {
    let exc = ctx.catch();
    exc.as_exception()
        .and_then(|e| e.message())
        .unwrap_or_else(|| "unknown error".to_string())
}

// ─── Result processing ────────────────────────────────────────────────────────
// These helpers capture eval/dispatch results and send them to the Go host via
// host_set_result, entirely in Rust using raw QuickJS C APIs.

/// JSON-stringify a JSValue and send it to the host via host_set_result.
/// For undefined/non-serializable values (functions, symbols), sends "null".
/// Does NOT free `val` — the caller is responsible for that.
unsafe fn send_result_value(ctx_ptr: *mut qjs::JSContext, val: qjs::JSValue) {
    unsafe {
        let json = qjs::JS_JSONStringify(ctx_ptr, val, qjs::JS_UNDEFINED, qjs::JS_UNDEFINED);
        if qjs::JS_IsException(json) {
            // Clear the exception so it doesn't leak into subsequent code.
            let exc = qjs::JS_GetException(ctx_ptr);
            qjs::JS_FreeValue(ctx_ptr, exc);
            let null_bytes = b"null";
            host_set_result(null_bytes.as_ptr(), null_bytes.len(), 0);
        } else if qjs::JS_IsUndefined(json) {
            // JSON.stringify returns undefined for functions, symbols, undefined, etc.
            let null_bytes = b"null";
            host_set_result(null_bytes.as_ptr(), null_bytes.len(), 0);
        } else {
            let mut len: usize = 0;
            let cstr = qjs::JS_ToCStringLen(ctx_ptr, &mut len, json);
            if !cstr.is_null() {
                host_set_result(cstr as *const u8, len, 0);
                qjs::JS_FreeCString(ctx_ptr, cstr);
            } else {
                let null_bytes = b"null";
                host_set_result(null_bytes.as_ptr(), null_bytes.len(), 0);
            }
        }
        qjs::JS_FreeValue(ctx_ptr, json);
    }
}

/// Extract a string property from a JSValue, returning an owned String.
/// Returns the fallback if the property is missing or not a string.
unsafe fn extract_js_string_prop(
    ctx_ptr: *mut qjs::JSContext,
    obj: qjs::JSValue,
    prop: &std::ffi::CStr,
    fallback: &str,
) -> String {
    unsafe {
        let val = qjs::JS_GetPropertyStr(ctx_ptr, obj, prop.as_ptr());
        let result = if qjs::JS_IsString(val) {
            let mut len: usize = 0;
            let cstr = qjs::JS_ToCStringLen(ctx_ptr, &mut len, val);
            if !cstr.is_null() {
                let s =
                    std::str::from_utf8(std::slice::from_raw_parts(cstr as *const u8, len))
                        .unwrap_or(fallback)
                        .to_owned();
                qjs::JS_FreeCString(ctx_ptr, cstr);
                s
            } else {
                fallback.to_owned()
            }
        } else {
            fallback.to_owned()
        };
        qjs::JS_FreeValue(ctx_ptr, val);
        result
    }
}

/// Static C-ABI callback for Promise .then() — JSON-stringify the resolved
/// value and send it to the host via host_set_result.
unsafe extern "C" fn promise_resolve_cb(
    ctx: *mut qjs::JSContext,
    _this: qjs::JSValue,
    argc: c_int,
    argv: *mut qjs::JSValue,
) -> qjs::JSValue {
    unsafe {
        let val = if argc > 0 { *argv } else { qjs::JS_UNDEFINED };
        // JS_EVAL_FLAG_ASYNC wraps the completion value in {value: <result>}.
        // Always unwrap by extracting the inner value before serializing.
        if qjs::JS_IsObject(val) {
            let atom = qjs::JS_NewAtom(ctx, c"value".as_ptr());
            let has = qjs::JS_HasProperty(ctx, val, atom);
            qjs::JS_FreeAtom(ctx, atom);
            if has > 0 {
                let inner = qjs::JS_GetPropertyStr(ctx, val, c"value".as_ptr());
                send_result_value(ctx, inner);
                qjs::JS_FreeValue(ctx, inner);
                return qjs::JS_UNDEFINED;
            }
        }
        send_result_value(ctx, val);
        qjs::JS_UNDEFINED
    }
}

/// Static C-ABI callback for Promise .catch() — extract the error message
/// and stack, format an error JSON, and send it via host_set_result.
unsafe extern "C" fn promise_reject_cb(
    ctx: *mut qjs::JSContext,
    _this: qjs::JSValue,
    argc: c_int,
    argv: *mut qjs::JSValue,
) -> qjs::JSValue {
    unsafe {
        let val = if argc > 0 { *argv } else { qjs::JS_UNDEFINED };

        let msg = if qjs::JS_IsString(val) {
            // Thrown string (e.g. throw "oops")
            let mut len: usize = 0;
            let cstr = qjs::JS_ToCStringLen(ctx, &mut len, val);
            if !cstr.is_null() {
                let s =
                    std::str::from_utf8(std::slice::from_raw_parts(cstr as *const u8, len))
                        .unwrap_or("unknown error")
                        .to_owned();
                qjs::JS_FreeCString(ctx, cstr);
                s
            } else {
                "unknown error".to_owned()
            }
        } else if qjs::JS_IsObject(val) {
            extract_js_string_prop(ctx, val, c"message", "unknown error")
        } else {
            "unknown error".to_owned()
        };

        let stack = extract_js_string_prop(ctx, val, c"stack", "");
        let error_type = classify_error(&msg);

        let json = if stack.is_empty() {
            format!(
                r#"{{"error":{},"errorType":"{}","stack":null}}"#,
                json_escape_string(&msg),
                error_type
            )
        } else {
            format!(
                r#"{{"error":{},"errorType":"{}","stack":{}}}"#,
                json_escape_string(&msg),
                error_type,
                json_escape_string(&stack)
            )
        };

        let b = json.as_bytes();
        host_set_result(b.as_ptr(), b.len(), 1);

        qjs::JS_UNDEFINED
    }
}

/// Attach .then()/.catch() callbacks to a Promise so that when it settles,
/// the resolved value (or rejection error) is sent to the host via
/// host_set_result. Does NOT free `promise`.
unsafe fn settle_promise_result(ctx_ptr: *mut qjs::JSContext, promise: qjs::JSValue) {
    unsafe {
        let then_fn = qjs::JS_GetPropertyStr(ctx_ptr, promise, c"then".as_ptr());

        let resolve = qjs::JS_NewCFunction2(
            ctx_ptr,
            Some(promise_resolve_cb),
            c"resolve".as_ptr(),
            1,
            qjs::JSCFunctionEnum_JS_CFUNC_generic,
            0,
        );
        let reject = qjs::JS_NewCFunction2(
            ctx_ptr,
            Some(promise_reject_cb),
            c"reject".as_ptr(),
            1,
            qjs::JSCFunctionEnum_JS_CFUNC_generic,
            0,
        );

        let mut args = [resolve, reject];
        let res = qjs::JS_Call(ctx_ptr, then_fn, promise, 2, args.as_mut_ptr());
        qjs::JS_FreeValue(ctx_ptr, res);
        qjs::JS_FreeValue(ctx_ptr, then_fn);
        // resolve and reject are consumed by JS_Call — do NOT free them.
    }
}

/// Process the result of an eval or dispatch. If the value is a Promise (and
/// we are in async mode), attach settlement callbacks. Otherwise,
/// JSON-stringify the value immediately and send it to the host.
/// Frees `val` after processing.
unsafe fn process_eval_result(ctx_ptr: *mut qjs::JSContext, val: qjs::JSValue, is_async: bool) {
    unsafe {
        if is_async
            && qjs::JS_PromiseState(ctx_ptr, val)
                != qjs::JSPromiseStateEnum_JS_PROMISE_NOT_A_PROMISE
        {
            settle_promise_result(ctx_ptr, val);
        } else {
            send_result_value(ctx_ptr, val);
        }
        qjs::JS_FreeValue(ctx_ptr, val);
    }
}

// ─── eval ─────────────────────────────────────────────────────────────────────
// Evaluates JS source globally using raw QuickJS C API to support
// JS_EVAL_FLAG_ASYNC for top-level await. The result of the last expression
// is captured and sent to the host. When the result is a Promise (async mode),
// settlement callbacks are attached so the resolved value is sent later.

#[unsafe(export_name = "eval")]
pub extern "C" fn eval(ptr: *const u8, len: usize, is_async: i32) {
    discard_stale_state();
    let js = RUNTIME.get().expect("wizer_initialize was not called");
    unsafe {
        EXEC_START = Some(Instant::now());
    }

    let source = unsafe {
        std::str::from_utf8(std::slice::from_raw_parts(ptr, len)).expect("invalid utf-8 in script")
    };

    js.ctx
        .with(|ctx| -> rquickjs::Result<()> {
            let ctx_ptr = ctx.as_raw().as_ptr();
            let src = match CString::new(source) {
                Ok(s) => s,
                Err(_) => {
                    send_error("eval: source contains null byte");
                    return Ok(());
                }
            };
            let filename = c"<eval>";
            let mut flags = qjs::JS_EVAL_TYPE_GLOBAL | qjs::JS_EVAL_FLAG_STRICT;
            if is_async != 0 {
                flags |= qjs::JS_EVAL_FLAG_ASYNC;
            }

            unsafe {
                let val = qjs::JS_Eval(
                    ctx_ptr,
                    src.as_ptr(),
                    source.len() as _,
                    filename.as_ptr(),
                    flags as i32,
                );

                if qjs::JS_IsException(val) {
                    send_error(&extract_exception_message(&ctx));
                } else {
                    process_eval_result(ctx_ptr, val, is_async != 0);
                }
            }

            if is_async != 0 {
                while ctx.execute_pending_job() {}
            }
            Ok(())
        })
        .unwrap_or_else(|e| send_error(&format!("eval context error: {e}")));
    unsafe {
        sample_elapsed_us();
        EXEC_START = None;
    }
}

// ─── compile ──────────────────────────────────────────────────────────────────
// Compiles JS source to QuickJS bytecode without executing it. Returns a
// packed u64: (out_ptr << 32) | out_len. Returns 0 and calls send_error on
// failure. The Go host must dealloc the returned buffer after copying.
//
// Uses raw QuickJS FFI because rquickjs only exposes Module::declare for
// module-type bytecode (JS_EVAL_TYPE_MODULE). We need script-type bytecode
// (JS_EVAL_TYPE_GLOBAL) so that top-level declarations become globals.

#[unsafe(export_name = "compile")]
pub extern "C" fn compile(ptr: *const u8, len: usize, is_async: i32) -> u64 {
    let js = RUNTIME.get().expect("wizer_initialize was not called");

    let source = unsafe {
        std::str::from_utf8(std::slice::from_raw_parts(ptr, len)).expect("invalid utf-8 in source")
    };

    js.ctx.with(|ctx| {
        let ctx_ptr = ctx.as_raw().as_ptr();
        let src = match CString::new(source) {
            Ok(s) => s,
            Err(_) => {
                send_error("compile: source contains null byte");
                return 0u64;
            }
        };
        let filename = c"<compile>";
        let mut flags =
            qjs::JS_EVAL_TYPE_GLOBAL | qjs::JS_EVAL_FLAG_STRICT | qjs::JS_EVAL_FLAG_COMPILE_ONLY;
        if is_async != 0 {
            flags |= qjs::JS_EVAL_FLAG_ASYNC;
        }

        unsafe {
            let val = qjs::JS_Eval(
                ctx_ptr,
                src.as_ptr(),
                source.len() as _,
                filename.as_ptr(),
                flags as i32,
            );

            if qjs::JS_IsException(val) {
                let msg = extract_exception_message(&ctx);
                send_error(&format!("compile: {msg}"));
                return 0u64;
            }

            // Serialize to bytecode
            let mut out_len: u32 = 0;
            let out_ptr = qjs::JS_WriteObject(
                ctx_ptr,
                &mut out_len,
                val,
                qjs::JS_WRITE_OBJ_BYTECODE as i32,
            );
            qjs::JS_FreeValue(ctx_ptr, val);

            if out_ptr.is_null() {
                send_error("compile: JS_WriteObject failed");
                return 0u64;
            }

            // Copy bytecode to WASM-managed memory (alloc'd buffer that Go
            // can dealloc) and free the QuickJS-allocated buffer.
            let bytecode_len = out_len as usize;
            let wasm_ptr = alloc(bytecode_len);
            if wasm_ptr.is_null() {
                qjs::js_free(ctx_ptr, out_ptr as *mut _);
                send_error("compile: alloc failed for bytecode");
                return 0u64;
            }
            std::ptr::copy_nonoverlapping(out_ptr, wasm_ptr, bytecode_len);
            qjs::js_free(ctx_ptr, out_ptr as *mut _);

            ((wasm_ptr as u64) << 32) | (bytecode_len as u64)
        }
    })
}

// ─── eval_compiled ────────────────────────────────────────────────────────────
// Executes previously compiled bytecode. The bytecode was produced by compile()
// and written into WASM memory by the Go host. The result of the last
// expression is captured and sent to the host (same as eval).

#[unsafe(export_name = "eval_compiled")]
pub extern "C" fn eval_compiled(ptr: *const u8, len: usize, is_async: i32) {
    discard_stale_state();
    let js = RUNTIME.get().expect("wizer_initialize was not called");
    unsafe {
        EXEC_START = Some(Instant::now());
    }

    js.ctx
        .with(|ctx| -> rquickjs::Result<()> {
            let ctx_ptr = ctx.as_raw().as_ptr();

            unsafe {
                // Deserialize bytecode → function object
                let obj =
                    qjs::JS_ReadObject(ctx_ptr, ptr, len as u32, qjs::JS_READ_OBJ_BYTECODE as i32);
                if qjs::JS_IsException(obj) {
                    send_error(&format!(
                        "eval_compiled: {}",
                        extract_exception_message(&ctx)
                    ));
                    return Ok(());
                }

                // Execute the compiled function (consumes obj)
                let result = qjs::JS_EvalFunction(ctx_ptr, obj);
                if qjs::JS_IsException(result) {
                    send_error(&extract_exception_message(&ctx));
                    return Ok(());
                }

                // Process the last expression result
                process_eval_result(ctx_ptr, result, is_async != 0);
            }

            if is_async != 0 {
                while ctx.execute_pending_job() {}
            }
            Ok(())
        })
        .unwrap_or_else(|e| send_error(&format!("eval_compiled error: {e}")));
    unsafe {
        sample_elapsed_us();
        EXEC_START = None;
    }
}

// ─── Memory exports ───────────────────────────────────────────────────────────
// *const u8 / usize are 32-bit in wasm32, matching the i32 the Go host expects.

#[unsafe(export_name = "alloc")]
pub extern "C" fn alloc(len: usize) -> *mut u8 {
    if len == 0 {
        return std::ptr::null_mut();
    }
    let layout = std::alloc::Layout::from_size_align(len, 1).unwrap();
    unsafe { std::alloc::alloc(layout) }
}

#[unsafe(export_name = "dealloc")]
pub extern "C" fn dealloc(ptr: *mut u8, len: usize) {
    if ptr.is_null() || len == 0 {
        return;
    }
    let layout = std::alloc::Layout::from_size_align(len, 1).unwrap();
    unsafe { std::alloc::dealloc(ptr, layout) };
}

// ─── Event dispatch ───────────────────────────────────────────────────────────
// Calls __dispatch_event(name, params) in JS. The JS function looks up the
// registered handler and calls it. If no handler is registered, it throws.
// The handler return value is captured and sent to the host. If the handler
// returns a Promise (async mode), settlement callbacks are attached.

#[unsafe(export_name = "dispatch_event")]
pub extern "C" fn dispatch_event(
    name_ptr: *const u8,
    name_len: usize,
    params_ptr: *const u8,
    params_len: usize,
    is_async: i32,
) {
    discard_stale_state();
    let js = RUNTIME.get().expect("wizer_initialize was not called");
    unsafe {
        EXEC_START = Some(Instant::now());
    }

    let name = unsafe {
        std::str::from_utf8(std::slice::from_raw_parts(name_ptr, name_len)).unwrap_or("")
    };
    let params = unsafe {
        std::str::from_utf8(std::slice::from_raw_parts(params_ptr, params_len)).unwrap_or("null")
    };

    js.ctx
        .with(|ctx| -> rquickjs::Result<()> {
            let ctx_ptr = ctx.as_raw().as_ptr();
            let func: Function = ctx.globals().get("__dispatch_event")?;
            match func.call::<_, Value>((name, params)) {
                Ok(val) => {
                    // Dup the raw JSValue because rquickjs will free it when
                    // `val` is dropped, and process_eval_result also frees.
                    let raw = unsafe { qjs::JS_DupValue(ctx_ptr, val.as_raw()) };
                    drop(val);
                    unsafe { process_eval_result(ctx_ptr, raw, is_async != 0) };
                }
                Err(Error::Exception) => {
                    send_error(&extract_exception_message(&ctx));
                }
                Err(e) => {
                    send_error(&format!("dispatch_event error: {e}"));
                }
            }

            if is_async != 0 {
                while ctx.execute_pending_job() {}
            }
            Ok(())
        })
        .unwrap_or_else(|e| send_error(&format!("dispatch_event error: {e}")));
    unsafe {
        sample_elapsed_us();
        EXEC_START = None;
    }
}

// ─── RPC settlement ───────────────────────────────────────────────────────────
#[unsafe(export_name = "resolve_rpc")]
pub extern "C" fn resolve_rpc(promise_id: i32, result_ptr: *const u8, result_len: usize) {
    call_js_rpc_settlement("__resolve_rpc", promise_id, result_ptr, result_len);
}

#[unsafe(export_name = "reject_rpc")]
pub extern "C" fn reject_rpc(promise_id: i32, error_ptr: *const u8, error_len: usize) {
    call_js_rpc_settlement("__reject_rpc", promise_id, error_ptr, error_len);
}

fn call_js_rpc_settlement(fn_name: &str, id: i32, ptr: *const u8, len: usize) {
    let js = match RUNTIME.get() {
        Some(r) => r,
        None => return,
    };

    let payload =
        unsafe { std::str::from_utf8(std::slice::from_raw_parts(ptr, len)).unwrap_or("null") };

    // Set EXEC_START so the interrupt handler tracks CPU time accurately
    // during promise settlement and any JS continuations that fire.
    unsafe {
        EXEC_START = Some(Instant::now());
    }

    js.ctx
        .with(|ctx| -> rquickjs::Result<()> {
            let func: Function = ctx.globals().get(fn_name)?;
            func.call::<_, ()>((id, payload))?;
            while ctx.execute_pending_job() {}
            Ok(())
        })
        .unwrap_or_else(|e| send_error(&format!("{fn_name} error: {e}")));

    unsafe {
        sample_elapsed_us();
        EXEC_START = None;
    }
}

// ─── Host imports ─────────────────────────────────────────────────────────────

#[link(wasm_import_module = "env")]
unsafe extern "C" {
    fn host_rpc(
        method_ptr: *const u8,
        method_len: usize,
        params_ptr: *const u8,
        params_len: usize,
        promise_id: i32,
    );
    fn host_set_result(result_ptr: *const u8, result_len: usize, is_error: i32);
    fn host_rpc_sync(
        method_ptr: *const u8,
        method_len: usize,
        params_ptr: *const u8,
        params_len: usize,
    ) -> u64;
    fn host_should_interrupt(instructions: u64, cpu_time_us: u64) -> i32;
}

fn main() {
    // Usually this is called at compile time by Wizer.
    if RUNTIME.get().is_none() {
        wizer_initialize();
    }
}
