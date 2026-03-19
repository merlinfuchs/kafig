mod alloc;
mod dispatch;
mod error;
mod eval;
mod init;
mod result;
mod rpc;
mod tracking;

// ─── Host imports ─────────────────────────────────────────────────────────────

#[link(wasm_import_module = "env")]
unsafe extern "C" {
    pub(crate) fn host_rpc(
        method_ptr: *const u8,
        method_len: usize,
        params_ptr: *const u8,
        params_len: usize,
        promise_id: i32,
    );
    pub(crate) fn host_set_result(result_ptr: *const u8, result_len: usize, is_error: i32);
    pub(crate) fn host_rpc_sync(
        method_ptr: *const u8,
        method_len: usize,
        params_ptr: *const u8,
        params_len: usize,
    ) -> u64;
    pub(crate) fn host_should_interrupt(instructions: u64, cpu_time_us: u64) -> i32;
    pub(crate) fn host_promise_rejection(
        error_json_ptr: *const u8,
        error_json_len: usize,
    ) -> i32;
}

fn main() {
    // Usually this is called at compile time by Wizer.
    if init::RUNTIME.get().is_none() {
        init::wizer_initialize();
    }
}
