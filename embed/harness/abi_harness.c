/*
 * abi_harness.c - plain C host for the MonsterMQ Edge C ABI (AC-05, AC-08,
 * AC-09 host-side checks). It links libmonstermq.a through the published
 * header only, runs a minimal "manager thread" that answers requests, and
 * exercises the documented edge cases. It needs no WinCC OA SDK.
 *
 * Run from embed/harness (make embed-test).
 */
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

#include "monstermq.h"

static int failures;
#define CHECK(cond, ...)                                  \
  do {                                                    \
    if (!(cond)) {                                        \
      failures++;                                         \
      fprintf(stderr, "FAIL %s:%d: ", __FILE__, __LINE__); \
      fprintf(stderr, __VA_ARGS__);                       \
      fprintf(stderr, "\n");                              \
    }                                                     \
  } while (0)

/* ---- request queue serviced by the "manager thread" ---- */

typedef struct req {
  uint64_t id;
  int64_t deadline;
  uint8_t *data;
  uint32_t len;
  struct req *next;
} req;

static pthread_mutex_t qmu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t qcv = PTHREAD_COND_INITIALIZER;
static req *qhead, *qtail;
static int qlen;
static const int qcap = 64;
static atomic_int running = 1;
static atomic_ulong handle_g;
static atomic_int submits, resolves, logs;
static char last_resolve[256];
static pthread_t manager_tid;
static atomic_int submit_from_manager;

static int64_t now_ms(void) {
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

static int32_t host_submit(void *user, uint64_t id, int64_t deadline, const uint8_t *data, uint32_t len) {
  (void)user;
  if (pthread_equal(pthread_self(), manager_tid)) atomic_fetch_add(&submit_from_manager, 1);
  pthread_mutex_lock(&qmu);
  if (qlen >= qcap) {
    pthread_mutex_unlock(&qmu);
    return MMQ_E_OVERLOAD;
  }
  req *r = calloc(1, sizeof(req));
  r->id = id;
  r->deadline = deadline;
  r->len = len;
  if (len) {
    r->data = malloc(len);
    memcpy(r->data, data, len); /* the buffer is only valid during this call */
  }
  if (qtail) qtail->next = r; else qhead = r;
  qtail = r;
  qlen++;
  atomic_fetch_add(&submits, 1);
  pthread_mutex_unlock(&qmu);
  return MMQ_OK;
}

static void host_wake(void *user) {
  (void)user;
  pthread_cond_signal(&qcv);
}

static void host_log(void *user, int32_t level, const char *msg, uint32_t len) {
  (void)user;
  (void)level;
  (void)msg;
  (void)len;
  atomic_fetch_add(&logs, 1);
}

/* ---- TLV helpers (spec section 3.1) ---- */

static void put_field(uint8_t *buf, uint32_t *off, uint8_t tag, const void *p, uint32_t n) {
  buf[(*off)++] = tag;
  buf[(*off)++] = n & 0xff;
  buf[(*off)++] = (n >> 8) & 0xff;
  buf[(*off)++] = (n >> 16) & 0xff;
  buf[(*off)++] = (n >> 24) & 0xff;
  memcpy(buf + *off, p, n);
  *off += n;
}

static const uint8_t *find_field(const uint8_t *d, uint32_t len, uint8_t tag, uint32_t *n) {
  uint32_t off = 0;
  while (off + 5 <= len) {
    uint8_t t = d[off];
    uint32_t l = d[off + 1] | (d[off + 2] << 8) | (d[off + 3] << 16) | ((uint32_t)d[off + 4] << 24);
    off += 5;
    if (off + l > len) return NULL;
    if (t == tag) {
      *n = l;
      return d + off;
    }
    off += l;
  }
  return NULL;
}

static uint32_t op_of(const req *r) {
  uint32_t n = 0;
  const uint8_t *p = find_field(r->data, r->len, 1, &n);
  if (!p || n != 4) return 0;
  return p[0] | (p[1] << 8) | (p[2] << 16) | ((uint32_t)p[3] << 24);
}

static void *manager_thread(void *arg) {
  (void)arg;
  while (atomic_load(&running)) {
    pthread_mutex_lock(&qmu);
    while (!qhead && atomic_load(&running)) {
      struct timespec ts;
      clock_gettime(CLOCK_REALTIME, &ts);
      ts.tv_nsec += 10 * 1000000;
      if (ts.tv_nsec >= 1000000000) {
        ts.tv_sec++;
        ts.tv_nsec -= 1000000000;
      }
      pthread_cond_timedwait(&qcv, &qmu, &ts);
    }
    req *r = qhead;
    if (r) {
      qhead = r->next;
      if (!qhead) qtail = NULL;
      qlen--;
    }
    pthread_mutex_unlock(&qmu);
    if (!r) continue;

    uint64_t h = atomic_load(&handle_g);
    uint8_t out[512];
    uint32_t off = 0;
    int32_t status = MMQ_OK;
    if (now_ms() > r->deadline) {
      status = MMQ_E_TIMEOUT; /* expired: never executed */
    } else {
      switch (op_of(r)) {
        case 2: /* SYSINFO */
          put_field(out, &off, 11, "HarnessSys", 10);
          break;
        case 1: { /* RESOLVE: nothing exists in the harness */
          uint32_t n = 0;
          const uint8_t *name = find_field(r->data, r->len, 2, &n);
          if (name && n < sizeof(last_resolve)) {
            memcpy(last_resolve, name, n);
            last_resolve[n] = 0;
          }
          atomic_fetch_add(&resolves, 1);
          uint8_t no = 0;
          put_field(out, &off, 12, &no, 1);
          put_field(out, &off, 11, "HarnessSys", 10);
          break;
        }
        default: {
          const char *e = "operation not supported by the harness";
          put_field(out, &off, 7, e, (uint32_t)strlen(e));
          status = MMQ_E_OA;
        }
      }
    }
    int32_t rc = mmq_complete(h, r->id, status, off ? out : NULL, off);
    (void)rc;
    free(r->data);
    free(r);
  }
  return NULL;
}

/* ---- helpers ---- */

static mmq_host make_host(void) {
  mmq_host host;
  memset(&host, 0, sizeof(host));
  host.struct_size = sizeof(host);
  host.abi_version = MMQ_ABI_VERSION;
  host.submit = host_submit;
  host.wake = host_wake;
  host.log = host_log;
  return host;
}

static mmq_config make_config(const char *path) {
  mmq_config cfg;
  memset(&cfg, 0, sizeof(cfg));
  cfg.struct_size = sizeof(cfg);
  cfg.abi_version = MMQ_ABI_VERSION;
  cfg.config_path = path;
  cfg.config_path_len = path ? (uint32_t)strlen(path) : 0;
  cfg.default_timeout_ms = 2000;
  cfg.log_level = MMQ_LOG_INFO;
  return cfg;
}

static int wait_state(uint64_t h, int want, int ms) {
  int st = 0;
  for (int i = 0; i < ms / 10; i++) {
    st = mmq_state(h, NULL, 0, NULL);
    if (st == want || st == MMQ_STATE_FAILED) return st;
    usleep(10000);
  }
  return st;
}

static int mqtt_subscribe_code(int port, const char *filter) {
  int fd = socket(AF_INET, SOCK_STREAM, 0);
  struct sockaddr_in a;
  memset(&a, 0, sizeof(a));
  a.sin_family = AF_INET;
  a.sin_port = htons((uint16_t)port);
  a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  if (connect(fd, (struct sockaddr *)&a, sizeof(a)) != 0) {
    close(fd);
    return -1;
  }
  struct timeval tv = {5, 0};
  setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
  const uint8_t conn[] = {0x10, 16, 0, 4, 'M', 'Q', 'T', 'T', 4, 2, 0, 60, 0, 4, 'h', 'a', 'r', 'n'};
  if (write(fd, conn, sizeof(conn)) != (ssize_t)sizeof(conn)) goto fail;
  uint8_t ack[4];
  if (read(fd, ack, 4) != 4 || ack[0] != 0x20 || ack[3] != 0) goto fail;
  uint8_t sub[256];
  size_t fl = strlen(filter);
  size_t rem = 2 + 2 + fl + 1;
  size_t o = 0;
  sub[o++] = 0x82;
  sub[o++] = (uint8_t)rem;
  sub[o++] = 0;
  sub[o++] = 1;
  sub[o++] = (uint8_t)(fl >> 8);
  sub[o++] = (uint8_t)fl;
  memcpy(sub + o, filter, fl);
  o += fl;
  sub[o++] = 1;
  if (write(fd, sub, o) != (ssize_t)o) goto fail;
  uint8_t sa[5];
  if (read(fd, sa, 5) != 5 || sa[0] != 0x90) goto fail;
  close(fd);
  return sa[4];
fail:
  close(fd);
  return -1;
}

static atomic_int sig_seen;
static void on_term(int sig) {
  (void)sig;
  atomic_store(&sig_seen, 1);
}

int main(void) {
  char cfg_path[] = "harness.yaml";
  mmq_host host = make_host();
  mmq_config cfg = make_config(cfg_path);
  uint64_t h = 0;

  manager_tid = 0;
  pthread_t mt;
  pthread_create(&mt, NULL, manager_thread, NULL);
  manager_tid = mt;

  /* Version and argument validation. */
  CHECK(mmq_abi_version() == MMQ_ABI_VERSION, "abi version %d", mmq_abi_version());
  CHECK(mmq_create(NULL, &host, &h) == MMQ_E_INVALID, "null cfg");
  CHECK(mmq_create(&cfg, NULL, &h) == MMQ_E_INVALID, "null host");
  CHECK(mmq_create(&cfg, &host, NULL) == MMQ_E_INVALID, "null out");
  mmq_config small = cfg;
  small.struct_size = 8;
  CHECK(mmq_create(&small, &host, &h) == MMQ_E_ABI, "short cfg struct");
  mmq_host bad = host;
  bad.abi_version = MMQ_ABI_VERSION + 1;
  CHECK(mmq_create(&cfg, &bad, &h) == MMQ_E_ABI, "abi mismatch");
  bad = host;
  bad.submit = NULL;
  CHECK(mmq_create(&cfg, &bad, &h) == MMQ_E_INVALID, "missing submit");
  mmq_config missing = make_config("does-not-exist.yaml");
  CHECK(mmq_create(&missing, &host, &h) == MMQ_E_INVALID, "invalid config path");
  CHECK(mmq_start(12345) == MMQ_E_STATE, "start without instance");

  /* Occupied listener port: start fails, resources are released. */
  int blocker = socket(AF_INET, SOCK_STREAM, 0);
  int one = 1;
  setsockopt(blocker, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
  struct sockaddr_in ba;
  memset(&ba, 0, sizeof(ba));
  ba.sin_family = AF_INET;
  ba.sin_port = htons(27191);
  ba.sin_addr.s_addr = htonl(INADDR_ANY);
  CHECK(bind(blocker, (struct sockaddr *)&ba, sizeof(ba)) == 0 && listen(blocker, 1) == 0, "blocker bind");
  mmq_config busy = make_config("harness-busy.yaml");
  CHECK(mmq_create(&busy, &host, &h) == MMQ_OK, "create busy");
  atomic_store(&handle_g, h);
  CHECK(mmq_start(h) == MMQ_OK, "start busy");
  CHECK(wait_state(h, MMQ_STATE_RUNNING, 5000) == MMQ_STATE_FAILED, "occupied port must fail");
  char err[256];
  uint32_t elen = 0;
  mmq_state(h, err, sizeof(err) - 1, &elen);
  err[elen < sizeof(err) - 1 ? elen : sizeof(err) - 1] = 0;
  CHECK(elen > 0 && strstr(err, "27191"), "failure reason: %s", err);
  CHECK(mmq_destroy(h) == MMQ_OK, "destroy failed instance");
  close(blocker);

  /* Normal lifecycle. */
  CHECK(mmq_create(&cfg, &host, &h) == MMQ_OK, "create");
  atomic_store(&handle_g, h);
  uint64_t h2 = 0;
  CHECK(mmq_create(&cfg, &host, &h2) == MMQ_E_STATE, "second instance must be rejected");
  CHECK(mmq_destroy(h + 1) == MMQ_E_STATE, "stale handle");
  CHECK(mmq_start(h) == MMQ_OK, "start");
  CHECK(mmq_start(h) == MMQ_E_STATE, "double start");
  int st = wait_state(h, MMQ_STATE_RUNNING, 10000);
  if (st != MMQ_STATE_RUNNING) {
    mmq_state(h, err, sizeof(err) - 1, &elen);
    err[elen < sizeof(err) - 1 ? elen : sizeof(err) - 1] = 0;
  }
  CHECK(st == MMQ_STATE_RUNNING, "state %d: %s", st, err);
  CHECK(mmq_destroy(h) == MMQ_E_STATE, "destroy while running");

  /* Completion and event edge cases. */
  uint8_t bin[] = {0, 1, 0, 0xff, 0};
  CHECK(mmq_complete(h, 999999, MMQ_OK, bin, sizeof(bin)) == MMQ_E_NOT_FOUND, "unknown completion");
  CHECK(mmq_complete(h, 999999, MMQ_OK, NULL, 0) == MMQ_E_NOT_FOUND, "zero-length unknown completion");
  CHECK(mmq_complete(h, 999999, MMQ_OK, NULL, 4) == MMQ_E_INVALID, "null data with length");
  uint8_t *big = calloc(1, MMQ_MAX_MESSAGE + 1);
  CHECK(mmq_complete(h, 999999, MMQ_OK, big, MMQ_MAX_MESSAGE + 1) == MMQ_E_TOO_LARGE, "oversized completion");
  CHECK(mmq_event(h, 77, big, MMQ_MAX_MESSAGE + 1) == MMQ_E_TOO_LARGE, "oversized event");
  free(big);
  CHECK(mmq_event(h, 77, bin, sizeof(bin)) == MMQ_OK, "binary event for unknown ref is accepted and dropped");
  CHECK(mmq_event(h, 77, NULL, 3) == MMQ_E_INVALID, "null event data");
  const char *stats = "{\"connects\":3,\"queued\":0}";
  CHECK(mmq_stats(h, (const uint8_t *)stats, (uint32_t)strlen(stats)) == MMQ_OK, "host stats");
  const char *badStats = "{\"a/b\":1}";
  CHECK(mmq_stats(h, (const uint8_t *)badStats, (uint32_t)strlen(badStats)) == MMQ_E_INVALID, "host stats with a topic separator");
  CHECK(mmq_stats(h, (const uint8_t *)"[1]", 3) == MMQ_E_INVALID, "host stats not an object");
  CHECK(mmq_state(h, NULL, 0, NULL) == MMQ_STATE_RUNNING, "still running after bad input");

  /* End-to-end: a native SUBSCRIBE triggers RESOLVE on the host. */
  int code = mqtt_subscribe_code(27190, "winccoa/systems/HarnessSys/tags/Pump1/speed");
  CHECK(code == 0x80, "native subscribe SUBACK 0x%02x", code);
  CHECK(atomic_load(&resolves) >= 1 && strcmp(last_resolve, "HarnessSys:Pump1.speed") == 0,
        "resolve %d name %s", atomic_load(&resolves), last_resolve);
  code = mqtt_subscribe_code(27190, "plain/topic");
  CHECK(code == 0x01, "plain subscribe SUBACK 0x%02x", code);
  CHECK(atomic_load(&submit_from_manager) == 0, "library submitted from the manager thread");

  /* A host signal handler installed after the Go runtime, with SA_ONSTACK,
   * coexists with Go: signals delivered to arbitrary threads while Go works
   * do not crash the process. */
  struct sigaction sa;
  memset(&sa, 0, sizeof(sa));
  sa.sa_handler = on_term;
  sa.sa_flags = SA_ONSTACK | SA_RESTART;
  sigemptyset(&sa.sa_mask);
  CHECK(sigaction(SIGTERM, &sa, NULL) == 0, "sigaction");
  for (int i = 0; i < 50; i++) {
    kill(getpid(), SIGTERM);
    mqtt_subscribe_code(27190, "plain/x");
  }
  CHECK(atomic_load(&sig_seen) == 1, "SIGTERM handler not called");
  CHECK(mmq_state(h, NULL, 0, NULL) == MMQ_STATE_RUNNING, "running after signals");

  /* Stop while the host keeps dispatching; idempotent. */
  CHECK(mmq_stop(h, 5000) == MMQ_OK, "stop");
  CHECK(mmq_stop(h, 5000) == MMQ_OK, "stop again");
  CHECK(wait_state(h, MMQ_STATE_STOPPED, 8000) == MMQ_STATE_STOPPED, "stopped");
  CHECK(mmq_complete(h, 1, MMQ_OK, NULL, 0) == MMQ_E_NOT_FOUND || mmq_complete(h, 1, MMQ_OK, NULL, 0) == MMQ_E_STATE,
        "completion after stop");
  CHECK(mmq_destroy(h) == MMQ_OK, "destroy");
  CHECK(mmq_state(h, NULL, 0, NULL) == MMQ_E_STATE, "state after destroy");
  CHECK(mmq_complete(h, 1, MMQ_OK, NULL, 0) == MMQ_E_STATE, "complete after destroy");
  CHECK(mmq_stop(h, 10) == MMQ_E_STATE, "stop after destroy");

  /* The port is free again after stop. */
  int probe = socket(AF_INET, SOCK_STREAM, 0);
  setsockopt(probe, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
  ba.sin_port = htons(27190);
  CHECK(bind(probe, (struct sockaddr *)&ba, sizeof(ba)) == 0, "listener port still bound after stop");
  close(probe);

  atomic_store(&running, 0);
  pthread_cond_signal(&qcv);
  pthread_join(mt, NULL);
  CHECK(atomic_load(&logs) > 0, "no log lines received");

  if (failures) {
    fprintf(stderr, "abi_harness: %d failure(s)\n", failures);
    return 1;
  }
  printf("abi_harness: all checks passed (%d submits, %d resolves, %d log lines)\n",
         atomic_load(&submits), atomic_load(&resolves), atomic_load(&logs));
  return 0;
}
