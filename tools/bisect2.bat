@echo off
setlocal
set EXE=%~dp0..\bin\devin-call.exe
set CAP=%~dp0..\captures-relay\capture-018-request.txt

for %%C in (
  "-drop 10"
  "-drop 2"
  "-drop 8"
  "-drop 21"
  "-drop 16"
  "-drop 15,28 -drop-md 28,30,31 -drop 10"
) do (
  echo === %%C ===
  "%EXE%" -replay "%CAP%" -quiet -thinking=false %%~C 2>&1 | findstr /c:"[error]" /c:"[stop]" /c:"--- done" /c:"FAILED"
  echo.
)
endlocal
