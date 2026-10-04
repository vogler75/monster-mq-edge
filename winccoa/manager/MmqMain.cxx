// Entry point of the MMQ embedding manager (WCCOAmmq).
#include "MmqManager.hxx"
#include "MmqResources.hxx"

#include <Utf8Argv.hxx>

int main(int argc, char *argv[])
{
  // The Go runtime installed its handlers before main; keep a copy so they
  // can be restored if WinCC OA replaces them.
  MmqManager::saveRuntimeSignals();

  Utf8Argv utf8Argv(argc, argv);
  MmqManager::installExitSignals();

  MmqResources::init(argc, argv);

  if (MmqResources::getHelpDbgFlag())
  {
    MmqResources::printHelpDbg();
    return 0;
  }
  if (MmqResources::getHelpFlag())
  {
    MmqResources::printHelp();
    return 0;
  }
  if (MmqResources::getHelpReportFlag())
  {
    MmqResources::printHelpReport();
    return 0;
  }

  MmqManager *mgr = new MmqManager;
  int rc = mgr->run();
  MmqManager::exit(rc);
  return rc;
}
