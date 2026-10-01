// Live test helper (AC-23): deletes MMQLive2; mmqLiveFixture.ctl recreates it.
main()
{
  if (dpExists("MMQLive2"))
    DebugTN("dpDelete MMQLive2", dpDelete("MMQLive2"));
}
