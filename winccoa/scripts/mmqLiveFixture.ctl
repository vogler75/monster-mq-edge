// Creates the datapoints used by the live acceptance tests
// (test/integration/winccoa_live_test.go): type MMQLiveTest and the
// datapoints MMQLive1, MMQLive2 and MMQLiveScalar. Safe to run repeatedly.

main()
{
  if (!(dynlen(dpTypes("MMQLiveTest")) > 0))
  {
    dyn_dyn_string n;
    dyn_dyn_int t;
    n[1] = makeDynString("MMQLiveTest", "");  t[1] = makeDynInt(DPEL_STRUCT);
    n[2] = makeDynString("", "speed");        t[2] = makeDynInt(0, DPEL_FLOAT);
    n[3] = makeDynString("", "running");      t[3] = makeDynInt(0, DPEL_BOOL);
    n[4] = makeDynString("", "name");         t[4] = makeDynInt(0, DPEL_STRING);
    n[5] = makeDynString("", "count");        t[5] = makeDynInt(0, DPEL_INT);
    n[6] = makeDynString("", "unsigned");     t[6] = makeDynInt(0, DPEL_UINT);
    n[7] = makeDynString("", "ts");           t[7] = makeDynInt(0, DPEL_TIME);
    n[8] = makeDynString("", "nested");       t[8] = makeDynInt(0, DPEL_STRUCT);
    n[9] = makeDynString("", "", "a");        t[9] = makeDynInt(0, 0, DPEL_FLOAT);
    DebugTN("dpTypeCreate MMQLiveTest", dpTypeCreate(n, t));
  }
  if (!(dynlen(dpTypes("MMQLiveScalar")) > 0))
  {
    dyn_dyn_string n;
    dyn_dyn_int t;
    n[1] = makeDynString("MMQLiveScalar");
    t[1] = makeDynInt(DPEL_FLOAT);
    DebugTN("dpTypeCreate MMQLiveScalar", dpTypeCreate(n, t));
  }
  if (!dpExists("MMQLive1")) dpCreate("MMQLive1", "MMQLiveTest");
  if (!dpExists("MMQLive2")) dpCreate("MMQLive2", "MMQLiveTest");
  if (!dpExists("MMQLiveScalar")) dpCreate("MMQLiveScalar", "MMQLiveScalar");
  delay(1);
  dpSetWait("MMQLive1.speed", 1.5, "MMQLive1.name", "init", "MMQLiveScalar.", 7.0);
  DebugTN("mmqLiveFixture done");
}
