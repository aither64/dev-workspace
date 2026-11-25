# frozen_string_literal: true

# Frozen source-side predicate from 46a547dce36e5f341b737e378fb721f8ac770f8e.
# The transition harness supplies the other unchanged host methods.
module DevWorkspaceHost
  module LiveSwitchPolicyTwo
    RUNTIME_CONTRACT = {'livePackageSwitchPolicy' => 2}.freeze

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
