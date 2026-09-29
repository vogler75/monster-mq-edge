// Creates the datapoint types of the MonsterMQ native stores
// (dev/plans/spec-winccoa-native.md section 5). Run once per project, e.g.
//   WCCOActrl -proj <project> mmqCreateTypes.ctl
// Existing types are left unchanged; the broker checks their layout at
// startup and refuses to run with a different one.

int createType(string name, dyn_string elements, dyn_int types)
{
  if ((dynlen(dpTypes(name)) > 0))
  {
    DebugTN("mmqCreateTypes: type exists, not changed", name);
    return 0;
  }
  dyn_dyn_string names;
  dyn_dyn_int elTypes;
  names[1] = makeDynString(name, "");
  elTypes[1] = makeDynInt(DPEL_STRUCT);
  for (int i = 1; i <= dynlen(elements); i++)
  {
    names[i + 1] = makeDynString("", elements[i]);
    elTypes[i + 1] = makeDynInt(0, types[i]);
  }
  int rc = dpTypeCreate(names, elTypes);
  DebugTN("mmqCreateTypes: created", name, rc);
  return rc;
}

main()
{
  createType("MMQConfigs", makeDynString("config", "type", "updated"),
             makeDynInt(DPEL_STRING, DPEL_STRING, DPEL_TIME));
  createType("MMQSessions", makeDynString("session", "subs", "connected", "nodeId", "updated"),
             makeDynInt(DPEL_STRING, DPEL_STRING, DPEL_BOOL, DPEL_STRING, DPEL_TIME));
}
