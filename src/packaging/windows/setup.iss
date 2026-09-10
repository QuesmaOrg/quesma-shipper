; Per-user bootstrap for the self-updating shipper. The installer owns shell integration and the
; scheduled task; TUF owns later replacements of quesma-shipper.exe at this stable path.

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
DefaultDirName={localappdata}\Programs\Quesma Shipper
DefaultGroupName=Quesma Shipper
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
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
Source: "{#BinaryPath}"; DestDir: "{app}"; DestName: "quesma-shipper.exe"; Flags: ignoreversion
Source: "{#SupervisorPath}"; DestDir: "{app}"; DestName: "quesma-shipper-supervisor.exe"; Flags: ignoreversion
Source: "{#SourcePath}\..\..\..\LICENSE"; DestDir: "{app}\licenses"; Flags: ignoreversion
Source: "{#SourcePath}\..\..\..\NOTICE"; DestDir: "{app}\licenses"; Flags: ignoreversion

[Code]
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ExistingProgram: String;
  ExitCode: Integer;
begin
  Result := '';
  ExistingProgram := ExpandConstant('{app}\quesma-shipper.exe');
  if not FileExists(ExistingProgram) then
    Exit;
  if not Exec(ExistingProgram, 'service uninstall', '', SW_HIDE,
              ewWaitUntilTerminated, ExitCode) then
    Log('Could not invoke the existing Quesma Shipper to remove its background task; continuing.')
  else if ExitCode <> 0 then
    Log('The existing Quesma Shipper could not remove its background task (exit code ' +
        IntToStr(ExitCode) + '); continuing so setup can repair the installation.');
end;

procedure StartShipper;
var
  ExitCode: Integer;
  ProgramPath: String;
begin
  ProgramPath := ExpandConstant('{app}\quesma-shipper.exe');
  if not Exec(ProgramPath, 'postinstall', '', SW_HIDE,
              ewWaitUntilTerminated, ExitCode) then
    RaiseException('Could not start Quesma Shipper after installation.');
  if ExitCode <> 0 then
    RaiseException('Quesma Shipper could not register or start its background task (exit code ' +
                   IntToStr(ExitCode) + ').');
end;

procedure StopShipperForUninstall;
var
  ExitCode: Integer;
  ProgramPath: String;
begin
  ProgramPath := ExpandConstant('{app}\quesma-shipper.exe');
  if not FileExists(ProgramPath) then
    Exit;
  if not Exec(ProgramPath, 'service uninstall', '', SW_HIDE,
              ewWaitUntilTerminated, ExitCode) then
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

procedure AddToUserPath;
var
  Existing, AppDir: String;
begin
  AppDir := ExpandConstant('{app}');
  RegQueryStringValue(HKCU, 'Environment', 'Path', Existing);
  if PathContains(Existing, AppDir) then
    Exit;
  if (Existing <> '') and (Existing[Length(Existing)] <> ';') then
    Existing := Existing + ';';
  RegWriteExpandStringValue(HKCU, 'Environment', 'Path', Existing + AppDir);
end;

procedure RemoveFromUserPath;
var
  Existing, Updated, AppDir, Remaining, Part: String;
begin
  if not RegQueryStringValue(HKCU, 'Environment', 'Path', Existing) then
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
    RegWriteExpandStringValue(HKCU, 'Environment', 'Path', Updated);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
  begin
    AddToUserPath;
    StartShipper;
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then
  begin
    StopShipperForUninstall;
    RemoveFromUserPath;
  end;
end;

[UninstallDelete]
Type: files; Name: "{app}\.quesma-shipper.exe.old"
Type: dirifempty; Name: "{app}"
