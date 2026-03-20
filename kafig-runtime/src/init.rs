use rquickjs::{Context, Function, Runtime, Value};
use rquickjs::convert::Coerced;
use std::sync::OnceLock;

use crate::error::build_js_error_json;
use crate::tracking::*;

const PRELUDE_SCRIPT: &str = include_str!("prelude.js");

pub(crate) struct JsRuntime {
    pub(crate) _rt: Runtime,
    pub(crate) ctx: Context,
}

impl std::fmt::Debug for JsRuntime {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "JsRuntime {{ rt: , ctx: }}")
    }
}

unsafe impl Send for JsRuntime {}
unsafe impl Sync for JsRuntime {}

pub(crate) static RUNTIME: OnceLock<JsRuntime> = OnceLock::new();

#[unsafe(export_name = "wizer.initialize")]
pub extern "C" fn wizer_initialize() {
    let rt = Runtime::new().unwrap();
    rt.set_memory_limit(JS_MEMORY_LIMIT);
    rt.set_interrupt_handler(Some(Box::new(|| unsafe {
        if DRAINING || FORCE_INTERRUPT {
            return true; // Immediately interrupt stale jobs or forced by rejection handler
        }
        OPCODE_COUNT += 10_000;
        if !INITIALIZED || OPCODE_COUNT < NEXT_CHECK {
            return false;
        }
        NEXT_CHECK = OPCODE_COUNT + CHECK_INTERVAL;
        let elapsed = sample_elapsed_us();
        crate::host_should_interrupt(OPCODE_COUNT, elapsed) != 0
    })));

    rt.set_host_promise_rejection_tracker(Some(Box::new(
        |_ctx: rquickjs::Ctx<'_>, _promise: Value<'_>, reason: Value<'_>, is_handled: bool| {
            if is_handled {
                return;
            }
            let (msg, name, stack) = if let Some(exc) = reason.as_exception() {
                let msg = exc
                    .message()
                    .unwrap_or_else(|| "unknown error".to_string());
                let name = exc
                    .as_object()
                    .get::<_, Option<Coerced<String>>>("name")
                    .ok()
                    .flatten()
                    .map(|c| c.0)
                    .unwrap_or_else(|| "Error".to_string());
                let stack = exc.stack();
                (msg, name, stack)
            } else {
                let msg = reason
                    .as_string()
                    .and_then(|s| s.to_string().ok())
                    .unwrap_or_else(|| "unknown error".to_string());
                (msg, "Error".to_string(), None)
            };
            let json = build_js_error_json(&name, &msg, stack.as_deref());
            let should_interrupt = unsafe {
                crate::host_promise_rejection(json.as_ptr(), json.len())
            } != 0;
            if should_interrupt {
                unsafe { FORCE_INTERRUPT = true; }
            }
        },
    )));

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
                    crate::host_rpc(
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
                        crate::host_rpc_sync(
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
                    let json_str =
                        unsafe { std::str::from_utf8_unchecked(&data[1..]) }.to_owned();

                    // Free the buffer that the Go host allocated via alloc().
                    crate::alloc::dealloc(ptr as *mut u8, len);

                    if tag == 1 {
                        // Error path: throw a JS Error with the message.
                        return Err(ctx.throw(
                            rquickjs::Exception::from_message(ctx.clone(), &json_str)?
                                .into_value(),
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

#[unsafe(export_name = "set_memory_limit")]
pub extern "C" fn set_memory_limit(limit: u32) {
    let js = RUNTIME.get().expect("wizer_initialize was not called");
    js._rt.set_memory_limit(limit as usize);
}

