@echo off
setlocal
set EXE=%~dp0..\bin\devin-call.exe
set CAP=%~dp0..\captures-relay\capture-018-request.txt

for %%C in (
  "-drop 15"
  "-drop 28"
  "-drop-md 31"
  "-drop-md 28"
  "-drop 28 -drop-md 28,30,31"
  "-drop 15,28 -drop-md 28,30,31"
) do (
  echo === %%C ===
  "%EXE%" -replay "%CAP%" -quiet -thinking=false %%~C 2>&1 | findstr /c:"[error]" /c:"[stop]" /c:"[usage]" /c:"--- done" /c:"FAILED"
  echo.
)
endlocal
