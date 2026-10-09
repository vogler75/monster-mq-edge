/*
 * monstermq.h - C ABI of the embeddable MonsterMQ Edge broker.
 *
 * Contract: winccoa/plans/spec-winccoa-native.md section 3. ABI version 2.
 *
 * Threading: every mmq_* function may be called from any thread. The host
 * callbacks (submit, wake, log) are called from arbitrary Go runtime
 * threads; they must copy their arguments, must not block, and must not
 * call into WinCC OA directly. Buffers passed in either direction are valid
 * only for the duration of the call.
 */
#ifndef MONSTERMQ_H
#define MONSTERMQ_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#include "monstermq_types.h"

/* Returns MMQ_ABI_VERSION of the library. */
int32_t mmq_abi_version(void);

/*
 * Validates the configuration and creates the (single) broker instance.
 * No listener is opened yet. Returns MMQ_E_STATE if an instance exists.
 */
int32_t mmq_create(const mmq_config *cfg, const mmq_host *host, uint64_t *out_handle);

/* Starts the broker asynchronously; poll mmq_state for RUNNING or FAILED. */
int32_t mmq_start(uint64_t handle);

/*
 * Returns the state (MMQ_STATE_*) or a negative status. The last error text
 * is copied into err (not NUL-terminated if truncated); *err_len receives
 * its full length.
 */
int32_t mmq_state(uint64_t handle, char *err, uint32_t err_cap, uint32_t *err_len);

/*
 * Requests an asynchronous stop, bounded by timeout_ms. Idempotent. The host
 * must keep dispatching WinCC OA messages until mmq_state reports STOPPED,
 * because stopping disconnects OA registrations through the host.
 */
int32_t mmq_stop(uint64_t handle, uint32_t timeout_ms);

/* Releases the instance. Only valid in CREATED, STOPPED or FAILED. */
int32_t mmq_destroy(uint64_t handle);

/*
 * Delivers the completion of a request. Non-blocking; data is copied.
 * Returns MMQ_E_NOT_FOUND for an unknown, duplicate or late completion; the
 * host must then release a registration it created for that request.
 */
int32_t mmq_complete(uint64_t handle, uint64_t request_id, int32_t status,
                     const uint8_t *data, uint32_t len);

/*
 * Delivers hotlink/query data for a subscription reference (ref 0: host
 * state changes). Non-blocking; data is copied. Returns MMQ_E_OVERLOAD when
 * the event queue is full (the event is dropped and counted).
 */
int32_t mmq_event(uint64_t handle, uint64_t ref, const uint8_t *data, uint32_t len);

/*
 * Delivers the host's counters as a JSON object of non-negative integers,
 * e.g. {"connects":3,"queued":0}; keys are single MQTT topic levels (at
 * most 64 keys). The broker publishes them retained as
 * $SYS/winccoa/manager/<key>. Non-blocking; data is copied. Returns
 * MMQ_E_INVALID for a malformed object.
 */
int32_t mmq_stats(uint64_t handle, const uint8_t *data, uint32_t len);

#ifdef __cplusplus
}
#endif

#endif /* MONSTERMQ_H */
