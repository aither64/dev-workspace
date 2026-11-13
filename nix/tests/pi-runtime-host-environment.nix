{ pkgs }:
let
  ownerHome = "/home/pi-host-environment";
  ownerName = "pi-host-environment";
  # Substitute only the interpreter; execute the real generated Bash/env-i wrapper.
  recorder = pkgs.writeShellScriptBin "ruby" ''
    exec ${pkgs.ruby}/bin/ruby - "$@" <<'RUBY'
    STDOUT.sync = true
    script = ARGV.shift
    command = File.basename(script || "")
    mode = ARGV.first
    unless script && File.file?(script) && File.realpath(script) == script &&
           File.basename(File.dirname(script)) == "libexec" &&
           %w[workspace-pi-host workspace-pi-session-entry].include?(command) &&
           %w[baseline adversarial].include?(mode) &&
           ARGV == [mode, "sentinel with spaces", '--literal=$value']
      abort "RECORDER_BOOTSTRAP_FAILED"
    end
    puts "RECORDER_BOOTSTRAP command=#{command} mode=#{mode} argv=preserved"
    bundle = "/etc/ssl/certs/ca-certificates.crt"
    missing = %w[NIX_SSL_CERT_FILE SSL_CERT_FILE].reject { |key| ENV[key] == bundle }
    abort "SYSTEM_CA_REQUIRED #{missing.join(' ')}" unless missing.empty?
    fixed = {
      "HOME" => "${ownerHome}", "USER" => "${ownerName}", "LOGNAME" => "${ownerName}",
      "PATH" => "/run/wrappers/bin:/run/current-system/sw/bin", "TMPDIR" => "/tmp",
      "LANG" => "C.UTF-8", "LC_ALL" => "C.UTF-8",
      "SUDO_UID" => mode == "baseline" ? "" : "1000",
      "INVOCATION_ID" => mode == "baseline" ? "" : "inert-invocation"
    }
    fixed.each { |key, value| abort "FIXED_ENVIRONMENT_FAILED #{key}" unless ENV[key] == value }
    forbidden = %w[OPENAI_API_KEY ANTHROPIC_API_KEY NODE_OPTIONS NODE_TLS_REJECT_UNAUTHORIZED
                   NODE_EXTRA_CA_CERTS RUBYOPT UNRELATED_MARKER]
    forbidden.each { |key| abort "FORBIDDEN_ENVIRONMENT_PRESENT #{key}" if ENV.key?(key) }
    puts "ENVIRONMENT_PASS command=#{command} mode=#{mode} both_system_ca=true retained_fixed=true forbidden_absent=true"
    RUBY
  '';
  host = pkgs.callPackage ../pi-runtime-host-package.nix {
    inherit ownerHome ownerName;
    ownerUid = 1000;
    stateRoot = "/tmp/pi-host-environment-state";
    ruby = recorder;
  };
in
pkgs.runCommand "dev-workspace-pi-runtime-host-environment" { } ''
  for command in workspace-pi-session-entry workspace-pi-host; do
    ${pkgs.coreutils}/bin/env -i ${host}/bin/"$command" \
      baseline 'sentinel with spaces' '--literal=$value'
    ${pkgs.coreutils}/bin/env -i \
      NIX_SSL_CERT_FILE=/wrong-nix-ca SSL_CERT_FILE=/wrong-ssl-ca \
      HOME=/wrong-home USER=wrong-user LOGNAME=wrong-logname PATH=/wrong-path TMPDIR=/wrong-tmp \
      LANG=wrong-language LC_ALL=wrong-locale SUDO_UID=1000 INVOCATION_ID=inert-invocation \
      OPENAI_API_KEY=inert ANTHROPIC_API_KEY=inert NODE_OPTIONS=inert \
      NODE_TLS_REJECT_UNAUTHORIZED=inert NODE_EXTRA_CA_CERTS=/wrong-extra-ca \
      RUBYOPT=inert UNRELATED_MARKER=inert \
      ${host}/bin/"$command" adversarial 'sentinel with spaces' '--literal=$value'
  done
  touch "$out"
''
