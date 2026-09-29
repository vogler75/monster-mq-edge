// Entry point of the MonsterMQ embedding manager (WCCOAmmq).
#include "MonsterMQManager.hxx"
#include "MonsterMQResources.hxx"

#include <Utf8Argv.hxx>

int main(int argc, char *argv[])
{
  // The Go runtime installed its handlers before main; keep a copy so they
  // can be restored if WinCC OA replaces them.
  MonsterMQManager::saveRuntimeSignals();

  Utf8Argv utf8Argv(argc, argv);
  MonsterMQManager::installExitSignals();

  MonsterMQResources::init(argc, argv);

  if (MonsterMQResources::getHelpDbgFlag())
  {
    MonsterMQResources::printHelpDbg();
    return 0;
  }
  if (MonsterMQResources::getHelpFlag())
  {
    MonsterMQResources::printHelp();
    return 0;
  }
  if (MonsterMQResources::getHelpReportFlag())
  {
    MonsterMQResources::printHelpReport();
    return 0;
  }

  MonsterMQManager *mgr = new MonsterMQManager;
  int rc = mgr->run();
  MonsterMQManager::exit(rc);
  return rc;
}
