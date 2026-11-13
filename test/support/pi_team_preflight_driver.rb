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
      slug = '2026-11-12-workflow'
      records = pi_creation_records(workspace, slug:)
      native = '11111111-1111-4111-8111-111111111111'
      records[:journal]['native']['native_id'] = native
      records[:completion]['nativeId'] = native
      records[:binding]['conversations'][0]['native_id'] = native
      saved = write_pi_creation_records(state, workspace, records, receipt: false)
      File.unlink(saved.delete(:binding))
      namespace = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      roster_path = File.join(state, 'codex-teams', namespace, slug + '.json')
      now = Time.now.utc.iso8601
      roster = {'schema' => 2, 'workspace' => workspace, 'slug' => slug, 'revision' => 1,
        'createdAt' => now, 'updatedAt' => now, 'backend' => 'pi', 'backendContract' => 1,
        'sessionIncarnation' => 'a' * 64, 'leadIncarnation' => 'b' * 64,
        'members' => [{'address' => 'implementer0', 'role' => 'implementer', 'index' => 0,
          'state' => 'ready', 'addedAt' => now, 'incarnation' => 'c' * 64, 'creationId' => 'd' * 64,
          'initialInputId' => '2' * 32, 'model' => 'model', 'reasoningEffort' => 'high',
          'behavior' => 'implementer', 'purpose' => 'implementation', 'instructions' => 'Retained fixture instructions', 'access' => 'workspace_write'}]}
      FileUtils.mkdir_p(File.dirname(roster_path), mode: 0700)
      File.write(roster_path, JSON.generate(roster), mode: 'w', perm: 0600)
      saved[:roster] = roster_path
      ref = {'Session' => {'Workspace' => workspace, 'Slug' => slug, 'Incarnation' => 'a' * 64},
        'Backend' => 'pi', 'Member' => 'lead', 'Incarnation' => 'b' * 64, 'NativeID' => native}
      namespace_hash = Digest::SHA256.hexdigest(workspace)
      %w[questions plans].each do |kind|
        file = File.join(state, 'pi-' + kind, namespace_hash, slug, 'a' * 64, 'lead', 'b' * 64, kind + '.json')
        record = {'schema' => 1, 'conversation' => ref, 'revision' => 0}
        record.merge!(kind == 'questions' ? {'nextSequence' => 1, 'questions' => []} : {'proposals' => [], 'acceptances' => []})
        FileUtils.mkdir_p(File.dirname(file), mode: 0700)
        File.write(file, JSON.generate(record), mode: 'w', perm: 0600)
        saved[kind.to_sym] = file
      end
      mapping = {'Schema' => 2, 'CreationID' => 'd' * 64, 'Conversation' => ref,
        'HistoryRef' => Digest::SHA256.hexdigest(JSON.generate(['pi-history', 1, 'pi', workspace, slug, 'a' * 64, 'lead', 'b' * 64, native])),
        'HistoryFile' => '', 'Generation' => 'e' * 64, 'HeldReason' => 'retained held worker',
        'Settings' => {'Provider' => 'native', 'AccountRef' => 'account', 'Model' => 'model', 'Thinking' => 'high', 'PolicyRevision' => 'f' * 64, 'Revision' => 1},
        'PendingSettings' => nil, 'Loaded' => false, 'Persisted' => false, 'WorkerPID' => 0, 'WorkerStart' => ''}
      mapping['Instructions'] = "Exact frozen rôle\n/path-looking-text"
      mapping['InitialSettings'] = mapping['Settings'].dup
      policy = {'Mode' => 'plan', 'Revision' => 1, 'Active' => ['read'], 'RegisteredCeiling' => ['read']}
      policy['Digest'] = Digest::SHA256.hexdigest(JSON.generate(['pi-tool-policy', 1, 'plan', ['read'], ['read']]))
      mapping['ToolPolicy'] = policy
      file = File.join(state, 'pi-runtime', namespace_hash, slug, 'a' * 64, 'members', 'lead', 'b' * 64, 'conversation.json')
      FileUtils.mkdir_p(File.dirname(file), mode: 0700)
      File.write(file, JSON.generate(mapping), mode: 'w', perm: 0600)
      saved[:mapping] = file
      original = saved.to_h { |key, path| [key, File.binread(path)] }
      contract_path = File.join(host.candidate, 'share/workspace-portal/runtime-contract.json')
      contract = JSON.parse(File.read(contract_path)).merge(WorkspaceSessionBackends::FIELDS)
      contract['piTeamRecords'] = WorkspaceSessionBackends::TEAM_FIELDS
      contract.merge!(WorkspaceSessionBackends::WORKFLOW_FIELDS)
      contract['piRoleInstructions'] = 1
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
        %w[cold held pending].each do |phase|
          value = Marshal.load(Marshal.dump(mapping))
          value['HeldReason'] = phase == 'held' ? 'retained held worker' : ''
          value['PendingToolPolicy'] = policy.merge('Revision' => 2) if phase == 'pending'
          File.write(saved.fetch(:mapping), JSON.generate(value), mode: 'w', perm: 0600)
          output = host.send(:run_pi_team_preflight, [workspace])
          claims = WorkspaceSessionBackends.team_preflight!(output, root: state, workspaces: [workspace]).fetch('sessions')
          rows = claims.select { |claim| %w[tool_policy role_instructions].include?(claim.fetch('source').fetch('kind')) }
          assert_equal(%w[role_instructions tool_policy], rows.map { |claim| claim['source']['kind'] }.sort)
          assert_equal(rows[0]['source']['id'], rows[1]['source']['id'])
          assert_equal(rows[0]['members'], rows[1]['members'])
          assert_equal('lead', rows[0]['members'][0]['address'])
          assert_equal('d' * 64, rows[0]['members'][0]['creationId'])
          refute(rows.any? { |claim| claim.key?('lead') })
          refute_includes(output, mapping.fetch('Instructions'))
          host.send(:require_compatible_session_backends!, host.candidate)
          assert_equal(JSON.generate(value).b, File.binread(saved.fetch(:mapping)))
        end
        File.write(saved.fetch(:mapping), original.fetch(:mapping), mode: 'w', perm: 0600)
        [nil, false, '1', 1.0, 0, 2].each do |version|
          File.write(contract_path, JSON.generate(contract.merge('piRoleInstructions' => version)))
          error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
          assert_match(/role instructions/, error.message)
        end
        File.write(contract_path, JSON.generate(contract.reject { |key, _| key == 'piRoleInstructions' }))
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        File.write(contract_path, JSON.generate(contract))
        ['', nil].each do |instructions|
          value = mapping.merge('Instructions' => instructions)
          File.write(saved.fetch(:mapping), JSON.generate(value), mode: 'w', perm: 0600)
          assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
          assert_equal(JSON.generate(value).b, File.binread(saved.fetch(:mapping)))
        end
        File.write(saved.fetch(:mapping), original.fetch(:mapping), mode: 'w', perm: 0600)
        if ENV['P2_TEAM_PRIOR_TARGET_ROOT']
          prior = ENV.fetch('P2_TEAM_PRIOR_TARGET_ROOT')
          target = make_package(paths.fetch(:root), 'actual-prior-R')
          FileUtils.cp(File.join(prior, 'portal/internal/session/runtime-contract.json'), File.join(target, 'share/workspace-portal/runtime-contract.json'))
          error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, target) }
          assert_match(/role instructions/, error.message)
          command = [ENV.fetch('P2_TEAM_PRIOR_PORTAL_HELPER'), 'pi-team-preflight', '--state-root', state, '--workspace', workspace]
          _, stderr, status = Open3.capture3(*command)
          refute(status.success?, 'actual prior R reader accepted Instructions')
          assert_match(/Pi runtime scope mismatch/, stderr)
          legacy = mapping.reject { |key, _| key == 'Instructions' }
          File.write(saved.fetch(:mapping), JSON.generate(legacy), mode: 'w', perm: 0600)
          host.send(:require_compatible_session_backends!, target)
          stdout, stderr, status = Open3.capture3(*command)
          assert(status.success?, stderr)
          refute_includes(stdout, 'role_instructions')
          assert_equal(JSON.generate(legacy).b, File.binread(saved.fetch(:mapping)))
          File.write(saved.fetch(:mapping), original.fetch(:mapping), mode: 'w', perm: 0600)
        end
        original.each { |key, bytes| assert_equal(bytes, File.binread(saved.fetch(key))) }
        WorkspaceSessionBackends::WORKFLOW_FIELDS.each_key do |field|
          unsupported = contract.reject { |key, _| key == field }
          File.write(contract_path, JSON.generate(unsupported))
          assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        end
        contract.delete('piTeamRecords')
        File.write(contract_path, JSON.generate(contract))
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        File.write(contract_path, JSON.generate(contract.merge('piTeamRecords' => WorkspaceSessionBackends::TEAM_FIELDS)))
        [:intent, :completion, :journal].each do |missing|
          bytes = File.binread(saved.fetch(missing))
          File.unlink(saved.fetch(missing))
          assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
          File.write(saved.fetch(missing), bytes, mode: 'w', perm: 0600)
        end
        pending = JSON.parse(original.fetch(:journal)).merge('state' => 'creating')
        File.write(saved.fetch(:journal), JSON.generate(pending), mode: 'w', perm: 0600)
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        File.write(saved.fetch(:journal), original.fetch(:journal), mode: 'w', perm: 0600)
        original.each { |key, bytes| assert_equal(bytes, File.binread(saved.fetch(key))) }
        File.unlink(saved.fetch(:journal))
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
      end
      refute(File.exist?(host.instance_variable_get(:@profile)))
      refute_includes(host.events, [:sessions_quiesced])
      refute_includes(host.events, [:configured])
      refute(File.exist?(WorkspaceSessionBackends.binding_directory(state, workspace) + '/' + slug + '.json'))
    end
  end
end
