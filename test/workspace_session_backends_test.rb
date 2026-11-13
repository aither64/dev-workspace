# frozen_string_literal: true

require 'minitest/autorun'
require 'tmpdir'
require_relative '../libexec/workspace-session-backends'
require_relative 'support/pi_creation_fixture'

class WorkspaceSessionBackendsTest < Minitest::Test
  include PiCreationFixture

  def test_preparation_preflight_is_independent_of_session_authority
    workspace = '/fixture/workspace'
    request = {'workspace' => workspace, 'requestId' => '11111111-1111-4111-8111-111111111111'}
    output = {'schema' => 2, 'required' => false, 'sessions' => [], 'piPreparations' => [request]}
    result = WorkspaceSessionBackends.team_preflight!(JSON.generate(output), root: '/unused', workspaces: [workspace])
    assert_equal(false, result.fetch('required'))
    assert_empty(result.fetch('sessions'))
    invalid = [output.merge('required' => true), output.merge('schema' => 1), output.merge('piPreparations' => []),
      output.merge('piPreparations' => [request, request]), output.merge('piPreparations' => [request.merge('requestId' => 'bad')])]
    invalid.each do |value|
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(JSON.generate(value), root: '/unused', workspaces: [workspace]) }
    end
    assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(JSON.generate(output), root: '/unused', workspaces: []) }
  end

  def test_preparation_target_requires_whole_creation_workflow_role_bundle
    Dir.mktmpdir('pi-preparation-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      expected = WorkspaceSessionBackends::FIELDS.merge(WorkspaceSessionBackends::WORKFLOW_FIELDS).merge('piRoleInstructions' => 1, 'piPreparationSchema' => 2)
      invalid = expected.keys.map { |key| expected.reject { |field, _| field == key } }
      [nil, false, '2', 2.0, 0, 1, 3].each { |value| invalid << expected.merge('piPreparationSchema' => value) }
      invalid.each do |value|
        bytes = JSON.generate(value)
        File.write(path, bytes)
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_preparation_contract!(package) }
        assert_equal(bytes, File.binread(path))
      end
      File.write(path, JSON.generate(expected))
      WorkspaceSessionBackends.require_preparation_contract!(package)
    end
  end

  def test_retained_ready_creation_fence_survives_cache_and_binding_retirement
    Dir.mktmpdir('pi-team-fence-') do |root|
      workspace = File.join(root, 'workspace')
      state = File.join(root, 'state')
      records = pi_creation_records(workspace)
      paths = write_pi_creation_records(state, workspace, records, receipt: false)
      File.unlink(paths.fetch(:binding))
      before = paths.reject { |key, _| key == :binding }.to_h { |key, path| [key, File.binread(path)] }
      proof = WorkspaceSessionBackends.retained_creation_fence!(state, workspace, 'example')
      assert_equal(records[:intent], proof.fetch(:intent))
      assert_equal(records[:completion], proof.fetch(:completion))
      before.each { |key, bytes| assert_equal(bytes, File.binread(paths.fetch(key))) }
      refute(File.exist?(paths.fetch(:binding)))
      # This proof alone must not turn an orphan journal into an admitted Pi
      # session. The new scanner will supply exact validated dependent refs.
      assert_raises(WorkspaceSessionBackends::Error) do
        WorkspaceSessionBackends.inspect_pi_state!(state, workspace, 'example')
      end
    end
  end

  def test_retained_fence_refuses_each_missing_or_conflicting_original_record
    [:intent, :completion, :journal].each do |missing|
      Dir.mktmpdir('pi-team-fence-') do |root|
        workspace = File.join(root, 'workspace')
        state = File.join(root, 'state')
        paths = write_pi_creation_records(state, workspace, pi_creation_records(workspace), receipt: false)
        File.unlink(paths.fetch(:binding))
        bytes = File.binread(paths.fetch(missing))
        File.unlink(paths.fetch(missing))
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.retained_creation_fence!(state, workspace, 'example') }
        File.write(paths.fetch(missing), bytes, mode: 'w', perm: 0600)
        record = JSON.parse(bytes)
        record[missing == :completion ? 'sessionIncarnation' : 'session_incarnation'] = 'f' * 64
        File.write(paths.fetch(missing), JSON.generate(record), mode: 'w', perm: 0600)
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.retained_creation_fence!(state, workspace, 'example') }
      end
    end
  end

  def test_team_records_require_the_complete_exact_integer_support_map
    Dir.mktmpdir('pi-team-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      expected = WorkspaceSessionBackends::TEAM_FIELDS
      invalid = [nil, {}, expected.merge('unknown' => 1)]
      expected.each_key do |key|
        invalid << expected.reject { |field, _| field == key }
        [nil, false, '2', 2.0, 0, 3].each { |value| invalid << expected.merge(key => value) }
      end
      invalid.each do |support|
        bytes = JSON.generate('piTeamRecords' => support)
        File.write(path, bytes)
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_team_contract!(package) }
        assert_equal(bytes, File.binread(path))
      end
      File.write(path, '{}')
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_team_contract!(package) }
      File.write(path, JSON.generate('piTeamRecords' => expected))
      WorkspaceSessionBackends.require_team_contract!(package)
    end
  end

  def test_domain_constraints_crosscheck_original_fence_after_binding_retirement
    Dir.mktmpdir('pi-team-claims-') do |root|
      workspace, state = File.join(root, 'workspace'), File.join(root, 'state')
      records = pi_creation_records(workspace)
      paths = write_pi_creation_records(state, workspace, records, receipt: false)
      File.unlink(paths.fetch(:binding))
      claim = {'source' => {'kind' => 'roster', 'id' => 'example'},
        'session' => {'workspace' => workspace, 'slug' => 'example', 'incarnation' => 'a' * 64},
        'lead' => {'incarnation' => 'b' * 64, 'nativeId' => 'native-lead'},
        'members' => [{'address' => 'implementer0', 'incarnation' => 'c' * 64, 'creationId' => 'd' * 64,
          'initialInputId' => '2' * 32, 'nativeId' => 'later-member-native'}]}
      result = {'schema' => 1, 'required' => true, 'sessions' => [claim]}
      before = paths.reject { |key, _| key == :binding }.to_h { |key, path| [key, File.binread(path)] }
      WorkspaceSessionBackends.team_preflight!(JSON.generate(result), root: state, workspaces: [workspace])
      assert(WorkspaceSessionBackends.inspect_pi_state!(state, workspace, 'example', retained: true))
      before.each { |key, bytes| assert_equal(bytes, File.binread(paths.fetch(key))) }
      %w[workspace incarnation].each do |field|
        changed = Marshal.load(Marshal.dump(result))
        changed['sessions'][0]['session'][field] = field == 'workspace' ? root : 'f' * 64
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(JSON.generate(changed), root: state, workspaces: [workspace]) }
      end
      %w[incarnation creationId].each do |field|
        changed = Marshal.load(Marshal.dump(result))
        changed['sessions'][0]['members'][0][field] = 'f' * 64
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(JSON.generate(changed), root: state, workspaces: [workspace]) }
      end
      duplicate = Marshal.load(Marshal.dump(claim))
      duplicate['source']['id'] = 'second-source'
      duplicate['members'][0]['initialInputId'] = '3' * 32
      result['sessions'] << duplicate
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(JSON.generate(result), root: state, workspaces: [workspace]) }
    end
  end

  def test_helper_result_rejects_empty_success_or_ambiguous_protocol
    invalid = [nil, '', 'null', '[]', 'true', 'false', '1', '"value"', '{}', '{"schema":1,"required":true,"sessions":[]}',
      '{"schema":1,"required":false,"sessions":[],"extra":1}',
      '{"schema":1,"schema":1,"required":false,"sessions":[]}',
      '{"schema":1,"required":false,"sessions":null}',
      '{"schema":1,"required":false,"sessions":[]}{}']
    invalid.each do |data|
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(data, root: '/unused', workspaces: []) }
    end
  end

  def test_workflow_contract_requires_complete_exact_bundle
    Dir.mktmpdir('pi-workflow-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      expected = WorkspaceSessionBackends::WORKFLOW_FIELDS
      invalid = [{}]
      expected.each do |field, version|
        invalid << expected.reject { |key, _| key == field }
        [nil, false, '1', 1.0, 0, 3].each { |value| invalid << expected.merge(field => value) }
      end
      %w[policy conversationSchema].each do |field|
        invalid << expected.merge('piToolPolicy' => expected.fetch('piToolPolicy').reject { |key, _| key == field })
        invalid << expected.merge('piToolPolicy' => expected.fetch('piToolPolicy').merge(field => nil))
      end
      invalid << expected.merge('piToolPolicy' => expected.fetch('piToolPolicy').merge('unknown' => 1))
      invalid.each do |contract|
        bytes = JSON.generate(contract)
        File.write(path, bytes)
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_workflow_contract!(package) }
        assert_equal(bytes, File.binread(path))
      end
      bytes = JSON.generate(expected.merge('unrelated' => true))
      File.write(path, bytes)
      WorkspaceSessionBackends.require_workflow_contract!(package)
      File.write(path, bytes.sub('"policy":1', '"policy":1,"policy":1'))
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_workflow_contract!(package) }
    end
  end

  def test_workflow_claims_bind_original_fence_after_retirement
    Dir.mktmpdir('pi-workflow-claims-') do |root|
      workspace, state = File.join(root, 'workspace'), File.join(root, 'state')
      paths = write_pi_creation_records(state, workspace, pi_creation_records(workspace), receipt: false)
      File.unlink(paths.fetch(:binding))
      %w[questions plans tool_policy role_instructions].each do |kind|
        claim = {'source' => {'kind' => kind, 'id' => 'f' * 64},
          'session' => {'workspace' => workspace, 'slug' => 'example', 'incarnation' => 'a' * 64},
          'members' => [{'address' => 'lead', 'incarnation' => 'b' * 64, 'nativeId' => 'native-lead', 'creationId' => 'd' * 64}]}
        result = {'schema' => 1, 'required' => true, 'sessions' => [claim]}
        WorkspaceSessionBackends.team_preflight!(JSON.generate(result), root: state, workspaces: [workspace])
        claim['members'][0]['creationId'] = 'e' * 64
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(JSON.generate(result), root: state, workspaces: [workspace]) }
      end
    end
  end

  def test_role_instructions_contract_requires_scalar_one_and_complete_workflow
    Dir.mktmpdir('pi-role-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      expected = WorkspaceSessionBackends::WORKFLOW_FIELDS.merge('piRoleInstructions' => 1)
      invalid = [expected.reject { |key, _| key == 'piRoleInstructions' }]
      [nil, false, '1', 1.0, 0, 2, {}, []].each { |value| invalid << expected.merge('piRoleInstructions' => value) }
      WorkspaceSessionBackends::WORKFLOW_FIELDS.each_key { |field| invalid << expected.reject { |key, _| key == field } }
      invalid.each do |contract|
        bytes = JSON.generate(contract)
        File.write(path, bytes)
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_role_instructions_contract!(package) }
        assert_equal(bytes, File.binread(path))
      end
      bytes = JSON.generate(expected.merge('unrelated' => true))
      File.write(path, bytes)
      WorkspaceSessionBackends.require_role_instructions_contract!(package)
      File.write(path, bytes.sub('"piRoleInstructions":1', '"piRoleInstructions":1,"piRoleInstructions":1'))
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_role_instructions_contract!(package) }
    end
  end

  def test_mapping_workflow_claims_require_exact_full_member_and_original_fence
    Dir.mktmpdir('pi-role-claims-') do |root|
      workspace, state = File.join(root, 'workspace'), File.join(root, 'state')
      records = pi_creation_records(workspace)
      paths = write_pi_creation_records(state, workspace, records, receipt: false)
      File.unlink(paths.fetch(:binding))
      %w[tool_policy role_instructions].each do |kind|
        claim = {'source' => {'kind' => kind, 'id' => 'f' * 64},
          'session' => {'workspace' => workspace, 'slug' => 'example', 'incarnation' => 'a' * 64},
          'members' => [{'address' => 'lead', 'incarnation' => 'b' * 64, 'nativeId' => 'native-lead', 'creationId' => 'd' * 64}]}
        check = lambda do |value|
          WorkspaceSessionBackends.team_preflight!(JSON.generate('schema' => 1, 'required' => true, 'sessions' => [value]), root: state, workspaces: [workspace])
        end
        check.call(claim)
        invalid = []
        %w[creationId nativeId].each do |field|
          copy = Marshal.load(Marshal.dump(claim))
          copy['members'][0].delete(field)
          invalid << copy
        end
        copy = Marshal.load(Marshal.dump(claim))
        copy['lead'] = copy['members'].shift.reject { |key, _| %w[address creationId].include?(key) }
        invalid << copy
        copy = Marshal.load(Marshal.dump(claim))
        copy['source']['id'] = 'not-a-path-sha'
        invalid << copy
        invalid.each { |value| assert_raises(WorkspaceSessionBackends::Error) { check.call(value) } }
        original = File.binread(paths.fetch(:completion))
        File.unlink(paths.fetch(:completion))
        assert_raises(WorkspaceSessionBackends::Error) { check.call(claim) }
        File.write(paths.fetch(:completion), original, mode: 'w', perm: 0600)
        journal = JSON.parse(File.binread(paths.fetch(:journal)))
        journal['state'] = 'creating'
        File.write(paths.fetch(:journal), JSON.generate(journal), mode: 'w', perm: 0600)
        assert_raises(WorkspaceSessionBackends::Error) { check.call(claim) }
        File.write(paths.fetch(:journal), JSON.generate(records[:journal]), mode: 'w', perm: 0600)
      end
    end
  end

  def test_workflow_namespaces_cannot_bypass_empty_registry
    Dir.mktmpdir('pi-workflow-orphan-') do |root|
      %w[pi-questions pi-plans].each do |name|
        path = File.join(root, name)
        Dir.mkdir(path, 0700)
        WorkspaceSessionBackends.require_empty_team_namespaces!(root)
        File.write(File.join(path, 'lost.json'), '{}', mode: 'w', perm: 0600)
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_empty_team_namespaces!(root) }
        File.unlink(File.join(path, 'lost.json'))
      end
    end
  end
end
