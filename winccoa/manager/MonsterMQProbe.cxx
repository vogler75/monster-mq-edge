// SDK probe (AC-02): records how the selected WinCC OA version delivers
// dpQueryConnectSingle answers/hotlinks and who owns callback objects on
// disconnect. Enabled by probeQuery / probeDpe in the [monstermq] section.
#include "MonsterMQManager.hxx"
#include "MonsterMQResources.hxx"

#include <iostream>
#include <sstream>

#include <AnswerGroup.hxx>
#include <AnswerItem.hxx>
#include <AnyTypeVar.hxx>
#include <DpIdentList.hxx>
#include <DpVCItem.hxx>
#include <DynVar.hxx>
#include <ErrClass.hxx>
#include <ErrHdl.hxx>
#include <FloatVar.hxx>

static void probeLog(const std::string &msg)
{
  std::cerr << "PROBE: " << msg << std::endl;
  ErrHdl::error(ErrClass::PRIO_INFO, ErrClass::ERR_CONTROL, ErrClass::UNEXPECTEDSTATE, "MonsterMQ", "probe",
                CharString(("PROBE: " + msg).c_str()));
}

static void describe(const Variable *v, int depth, std::ostringstream &out)
{
  if (!v)
  {
    out << "null";
    return;
  }
  out << "isA=0x" << std::hex << (unsigned long)v->isA() << std::dec;
  if (v->isA() == ANYTYPE_VAR)
  {
    out << "{";
    describe(static_cast<const AnyTypeVar *>(v)->getVar(), depth, out);
    out << "}";
    return;
  }
  if (v->isDynVar())
  {
    const DynVar *d = static_cast<const DynVar *>(v);
    out << " len=" << d->getArrayLength();
    if (depth < 2)
    {
      out << " [";
      for (unsigned int i = 0; i < d->getArrayLength() && i < 3; i++)
      {
        if (i)
          out << ", ";
        describe(d->getAt(i), depth + 1, out);
      }
      out << "]";
    }
    return;
  }
  mmq::Writer w;
  w.value(mmq::TagValue, v);
  if (w.size() > 5)
    out << " kind=" << (int)w.data()[5] << " bytes=" << (w.size() - 6);
}

class ProbeWait : public HotLinkWaitForAnswer
{
  public:
    explicit ProbeWait(const std::string &l) : label(l) { mmqLiveWaits++; }
    ~ProbeWait() override
    {
      mmqLiveWaits--;
      probeLog(label + ": callback object destroyed");
    }
    void hotLinkCallBack(DpMsgAnswer &answer) override
    {
      int groups = 0;
      for (AnswerGroup *g = answer.getFirstGroup(); g; g = answer.getNextGroup())
      {
        groups++;
        std::ostringstream out;
        out << label << ": answer group " << groups << " ok=" << (g->wasOk() ? 1 : 0);
        if (!g->wasOk() && g->getErrorPtr())
          out << " error=" << (const char *)g->getErrorPtr()->getErrorText();
        int items = 0;
        for (AnswerItem *it = g->getFirstItem(); it; it = g->getNextItem())
        {
          CharString n;
          Manager::getName(it->getDpIdentifier(), n);
          out << "\n  item " << ++items << " dpe=" << (const char *)n << " value ";
          describe(it->getValuePtr(), 0, out);
        }
        probeLog(out.str());
      }
      if (!groups)
        probeLog(label + ": answer without groups");
    }

  protected:
    void hotLinkCallBack(DpHLGroup &group) override
    {
      std::ostringstream out;
      out << label << ": hotlink identifier=" << group.getIdentifier();
      int items = 0;
      for (DpVCItem *it = group.getFirstItem(); it; it = group.getNextItem())
      {
        CharString n;
        Manager::getName(it->getDpIdentifier(), n);
        out << "\n  item " << ++items << " dpe=" << (const char *)n << " value ";
        describe(it->getValuePtr(), 0, out);
      }
      probeLog(out.str());
    }

  private:
    std::string label;
};

// Dispatches for ms and, when a writable probe DPE is known, changes it
// once per second so hotlinks are produced.
static DpIdentifier probeSetId;
static bool probeSetValid = false;
static double probeSetValue = 1000.0;

static void pump(MonsterMQManager *m, int ms)
{
  (void)m;
  auto end = std::chrono::steady_clock::now() + std::chrono::milliseconds(ms);
  auto nextSet = std::chrono::steady_clock::now() + std::chrono::milliseconds(500);
  while (std::chrono::steady_clock::now() < end)
  {
    long sec = 0, usec = 20000;
    Manager::dispatch(sec, usec);
    if (probeSetValid && std::chrono::steady_clock::now() >= nextSet)
    {
      nextSet += std::chrono::seconds(1);
      probeSetValue += 1.0;
      Manager::dpSet(probeSetId, FloatVar(probeSetValue));
    }
  }
}

int MonsterMQManager::runProbe()
{
  probeLog("local system " + std::string((const char *)localSystem) + " number " + std::to_string((int)localSysNum));
  DpIdentifier dist;
  probeLog(std::string("_DistManager.State.SystemNums exists: ") +
           (getId("_DistManager.State.SystemNums:_online.._value", dist) ? "yes" : "no"));

  const CharString &setName = MonsterMQResources::getProbeSet();
  if (!setName.isEmpty())
  {
    probeSetValid = getId(setName, probeSetId);
    probeLog(std::string("probe writes ") + (const char *)setName + (probeSetValid ? " once per second" : ": not found"));
  }

  const CharString &query = MonsterMQResources::getProbeQuery();
  if (!query.isEmpty())
  {
    for (int variant = 0; variant < 3; variant++)
    {
      bool values = variant != 1;
      bool del = variant == 2;
      std::string label = std::string("query(values=") + (values ? "true" : "false") + ", del=" + (del ? "true" : "false") + ")";
      ProbeWait *w = new ProbeWait(label);
      PVSSulong qid = 0;
      bool sent = dpQueryConnectSingle(query, qid, values, w, del);
      probeLog(label + " sent=" + (sent ? "1" : "0") + " queryId=" + std::to_string((unsigned long)qid) +
               " (change matching values now; listening 15 s)");
      if (!sent)
      {
        delete w;
        continue;
      }
      pump(this, 8000);
      long before = mmqLiveWaits.load();
      bool dsent = dpQueryDisconnect(qid, w);
      pump(this, 3000);
      probeLog(label + " disconnect sent=" + (dsent ? "1" : "0") + " callback deleted by framework: " +
               (mmqLiveWaits.load() < before ? "yes" : "no"));
    }
  }

  const CharString &dpe = MonsterMQResources::getProbeDpe();
  if (!dpe.isEmpty())
  {
    DpIdentifier id;
    if (!getId(dpe, id))
    {
      probeLog("probe DPE not found: " + std::string((const char *)dpe));
      return 1;
    }
    DpIdentList list;
    list.append(id);

    ProbeWait *a = new ProbeWait("connect(list, del=false)");
    probeLog(std::string("connect(list, del=false) sent=") + (dpConnect(list, a, false) ? "1" : "0") + " (listening 8 s)");
    pump(this, 8000);
    long before = mmqLiveWaits.load();
    dpDisconnect(list, a);
    pump(this, 3000);
    probeLog(std::string("connect(list, del=false): list disconnect deleted callback: ") + (mmqLiveWaits.load() < before ? "yes" : "no"));

    ProbeWait *b = new ProbeWait("connect(single, del=false)");
    probeLog(std::string("connect(single, del=false) sent=") + (dpConnect(id, b, false) ? "1" : "0"));
    pump(this, 3000);
    before = mmqLiveWaits.load();
    dpDisconnect(id, b);
    pump(this, 3000);
    probeLog(std::string("connect(single, del=false): single disconnect deleted callback: ") + (mmqLiveWaits.load() < before ? "yes" : "no"));

    ProbeWait *c = new ProbeWait("connect(list, del=true)");
    before = mmqLiveWaits.load();
    probeLog(std::string("connect(list, del=true) sent=") + (dpConnect(list, c, true) ? "1" : "0"));
    pump(this, 3000);
    bool gone = mmqLiveWaits.load() < before;
    probeLog(std::string("connect(list, del=true): callback deleted after the answer: ") + (gone ? "yes" : "no"));
    if (!gone)
    {
      dpDisconnect(list, c);
      pump(this, 3000);
    }
    probeLog("remaining live callback objects: " + std::to_string(mmqLiveWaits.load()));
  }
  return 0;
}
