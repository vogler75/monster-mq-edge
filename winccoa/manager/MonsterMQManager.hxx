// WinCC OA API manager that embeds the MonsterMQ Edge broker (Go c-archive)
// and executes its WinCC OA requests on the manager thread
// (dev/plans/plan-winccoa-broker-embedded-manager.md, spec-winccoa-native.md).
#ifndef MONSTERMQ_MANAGER_HXX
#define MONSTERMQ_MANAGER_HXX

#include <atomic>
#include <chrono>
#include <cstdint>
#include <deque>
#include <map>
#include <mutex>
#include <set>
#include <unordered_map>
#include <string>
#include <thread>
#include <utility>
#include <vector>

#include <DpHLGroup.hxx>
#include <DpIdentifier.hxx>
#include <DpMsgAnswer.hxx>
#include <HotLinkWaitForAnswer.hxx>
#include <Manager.hxx>
#include <WaitForAnswer.hxx>
#include <DpIdValueList.hxx>

#include "MmqTlv.hxx"
#include "monstermq.h"

class MonsterMQManager;

// Counts live callback objects so tests and the probe can check ownership
// (a registration callback must be released exactly once).
extern std::atomic<long> mmqLiveWaits;

typedef std::vector<std::pair<DpIdentifier, std::string>> NamedIds;

// Callback of one single-element dpConnect. A connect request of up to 100
// names creates one registration per name: a list dpConnect would deliver
// every element of the list whenever any of them changes (verified on
// 3.21), republishing unchanged values.
class ConnectWait : public HotLinkWaitForAnswer
{
  public:
    ConnectWait(MonsterMQManager *m, uint64_t ref, size_t index);
    ~ConnectWait() override;
    void hotLinkCallBack(DpMsgAnswer &answer) override;

    uint64_t ref;
    size_t index;

  protected:
    void hotLinkCallBack(DpHLGroup &group) override;

  private:
    MonsterMQManager *mgr;
};

class QueryWait : public HotLinkWaitForAnswer
{
  public:
    QueryWait(MonsterMQManager *m, uint64_t ref, uint64_t reqId, bool answer);
    ~QueryWait() override;
    void hotLinkCallBack(DpMsgAnswer &answer) override;

    uint64_t ref;
    uint64_t reqId;
    bool wantAnswer;

  protected:
    void hotLinkCallBack(DpHLGroup &group) override;

  private:
    MonsterMQManager *mgr;
};

// Answer handler of one batched dpSet that carries the items of several
// requests; answer groups follow the item order. Deleted by the framework.
class SetBatchWait : public WaitForAnswer
{
  public:
    struct Entry
    {
      uint64_t reqId;
      size_t items;
    };
    SetBatchWait(MonsterMQManager *m, std::vector<Entry> entries);
    ~SetBatchWait() override;
    void callBack(DpMsgAnswer &answer) override;

  private:
    MonsterMQManager *mgr;
    std::vector<Entry> entries;
};

// One-shot answer handler (set, get, create, delete). Deleted by the
// framework after the answer.
class RequestWait : public WaitForAnswer
{
  public:
    enum Kind { Set, Get, Create, Delete };
    RequestWait(MonsterMQManager *m, Kind k, uint64_t reqId, const std::string &name = std::string());
    ~RequestWait() override;
    void callBack(DpMsgAnswer &answer) override;

  private:
    MonsterMQManager *mgr;
    Kind kind;
    uint64_t reqId;
    std::string name;
};

class DistWait : public HotLinkWaitForAnswer
{
  public:
    explicit DistWait(MonsterMQManager *m);
    ~DistWait() override;
    void hotLinkCallBack(DpMsgAnswer &answer) override;

  protected:
    void hotLinkCallBack(DpHLGroup &group) override;

  private:
    MonsterMQManager *mgr;
};

class MonsterMQManager : public Manager
{
  public:
    MonsterMQManager();

    // Runs the manager; returns the process exit code.
    int run();

    // Host callbacks, called on Go runtime threads: copy and enqueue only.
    static int32_t cbSubmit(void *user, uint64_t id, int64_t deadline, const uint8_t *data, uint32_t len);
    static void cbLog(void *user, int32_t level, const char *msg, uint32_t len);

    // Manager-thread handlers used by the callback objects.
    void onConnectAnswer(ConnectWait *w, DpMsgAnswer &answer);
    void onConnectHotlink(ConnectWait *w, DpHLGroup &group);
    void onQueryAnswer(QueryWait *w, DpMsgAnswer &answer);
    void onQueryHotlink(QueryWait *w, DpHLGroup &group);
    void onRequestAnswer(RequestWait::Kind kind, uint64_t reqId, const std::string &name, DpMsgAnswer &answer);
    void onDistState(const Variable *v);
    void onSetBatchAnswer(const std::vector<SetBatchWait::Entry> &entries, DpMsgAnswer &answer);

    // Saves the Go runtime's handlers for the synchronous and runtime
    // signals before WinCC OA installs its own (called first in main).
    static void saveRuntimeSignals();

    // Installs the raw SIGINT/SIGTERM handler (see requestExit).
    static void installExitSignals();
    static void requestExit();

  protected:
    void signalHandler(int sig) override;
    void dpDeleted(SystemNumType system, DpIdType dp) override;
    void dpCreated(SystemNumType system, const CharString &dpName, const DpIdentifier &dpId) override;
    void dpTypeChanged(SystemNumType system, const DpType &changedType) override;

  private:
    struct Pending
    {
      uint64_t id;
      int64_t deadline;
      std::vector<uint8_t> data;
    };
    struct ConnReg
    {
      uint64_t reqId;
      bool wantAnswer;
      NamedIds ids;
      std::vector<ConnectWait *> waits;  // parallel to ids; nullptr = not registered
      size_t pending;                    // answers still expected
      std::string err;
      mmq::Writer answerEv;
      bool completed;
    };
    struct QueryReg
    {
      QueryWait *wait;
      PVSSulong queryId;
    };

    static std::atomic<bool> doExit;
    static std::atomic<bool> brokerStarted;

    uint64_t handle = 0;
    CharString localSystem;
    SystemNumType localSysNum = 0;
    std::set<SystemNumType> distUp;
    bool distKnown = false;
    DpIdentifier distId;
    DistWait *distWait = nullptr;

    std::mutex qmu;
    std::deque<Pending> queue;
    std::deque<Pending> retry;
    size_t queueCap = 4096;
    size_t queueHighWater = 0;
    std::atomic<unsigned long> overloads{0};
    std::atomic<unsigned long> offThreadCalls{0};
    std::thread::id managerThread;
    unsigned long setMessages = 0, setItems = 0, hotlinkGroups = 0, hotlinkItems = 0, loops = 0;

    std::mutex logmu;
    std::deque<std::pair<int32_t, std::string>> logs;
    unsigned long logsDropped = 0;

    std::map<uint64_t, ConnReg> conns;
    std::map<uint64_t, QueryReg> queries;
    std::deque<uint64_t> connDrop, queryDrop;
    std::map<std::string, std::chrono::steady_clock::time_point> recentCreates;

    int32_t complete(uint64_t reqId, int32_t status, const mmq::Writer *w = nullptr, const std::string &err = std::string());
    void event(uint64_t ref, const mmq::Writer &w);
    void drainRequests();
    bool process(const Pending &p);  // false = retry later
    void processDeferredDisconnects();
    void disconnectConn(uint64_t ref);
    void finishConnect(uint64_t ref);
    void disconnectQuery(uint64_t ref);
    void releaseAll();
    void drainLogs();
    void logLine(int32_t level, const std::string &msg);

    int32_t opResolve(const mmq::Message &m, mmq::Writer &w, std::string &err);
    void opDpConnect(uint64_t reqId, const mmq::Message &m);
    void opQueryConnect(uint64_t reqId, const mmq::Message &m);
    // Adds a write request to the pending batch; false = retry later.
    bool opDpSet(uint64_t reqId, const mmq::Message &m);
    void flushSetBatch();
    bool resolveCached(const std::string &name, DpIdentifier &id);

    DpIdValueList setBatch;
    std::vector<SetBatchWait::Entry> setEntries;
    std::unordered_map<std::string, DpIdentifier> idCache;
    void opDpGet(uint64_t reqId, const mmq::Message &m);
    int32_t opDpNames(const mmq::Message &m, mmq::Writer &w, std::string &err);
    void opDpCreate(uint64_t reqId, const mmq::Message &m);
    void opDpDelete(uint64_t reqId, const mmq::Message &m);
    int32_t opTypeCheck(const mmq::Message &m, std::string &err);

    void connectDistState();
    void reportDpChange(SystemNumType system, DpIdType dp, DpTypeId type);
    static std::string answerError(DpMsgAnswer &answer);
    static void restoreRuntimeSignals();
    static void addOnStackToHandlers();
    int runProbe();
};

#endif
