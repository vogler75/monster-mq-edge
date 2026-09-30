// Backup/restore test helper (AC-18): deletes all MMQConfigs_*,
// MMQSessions_* and MMQRetained_* datapoints. Stop WCCOAmmq first.
main()
{
  dyn_string dps = dpNames("MMQConfigs_*", "MMQConfigs");
  dynAppend(dps, dpNames("MMQSessions_*", "MMQSessions"));
  dynAppend(dps, dpNames("MMQRetained_*", "MMQRetained"));
  for (int i = 1; i <= dynlen(dps); i++)
    dpDelete(dpSubStr(dps[i], DPSUB_DP));
  DebugTN("mmqDeleteStores deleted", dynlen(dps));
}
