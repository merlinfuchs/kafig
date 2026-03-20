use rquickjs::qjs;
use rquickjs::{Error, Function, Value};
use std::time::Instant;

use crate::error::{extract_exception, send_exception, send_runtime_error};
use crate::init::RUNTIME;
use crate::result::process_eval_result;
use crate::tracking::{discard_stale_state, sample_elapsed_us, EXEC_START};

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
                    let (msg, ename, stack) = extract_exception(&ctx);
                    send_exception(&ename, &msg, stack.as_deref());
                }
                Err(e) => {
                    send_runtime_error("runtime_error", &format!("dispatch_event error: {e}"));
                }
            }

            if is_async != 0 {
                while ctx.execute_pending_job() {}
            }
            Ok(())
        })
        .unwrap_or_else(|e| send_runtime_error("runtime_error", &format!("dispatch_event error: {e}")));
    unsafe {
        sample_elapsed_us();
        EXEC_START = None;
    }
}
