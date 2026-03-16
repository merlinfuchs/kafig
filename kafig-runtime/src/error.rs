pub(crate) fn classify_error(msg: &str) -> &'static str {
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

pub(crate) fn send_error(msg: &str) {
    let error_type = classify_error(msg);
    let json = format!(
        r#"{{"error":{},"errorType":"{}","stack":null}}"#,
        json_escape_string(msg),
        error_type
    );
    let b = json.as_bytes();
    unsafe { crate::host_set_result(b.as_ptr(), b.len(), 1) };
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

/// Extract the error message from a QuickJS exception on the given context.
pub(crate) fn extract_exception_message(ctx: &rquickjs::Ctx<'_>) -> String {
    let exc = ctx.catch();
    exc.as_exception()
        .and_then(|e| e.message())
        .unwrap_or_else(|| "unknown error".to_string())
}
