{
  lib,
  paths ? null,
}:
let
  defaults = {
    routerSocket = "/run/dev-workspaces/router.sock";
    lockFile = "/run/lock/dev-workspace-substrate.lock";
    passwordFile = "/var/lib/dev-workspaces/password/password";
    htpasswdFile = "/var/lib/dev-workspaces/auth/htpasswd";
    caStateDirectory = "/var/lib/dev-workspaces/pki";
    certificateDirectory = "/var/lib/dev-workspaces/tls";
    publicCaFile = "/var/lib/dev-workspaces/public/ca.pem";
  };
  selectedPaths = if paths == null then defaults else paths;
  managedDirectories = {
    router = builtins.dirOf selectedPaths.routerSocket;
    lock = builtins.dirOf selectedPaths.lockFile;
    password = builtins.dirOf selectedPaths.passwordFile;
    authentication = builtins.dirOf selectedPaths.htpasswdFile;
    authority = selectedPaths.caStateDirectory;
    certificates = selectedPaths.certificateDirectory;
    publicCa = builtins.dirOf selectedPaths.publicCaFile;
  };
  persistentDirectories = with managedDirectories; [
    password
    authentication
    authority
    certificates
    publicCa
  ];
  directories = builtins.attrValues managedDirectories;
  safeAbsolutePath =
    path:
    let
      components = lib.splitString "/" path;
    in
    lib.hasPrefix "/" path
    && path != "/"
    && lib.all (component: component != "" && component != "." && component != "..") (
      builtins.tail components
    );
  pathContains = parent: child: child == parent || lib.hasPrefix "${parent}/" child;
  stateInVarLib = lib.all (
    directory: directory != "/var/lib" && pathContains "/var/lib" directory
  ) persistentDirectories;
  routerInRun = managedDirectories.router != "/run" && pathContains "/run" managedDirectories.router;
  indexes = lib.range 0 (builtins.length directories - 1);
  directoriesDisjoint = lib.all (
    leftIndex:
    lib.all (
      rightIndex:
      leftIndex == rightIndex
      || (
        let
          left = builtins.elemAt directories leftIndex;
          right = builtins.elemAt directories rightIndex;
        in
        !(pathContains left right) && !(pathContains right left)
      )
    ) indexes
  ) indexes;
in
{
  allAbsolute = lib.all safeAbsolutePath (builtins.attrValues selectedPaths);
  inherit defaults;
  lockInRunLock = builtins.dirOf selectedPaths.lockFile == "/run/lock";
  inherit
    directoriesDisjoint
    managedDirectories
    persistentDirectories
    routerInRun
    stateInVarLib
    ;
  valid =
    lib.all safeAbsolutePath (builtins.attrValues selectedPaths)
    && stateInVarLib
    && routerInRun
    && builtins.dirOf selectedPaths.lockFile == "/run/lock"
    && directoriesDisjoint;
}
