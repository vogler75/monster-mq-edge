// Configuration of the MonsterMQ embedding manager: config section
// [monstermq] (or [monstermq_<num>]) of the WinCC OA project config.
#ifndef MONSTERMQ_RESOURCES_HXX
#define MONSTERMQ_RESOURCES_HXX

#include <Resources.hxx>

class MonsterMQResources : public Resources
{
  public:
    static void init(int &argc, char *argv[]);
    static PVSSboolean readSection();

    // Path of the broker config.yaml (bootstrap configuration).
    static const CharString &getBrokerConfig() { return brokerConfig; }
    // Maximum wait of one dispatch() call in milliseconds.
    static PVSSlong getDispatchMs() { return dispatchMs; }
    // Requests drained per dispatch tick and time budget in milliseconds.
    static PVSSlong getTickBudget() { return tickBudget; }
    static PVSSlong getTickBudgetMs() { return tickBudgetMs; }
    // Host request queue capacity.
    static PVSSlong getQueueCapacity() { return queueCapacity; }
    // Bounded time for the broker to stop, in milliseconds.
    static PVSSlong getStopTimeoutMs() { return stopTimeoutMs; }
    // Interval of the statistics log line (logged with -dbg USR1).
    static PVSSlong getStatsSeconds() { return statsSeconds; }
    // SDK probe (AC-02): query and DPE used by -probe runs; empty = normal run.
    static const CharString &getProbeQuery() { return probeQuery; }
    static const CharString &getProbeDpe() { return probeDpe; }
    // Float DPE (with :_original.._value) the probe changes to produce hotlinks.
    static const CharString &getProbeSet() { return probeSet; }

  private:
    static CharString brokerConfig;
    static PVSSlong dispatchMs;
    static PVSSlong tickBudget;
    static PVSSlong tickBudgetMs;
    static PVSSlong queueCapacity;
    static PVSSlong stopTimeoutMs;
    static PVSSlong statsSeconds;
    static CharString probeQuery;
    static CharString probeDpe;
    static CharString probeSet;
};

#endif
