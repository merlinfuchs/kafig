use rquickjs::Function;
use std::time::Instant;

use crate::init::RUNTIME;

pub(crate) const JS_MEMORY_LIMIT: usize = 32 * 1024 * 1024;
pub(crate) const JS_STACK_LIMIT: usize = 512 * 1024;

pub(crate) static mut OPCODE_COUNT: u64 = 0;
pub(crate) static mut EXEC_START: Option<Instant> = None;
pub(crate) static mut CPU_ELAPSED_US: u64 = 0;
pub(crate) static mut DRAINING: bool = false;
pub(crate) static mut INITIALIZED: bool = false;
pub(crate) static mut USED_RPC_CALLS: bool = false;

// Call host_should_interrupt on every QuickJS interrupt handler invocation.
// QuickJS fires the handler every ~10,000 branch/loop opcodes
// (controlled by JS_INTERRUPT_COUNTER_INIT = 10000). We increment
// OPCODE_COUNT by 10,000 per call to approximate real opcode counts.
pub(crate) const CHECK_INTERVAL: u64 = 1;
pub(crate) static mut NEXT_CHECK: u64 = CHECK_INTERVAL;

pub(crate) unsafe fn sample_elapsed_us() -> u64 {
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

// Called at the start of each eval/dispatch to discard leftover pending jobs
// and orphaned pending RPC promises from a previous invocation. Since QuickJS
// has no API to drop pending jobs without executing them, we set a DRAINING
// flag so the interrupt handler returns true immediately — each stale job
// throws "interrupted" on its first opcode without executing meaningful code.
// Execution stats are saved and restored so the drain doesn't affect the
// current dispatch's tracking.

pub(crate) fn discard_stale_state() {
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
