# frozen_string_literal: true

# Frozen source-side predicate from eabb56a278bd8eb1759041467eaebfa6392ddcb2.
# This small boundary fixture keeps policy1 independent of the current package
# default; the ordinary transition/maintenance harness supplies all other paths.
# Original method SHA256: 2e16ae2c2a3aeaac89a042a179b347847142eafb67502136f9748a5051ee5561
module DevWorkspaceHost
  module LiveSwitchPolicyOne
    RUNTIME_CONTRACT = {'livePackageSwitchPolicy' => 1}.freeze

    private

    def live_package_switch_contract?(package)
      return false unless package.is_a?(String)

      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      payload = File.binread(path, 64 * 1024 + 1)
      return false if payload.bytesize > 64 * 1024

      contract = parse_strict_json_object(payload, 'live package switch contract', 64 * 1024)
      contract['livePackageSwitchPolicy'] == RUNTIME_CONTRACT.fetch('livePackageSwitchPolicy')
    rescue Error, SystemCallError
      false
    end
  end
end
