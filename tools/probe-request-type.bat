@echo off
REM Sweeps the remaining request_type values looking for one that skips the
REM reasoning phase. If one exists it would cut autocomplete cost and latency
REM dramatically, so it is worth the few calls. 0, 1 and 5 were checked earlier
REM and all produced thinking.
cd /d "%~dp0.."

for %%i in (2 3 4 6 7) do (
  echo --- reqtype=%%i ---
  bin\devin-call.exe -quiet -thinking false -reqtype %%i -prompt "Reply with exactly the word: pong" > bin\rt-%%i.txt 2>&1
  findstr /c:"thinking (" /c:"[text]" /c:"[error]" bin\rt-%%i.txt
)
