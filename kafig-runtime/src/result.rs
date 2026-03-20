use rquickjs::qjs;
use std::ffi::c_int;

use crate::error::{classify_error_code, send_js_error, send_runtime_error};

/// JSON-stringify a JSValue and send it to the host via host_set_result.
/// For undefined/non-serializable values (functions, symbols), sends "null".
/// Does NOT free `val` — the caller is responsible for that.
pub(crate) unsafe fn send_result_value(ctx_ptr: *mut qjs::JSContext, val: qjs::JSValue) {
    unsafe {
        let json = qjs::JS_JSONStringify(ctx_ptr, val, qjs::JS_UNDEFINED, qjs::JS_UNDEFINED);
        if qjs::JS_IsException(json) {
            // Clear the exception so it doesn't leak into subsequent code.
            let exc = qjs::JS_GetException(ctx_ptr);
            qjs::JS_FreeValue(ctx_ptr, exc);
            let null_bytes = b"null";
            crate::host_set_result(null_bytes.as_ptr(), null_bytes.len(), 0);
        } else if qjs::JS_IsUndefined(json) {
            // JSON.stringify returns undefined for functions, symbols, undefined, etc.
            let null_bytes = b"null";
            crate::host_set_result(null_bytes.as_ptr(), null_bytes.len(), 0);
        } else {
            let mut len: usize = 0;
            let cstr = qjs::JS_ToCStringLen(ctx_ptr, &mut len, json);
            if !cstr.is_null() {
                crate::host_set_result(cstr as *const u8, len, 0);
                qjs::JS_FreeCString(ctx_ptr, cstr);
            } else {
                let null_bytes = b"null";
                crate::host_set_result(null_bytes.as_ptr(), null_bytes.len(), 0);
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

/// Static C-ABI callback for Promise .catch() — extract the error name,
/// message, and stack, then send via the appropriate error function.
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

        let name = extract_js_string_prop(ctx, val, c"name", "Error");
        let stack = extract_js_string_prop(ctx, val, c"stack", "");

        let code = classify_error_code(&msg);
        if code != "runtime_error" {
            send_runtime_error(code, &msg);
        } else {
            let stack_opt = if stack.is_empty() { None } else { Some(stack.as_str()) };
            send_js_error(&name, &msg, stack_opt);
        }

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
pub(crate) unsafe fn process_eval_result(
    ctx_ptr: *mut qjs::JSContext,
    val: qjs::JSValue,
    is_async: bool,
) {
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
