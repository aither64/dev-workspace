# frozen_string_literal: true

# Owned disposable cross-language fixture. The predecessor harness, when given,
# remains its archived source; only this driver and synthetic records are new.
source = ENV.fetch('P2_TEAM_SOURCE_ROOT')
old = ENV['P2_TEAM_PREDECESSOR_ROOT']
ARGV.replace(['--name', 'test_pi_team_actual_scanner_retained_fence'])
require File.join(old || source, 'test/workspace_host_test')
require File.join(source, 'libexec/workspace-session-backends')
require File.join(source, 'test/support/pi_creation_fixture')

class WorkspaceHostTest
  include PiCreationFixture

  def test_pi_team_actual_scanner_retained_fence
    with_transition_host do |host, paths|
      workspace = host.send(:registry).entries.first.fetch('root')
      state = host.instance_variable_get(:@state)
      records = pi_creation_records(workspace)
      saved = write_pi_creation_records(state, workspace, records, receipt: false)
      File.unlink(saved.delete(:binding))
      namespace = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      roster_path = File.join(state, 'codex-teams', namespace, 'example.json')
      now = Time.now.utc.iso8601
      roster = {'schema' => 2, 'workspace' => workspace, 'slug' => 'example', 'revision' => 1,
        'createdAt' => now, 'updatedAt' => now, 'backend' => 'pi', 'backendContract' => 1,
        'sessionIncarnation' => 'a' * 64, 'leadIncarnation' => 'b' * 64,
        'members' => [{'address' => 'implementer0', 'role' => 'implementer', 'index' => 0,
          'state' => 'ready', 'addedAt' => now, 'incarnation' => 'c' * 64, 'creationId' => 'd' * 64,
          'initialInputId' => '2' * 32, 'model' => 'model', 'reasoningEffort' => 'high',
          'behavior' => 'implementer', 'purpose' => 'implementation', 'instructions' => 'Retained fixture instructions', 'access' => 'workspace_write'}]}
      FileUtils.mkdir_p(File.dirname(roster_path), mode: 0700)
      File.write(roster_path, JSON.generate(roster), mode: 'w', perm: 0600)
      saved[:roster] = roster_path
      original = saved.to_h { |key, path| [key, File.binread(path)] }
      contract_path = File.join(host.candidate, 'share/workspace-portal/runtime-contract.json')
      contract = JSON.parse(File.read(contract_path)).merge(WorkspaceSessionBackends::FIELDS)
      contract['piTeamRecords'] = WorkspaceSessionBackends::TEAM_FIELDS
      File.write(contract_path, JSON.generate(contract))
      marker = File.join(paths.fetch(:root), 'scanner-called')
      reader = make_package(paths.fetch(:root), 'executing-reader')
      helper = File.join(reader, 'bin/workspace-portal')
      script = "#!/bin/sh\nprintf called >> #{marker.shellescape}\nexec #{ENV.fetch('P2_TEAM_PORTAL_HELPER').shellescape} \"$@\"\n"
      File.write(helper, script, mode: 'w', perm: 0700)
      host.executing_package = reader
      if ENV['P2_TEAM_PREDECESSOR_ROOT']
        # The actual old parser knows no scanner. Its recognized journal must
        # refuse both actual entry points even with cache and binding absent.
        candidate = host.candidate
        host.define_singleton_method(:package_root) { candidate }
        [false, true].each do |from_candidate|
          arguments = ['--source', paths.fetch(:source)]
          arguments << '--from-candidate' if from_candidate
          error = assert_raises(DevWorkspaceHost::Error) { host.send(:prepare_profile, arguments, from_candidate:) }
          assert_match(/creation.*(schema|state)/, error.message)
          original.each { |key, bytes| assert_equal(bytes, File.binread(saved.fetch(key))) }
        end
        refute(File.exist?(marker), 'predecessor unexpectedly reached the new scanner')
      else
        host.define_singleton_method(:run_pi_team_preflight) do |roots|
          DevWorkspaceHost::Host.instance_method(:run_pi_team_preflight).bind_call(self, roots)
        end
        host.send(:require_compatible_session_backends!, host.candidate)
        assert(File.exist?(marker), 'actual Go scanner was not invoked')
        original.each { |key, bytes| assert_equal(bytes, File.binread(saved.fetch(key))) }
        contract.delete('piTeamRecords')
        File.write(contract_path, JSON.generate(contract))
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        File.unlink(saved.fetch(:journal))
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
      end
      refute(File.exist?(host.instance_variable_get(:@profile)))
      refute_includes(host.events, [:sessions_quiesced])
      refute_includes(host.events, [:configured])
      refute(File.exist?(WorkspaceSessionBackends.binding_directory(state, workspace) + '/example.json'))
    end
  end
end
