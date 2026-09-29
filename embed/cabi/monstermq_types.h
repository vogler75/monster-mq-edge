/*
 * monstermq_types.h - types and constants of the MonsterMQ Edge C ABI.
 * Included by monstermq.h; see there for the contract.
 */
#ifndef MONSTERMQ_TYPES_H
#define MONSTERMQ_TYPES_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define MMQ_ABI_VERSION 1u

/* Status codes. */
#define MMQ_OK              0
#define MMQ_E_INVALID      -1
#define MMQ_E_STATE        -2
#define MMQ_E_NOT_FOUND    -3
#define MMQ_E_UNAUTHORIZED -4
#define MMQ_E_TYPE         -5
#define MMQ_E_TIMEOUT      -6
#define MMQ_E_OVERLOAD     -7
#define MMQ_E_OA           -8
#define MMQ_E_PERSIST      -9
#define MMQ_E_ABI         -10
#define MMQ_E_TOO_LARGE   -11
#define MMQ_E_UNAVAILABLE -12

/* Broker lifecycle states returned by mmq_state. */
#define MMQ_STATE_CREATED  1
#define MMQ_STATE_STARTING 2
#define MMQ_STATE_RUNNING  3
#define MMQ_STATE_STOPPING 4
#define MMQ_STATE_STOPPED  5
#define MMQ_STATE_FAILED   6

/* Log levels passed to the log callback. */
#define MMQ_LOG_DEBUG 0
#define MMQ_LOG_INFO  1
#define MMQ_LOG_WARN  2
#define MMQ_LOG_ERROR 3

/* Maximum size of one request, completion or event message. */
#define MMQ_MAX_MESSAGE (1u << 20)

/*
 * Go -> host request. Copy data and return MMQ_OK, or MMQ_E_OVERLOAD when
 * the host queue is full. deadline_unix_ms is the absolute deadline; the
 * host drops a request whose deadline passed before execution and completes
 * it with MMQ_E_TIMEOUT.
 */
typedef int32_t (*mmq_submit_fn)(void *user, uint64_t request_id, int64_t deadline_unix_ms,
                                 const uint8_t *data, uint32_t len);
/* Optional: interrupt the host's dispatch wait after a submit. */
typedef void (*mmq_wake_fn)(void *user);
/* Optional: log line (UTF-8, not NUL-terminated). */
typedef void (*mmq_log_fn)(void *user, int32_t level, const char *msg, uint32_t len);

typedef struct mmq_host {
  uint32_t struct_size; /* sizeof(mmq_host) */
  uint32_t abi_version; /* MMQ_ABI_VERSION */
  void *user;
  mmq_submit_fn submit; /* required */
  mmq_wake_fn wake;     /* optional */
  mmq_log_fn log;       /* optional */
} mmq_host;

typedef struct mmq_config {
  uint32_t struct_size; /* sizeof(mmq_config) */
  uint32_t abi_version; /* MMQ_ABI_VERSION */
  const char *config_path; /* broker config.yaml; may be NULL for defaults */
  uint32_t config_path_len;
  uint32_t max_pending;        /* 0 = default 4096 */
  uint32_t event_queue;        /* 0 = default 16384 */
  uint32_t default_timeout_ms; /* 0 = default 5000 */
  int32_t log_level;           /* MMQ_LOG_* minimum level */
} mmq_config;

#ifdef __cplusplus
}
#endif

#endif /* MONSTERMQ_TYPES_H */
