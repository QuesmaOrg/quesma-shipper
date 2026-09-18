; Per-user bootstrap for the self-updating shipper. The installer owns shell integration and the
; scheduled task; TUF owns later replacements of quesma-shipper.exe at this stable path.
; Built with /DScope=machine it installs for every user instead: Program Files, one all-users task,
; the updater off (install-scope marker), and a provisioning file from /SERVER= and /ENROLLTOKEN=.

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
#ifndef Scope
  #define Scope "user"
#endif

#if Architecture == "amd64"
  #define ArchitectureConstraint "x64compatible and not arm64"
#elif Architecture == "arm64"
  #define ArchitectureConstraint "arm64"
#else
  #error Architecture must be amd64 or arm64
#endif

#if Scope == "machine"
  #define Machine
  #define PathRoot "HKLM"
  #define PathKey "SYSTEM\CurrentControlSet\Control\Session Manager\Environment"
#elif Scope == "user"
  #define PathRoot "HKCU"
  #define PathKey "Environment"
#else
  #error Scope must be user or machine
#endif

[Setup]
#ifdef Machine
AppId={{9B2F6C51-4D7A-4E8B-A3C0-1F5E7D2B8C64}
DefaultDirName={autopf}\Quesma Shipper
PrivilegesRequired=admin
ArchitecturesInstallIn64BitMode={#ArchitectureConstraint}
OutputBaseFilename=QuesmaShipperSetup-machine-{#Architecture}
UninstallDisplayName=Quesma Shipper (all users)
VersionInfoDescription=Quesma Shipper installer (all users)
#else
AppId={{C65D423E-3F9B-49AF-A68F-08C88093F01F}
DefaultDirName={localappdata}\Programs\Quesma Shipper
PrivilegesRequired=lowest
OutputBaseFilename=QuesmaShipperSetup-{#Architecture}
UninstallDisplayName=Quesma Shipper
VersionInfoDescription=Quesma Shipper installer
#endif
AppName=Quesma Shipper
AppVersion={#ReleaseVersion}
AppVerName=Quesma Shipper {#ReleaseVersion}
AppPublisher=Quesma Poland Sp. z o.o.
AppPublisherURL=https://quesma.com/
AppSupportURL=https://github.com/QuesmaOrg/quesma-shipper/issues
AppUpdatesURL=https://github.com/QuesmaOrg/quesma-shipper
DefaultGroupName=Quesma Shipper
DisableProgramGroupPage=yes
ArchitecturesAllowed={#ArchitectureConstraint}
MinVersion=10.0.17763
OutputDir={#OutputDir}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
ChangesEnvironment=yes
CloseApplications=yes
RestartApplications=no
UninstallDisplayIcon={app}\quesma-shipper.exe
VersionInfoCompany=Quesma Poland Sp. z o.o.
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
#ifdef Machine
Source: "{#SourcePath}\install-scope.machine"; DestDir: "{app}"; DestName: "install-scope"; Flags: ignoreversion
#endif

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

procedure AddToPath;
var
  Existing, AppDir: String;
begin
  AppDir := ExpandConstant('{app}');
  RegQueryStringValue({#PathRoot}, '{#PathKey}', 'Path', Existing);
  if PathContains(Existing, AppDir) then
    Exit;
  if (Existing <> '') and (Existing[Length(Existing)] <> ';') then
    Existing := Existing + ';';
  RegWriteExpandStringValue({#PathRoot}, '{#PathKey}', 'Path', Existing + AppDir);
end;

procedure RemoveFromPath;
var
  Existing, Updated, AppDir, Remaining, Part: String;
begin
  if not RegQueryStringValue({#PathRoot}, '{#PathKey}', 'Path', Existing) then
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
    RegWriteExpandStringValue({#PathRoot}, '{#PathKey}', 'Path', Updated);
end;

#ifdef Machine
function JSONString(const S: String): String;
var
  I: Integer;
  C: Char;
begin
  Result := '';
  for I := 1 to Length(S) do
  begin
    C := S[I];
    if (C = '\') or (C = '"') then
      Result := Result + '\' + C
    else if Ord(C) < 32 then
      Result := Result + ' '
    else
      Result := Result + C;
  end;
end;

// Written before the task is registered so the first session to start finds it. Both values or
// neither: half a provisioning file would enroll nowhere and say nothing. The directory is recreated
// by this elevated process: ProgramData lets any user pre-create it, and the shipper trusts the file
// only when nobody but an administrator owns or can change it.
procedure WriteProvisioning;
var
  Server, Token, Dir, Path: String;
  Body: TArrayOfString;
begin
  Server := Trim(ExpandConstant('{param:SERVER|}'));
  Token := Trim(ExpandConstant('{param:ENROLLTOKEN|}'));
  if (Server = '') and (Token = '') then
    Exit;
  if (Server = '') or (Token = '') then
    RaiseException('/SERVER and /ENROLLTOKEN must be given together.');
  Dir := ExpandConstant('{commonappdata}\Quesma Shipper');
  Path := Dir + '\provisioning.json';
  if DirExists(Dir) and not DelTree(Dir, True, True, True) then
    RaiseException('Could not replace ' + Dir + '.');
  if not CreateDir(Dir) then
    RaiseException('Could not create ' + Dir + '.');
  SetArrayLength(Body, 1);
  Body[0] := '{"provisioning_schema":1,"server":"' + JSONString(Server) +
             '","token":"' + JSONString(Token) + '"}';
  if not SaveStringsToUTF8FileWithoutBOM(Path, Body, False) then
    RaiseException('Could not write ' + Path + '.');
end;
#endif

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
  begin
    AddToPath;
#ifdef Machine
    WriteProvisioning;
#endif
    StartShipper;
  end;
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
#ifdef Machine
Type: files; Name: "{commonappdata}\Quesma Shipper\provisioning.json"
Type: dirifempty; Name: "{commonappdata}\Quesma Shipper"
#endif
