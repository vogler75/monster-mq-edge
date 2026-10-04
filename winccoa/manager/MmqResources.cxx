#include <MmqResources.hxx>
#include <ErrHdl.hxx>

CharString MmqResources::brokerConfig = "config/mmq.yaml";
PVSSlong MmqResources::dispatchMs = 2;
PVSSlong MmqResources::tickBudget = 256;
PVSSlong MmqResources::tickBudgetMs = 5;
PVSSlong MmqResources::queueCapacity = 4096;
PVSSlong MmqResources::stopTimeoutMs = 10000;
PVSSlong MmqResources::statsSeconds = 60;
CharString MmqResources::probeQuery = "";
CharString MmqResources::probeDpe = "";
CharString MmqResources::probeSet = "";

void MmqResources::init(int &argc, char *argv[])
{
  begin(argc, argv);
  while (readSection() || generalSection())
    ;
  end(argc, argv);
}

PVSSboolean MmqResources::readSection()
{
  if (!isSection("mmq"))
    return PVSS_FALSE;

  getNextEntry();
  while ((getCfgState() != CFG_SECT_START) && (getCfgState() != CFG_EOF))
  {
    if (!getKeyWord().icmp("brokerConfig"))
      getCfgStream() >> brokerConfig;
    else if (!getKeyWord().icmp("dispatchMs"))
      getCfgStream() >> dispatchMs;
    else if (!getKeyWord().icmp("tickBudget"))
      getCfgStream() >> tickBudget;
    else if (!getKeyWord().icmp("tickBudgetMs"))
      getCfgStream() >> tickBudgetMs;
    else if (!getKeyWord().icmp("queueCapacity"))
      getCfgStream() >> queueCapacity;
    else if (!getKeyWord().icmp("stopTimeoutMs"))
      getCfgStream() >> stopTimeoutMs;
    else if (!getKeyWord().icmp("statsSeconds"))
      getCfgStream() >> statsSeconds;
    else if (!getKeyWord().icmp("probeQuery"))
      getCfgStream() >> probeQuery;
    else if (!getKeyWord().icmp("probeDpe"))
      getCfgStream() >> probeDpe;
    else if (!getKeyWord().icmp("probeSet"))
      getCfgStream() >> probeSet;
    else if (!readGeneralKeyWords())
      unknownKeyWordError();
    getNextEntry();
  }
  if (dispatchMs < 1)
    dispatchMs = 1;
  if (tickBudget < 1)
    tickBudget = 1;
  if (statsSeconds < 1)
    statsSeconds = 1;
  if (queueCapacity < 16)
    queueCapacity = 16;
  return getCfgState() != CFG_EOF;
}
