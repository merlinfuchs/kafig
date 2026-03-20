use rquickjs::convert::Coerced;

pub(crate) fn classify_error_code(msg: &str) -> &'static str {
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

/// Build a JS error JSON string without sending it to the host.
/// Used by both `send_js_error` and the promise rejection tracker.
pub(crate) fn build_js_error_json(name: &str, msg: &str, stack: Option<&str>) -> String {
    match stack {
        Some(s) => format!(
            r#"{{"kind":"js_error","name":{},"message":{},"stack":{}}}"#,
            json_escape_string(name),
            json_escape_string(msg),
            json_escape_string(s),
        ),
        None => format!(
            r#"{{"kind":"js_error","name":{},"message":{},"stack":null}}"#,
            json_escape_string(name),
            json_escape_string(msg),
        ),
    }
}

/// Send a JS error (thrown exception) to the host.
/// JSON shape: {"kind":"js_error","name":"TypeError","message":"...","stack":"..."}
pub(crate) fn send_js_error(name: &str, msg: &str, stack: Option<&str>) {
    let json = build_js_error_json(name, msg, stack);
    let b = json.as_bytes();
    unsafe { crate::host_set_result(b.as_ptr(), b.len(), 1) };
}

/// Send a runtime error (resource limits, internal errors) to the host.
/// JSON shape: {"kind":"runtime_error","code":"cpu_limit_exceeded","message":"..."}
pub(crate) fn send_runtime_error(code: &str, msg: &str) {
    let json = format!(
        r#"{{"kind":"runtime_error","code":"{}","message":{}}}"#,
        code,
        json_escape_string(msg),
    );
    let b = json.as_bytes();
    unsafe { crate::host_set_result(b.as_ptr(), b.len(), 1) };
}

/// Send an exception, routing to send_js_error or send_runtime_error based on classification.
pub(crate) fn send_exception(name: &str, msg: &str, stack: Option<&str>) {
    let code = classify_error_code(msg);
    if code != "runtime_error" {
        send_runtime_error(code, msg);
    } else {
        send_js_error(name, msg, stack);
    }
}

/// Properly escape a string for embedding in JSON, including control characters.
pub(crate) fn json_escape_string(s: &str) -> String {
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

/// Extract the error message, name, and stack trace from a QuickJS exception.
pub(crate) fn extract_exception(ctx: &rquickjs::Ctx<'_>) -> (String, String, Option<String>) {
    let exc = ctx.catch();
    if let Some(e) = exc.as_exception() {
        let msg = e.message().unwrap_or_else(|| "unknown error".to_string());
        let name = e
            .as_object()
            .get::<_, Option<Coerced<String>>>("name")
            .ok()
            .flatten()
            .map(|c| c.0)
            .unwrap_or_else(|| "Error".to_string());
        let stack = e.stack();
        (msg, name, stack)
    } else {
        // Non-Error throw (e.g. `throw "oops"`)
        let msg = exc
            .as_string()
            .and_then(|s| s.to_string().ok())
            .unwrap_or_else(|| "unknown error".to_string());
        (msg, "Error".to_string(), None)
    }
}
