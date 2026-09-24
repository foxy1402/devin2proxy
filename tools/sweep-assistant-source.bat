@echo off
REM Sweeps the ChatMessageSource value used for an assistant turn, to find the
REM one the backend accepts. The plugin schema only documented 0,1,2,4.
cd /d "%~dp0.."

for %%i in (0 1 2 4 5 6) do (
  echo --- asrc=%%i ---
  bin\devin-call.exe -quiet -asrc %%i -turns "user:My favourite colour is teal. Acknowledge in one word.;assistant:Noted.;user:What colour did I just tell you? One word." > bin\asrc-%%i.txt 2>&1
  findstr /c:"[text]" /c:"[error]" bin\asrc-%%i.txt
)
