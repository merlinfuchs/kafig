// Wrap all internal state in an IIFE to prevent user code from accessing it.
// Only the globals that the Rust resolve/reject exports reference directly
// (__resolve_rpc, __reject_rpc, __dispatch_event) remain on globalThis —
// defined as non-configurable and non-writable so user code cannot tamper.
(function () {
    // Capture the native bridges set by Rust and remove them from globals
    // so user code cannot call them directly.
    const hostRpc = __host_rpc_native;
    const hostRpcSync = __host_rpc_sync_native;
    const hostSetResult = __host_set_result_native;
    delete globalThis.__host_rpc_native;
    delete globalThis.__host_rpc_sync_native;
    delete globalThis.__host_set_result_native;

    // ── Async RPC core ──────────────────────────────────────────────────────
    const pendingRpcs = new Map();
    let nextRpcId = 1;

    function makeRpcPromise(method, params) {
        const id = (nextRpcId++) & 0x7FFFFFFF; // keep within i32 range
        return new Promise((resolve, reject) => {
            pendingRpcs.set(id, { resolve, reject });
            hostRpc(method, JSON.stringify(params ?? null), id);
        });
    }

    function resolveRpc(id, resultJson) {
        const entry = pendingRpcs.get(id);
        if (!entry) return;
        pendingRpcs.delete(id);
        try { entry.resolve(JSON.parse(resultJson)); } catch (e) { entry.resolve(null); }
    }

    function rejectRpc(id, errorJson) {
        const entry = pendingRpcs.get(id);
        if (!entry) return;
        pendingRpcs.delete(id);
        let msg;
        try { msg = JSON.parse(errorJson)?.message ?? String(errorJson); }
        catch (e) { msg = String(errorJson); }
        entry.reject(new Error(msg));
    }

    // ── Event handlers ──────────────────────────────────────────────────────
    const eventHandlers = new Map();

    function dispatchEvent(name, paramsJson) {
        const handler = eventHandlers.get(name);
        if (!handler) {
            throw new Error('No handler registered for event: ' + name);
        }
        handler(JSON.parse(paramsJson));
    }

    // ── Reset function for clearing stale state between dispatches ────
    function reset() {
        pendingRpcs.clear();
    }

    // ── Expose required globals as non-configurable, non-writable ────────
    const frozen = { writable: false, configurable: false };

    Object.defineProperty(globalThis, '__reset', { ...frozen, value: reset });
    Object.defineProperty(globalThis, '__resolve_rpc', { ...frozen, value: resolveRpc });
    Object.defineProperty(globalThis, '__reject_rpc', { ...frozen, value: rejectRpc });
    Object.defineProperty(globalThis, '__dispatch_event', { ...frozen, value: dispatchEvent });

    // ── Host API ────────────────────────────────────────────────────────────
    globalThis.host = Object.freeze({
        rpc(method, params) { return makeRpcPromise(method, params); },
        rpcSync(method, params) {
            return JSON.parse(hostRpcSync(method, JSON.stringify(params ?? null)));
        },
        on(name, handler) { eventHandlers.set(name, handler); },
        result(value) { hostSetResult(JSON.stringify(value ?? null), false); },
    });
})();
