use rquickjs::Function;
use std::time::Instant;

use crate::error::send_runtime_error;
use crate::init::RUNTIME;
use crate::tracking::{sample_elapsed_us, EXEC_START};

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
        .unwrap_or_else(|e| send_runtime_error("runtime_error", &format!("{fn_name} error: {e}")));

    unsafe {
        sample_elapsed_us();
        EXEC_START = None;
    }
}
