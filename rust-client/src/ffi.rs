//! C-compatible FFI for iOS/Swift integration.
//!
//! All functions use opaque pointers and C types. Strings are null-terminated UTF-8.
//! The caller must free returned strings/buffers with the corresponding free function.

use std::ffi::{CStr, CString, c_char, c_int, c_void};
use std::pin::Pin;
use std::ptr;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use crate::client::HubSyncClient;

/// Wrapper that lets us send a pointer to HubSyncClient across threads.
///
/// SAFETY: The pointee is pinned in HubSyncHandle (Pin<Box<HubSyncClient>>),
/// and the sync thread is always joined before the client is dropped.
/// The caller (hubsync_free) enforces join-before-drop.
struct SendPtr {
    ptr: usize,
}
unsafe impl Send for SendPtr {}

impl SendPtr {
    fn new(client: &HubSyncClient) -> Self {
        SendPtr { ptr: client as *const HubSyncClient as usize }
    }

    /// SAFETY: caller must ensure the pointee is still alive.
    unsafe fn get(&self) -> &HubSyncClient {
        &*(self.ptr as *const HubSyncClient)
    }
}

/// Opaque handle to a HubSyncClient.
pub struct HubSyncHandle {
    /// Pinned so the pointer to client remains stable while sync thread runs.
    client: Pin<Box<HubSyncClient>>,
    cancel: Arc<AtomicBool>,
    sync_thread: Option<std::thread::JoinHandle<()>>,
}

impl HubSyncHandle {
    /// Stop sync thread and wait for it to finish. Safe to call multiple times.
    fn stop_sync(&mut self) {
        self.cancel.store(true, Ordering::Relaxed);
        if let Some(thread) = self.sync_thread.take() {
            let _ = thread.join();
        }
    }
}

impl Drop for HubSyncHandle {
    fn drop(&mut self) {
        // Always join the sync thread before dropping the client.
        self.stop_sync();
    }
}

// -- Lifecycle --

/// Open a hubsync client with SQLite content backend.
/// Returns NULL on failure.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_open(
    db_path: *const c_char,
    hub_url: *const c_char,
    token: *const c_char,
) -> *mut HubSyncHandle {
    let db_path = match unsafe_cstr_to_str(db_path) {
        Some(s) => s,
        None => return ptr::null_mut(),
    };
    let hub_url = match unsafe_cstr_to_str(hub_url) {
        Some(s) => s,
        None => return ptr::null_mut(),
    };
    let token = unsafe_cstr_to_str(token);

    match HubSyncClient::open_sqlite(db_path, hub_url, token) {
        Ok(client) => Box::into_raw(Box::new(HubSyncHandle {
            client: Box::pin(client),
            cancel: Arc::new(AtomicBool::new(false)),
            sync_thread: None,
        })),
        Err(e) => {
            eprintln!("hubsync_open: {}", e);
            ptr::null_mut()
        }
    }
}

/// Free a hubsync client. Stops sync if running.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_free(handle: *mut HubSyncHandle) {
    if handle.is_null() {
        return;
    }
    // Drop calls stop_sync() which joins the thread before freeing client.
    let _ = unsafe { Box::from_raw(handle) };
}

// -- Sync --

/// Start tree sync in a background thread. Returns immediately.
/// Returns 0 on success, -1 if already syncing or handle is null.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_start_sync(handle: *mut HubSyncHandle) -> c_int {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return -1,
    };

    if handle.sync_thread.is_some() {
        return -1; // already syncing
    }

    handle.cancel.store(false, Ordering::Relaxed);
    let cancel = handle.cancel.clone();
    let ptr = SendPtr::new(&handle.client);

    let thread = std::thread::spawn(move || {
        // SAFETY: handle.client is Pin<Box<>>, won't move. Thread is joined before drop.
        let client = unsafe { ptr.get() };
        if let Err(e) = client.sync(cancel) {
            eprintln!("hubsync sync: {}", e);
        }
    });

    handle.sync_thread = Some(thread);
    0
}

/// Stop the background sync thread.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_stop_sync(handle: *mut HubSyncHandle) {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return,
    };
    handle.stop_sync();
}

// -- Content --

/// Read file content by path. Fetches from hub if not cached.
/// On success: sets *out_data and *out_len, returns 0.
/// On failure: returns -1.
/// Caller must free the data with hubsync_free_data.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_read(
    handle: *mut HubSyncHandle,
    path: *const c_char,
    out_data: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return -1,
    };
    let path = match unsafe_cstr_to_str(path) {
        Some(s) => s,
        None => return -1,
    };

    match handle.client.read(path) {
        Ok(data) => {
            let len = data.len();
            let ptr = Box::into_raw(data.into_boxed_slice()) as *mut u8;
            unsafe {
                *out_data = ptr;
                *out_len = len;
            }
            0
        }
        Err(e) => {
            eprintln!("hubsync_read: {}", e);
            -1
        }
    }
}

/// Free data returned by hubsync_read.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_free_data(data: *mut u8, len: usize) {
    if data.is_null() {
        return;
    }
    unsafe {
        let _ = Box::from_raw(std::slice::from_raw_parts_mut(data, len));
    }
}

// -- Cache management --

/// Prefetch content for files matching a glob pattern.
/// Returns bytes fetched, or -1 on error.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_prefetch(handle: *mut HubSyncHandle, glob: *const c_char) -> i64 {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return -1,
    };
    let glob = match unsafe_cstr_to_str(glob) {
        Some(s) => s,
        None => return -1,
    };

    match handle.client.prefetch(glob) {
        Ok(n) => n as i64,
        Err(e) => {
            eprintln!("hubsync_prefetch: {}", e);
            -1
        }
    }
}

/// Evict cached content to stay within target_bytes. Returns bytes freed.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_evict(handle: *mut HubSyncHandle, target_bytes: u64) -> i64 {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return -1,
    };

    match handle.client.evict(target_bytes) {
        Ok(n) => n as i64,
        Err(e) => {
            eprintln!("hubsync_evict: {}", e);
            -1
        }
    }
}

/// Pin files matching glob. Returns count of files pinned.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_pin(handle: *mut HubSyncHandle, glob: *const c_char) -> i64 {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return -1,
    };
    let glob = match unsafe_cstr_to_str(glob) {
        Some(s) => s,
        None => return -1,
    };

    match handle.client.pin(glob) {
        Ok(n) => n as i64,
        Err(e) => {
            eprintln!("hubsync_pin: {}", e);
            -1
        }
    }
}

/// Unpin files matching glob. Returns count of files unpinned.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_unpin(handle: *mut HubSyncHandle, glob: *const c_char) -> i64 {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return -1,
    };
    let glob = match unsafe_cstr_to_str(glob) {
        Some(s) => s,
        None => return -1,
    };

    match handle.client.unpin(glob) {
        Ok(n) => n as i64,
        Err(e) => {
            eprintln!("hubsync_unpin: {}", e);
            -1
        }
    }
}

// -- Queries --

/// Get the current hub version. Returns -1 on error.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_hub_version(handle: *mut HubSyncHandle) -> i64 {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return -1,
    };

    match handle.client.store.hub_version() {
        Ok(v) => v,
        Err(e) => {
            eprintln!("hubsync_hub_version: {}", e);
            -1
        }
    }
}

/// Get the database path. Caller must free with hubsync_free_string.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_db_path(handle: *mut HubSyncHandle) -> *mut c_char {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return ptr::null_mut(),
    };

    let path = handle.client.store.conn().path().unwrap_or("");
    match CString::new(path) {
        Ok(s) => s.into_raw(),
        Err(_) => ptr::null_mut(),
    }
}

/// Free a string returned by hubsync functions.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_free_string(s: *mut c_char) {
    if s.is_null() {
        return;
    }
    unsafe {
        let _ = CString::from_raw(s);
    }
}

// -- Callback-based sync --

/// Type for the sync callback. Called after each event is applied.
pub type HubSyncCallback = extern "C" fn(ctx: *mut c_void);

/// Start sync with a callback fired after each event.
/// The callback runs on the sync thread.
/// Returns 0 on success, -1 on error.
#[unsafe(no_mangle)]
pub extern "C" fn hubsync_start_sync_with_callback(
    handle: *mut HubSyncHandle,
    callback: HubSyncCallback,
    ctx: *mut c_void,
) -> c_int {
    let handle = match unsafe_handle(handle) {
        Some(h) => h,
        None => return -1,
    };

    if handle.sync_thread.is_some() {
        return -1;
    }

    handle.cancel.store(false, Ordering::Relaxed);
    let cancel = handle.cancel.clone();
    let ptr = SendPtr::new(&handle.client);
    let ctx_ptr = ctx as usize;

    let thread = std::thread::spawn(move || {
        // SAFETY: handle.client is Pin<Box<>>, won't move. Thread is joined before drop.
        let client = unsafe { ptr.get() };
        let _ = client.sync_with_callback(cancel, |_event| {
            callback(ctx_ptr as *mut c_void);
        });
    });

    handle.sync_thread = Some(thread);
    0
}

// -- Helpers --

fn unsafe_cstr_to_str<'a>(ptr: *const c_char) -> Option<&'a str> {
    if ptr.is_null() {
        return None;
    }
    unsafe { CStr::from_ptr(ptr).to_str().ok() }
}

fn unsafe_handle<'a>(ptr: *mut HubSyncHandle) -> Option<&'a mut HubSyncHandle> {
    if ptr.is_null() {
        return None;
    }
    unsafe { Some(&mut *ptr) }
}
