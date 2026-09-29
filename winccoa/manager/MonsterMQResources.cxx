#include <MonsterMQResources.hxx>
#include <ErrHdl.hxx>

CharString MonsterMQResources::brokerConfig = "config/monstermq.yaml";
PVSSlong MonsterMQResources::dispatchMs = 2;
PVSSlong MonsterMQResources::tickBudget = 256;
PVSSlong MonsterMQResources::tickBudgetMs = 5;
PVSSlong MonsterMQResources::queueCapacity = 4096;
PVSSlong MonsterMQResources::stopTimeoutMs = 10000;
PVSSlong MonsterMQResources::statsSeconds = 60;
CharString MonsterMQResources::probeQuery = "";
CharString MonsterMQResources::probeDpe = "";
CharString MonsterMQResources::probeSet = "";

void MonsterMQResources::init(int &argc, char *argv[])
{
  begin(argc, argv);
  while (readSection() || generalSection())
    ;
  end(argc, argv);
}

PVSSboolean MonsterMQResources::readSection()
{
  if (!isSection("monstermq"))
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
