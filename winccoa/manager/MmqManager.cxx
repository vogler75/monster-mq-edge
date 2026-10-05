#include "MmqManager.hxx"
#include "MmqResources.hxx"

#include <csignal>
#include <unistd.h>
#include <cerrno>
#include <cstring>
#include <iostream>
#include <thread>

#include <AnswerGroup.hxx>
#include <AnswerItem.hxx>
#include <DpIdentList.hxx>
#include <DpIdValueList.hxx>
#include <DpIdentification.hxx>
#include <DpType.hxx>
#include <DpTypeContainer.hxx>
#include <DpTypeDefinition.hxx>
#include <DpTypeNode.hxx>
#include <DpVCItem.hxx>
#include <DynVar.hxx>
#include <ErrClass.hxx>
#include <ErrHdl.hxx>
#include <IntegerVar.hxx>
#include <StartDpInitSysMsg.hxx>
#include <UIntegerVar.hxx>

using namespace mmq;

std::atomic<long> mmqLiveWaits(0);
std::atomic<bool> MmqManager::doExit(false);
std::atomic<bool> MmqManager::brokerStarted(false);

static int64_t nowMs()
{
  return std::chrono::duration_cast<std::chrono::milliseconds>(
             std::chrono::system_clock::now().time_since_epoch())
      .count();
}

// ---------------------------------------------------------------------------
// Callback objects

ConnectWait::ConnectWait(MmqManager *m, uint64_t r, size_t i) : ref(r), index(i), mgr(m)
{
  mmqLiveWaits++;
}
ConnectWait::~ConnectWait() { mmqLiveWaits--; }
void ConnectWait::hotLinkCallBack(DpMsgAnswer &answer) { mgr->onConnectAnswer(this, answer); }
void ConnectWait::hotLinkCallBack(DpHLGroup &group) { mgr->onConnectHotlink(this, group); }

QueryWait::QueryWait(MmqManager *m, uint64_t r, uint64_t q, bool a)
    : ref(r), reqId(q), wantAnswer(a), mgr(m)
{
  mmqLiveWaits++;
}
QueryWait::~QueryWait() { mmqLiveWaits--; }
void QueryWait::hotLinkCallBack(DpMsgAnswer &answer) { mgr->onQueryAnswer(this, answer); }
void QueryWait::hotLinkCallBack(DpHLGroup &group) { mgr->onQueryHotlink(this, group); }

RequestWait::RequestWait(MmqManager *m, Kind k, uint64_t q, const std::string &n)
    : mgr(m), kind(k), reqId(q), name(n)
{
  mmqLiveWaits++;
}
RequestWait::~RequestWait() { mmqLiveWaits--; }
void RequestWait::callBack(DpMsgAnswer &answer) { mgr->onRequestAnswer(kind, reqId, name, answer); }

SetBatchWait::SetBatchWait(MmqManager *m, std::vector<Entry> e) : mgr(m), entries(std::move(e)) { mmqLiveWaits++; }
SetBatchWait::~SetBatchWait() { mmqLiveWaits--; }
void SetBatchWait::callBack(DpMsgAnswer &answer) { mgr->onSetBatchAnswer(entries, answer); }

DistWait::DistWait(MmqManager *m) : mgr(m) { mmqLiveWaits++; }
DistWait::~DistWait() { mmqLiveWaits--; }
void DistWait::hotLinkCallBack(DpMsgAnswer &answer)
{
  for (AnswerGroup *g = answer.getFirstGroup(); g; g = answer.getNextGroup())
    for (AnswerItem *it = g->getFirstItem(); it; it = g->getNextItem())
      mgr->onDistState(it->getValuePtr());
}
void DistWait::hotLinkCallBack(DpHLGroup &group)
{
  for (DpVCItem *it = group.getFirstItem(); it; it = group.getNextItem())
    mgr->onDistState(it->getValuePtr());
}

// ---------------------------------------------------------------------------
// Signals: the Go runtime (c-archive) installs handlers for synchronous and
// runtime signals before main. WinCC OA may install its own afterwards.

static const int runtimeSignals[] = {SIGSEGV, SIGBUS, SIGFPE, SIGILL, SIGURG, SIGPROF};
static struct sigaction goHandlers[sizeof(runtimeSignals) / sizeof(runtimeSignals[0])];

void MmqManager::saveRuntimeSignals()
{
  for (size_t i = 0; i < sizeof(runtimeSignals) / sizeof(runtimeSignals[0]); i++)
    sigaction(runtimeSignals[i], nullptr, &goHandlers[i]);
}

// Reinstalls the Go handlers where WinCC OA replaced them, so Go runtime
// faults and preemption keep working inside the embedded library.
void MmqManager::restoreRuntimeSignals()
{
  for (size_t i = 0; i < sizeof(runtimeSignals) / sizeof(runtimeSignals[0]); i++)
  {
    struct sigaction cur;
    if (sigaction(runtimeSignals[i], nullptr, &cur) != 0)
      continue;
    if (cur.sa_handler != goHandlers[i].sa_handler)
      sigaction(runtimeSignals[i], &goHandlers[i], nullptr);
  }
}

// Every non-default handler must run on the alternate signal stack, because
// the signal can arrive on a Go-owned thread with a small stack.
void MmqManager::addOnStackToHandlers()
{
  for (int sig = 1; sig < NSIG; sig++)
  {
    struct sigaction sa;
    if (sigaction(sig, nullptr, &sa) != 0)
      continue;
    if (sa.sa_handler == SIG_DFL || sa.sa_handler == SIG_IGN || (sa.sa_flags & SA_ONSTACK))
      continue;
    sa.sa_flags |= SA_ONSTACK;
    sigaction(sig, &sa, nullptr);
  }
}

void MmqManager::signalHandler(int sig)
{
  if (sig == SIGINT || sig == SIGTERM)
  {
    doExit = true;
    return;
  }
  Manager::signalHandler(sig);
}

// Raw SIGINT/SIGTERM handler. The SDK's sigHdl defers to dispatch(), which
// does not run while connectToEvent sleeps between 20 s retries. Before the
// broker exists there is nothing to flush, so the process ends at once
// (_exit is async-signal-safe); afterwards only the exit flag is set and the
// main loop stops the broker in order. Installed with SA_ONSTACK because
// the signal may arrive on a Go runtime thread.
static void onExitSignal(int)
{
  MmqManager::requestExit();
}

void MmqManager::requestExit()
{
  if (!brokerStarted)
    _exit(0);
  doExit = true;
}

void MmqManager::installExitSignals()
{
  struct sigaction sa;
  memset(&sa, 0, sizeof(sa));
  sa.sa_handler = onExitSignal;
  sa.sa_flags = SA_ONSTACK | SA_RESTART;
  sigemptyset(&sa.sa_mask);
  sigaction(SIGINT, &sa, nullptr);
  sigaction(SIGTERM, &sa, nullptr);
}

// ---------------------------------------------------------------------------

MmqManager::MmqManager()
    : Manager(ManagerIdentifier(API_MAN, Resources::getManNum()))
{
  queueCap = (size_t)MmqResources::getQueueCapacity();
}

int32_t MmqManager::cbSubmit(void *user, uint64_t id, int64_t deadline, const uint8_t *data, uint32_t len)
{
  MmqManager *self = static_cast<MmqManager *>(user);
  std::lock_guard<std::mutex> lock(self->qmu);
  if (self->queue.size() >= self->queueCap)
  {
    self->overloads++;
    return MMQ_E_OVERLOAD;
  }
  Pending p;
  p.id = id;
  p.deadline = deadline;
  if (len)
    p.data.assign(data, data + len);
  self->queue.push_back(std::move(p));
  if (self->queue.size() > self->queueHighWater)
    self->queueHighWater = self->queue.size();
  return MMQ_OK;
}

void MmqManager::cbLog(void *user, int32_t level, const char *msg, uint32_t len)
{
  MmqManager *self = static_cast<MmqManager *>(user);
  std::lock_guard<std::mutex> lock(self->logmu);
  if (self->logs.size() >= 10000)
  {
    self->logs.pop_front();
    self->logsDropped++;
  }
  self->logs.emplace_back(level, std::string(msg, len));
}

void MmqManager::logLine(int32_t level, const std::string &msg)
{
  ErrClass::ErrPrio prio = ErrClass::PRIO_INFO;
  if (level >= MMQ_LOG_ERROR)
    prio = ErrClass::PRIO_SEVERE;
  else if (level == MMQ_LOG_WARN)
    prio = ErrClass::PRIO_WARNING;
  // DEBUG lines are filtered by the broker (-dbg USR1 or Logging.Level).
  // Informational lines use code 0 (no error) so the log does not show
  // "unexpected state" for normal broker output.
  ErrHdl::error(prio, ErrClass::ERR_CONTROL, prio == ErrClass::PRIO_INFO ? ErrClass::NOERR : ErrClass::UNEXPECTEDSTATE,
                "MMQ", "broker", CharString(msg.c_str()));
}

void MmqManager::drainLogs()
{
  std::deque<std::pair<int32_t, std::string>> batch;
  unsigned long dropped;
  {
    std::lock_guard<std::mutex> lock(logmu);
    batch.swap(logs);
    dropped = logsDropped;
    logsDropped = 0;
  }
  for (auto &l : batch)
    logLine(l.first, l.second);
  if (dropped)
    logLine(MMQ_LOG_WARN, "log queue overflow: " + std::to_string(dropped) + " lines dropped");
}

int32_t MmqManager::complete(uint64_t reqId, int32_t status, const Writer *w, const std::string &err)
{
  Writer out;
  const Writer *use = w;
  if (!err.empty())
  {
    if (w)
      out = *w;
    out.str(TagError, err);
    use = &out;
  }
  return mmq_complete(handle, reqId, status, use ? use->data() : nullptr, use ? use->size() : 0);
}

void MmqManager::event(uint64_t ref, const Writer &w)
{
  // A full queue drops the event (counted by the library); anything else
  // is logged, since the broker never sees it.
  int32_t rc = mmq_event(handle, ref, w.data(), w.size());
  if (rc != MMQ_OK && rc != MMQ_E_OVERLOAD)
    logLine(MMQ_LOG_WARN, "event for reference " + std::to_string(ref) + " (" + std::to_string(w.size()) +
                              " bytes) not delivered: " + std::to_string(rc));
}

// Sends a query table as one or more events below the ABI message limit;
// every chunk repeats the header row and all but the last initial-answer
// chunk carry FlagMore.
bool MmqManager::sendTable(uint64_t ref, const Variable *table, bool answer)
{
  return encodeTableChunks(table, MMQ_MAX_MESSAGE / 2, [&](const Writer &rows, bool last) {
    Writer ev;
    uint32_t flags = (answer ? FlagAnswer : 0) | (last ? 0 : FlagMore);
    if (flags)
      ev.u32(TagFlags, flags);
    ev.append(rows);
    event(ref, ev);
  });
}

std::string MmqManager::answerError(DpMsgAnswer &answer)
{
  std::string err;
  for (AnswerGroup *g = answer.getFirstGroup(); g; g = answer.getNextGroup())
  {
    if (g->wasOk())
      continue;
    if (!err.empty())
      err += "; ";
    ErrClass *e = g->getErrorPtr();
    err += e ? std::string((const char *)e->getErrorText()) : std::string("unknown WinCC OA error");
  }
  return err;
}

// ---------------------------------------------------------------------------
// Request processing (manager thread only)

void MmqManager::drainRequests()
{
  std::deque<Pending> work;
  {
    std::lock_guard<std::mutex> lock(qmu);
    work.swap(retry);
  }
  auto start = std::chrono::steady_clock::now();
  long budget = MmqResources::getTickBudget();
  long budgetMs = MmqResources::getTickBudgetMs();
  for (long n = 0; n < budget; n++)
  {
    if (work.empty())
    {
      std::lock_guard<std::mutex> lock(qmu);
      if (queue.empty())
        break;
      work.push_back(std::move(queue.front()));
      queue.pop_front();
    }
    Pending p = std::move(work.front());
    work.pop_front();
    if (nowMs() > p.deadline)
    {
      // Expired before execution: never executed.
      complete(p.id, MMQ_E_TIMEOUT, nullptr, "deadline passed before execution");
    }
    else if (!process(p))
    {
      std::lock_guard<std::mutex> lock(qmu);
      retry.push_back(std::move(p));
    }
    if (std::chrono::steady_clock::now() - start > std::chrono::milliseconds(budgetMs))
      break;
  }
  if (!work.empty())
  {
    std::lock_guard<std::mutex> lock(qmu);
    for (auto &p : work)
      retry.push_back(std::move(p));
  }
  flushSetBatch();
}

bool MmqManager::process(const Pending &p)
{
  // Every WinCC OA call happens in process() or in callbacks invoked by
  // dispatch(); count any execution off the manager thread (AC-06).
  if (std::this_thread::get_id() != managerThread)
    offThreadCalls++;
  Message m;
  if (!m.parse(p.data.data(), (uint32_t)p.data.size()))
  {
    complete(p.id, MMQ_E_INVALID, nullptr, "malformed request");
    return true;
  }
  uint32_t op = 0;
  m.u32(TagOp, op);
  Writer w;
  std::string err;
  int32_t st = MMQ_OK;
  switch (op)
  {
    case OpSysInfo:
    {
      w.str(TagSysName, std::string((const char *)localSystem));
      // Redundancy facts: the broker derives its own host (1 or 2) and
      // watches _ReduManager[_2].Status.Active for its role.
      w.boolean(TagRedundant, Resources::isRedundant());
      w.u32(TagReplica, (uint32_t)Resources::getReplica());
      w.str(TagHost, std::string((const char *)Resources::getPrimaryEMHostName()));
      w.str(TagHost, std::string((const char *)Resources::getSecondaryEMHostName()));
      char host[256] = {0};
      if (gethostname(host, sizeof(host) - 1) == 0)
        w.str(TagLocalHost, std::string(host));
      complete(p.id, MMQ_OK, &w);
      return true;
    }
    case OpResolve:
      st = opResolve(m, w, err);
      complete(p.id, st, &w, err);
      return true;
    case OpDpConnect:
      opDpConnect(p.id, m);
      return true;
    case OpDpDisconnect:
    {
      uint64_t ref = 0;
      m.u64(TagRef, ref);
      if (!conns.count(ref))
      {
        complete(p.id, MMQ_E_NOT_FOUND, nullptr, "unknown connect reference");
        return true;
      }
      disconnectConn(ref);
      complete(p.id, MMQ_OK);
      return true;
    }
    case OpQueryConnect:
      opQueryConnect(p.id, m);
      return true;
    case OpQueryDisconnect:
    {
      uint64_t ref = 0;
      m.u64(TagRef, ref);
      if (!queries.count(ref))
      {
        complete(p.id, MMQ_E_NOT_FOUND, nullptr, "unknown query reference");
        return true;
      }
      disconnectQuery(ref);
      complete(p.id, MMQ_OK);
      return true;
    }
    case OpDpSet:
      return opDpSet(p.id, m);
    case OpDpGet:
      opDpGet(p.id, m);
      return true;
    case OpDpNames:
      st = opDpNames(m, w, err);
      complete(p.id, st, &w, err);
      return true;
    case OpDpCreate:
      opDpCreate(p.id, m);
      return true;
    case OpDpDelete:
      opDpDelete(p.id, m);
      return true;
    case OpTypeCheck:
      opTypeCheck(p.id, m);
      return true;
  }
  complete(p.id, MMQ_E_INVALID, nullptr, "unknown operation " + std::to_string(op));
  return true;
}

// Splits "Sys:rest" when the prefix is a system name (contains no dot).
static void splitSystem(const std::string &name, std::string &sys, std::string &rest)
{
  size_t c = name.find(':');
  if (c != std::string::npos && name.substr(0, c).find('.') == std::string::npos)
  {
    sys = name.substr(0, c);
    rest = name.substr(c + 1);
    return;
  }
  sys.clear();
  rest = name;
}

int32_t MmqManager::opResolve(const Message &m, Writer &w, std::string &err)
{
  std::string name = m.str(TagName), sys, rest;
  splitSystem(name, sys, rest);
  std::string local((const char *)localSystem);
  if (!sys.empty() && sys != local)
  {
    SystemNumType num = 0;
    if (!getSystemId(CharString(sys.c_str()), num))
    {
      err = "unknown system " + sys;
      return MMQ_E_UNAVAILABLE;
    }
    if (!distUp.count(num))
    {
      err = "system " + sys + " is not connected";
      return MMQ_E_UNAVAILABLE;
    }
  }
  DpIdentifier id;
  if (!getId(CharString(name.c_str()), id))
  {
    w.boolean(TagExists, false);
    w.str(TagSysName, sys.empty() ? local : sys);
    return MMQ_OK;
  }
  DpElementType et = DPELEMENT_NOELEMENT;
  if (!getElementType(id, et))
    et = DPELEMENT_NOELEMENT;
  CharString typeName, sysName;
  getTypeName(id.getDpType(), typeName, id.getSystem());
  getSystemName(id.getSystem(), sysName);
  w.boolean(TagExists, true);
  w.str(TagName, name);
  w.str(TagTypeName, std::string((const char *)typeName));
  w.u32(TagElemType, kindOfElement(et));
  w.str(TagSysName, std::string((const char *)sysName));
  return MMQ_OK;
}

void MmqManager::opDpConnect(uint64_t reqId, const Message &m)
{
  uint64_t ref = 0;
  uint32_t flags = 0;
  m.u64(TagRef, ref);
  m.u32(TagFlags, flags);
  std::vector<const Field *> names = m.all(TagName);
  if (names.empty() || names.size() > 100)
  {
    complete(reqId, MMQ_E_INVALID, nullptr, "connect batch must hold 1..100 names");
    return;
  }
  if (conns.count(ref))
  {
    complete(reqId, MMQ_E_INVALID, nullptr, "duplicate connect reference");
    return;
  }
  ConnReg reg;
  reg.reqId = reqId;
  reg.wantAnswer = (flags & FlagAnswer) != 0;
  reg.pending = 0;
  reg.completed = false;
  if (reg.wantAnswer)
    reg.answerEv.u32(TagFlags, FlagAnswer);
  for (const Field *f : names)
  {
    std::string n = fieldString(f);
    DpIdentifier id;
    if (!resolveCached(n, id))
    {
      complete(reqId, MMQ_E_NOT_FOUND, nullptr, "datapoint element not found: " + n);
      return;
    }
    reg.ids.emplace_back(id, n);
  }
  ConnReg &r = conns[ref] = reg;
  r.waits.assign(r.ids.size(), nullptr);
  r.stimes.assign(r.ids.size(), {false, DpIdentifier()});
  for (size_t i = 0; i < r.ids.size(); i++)
  {
    // del=true: verified on 3.21 (AC-02 probe) to keep the callback after
    // the answer and delete it on dpDisconnect.
    ConnectWait *w = new ConnectWait(this, ref, i);
    DpIdentList list;
    list.append(r.ids[i].first);
    if (flags & FlagSourceTime)
    {
      // The source time is connected with the value in one dpConnect, so
      // both arrive in the same answer and hotlink group.
      std::string n = r.ids[i].second;
      size_t pos = n.rfind(":_");
      std::string base = pos == std::string::npos ? n : n.substr(0, pos);
      DpIdentifier sid;
      if ((pos == std::string::npos || n.compare(pos, std::string::npos, ":_online.._stime") != 0) &&
          resolveCached(base + ":_online.._stime", sid))
      {
        list.append(sid);
        r.stimes[i] = {true, sid};
      }
    }
    bool sent = (flags & FlagNoSource) ? dpConnectNoSource(list, w, true) : dpConnect(list, w, true);
    if (!sent)
    {
      // Ownership of an unsent callback is undocumented; leaking it is
      // safer than a double delete.
      r.err = "dpConnect message not sent for " + r.ids[i].second;
      break;
    }
    r.waits[i] = w;
    r.pending++;
  }
  if (r.pending == 0)
  {
    complete(reqId, MMQ_E_OA, nullptr, r.err);
    conns.erase(ref);
  }
  // With some registrations sent and one failed, the error is reported
  // once all sent registrations answered (finishConnect).
}

// Called when the last answer of a connect request arrived.
void MmqManager::finishConnect(uint64_t ref)
{
  ConnReg &r = conns[ref];
  r.completed = true;
  if (!r.err.empty())
  {
    // Sent registrations must be disconnected even when an answer failed.
    connDrop.push_back(ref);
    complete(r.reqId, MMQ_E_OA, nullptr, r.err);
    return;
  }
  if (complete(r.reqId, MMQ_OK) == MMQ_E_NOT_FOUND)
  {
    // Nobody waits for this registration any more (timeout): release it.
    connDrop.push_back(ref);
    return;
  }
  if (r.wantAnswer)
    event(ref, r.answerEv);
  r.answerEv = Writer();
}

void MmqManager::onConnectAnswer(ConnectWait *w, DpMsgAnswer &answer)
{
  auto it = conns.find(w->ref);
  if (it == conns.end() || w->index >= it->second.waits.size() || it->second.waits[w->index] != w || it->second.completed)
    return;
  ConnReg &r = it->second;
  std::string err = answerError(answer);
  if (!err.empty())
  {
    if (!r.err.empty())
      r.err += "; ";
    r.err += r.ids[w->index].second + ": " + err;
  }
  else if (r.wantAnswer)
  {
    const Variable *value = nullptr, *stime = nullptr;
    for (AnswerGroup *g = answer.getFirstGroup(); g; g = answer.getNextGroup())
      for (AnswerItem *item = g->getFirstItem(); item; item = g->getNextItem())
        pickItem(r, w->index, item->getDpIdentifier(), item->getValuePtr(), value, stime);
    putItem(r.answerEv, r.ids[w->index].second, value, stime);
  }
  if (r.pending > 0 && --r.pending == 0)
    finishConnect(w->ref);
}

void MmqManager::onConnectHotlink(ConnectWait *w, DpHLGroup &group)
{
  auto it = conns.find(w->ref);
  if (it == conns.end() || w->index >= it->second.waits.size() || it->second.waits[w->index] != w)
    return;
  ConnReg &r = it->second;
  Writer ev;
  hotlinkGroups++;
  const Variable *value = nullptr, *stime = nullptr;
  for (DpVCItem *item = group.getFirstItem(); item; item = group.getNextItem(), hotlinkItems++)
    pickItem(r, w->index, item->getDpIdentifier(), item->getValuePtr(), value, stime);
  putItem(ev, r.ids[w->index].second, value, stime);
  if (!ev.empty())
    event(w->ref, ev);
}

// pickItem sorts one item of a connect group into the value or the paired
// source time of registration index i.
void MmqManager::pickItem(const ConnReg &r, size_t i, const DpIdentifier &id, const Variable *v, const Variable *&value,
                          const Variable *&stime)
{
  if (r.stimes[i].first && id == r.stimes[i].second)
    stime = v;
  else
    value = v;
}

// putItem writes one event item: name, value and, when known, the source
// time (TagTime, Unix ms).
void MmqManager::putItem(mmq::Writer &ev, const std::string &name, const Variable *value, const Variable *stime)
{
  if (!value)
    return;
  ev.str(TagName, name);
  ev.value(TagValue, value);
  if (stime && stime->isA() == TIME_VAR)
  {
    const TimeVar *t = static_cast<const TimeVar *>(stime);
    ev.u64(TagTime, (uint64_t)((int64_t)t->getSeconds() * 1000 + t->getMilli()));
  }
}

void MmqManager::disconnectConn(uint64_t ref)
{
  auto it = conns.find(ref);
  if (it == conns.end())
    return;
  ConnReg &r = it->second;
  for (size_t i = 0; i < r.waits.size(); i++)
  {
    if (!r.waits[i])
      continue;
    // The same callback and ids as the connect; the framework deletes it.
    DpIdentList list;
    list.append(r.ids[i].first);
    if (r.stimes[i].first)
      list.append(r.stimes[i].second);
    if (!dpDisconnect(list, r.waits[i]))
      logLine(MMQ_LOG_WARN, "dpDisconnect message not sent for " + r.ids[i].second);
  }
  conns.erase(it);
}

void MmqManager::opQueryConnect(uint64_t reqId, const Message &m)
{
  uint64_t ref = 0;
  uint32_t flags = 0;
  m.u64(TagRef, ref);
  m.u32(TagFlags, flags);
  std::string query = m.str(TagQuery);
  if (query.empty() || queries.count(ref))
  {
    complete(reqId, MMQ_E_INVALID, nullptr, "empty query or duplicate reference");
    return;
  }
  // values=false suppresses the initial result rows; del=true lets the
  // framework delete the callback on dpQueryDisconnect (both verified on
  // 3.21 by the AC-02 probe).
  bool answer = (flags & FlagAnswer) != 0;
  QueryWait *w = new QueryWait(this, ref, reqId, answer);
  PVSSulong qid = 0;
  if (!dpQueryConnectSingle(CharString(query.c_str()), qid, answer, w, true))
  {
    complete(reqId, MMQ_E_OA, nullptr, "dpQueryConnectSingle message not sent");
    return;
  }
  queries[ref] = QueryReg{w, qid};
}

void MmqManager::onQueryAnswer(QueryWait *w, DpMsgAnswer &answer)
{
  auto it = queries.find(w->ref);
  if (it == queries.end() || it->second.wait != w)
    return;
  std::string err = answerError(answer);
  if (!err.empty())
  {
    queryDrop.push_back(w->ref);
    complete(w->reqId, MMQ_E_OA, nullptr, err);
    return;
  }
  if (complete(w->reqId, MMQ_OK) == MMQ_E_NOT_FOUND)
  {
    queryDrop.push_back(w->ref);
    return;
  }
  if (!w->wantAnswer)
    return;
  for (AnswerGroup *g = answer.getFirstGroup(); g; g = answer.getNextGroup())
    for (AnswerItem *item = g->getFirstItem(); item; item = g->getNextItem())
      if (sendTable(w->ref, item->getValuePtr(), true))
        return;
}

void MmqManager::onQueryHotlink(QueryWait *w, DpHLGroup &group)
{
  auto it = queries.find(w->ref);
  if (it == queries.end() || it->second.wait != w)
    return;
  for (DpVCItem *item = group.getFirstItem(); item; item = group.getNextItem())
    sendTable(w->ref, item->getValuePtr(), false);
}

void MmqManager::disconnectQuery(uint64_t ref)
{
  auto it = queries.find(ref);
  if (it == queries.end())
    return;
  // The framework deletes the callback object of the connect.
  if (!dpQueryDisconnect(it->second.queryId, it->second.wait))
    logLine(MMQ_LOG_WARN, "dpQueryDisconnect message not sent for reference " + std::to_string(ref));
  queries.erase(it);
}

void MmqManager::processDeferredDisconnects()
{
  while (!connDrop.empty())
  {
    disconnectConn(connDrop.front());
    connDrop.pop_front();
  }
  while (!queryDrop.empty())
  {
    disconnectQuery(queryDrop.front());
    queryDrop.pop_front();
  }
}

void MmqManager::releaseAll()
{
  std::vector<uint64_t> refs;
  for (auto &c : conns)
    refs.push_back(c.first);
  for (uint64_t r : refs)
    disconnectConn(r);
  refs.clear();
  for (auto &q : queries)
    refs.push_back(q.first);
  for (uint64_t r : refs)
    disconnectQuery(r);
}

// Name resolution is on the hot path of every write; results are cached
// and the cache is dropped on any datapoint identification change.
bool MmqManager::resolveCached(const std::string &name, DpIdentifier &id)
{
  auto it = idCache.find(name);
  if (it != idCache.end())
  {
    id = it->second;
    return true;
  }
  if (!getId(CharString(name.c_str()), id))
    return false;
  if (idCache.size() > 200000)
    idCache.clear();
  idCache.emplace(name, id);
  return true;
}

// Element type to decode a value of a config attribute other than the
// value (e.g. _original.._last_value_storage_off) with.
static DpElementType attributeElementType(const DpIdentifier &id)
{
  DpIdentification *ident = Manager::getDpIdentificationPtr();
  VariableType vt = NO_VAR;
  if (!ident || ident->getAttributeType(id, vt) != DpIdentOK)
    return DPELEMENT_NOELEMENT;
  switch (vt)
  {
    case BIT_VAR:
      return DPELEMENT_BIT;
    case INTEGER_VAR:
      return DPELEMENT_INT;
    case UINTEGER_VAR:
      return DPELEMENT_UINT;
    case FLOAT_VAR:
      return DPELEMENT_FLOAT;
    case TEXT_VAR:
      return DPELEMENT_TEXT;
    case TIME_VAR:
      return DPELEMENT_TIME;
    case BIT32_VAR:
      return DPELEMENT_32BIT;
    default:
      return DPELEMENT_NOELEMENT;
  }
}

bool MmqManager::opDpSet(uint64_t reqId, const Message &m)
{
  std::vector<const Field *> names = m.all(TagName);
  std::vector<const Field *> values = m.all(TagValue);
  if (names.empty() || names.size() != values.size())
  {
    complete(reqId, MMQ_E_INVALID, nullptr, "names and values differ");
    return true;
  }
  std::vector<std::pair<DpIdentifier, Variable *>> items;
  auto release = [&items]() {
    for (auto &i : items)
      delete i.second;
  };
  for (size_t i = 0; i < names.size(); i++)
  {
    std::string n = fieldString(names[i]);
    DpIdentifier id;
    if (!resolveCached(n, id))
    {
      release();
      // A datapoint created a moment ago may not be in the identification
      // yet; retry until the request deadline.
      std::string sys, rest;
      splitSystem(n, sys, rest);
      std::string dp = rest.substr(0, rest.find('.'));
      auto rc = recentCreates.find(dp);
      if (rc != recentCreates.end() && std::chrono::steady_clock::now() - rc->second < std::chrono::seconds(10))
        return false;
      complete(reqId, MMQ_E_NOT_FOUND, nullptr, "datapoint element not found: " + n);
      return true;
    }
    DpElementType et = DPELEMENT_NOELEMENT;
    getElementType(id, et);
    std::string err;
    Variable *v = decodeValue(*values[i], et, err);
    if (!v)
    {
      DpElementType at = attributeElementType(id);
      std::string aerr;
      if (at != DPELEMENT_NOELEMENT && at != et)
        v = decodeValue(*values[i], at, aerr);
    }
    if (!v)
    {
      release();
      complete(reqId, MMQ_E_TYPE, nullptr, n + ": " + err);
      return true;
    }
    items.emplace_back(id, v);
  }
  // All writes drained in one tick go out as one dpSet message; the answer
  // has one group per item, so every request is still confirmed on its own.
  for (auto &i : items)
  {
    setBatch.appendItem(i.first, *i.second);
    delete i.second;
  }
  setEntries.push_back(SetBatchWait::Entry{reqId, items.size()});
  return true;
}

void MmqManager::flushSetBatch()
{
  if (setEntries.empty())
    return;
  std::vector<SetBatchWait::Entry> entries;
  entries.swap(setEntries);
  setMessages++;
  setItems += setBatch.getNumberOfItems();
  SetBatchWait *w = new SetBatchWait(this, entries);
  if (!dpSet(setBatch, w, true))
  {
    for (auto &e : entries)
      complete(e.reqId, MMQ_E_OA, nullptr, "dpSet message not sent");
  }
  setBatch.clear();
}

void MmqManager::onSetBatchAnswer(const std::vector<SetBatchWait::Entry> &entries, DpMsgAnswer &answer)
{
  std::vector<AnswerGroup *> groups;
  for (AnswerGroup *g = answer.getFirstGroup(); g; g = answer.getNextGroup())
    groups.push_back(g);
  size_t total = 0;
  for (auto &e : entries)
    total += e.items;
  if (groups.size() != total)
  {
    // Not one group per item: report the combined outcome to everyone.
    std::string err = answerError(answer);
    for (auto &e : entries)
      complete(e.reqId, err.empty() ? MMQ_OK : MMQ_E_OA, nullptr, err);
    return;
  }
  size_t g = 0;
  for (auto &e : entries)
  {
    std::string err;
    for (size_t i = 0; i < e.items; i++, g++)
      if (!groups[g]->wasOk())
      {
        ErrClass *ec = groups[g]->getErrorPtr();
        if (!err.empty())
          err += "; ";
        err += ec ? std::string((const char *)ec->getErrorText()) : std::string("WinCC OA error");
      }
    complete(e.reqId, err.empty() ? MMQ_OK : MMQ_E_OA, nullptr, err);
  }
}

void MmqManager::opDpGet(uint64_t reqId, const Message &m)
{
  DpIdentList list;
  for (const Field *f : m.all(TagName))
  {
    std::string n = fieldString(f);
    DpIdentifier id;
    if (!getId(CharString(n.c_str()), id))
    {
      complete(reqId, MMQ_E_NOT_FOUND, nullptr, "datapoint element not found: " + n);
      return;
    }
    list.append(id);
  }
  if (list.getNumberOfItems() == 0)
  {
    complete(reqId, MMQ_OK);
    return;
  }
  RequestWait *w = new RequestWait(this, RequestWait::Get, reqId);
  if (!dpGet(list, w, true))
  {
    delete w;
    complete(reqId, MMQ_E_OA, nullptr, "dpGet message not sent");
  }
}

void MmqManager::onRequestAnswer(RequestWait::Kind kind, uint64_t reqId, const std::string &name, DpMsgAnswer &answer)
{
  std::string err = answerError(answer);
  if (!err.empty())
  {
    complete(reqId, MMQ_E_OA, nullptr, err);
    return;
  }
  Writer w;
  if (kind == RequestWait::Get)
    for (AnswerGroup *g = answer.getFirstGroup(); g; g = answer.getNextGroup())
      for (AnswerItem *item = g->getFirstItem(); item; item = g->getNextItem())
        w.value(TagValue, item->getValuePtr());
  if (kind == RequestWait::Create)
    recentCreates[name] = std::chrono::steady_clock::now();
  if (kind == RequestWait::TypeCreate)
    logLine(MMQ_LOG_INFO, "created datapoint type " + name);
  complete(reqId, MMQ_OK, &w);
}

int32_t MmqManager::opDpNames(const Message &m, Writer &w, std::string &err)
{
  std::string pattern = m.str(TagName), type = m.str(TagTypeName);
  DpTypeId tid = 0;
  if (!type.empty() && !getTypeId(CharString(type.c_str()), tid))
    return MMQ_OK;  // no such type: no datapoints
  DpIdentifier *arr = nullptr;
  PVSSlong count = 0;
  if (!getIdSet(CharString(pattern.c_str()), arr, count, tid))
  {
    delete[] arr;
    return MMQ_OK;
  }
  for (PVSSlong i = 0; i < count; i++)
  {
    CharString n;
    if (!getName(arr[i], n))
      continue;
    std::string s((const char *)n);
    while (!s.empty() && s.back() == '.')
      s.pop_back();
    w.str(TagName, s);
  }
  delete[] arr;
  (void)err;
  return MMQ_OK;
}

void MmqManager::opDpCreate(uint64_t reqId, const Message &m)
{
  std::string name = m.str(TagName), type = m.str(TagTypeName), sys, dp;
  splitSystem(name, sys, dp);
  if (dp.empty() || dp.find('.') != std::string::npos)
  {
    complete(reqId, MMQ_E_INVALID, nullptr, "dpCreate takes a DP name without element or trailing dot");
    return;
  }
  SystemNumType num = DpIdentification::getDefaultSystem();
  if (!sys.empty() && sys != std::string((const char *)localSystem))
  {
    if (!getSystemId(CharString(sys.c_str()), num))
    {
      complete(reqId, MMQ_E_UNAVAILABLE, nullptr, "unknown system " + sys);
      return;
    }
    if (!distUp.count(num))
    {
      complete(reqId, MMQ_E_UNAVAILABLE, nullptr, "system " + sys + " is not connected");
      return;
    }
  }
  // Type ids are per system.
  DpTypeId tid = 0;
  if (!getTypeId(CharString(type.c_str()), tid, num))
  {
    complete(reqId, MMQ_E_NOT_FOUND, nullptr, "datapoint type not found: " + (sys.empty() ? type : sys + ":" + type));
    return;
  }
  // The framework deletes the answer object.
  if (!dpCreate(CharString(dp.c_str()), tid, new RequestWait(this, RequestWait::Create, reqId, dp), num))
    complete(reqId, MMQ_E_OA, nullptr, "dpCreate message not sent");
}

void MmqManager::opDpDelete(uint64_t reqId, const Message &m)
{
  std::string name = m.str(TagName), sys, dp;
  splitSystem(name, sys, dp);
  if (dp.empty() || dp.find('.') != std::string::npos)
  {
    complete(reqId, MMQ_E_INVALID, nullptr, "dpDelete takes a DP name without element or trailing dot");
    return;
  }
  DpIdentifier id;
  if (!getId(CharString(name.c_str()), id) && !getId(CharString((name + ".").c_str()), id))
  {
    complete(reqId, MMQ_E_NOT_FOUND, nullptr, "datapoint not found: " + name);
    return;
  }
  // The framework deletes the answer object.
  if (!dpDelete(id, new RequestWait(this, RequestWait::Delete, reqId, dp)))
    complete(reqId, MMQ_E_OA, nullptr, "dpDelete message not sent");
}

// opTypeCheck checks a type's layout. With FlagCreate a missing type is
// created from the given element names and kinds (a flat structure); an
// existing type is never changed. The broker checks the layout again once
// the new type is known.
void MmqManager::opTypeCheck(uint64_t reqId, const Message &m)
{
  std::string type = m.str(TagTypeName), err;
  uint32_t flags = 0;
  m.u32(TagFlags, flags);
  DpTypeId tid = 0;
  if ((flags & FlagCreate) && !getTypeId(CharString(type.c_str()), tid))
  {
    DpTypeDefinition def(CharString(type.c_str()), DPELEMENT_RECORD);
    std::vector<const Field *> names = m.all(TagName), kinds = m.all(TagElemType);
    for (size_t i = 0; i < names.size(); i++)
    {
      uint32_t kind = ElemUnsupported;
      if (i < kinds.size() && kinds[i]->n == 4)
        memcpy(&kind, kinds[i]->p, 4);
      std::string el = fieldString(names[i]);
      DpElementType et = elementOfKind(kind);
      if (et == DPELEMENT_NOELEMENT || !def.addChild(CharString(el.c_str()), et))
      {
        complete(reqId, MMQ_E_INVALID, nullptr, "cannot create element " + type + "." + el);
        return;
      }
    }
    // The framework deletes the answer object.
    if (!dpTypeCreate(def, new RequestWait(this, RequestWait::TypeCreate, reqId, type)))
      complete(reqId, MMQ_E_OA, nullptr, "dpTypeCreate message not sent");
    return;
  }
  int32_t st = checkType(m, err);
  complete(reqId, st, nullptr, err);
}

int32_t MmqManager::checkType(const Message &m, std::string &err)
{
  std::string type = m.str(TagTypeName);
  DpTypeId tid = 0;
  if (!getTypeId(CharString(type.c_str()), tid))
  {
    err = "datapoint type not found: " + type;
    return MMQ_E_NOT_FOUND;
  }
  std::vector<const Field *> names = m.all(TagName), kinds = m.all(TagElemType);
  DpIdentification *ident = getDpIdentificationPtr();
  const DpType *t = getTypeContainerPtr()->getTypePtr(tid);
  for (size_t i = 0; i < names.size(); i++)
  {
    std::string el = fieldString(names[i]);
    DpElementId elId;
    if (!ident || ident->getElementId(tid, CharString(("." + el).c_str()), elId) != DpIdentOK)
    {
      err = "element " + type + "." + el + " not found";
      return MMQ_E_NOT_FOUND;
    }
    const DpTypeNode *node = t ? t->getTypeNodePtr(elId) : nullptr;
    uint32_t have = node ? kindOfElement(node->getElementType()) : ElemUnsupported;
    uint32_t want = 0;
    if (i < kinds.size() && kinds[i]->n == 4)
      memcpy(&want, kinds[i]->p, 4);
    if (i < kinds.size() && have != want)
    {
      err = "element " + type + "." + el + " has kind " + std::to_string(have) + ", expected " + std::to_string(want);
      return MMQ_E_TYPE;
    }
  }
  return MMQ_OK;
}

// ---------------------------------------------------------------------------
// Distributed systems and catalog changes (reported on reference 0)

void MmqManager::connectDistState()
{
  if (!getId("_DistManager.State.SystemNums:_online.._value", distId))
  {
    logLine(MMQ_LOG_INFO, "no _DistManager datapoint: only the local system is available");
    return;
  }
  DpIdentList list;
  list.append(distId);
  distWait = new DistWait(this);
  if (!dpConnect(list, distWait, true))
  {
    distWait = nullptr;
    logLine(MMQ_LOG_WARN, "cannot connect to _DistManager.State.SystemNums");
  }
}

void MmqManager::onDistState(const Variable *v)
{
  std::set<SystemNumType> now;
  if (v && v->isDynVar())
  {
    const DynVar *d = static_cast<const DynVar *>(v);
    for (unsigned int i = 0; i < d->getArrayLength(); i++)
    {
      const Variable *x = d->getAt(i);
      if (x && x->isA() == INTEGER_VAR)
        now.insert((SystemNumType) static_cast<const IntegerVar *>(x)->getValue());
      else if (x && x->isA() == UINTEGER_VAR)
        now.insert((SystemNumType) static_cast<const UIntegerVar *>(x)->getValue());
    }
  }
  Writer ev;
  for (SystemNumType s : now)
    if (!distUp.count(s) || !distKnown)
    {
      CharString name;
      if (getSystemName(s, name))
      {
        ev.str(TagSysName, std::string((const char *)name));
        ev.boolean(TagExists, true);
      }
    }
  for (SystemNumType s : distUp)
    if (!now.count(s))
    {
      CharString name;
      if (getSystemName(s, name))
      {
        ev.str(TagSysName, std::string((const char *)name));
        ev.boolean(TagExists, false);
      }
    }
  distUp = now;
  distKnown = true;
  if (!ev.empty() && handle)
    event(0, ev);
}

void MmqManager::reportDpChange(SystemNumType system, DpIdType dp, DpTypeId type)
{
  std::set<std::string> dps;
  for (auto &c : conns)
    for (auto &id : c.second.ids)
      if (id.first.getSystem() == system && (id.first.getDp() == dp || (type != 0 && id.first.getDpType() == type)))
      {
        std::string sys, rest;
        splitSystem(id.second, sys, rest);
        dps.insert((sys.empty() ? std::string((const char *)localSystem) : sys) + ":" + rest.substr(0, rest.find('.')));
      }
  if (dps.empty() || !handle)
    return;
  Writer ev;
  for (auto &n : dps)
  {
    ev.str(TagName, n);
    ev.boolean(TagExists, false);
  }
  event(0, ev);
}

void MmqManager::dpDeleted(SystemNumType system, DpIdType dp)
{
  idCache.clear();
  reportDpChange(system, dp, 0);
}

// Creations of MMQTopic datapoints are reported, on every system, so
// waiting topic subscriptions connect at once.
void MmqManager::dpCreated(SystemNumType system, const CharString &dpName, const DpIdentifier &)
{
  idCache.clear();
  std::string sysPart, dp;
  splitSystem(std::string((const char *)dpName), sysPart, dp);
  if (!handle || dp.compare(0, 9, "MMQTopic_") != 0)
    return;
  while (!dp.empty() && dp.back() == '.')
    dp.pop_back();
  CharString sysName;
  if (!getSystemName(system, sysName))
    return;
  Writer ev;
  ev.str(TagName, std::string((const char *)sysName) + ":" + dp);
  ev.boolean(TagExists, true);
  event(0, ev);
}

void MmqManager::dpTypeChanged(SystemNumType system, const DpType &changedType)
{
  idCache.clear();
  reportDpChange(system, 0, changedType.getName());
}

// ---------------------------------------------------------------------------

int MmqManager::run()
{
  long sec, usec;
  managerThread = std::this_thread::get_id();
  installExitSignals();
  connectToData(StartDpInitSysMsg::TYPE_CONTAINER | StartDpInitSysMsg::DP_IDENTIFICATION);
  while (getManagerState() == STATE_INIT && !doExit)
  {
    sec = 0;
    usec = 100000;
    dispatch(sec, usec);
  }
  if (doExit)
    return 0;
  connectToEvent();
  while (getManagerState() != STATE_RUNNING && !doExit)
  {
    sec = 0;
    usec = 100000;
    dispatch(sec, usec);
  }
  if (doExit)
    return 0;

  restoreRuntimeSignals();
  installExitSignals();
  addOnStackToHandlers();
  getSystemName(localSystem);
  localSysNum = DpIdentification::getDefaultSystem();

  if (!MmqResources::getProbeQuery().isEmpty() || !MmqResources::getProbeDpe().isEmpty())
    return runProbe();

  connectDistState();

  mmq_host host;
  memset(&host, 0, sizeof(host));
  host.struct_size = sizeof(host);
  host.abi_version = MMQ_ABI_VERSION;
  host.user = this;
  host.submit = &MmqManager::cbSubmit;
  host.log = &MmqManager::cbLog;

  // Relative paths in the broker config (SQLite.Path, key stores, ...) are
  // resolved against the project directory, not PMON's working directory.
  if (chdir((const char *)Resources::getProjDir()) != 0)
    logLine(MMQ_LOG_WARN, "cannot change to project directory " + std::string((const char *)Resources::getProjDir()) +
                              ": " + std::strerror(errno));

  CharString cfgPath = MmqResources::getBrokerConfig();
  if (cfgPath.len() && ((const char *)cfgPath)[0] != '/')
    cfgPath = Resources::getProjDir() + "/" + cfgPath;
  mmq_config cfg;
  memset(&cfg, 0, sizeof(cfg));
  cfg.struct_size = sizeof(cfg);
  cfg.abi_version = MMQ_ABI_VERSION;
  cfg.config_path = (const char *)cfgPath;
  cfg.config_path_len = (uint32_t)cfgPath.len();
  cfg.log_level = Resources::isDbgFlag(Resources::DBG_API_USR1) ? MMQ_LOG_DEBUG : MMQ_LOG_INFO;

  if (doExit)
    return 0;
  brokerStarted = true;
  int32_t rc = mmq_create(&cfg, &host, &handle);
  if (rc != MMQ_OK)
  {
    logLine(MMQ_LOG_ERROR, "mmq_create failed with " + std::to_string(rc) + " for config " + std::string((const char *)cfgPath));
    return 1;
  }
  logLine(MMQ_LOG_INFO, "broker created, local system " + std::string((const char *)localSystem));
  // Systems connected before the broker existed are announced once.
  if (!distUp.empty())
  {
    Writer ev;
    for (SystemNumType s : distUp)
    {
      CharString name;
      if (getSystemName(s, name))
      {
        ev.str(TagSysName, std::string((const char *)name));
        ev.boolean(TagExists, true);
      }
    }
    if (!ev.empty())
      event(0, ev);
  }
  mmq_start(handle);

  bool failed = false, announced = false;
  char errBuf[1024];
  auto lastStats = std::chrono::steady_clock::now();
  while (true)
  {
    sec = 0;
    usec = MmqResources::getDispatchMs() * 1000;
    dispatch(sec, usec);
    loops++;
    drainRequests();
    processDeferredDisconnects();
    drainLogs();
    uint32_t elen = 0;
    int32_t st = mmq_state(handle, errBuf, sizeof(errBuf) - 1, &elen);
    if (st == MMQ_STATE_RUNNING && !announced)
    {
      announced = true;
      logLine(MMQ_LOG_INFO, "broker running");
    }
    if (st == MMQ_STATE_FAILED)
    {
      errBuf[elen < sizeof(errBuf) - 1 ? elen : sizeof(errBuf) - 1] = 0;
      logLine(MMQ_LOG_ERROR, std::string("broker failed: ") + errBuf);
      failed = true;
      break;
    }
    if (doExit)
      break;
    if (std::chrono::steady_clock::now() - lastStats > std::chrono::seconds(MmqResources::getStatsSeconds()))
    {
      lastStats = std::chrono::steady_clock::now();
      size_t q;
      {
        std::lock_guard<std::mutex> lock(qmu);
        q = queue.size() + retry.size();
      }
      long live = mmqLiveWaits.load();
      std::vector<std::pair<const char *, unsigned long long>> st = {
          {"connects", conns.size()},
          {"queries", queries.size()},
          {"liveCallbacks", live > 0 ? (unsigned long long)live : 0},
          {"queued", q},
          {"queueHighWater", queueHighWater},
          {"overloads", overloads.load()},
          {"offThreadCalls", offThreadCalls.load()},
          {"setMessages", setMessages},
          {"setItems", setItems},
          {"hotlinkGroups", hotlinkGroups},
          {"hotlinkItems", hotlinkItems},
          {"loops", loops}};
      // Published by the broker as $SYS/winccoa/manager/<name>; the log
      // line only with -dbg USR1.
      std::string json = "{", line = "stats";
      for (auto &f : st)
      {
        std::string v = std::to_string(f.second);
        json += (json.size() > 1 ? ",\"" : "\"") + std::string(f.first) + "\":" + v;
        line += std::string(" ") + f.first + "=" + v;
      }
      json += "}";
      mmq_stats(handle, (const uint8_t *)json.data(), (uint32_t)json.size());
      if (Resources::isDbgFlag(Resources::DBG_API_USR1))
        logLine(MMQ_LOG_DEBUG, line);
    }
  }

  // Shutdown: keep dispatching so the broker can disconnect its OA
  // registrations through this thread, bounded by stopTimeoutMs.
  mmq_stop(handle, (uint32_t)MmqResources::getStopTimeoutMs());
  auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(MmqResources::getStopTimeoutMs() + 1000);
  while (mmq_state(handle, nullptr, 0, nullptr) != MMQ_STATE_STOPPED && std::chrono::steady_clock::now() < deadline)
  {
    sec = 0;
    usec = 10000;
    dispatch(sec, usec);
    drainRequests();
    processDeferredDisconnects();
    drainLogs();
  }
  releaseAll();
  {
    std::lock_guard<std::mutex> lock(qmu);
    for (auto &p : queue)
      complete(p.id, MMQ_E_STATE, nullptr, "manager stopping");
    for (auto &p : retry)
      complete(p.id, MMQ_E_STATE, nullptr, "manager stopping");
    queue.clear();
    retry.clear();
  }
  if (distWait)
  {
    DpIdentList list;
    list.append(distId);
    dpDisconnect(list, distWait);
    distWait = nullptr;
  }
  for (int i = 0; i < 20; i++)
  {
    sec = 0;
    usec = 10000;
    dispatch(sec, usec);
  }
  drainLogs();
  if (mmq_destroy(handle) != MMQ_OK)
    logLine(MMQ_LOG_WARN, "broker did not stop within stopTimeoutMs");
  drainLogs();
  return failed ? 1 : 0;
}
