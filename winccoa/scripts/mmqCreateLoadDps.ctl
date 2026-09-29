// Creates the datapoints used by cmd/mmqload (AC-34): type MMQLoad with a
// float element "value" and <count> datapoints MMQLoad00000...
const int count = 5000;

main()
{
  if (!(dynlen(dpTypes("MMQLoad")) > 0))
  {
    dyn_dyn_string names;
    dyn_dyn_int types;
    names[1] = makeDynString("MMQLoad", "");
    types[1] = makeDynInt(DPEL_STRUCT);
    names[2] = makeDynString("", "value");
    types[2] = makeDynInt(0, DPEL_FLOAT);
    DebugTN("dpTypeCreate MMQLoad", dpTypeCreate(names, types));
  }
  for (int i = 0; i < count; i++)
  {
    string dp;
    sprintf(dp, "MMQLoad%05d", i);
    if (!dpExists(dp))
      dpCreate(dp, "MMQLoad");
  }
  DebugTN("mmqCreateLoadDps done", count);
}
