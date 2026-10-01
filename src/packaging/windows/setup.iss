; Personal installs self-update; all-users installs are owned by administrators or MDM.
; Both scopes run collectors in each interactive user's security context.

#ifndef ReleaseVersion
  #error ReleaseVersion is required
#endif
#ifndef Architecture
  #error Architecture is required
#endif
#ifndef BinaryPath
  #error BinaryPath is required
#endif
#ifndef SupervisorPath
  #error SupervisorPath is required
#endif
#ifndef FileVersion
  #error FileVersion is required
#endif
#ifndef OutputDir
  #define OutputDir "."
#endif

#if Architecture == "amd64"
  #define ArchitectureConstraint "x64compatible and not arm64"
#elif Architecture == "arm64"
  #define ArchitectureConstraint "arm64"
#else
  #error Architecture must be amd64 or arm64
#endif

[Setup]
AppId={{C65D423E-3F9B-49AF-A68F-08C88093F01F}
AppName=Quesma Shipper
AppVersion={#ReleaseVersion}
AppVerName=Quesma Shipper {#ReleaseVersion}
AppPublisher=Quesma Poland Sp. z o.o.
AppPublisherURL=https://quesma.com/
AppSupportURL=https://github.com/QuesmaOrg/quesma-shipper/issues
AppUpdatesURL=https://github.com/QuesmaOrg/quesma-shipper
DefaultDirName={code:DefaultInstallDir}
DefaultGroupName=Quesma Shipper
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
UsePreviousPrivileges=yes
; Preserve the uninstall registration used by existing personal installations.
ArchitecturesInstallIn64BitMode=
ArchitecturesAllowed={#ArchitectureConstraint}
MinVersion=10.0.17763
OutputDir={#OutputDir}
OutputBaseFilename=QuesmaShipperSetup-{#Architecture}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
ChangesEnvironment=yes
CloseApplications=yes
RestartApplications=no
SetupLogging=yes
UninstallLogging=yes
UninstallDisplayName=Quesma Shipper
UninstallDisplayIcon={app}\quesma-shipper.exe
VersionInfoCompany=Quesma Poland Sp. z o.o.
VersionInfoDescription=Quesma Shipper installer
VersionInfoProductName=Quesma Shipper
VersionInfoVersion={#FileVersion}
VersionInfoProductVersion={#FileVersion}
VersionInfoProductTextVersion={#ReleaseVersion}
LicenseFile={#SourcePath}\..\..\..\LICENSE

[Files]
Source: "{#BinaryPath}"; DestName: "quesma-shipper-setup-helper.exe"; Flags: dontcopy
Source: "{#BinaryPath}"; DestDir: "{app}"; DestName: "quesma-shipper.exe"; Flags: ignoreversion
Source: "{#SupervisorPath}"; DestDir: "{app}"; DestName: "quesma-shipper-supervisor.exe"; Flags: ignoreversion
Source: "{#SourcePath}\..\..\..\LICENSE"; DestDir: "{app}\licenses"; Flags: ignoreversion
Source: "{#SourcePath}\..\..\..\NOTICE"; DestDir: "{app}\licenses"; Flags: ignoreversion

[Registry]
Root: HKLM64; Subkey: "Software\Quesma\Shipper"; ValueType: string; ValueName: "InstallDir"; ValueData: "{app}"; Flags: uninsdeletevalue uninsdeletekeyifempty; Check: IsAdminInstallMode
Root: HKLM64; Subkey: "Software\Quesma\Shipper"; ValueType: string; ValueName: "Version"; ValueData: "{#ReleaseVersion}"; Flags: uninsdeletevalue uninsdeletekeyifempty; Check: IsAdminInstallMode

[Code]
var
  MachinePrepared, MachineInstalled, InstallationFailed: Boolean;
  SetupHelper, RecoveryFile, RecoveryInstallDir, LifecycleMessage: String;

procedure LifecycleLog(const Line: String; const Error, FirstLine: Boolean);
begin
  Log(Line);
  if (not Error) and (LifecycleMessage = '') and (Trim(Line) <> '') then
    LifecycleMessage := Copy(Line, 1, 2048);
end;

function ExecLifecycle(const ProgramPath, Arguments: String; var ExitCode: Integer): Boolean;
begin
  LifecycleMessage := '';
  Result := ExecAndLogOutput(ProgramPath, Arguments, '', SW_HIDE,
                            ewWaitUntilTerminated, ExitCode, @LifecycleLog);
end;

function DefaultInstallDir(Param: String): String;
begin
  if IsAdminInstallMode then
    Result := ExpandConstant('{commonpf64}\Quesma Shipper')
  else
    Result := ExpandConstant('{localappdata}\Programs\Quesma Shipper');
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Arguments: String;
  ExitCode: Integer;
begin
  Result := '';
  ExtractTemporaryFile('quesma-shipper-setup-helper.exe');
  SetupHelper := ExpandConstant('{tmp}\quesma-shipper-setup-helper.exe');
  if IsAdminInstallMode then
  begin
    if CompareText(RemoveBackslashUnlessRoot(ExpandConstant('{app}')),
                   ExpandConstant('{commonpf64}\Quesma Shipper')) <> 0 then
    begin
      Result := 'All-users installation requires the native Program Files\Quesma Shipper directory.';
      Exit;
    end;
    RecoveryFile := ExpandConstant('{tmp}\quesma-shipper-recovery.json');
    RecoveryInstallDir := ExpandConstant('{app}');
    Arguments := 'prepare-system-install --install-dir "' + ExpandConstant('{app}') +
                 '" --recovery-file "' + RecoveryFile + '"';
    MachinePrepared := True;
  end
  else
    Arguments := 'prepare-user-install --install-dir "' + ExpandConstant('{app}') + '"';

  if not ExecLifecycle(SetupHelper, Arguments, ExitCode) then
  begin
    Result := 'Could not validate or prepare the Quesma Shipper installation.';
    Exit;
  end;
  if ExitCode <> 0 then
  begin
    Result := 'Quesma Shipper installation preparation failed (exit code ' + IntToStr(ExitCode) +
              '). ' + LifecycleMessage;
    Exit;
  end;
  if IsAdminInstallMode then
    Exit;

  if not ExecLifecycle(SetupHelper, 'service uninstall', ExitCode) then
    Log('Could not remove the existing personal background task; continuing so setup can repair the installation.')
  else if ExitCode <> 0 then
    Log('The existing personal background task could not be removed (exit code ' +
        IntToStr(ExitCode) + '); continuing so setup can repair the installation.');
end;

procedure DeinitializeSetup;
var
  ExitCode: Integer;
begin
  if MachinePrepared and not MachineInstalled and FileExists(RecoveryFile) then
  begin
    if not ExecLifecycle(SetupHelper, 'resume-system-install --install-dir "' + RecoveryInstallDir +
                         '" --recovery-file "' + RecoveryFile + '"', ExitCode) then
      Log('Could not restore the managed task after setup failed. Rerun the all-users installer to repair collection.')
    else if ExitCode <> 0 then
      Log('Managed-task recovery failed (exit code ' + IntToStr(ExitCode) +
          '). Rerun the all-users installer to repair collection.');
  end;
end;

procedure StartShipper;
var
  ExitCode: Integer;
  ProgramPath: String;
begin
  ProgramPath := ExpandConstant('{app}\quesma-shipper.exe');
  if not ExecLifecycle(ProgramPath, 'postinstall', ExitCode) then
    RaiseException('Could not start Quesma Shipper after installation.');
  if ExitCode <> 0 then
    RaiseException('Quesma Shipper could not register or start its background task (exit code ' +
                   IntToStr(ExitCode) + '). ' + LifecycleMessage);
end;

procedure StopManagedTaskForUninstall;
var
  Scheduler, Folder, Tasks, Task, Definition, Actions, Action, Instances: Variant;
  Index, Attempt: Integer;
  ExpectedRunner: String;
begin
  if CompareText(RemoveBackslashUnlessRoot(ExpandConstant('{app}')),
                 ExpandConstant('{commonpf64}\Quesma Shipper')) <> 0 then
    RaiseException('The managed uninstaller must run from the native Program Files\Quesma Shipper directory.');
  ExpectedRunner := ExpandConstant('{app}\quesma-shipper-supervisor.exe');
  Scheduler := CreateOleObject('Schedule.Service');
  Scheduler.Connect();
  Folder := Scheduler.GetFolder('\');
  Tasks := Folder.GetTasks(1);
  for Index := 1 to Tasks.Count do
  begin
    Task := Tasks.Item(Index);
    if CompareText(Task.Name, 'Quesma Shipper Managed') = 0 then
    begin
      Definition := Task.Definition;
      Actions := Definition.Actions;
      if Actions.Count <> 1 then
        RaiseException('The managed task name belongs to another installation.');
      Action := Actions.Item(1);
      if (CompareText(Action.Path, ExpectedRunner) <> 0) or
         (Action.Arguments <> '--managed') then
        RaiseException('The managed task name belongs to another installation.');
      Task.Enabled := False;
      Task.Stop(0);
      for Attempt := 1 to 300 do
      begin
        Instances := Task.GetInstances(0);
        if Instances.Count = 0 then
        begin
          Folder.DeleteTask('Quesma Shipper Managed', 0);
          Exit;
        end;
        Sleep(100);
      end;
      RaiseException('Managed collectors did not stop. Retry uninstall after closing user sessions.');
    end;
  end;
end;

procedure StopShipperForUninstall;
var
  ExitCode: Integer;
  ProgramPath: String;
begin
  if IsAdminInstallMode then
  begin
    StopManagedTaskForUninstall;
    Exit;
  end;
  ProgramPath := ExpandConstant('{app}\quesma-shipper.exe');
  if not FileExists(ProgramPath) then
    Exit;
  if not ExecLifecycle(ProgramPath, 'service uninstall', ExitCode) then
    RaiseException('Could not stop the Quesma Shipper background task.');
  if ExitCode <> 0 then
    RaiseException('The Quesma Shipper background task could not be removed (exit code ' +
                   IntToStr(ExitCode) + ').');
end;

function NormalizedPath(const Value: String): String;
begin
  Result := RemoveBackslashUnlessRoot(Trim(Value));
end;

function TakePathPart(var Remaining: String): String;
var
  Separator: Integer;
begin
  Separator := Pos(';', Remaining);
  if Separator = 0 then
  begin
    Result := Remaining;
    Remaining := '';
  end
  else
  begin
    Result := Copy(Remaining, 1, Separator - 1);
    Delete(Remaining, 1, Separator);
  end;
end;

function PathContains(const PathValue, Entry: String): Boolean;
var
  Remaining, Part: String;
begin
  Result := False;
  Remaining := PathValue;
  while Remaining <> '' do
  begin
    Part := TakePathPart(Remaining);
    if CompareText(NormalizedPath(Part), NormalizedPath(Entry)) = 0 then
    begin
      Result := True;
      Exit;
    end;
  end;
end;

function EnvironmentRoot: Integer;
begin
  if IsAdminInstallMode then
    Result := HKLM64
  else
    Result := HKCU;
end;

function EnvironmentKey: String;
begin
  if IsAdminInstallMode then
    Result := 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment'
  else
    Result := 'Environment';
end;

procedure AddToPath;
var
  Existing, AppDir: String;
begin
  AppDir := ExpandConstant('{app}');
  RegQueryStringValue(EnvironmentRoot, EnvironmentKey, 'Path', Existing);
  if PathContains(Existing, AppDir) then
    Exit;
  if (Existing <> '') and (Existing[Length(Existing)] <> ';') then
    Existing := Existing + ';';
  RegWriteExpandStringValue(EnvironmentRoot, EnvironmentKey, 'Path', Existing + AppDir);
end;

procedure RemoveFromPath;
var
  Existing, Updated, AppDir, Remaining, Part: String;
begin
  if not RegQueryStringValue(EnvironmentRoot, EnvironmentKey, 'Path', Existing) then
    Exit;
  AppDir := ExpandConstant('{app}');
  Updated := '';
  Remaining := Existing;
  while Remaining <> '' do
  begin
    Part := TakePathPart(Remaining);
    if (Trim(Part) <> '') and
       (CompareText(NormalizedPath(Part), NormalizedPath(AppDir)) <> 0) then
    begin
      if Updated <> '' then
        Updated := Updated + ';';
      Updated := Updated + Part;
    end;
  end;
  if Updated <> Existing then
    RegWriteExpandStringValue(EnvironmentRoot, EnvironmentKey, 'Path', Updated);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
  begin
    InstallationFailed := True;
    AddToPath;
    StartShipper;
    MachineInstalled := IsAdminInstallMode;
    InstallationFailed := False;
  end;
end;

function GetCustomSetupExitCode: Integer;
begin
  if InstallationFailed then
    Result := 1
  else
    Result := 0;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then
  begin
    StopShipperForUninstall;
    RemoveFromPath;
  end;
end;

[UninstallDelete]
Type: files; Name: "{app}\.quesma-shipper.exe.old"
Type: dirifempty; Name: "{app}"
