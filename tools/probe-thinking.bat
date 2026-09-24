@echo off
REM Probes whether the model's reasoning phase can be suppressed, which is what
REM makes an autocomplete (FIM) request slow. Compares request_type and
REM planner_mode values and reports thinking bytes and wall-clock latency.
cd /d "%~dp0.."

echo === reqtype=5 planner=1 (what the CLI sends) ===
bin\devin-call.exe -quiet -thinking false -reqtype 5 -planner 1 -prompt "Reply with exactly the word: pong" 2>&1 | findstr /c:"thinking (" /c:"[text]" /c:"[error]"

echo === reqtype=5 planner=0 (planner omitted) ===
bin\devin-call.exe -quiet -thinking false -reqtype 5 -planner 0 -prompt "Reply with exactly the word: pong" 2>&1 | findstr /c:"thinking (" /c:"[text]" /c:"[error]"

echo === reqtype=0 (request_type omitted) ===
bin\devin-call.exe -quiet -thinking false -reqtype 0 -planner 1 -prompt "Reply with exactly the word: pong" 2>&1 | findstr /c:"thinking (" /c:"[text]" /c:"[error]"

echo === reqtype=1 ===
bin\devin-call.exe -quiet -thinking false -reqtype 1 -planner 1 -prompt "Reply with exactly the word: pong" 2>&1 | findstr /c:"thinking (" /c:"[text]" /c:"[error]"
