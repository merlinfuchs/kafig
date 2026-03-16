use rquickjs::qjs;
use std::ffi::CString;
use std::time::Instant;

use crate::error::{extract_exception_message, send_error};
use crate::init::RUNTIME;
use crate::result::process_eval_result;
use crate::tracking::{discard_stale_state, sample_elapsed_us, EXEC_START};

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
        std::str::from_utf8(std::slice::from_raw_parts(ptr, len))
            .expect("invalid utf-8 in source")
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
            let wasm_ptr = crate::alloc::alloc(bytecode_len);
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
