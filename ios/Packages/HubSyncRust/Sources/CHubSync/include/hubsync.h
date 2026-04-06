#ifndef HUBSYNC_H
#define HUBSYNC_H

#include <stdint.h>
#include <stddef.h>

/// Opaque handle to a HubSync client.
typedef struct HubSyncHandle HubSyncHandle;

/// Callback type fired during sync events.
typedef void (*HubSyncCallback)(void *ctx);

/// Callback type for bootstrap progress.
/// count=0, total=N means tree import complete (N entries).
/// count>0 reports blob fetch progress (count of total).
typedef void (*HubSyncBootstrapCallback)(uint64_t count, uint64_t total, void *ctx);

/// Open a hubsync client with SQLite content backend.
/// @param db_path Path to SQLite database (created if needed).
/// @param hub_url Hub server URL (e.g. "http://localhost:8080").
/// @param token Bearer token for auth, or NULL for no auth.
/// @return Handle, or NULL on failure.
HubSyncHandle *hubsync_open(const char *db_path, const char *hub_url, const char *token);

/// Free a hubsync client. Stops sync if running.
void hubsync_free(HubSyncHandle *handle);

/// Start tree sync in a background thread. Returns immediately.
/// @return 0 on success, -1 if already syncing or handle is null.
int hubsync_start_sync(HubSyncHandle *handle);

/// Start sync with a callback fired after events are applied.
/// @return 0 on success, -1 on error.
int hubsync_start_sync_with_callback(HubSyncHandle *handle, HubSyncCallback callback, void *ctx);

/// Start sync with separate bootstrap and event callbacks.
/// @return 0 on success, -1 on error.
int hubsync_start_sync_with_callbacks(HubSyncHandle *handle,
                                       HubSyncBootstrapCallback bootstrap_cb,
                                       HubSyncCallback event_cb,
                                       void *ctx);

/// Stop the background sync thread.
void hubsync_stop_sync(HubSyncHandle *handle);

/// Read file content by path. Fetches from hub if not cached.
/// @param out_data Pointer to receive data buffer.
/// @param out_len Pointer to receive data length.
/// @return 0 on success, -1 on failure.
int hubsync_read(HubSyncHandle *handle, const char *path, uint8_t **out_data, size_t *out_len);

/// Free data returned by hubsync_read.
void hubsync_free_data(uint8_t *data, size_t len);

/// Prefetch content for files matching a glob pattern.
/// @return Bytes fetched, or -1 on error.
int64_t hubsync_prefetch(HubSyncHandle *handle, const char *glob);

/// Evict cached content to stay within target_bytes.
/// @return Bytes freed, or -1 on error.
int64_t hubsync_evict(HubSyncHandle *handle, uint64_t target_bytes);

/// Pin files matching glob (prevents LRU eviction).
/// @return Files pinned, or -1 on error.
int64_t hubsync_pin(HubSyncHandle *handle, const char *glob);

/// Unpin files matching glob.
/// @return Files unpinned, or -1 on error.
int64_t hubsync_unpin(HubSyncHandle *handle, const char *glob);

/// Get the current hub sync version.
/// @return Version number, or -1 on error.
int64_t hubsync_hub_version(HubSyncHandle *handle);

/// Get the database path. Caller must free with hubsync_free_string.
char *hubsync_db_path(HubSyncHandle *handle);

/// Free a string returned by hubsync functions.
void hubsync_free_string(char *s);

#endif /* HUBSYNC_H */
