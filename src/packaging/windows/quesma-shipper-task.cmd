@echo off
rem Keep the replaceable program under one stable Task Scheduler process across updates and crashes.
set "SHIPPER_SUPERVISED=1"
:restart
"%~dp0quesma-shipper.exe" run
%SystemRoot%\System32\timeout.exe /t 30 /nobreak >nul
goto restart
