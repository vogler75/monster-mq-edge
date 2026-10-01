// Backup/restore test helper (AC-18): deletes all MMQConfigs_*,
// MMQSessions_*, MMQRetained_* and MMQUsers_* datapoints. Stop WCCOAmmq first.
main()
{
  dyn_string dps = dpNames("MMQConfigs_*", "MMQConfigs");
  dynAppend(dps, dpNames("MMQSessions_*", "MMQSessions"));
  dynAppend(dps, dpNames("MMQRetained_*", "MMQRetained"));
  dynAppend(dps, dpNames("MMQUsers_*", "MMQUsers"));
  for (int i = 1; i <= dynlen(dps); i++)
    dpDelete(dpSubStr(dps[i], DPSUB_DP));
  DebugTN("mmqDeleteStores deleted", dynlen(dps));
}
