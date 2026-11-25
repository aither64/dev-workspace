# frozen_string_literal: true

require 'minitest/autorun'
require 'tmpdir'
require_relative '../libexec/workspace-session-backends'
require_relative 'support/pi_creation_fixture'

class WorkspaceSessionBackendsTest < Minitest::Test
  include PiCreationFixture

  def test_pi_lifecycle_capture_keeps_archived_and_removed_scopes_after_same_slug_recreation
    Dir.mktmpdir('pi-lifecycle-reader-') do |root|
      workspace, state, slug = File.join(root, 'workspace'), File.join(root, 'state'), '2026-06-06-pi-lifecycle'
      records = pi_creation_records(workspace, slug:)
      paths = write_pi_creation_records(state, workspace, records)
      runtime = {'Session' => {'Workspace' => workspace, 'Slug' => slug, 'Incarnation' => 'a' * 64},
        'InstanceID' => '11111111-1111-4111-8111-111111111111', 'RequestID' => '22222222-2222-4222-8222-222222222222', 'Digest' => 'f' * 64, 'Generation' => 'e' * 64}
      authority = {'schema' => 2, 'backend' => 'pi', 'backend_contract' => 1, 'workspace' => workspace, 'slug' => slug,
        'session_incarnation' => 'a' * 64, 'binding_revision' => 2, 'generation' => 'e' * 64, 'state' => 'ready'}
      authority_path = File.join(root, 'authority.json')
      File.write(authority_path, JSON.generate(authority), mode: 'w', perm: 0600)
      originals = {'binding' => paths[:binding], 'creation' => paths[:journal], 'completion' => paths[:completion],
        'companion' => paths[:intent], 'authority' => authority_path}
      selectors = originals.map { |name, path| {'Name' => name, 'Path' => path, 'SHA256' => Digest::SHA256.file(path).hexdigest} }
      capture = pi_lifecycle_capture_record(records, authority, runtime, selectors:)
      File.unlink(authority_path) # Retained proof outlives the live projection.
      scope = {'workspace' => workspace, 'slug' => slug, 'incarnation' => 'a' * 64}
      claim = {'source' => {'kind' => 'role_instructions', 'id' => 'f' * 64}, 'session' => scope,
        'members' => [{'address' => 'lead', 'incarnation' => 'b' * 64, 'nativeId' => 'native-lead', 'creationId' => 'd' * 64}]}
      output = {'schema' => 1, 'required' => true, 'sessions' => [claim]}
      # Active evidence is still joined to its original creation fence.
      WorkspaceSessionBackends.team_preflight!(JSON.generate(output), root: state, workspaces: [workspace])
      id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      archive = File.join(state, 'archived-worktrees', id, slug, '4' * 64, 'lifecycle.json')
      removal = File.join(state, 'removed', id, "stamp-#{slug}-fixture", 'recovery.json')
      [archive, removal].each_with_index do |path, index|
        FileUtils.mkdir_p(File.dirname(path), mode: 0700)
        File.write(path, JSON.generate('schema' => index.zero? ? 5 : 2, 'workspace' => workspace, 'slug' => slug,
          'operation_id' => (index.zero? ? '4' : '5') * 64, 'pi' => capture), mode: 'w', perm: 0600)
        assert_equal(capture, WorkspaceSessionBackends.lifecycle_captures(state, workspace, slug).first)
        # The new current slug cannot substitute its binding/native ID for the
        # original archived/removed session's retained workflow claim.
        fresh = JSON.parse(JSON.generate(pi_creation_records(workspace, slug:)).gsub('a' * 64, '8' * 64).gsub('d' * 64, '7' * 64).gsub('native-lead', 'new-native-lead')).transform_keys(&:to_sym)
        write_pi_creation_records(state, workspace, fresh)
        WorkspaceSessionBackends.team_preflight!(JSON.generate(output), root: state, workspaces: [workspace])
        wrong = Marshal.load(Marshal.dump(output))
        wrong['sessions'][0]['members'][0]['nativeId'] = 'new-native-lead'
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(JSON.generate(wrong), root: state, workspaces: [workspace]) }
        File.unlink(path)
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(JSON.generate(output), root: state, workspaces: [workspace]) }
      end
    end
  end

  def test_pi_lifecycle_capability_refuses_old_or_noninteger_packages
    Dir.mktmpdir('pi-lifecycle-capability-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      [{}, *[nil, false, '1', 1.0, 0, 2].map { |v| {'piLifecycle' => v} }].each do |contract|
        File.write(path, JSON.generate(contract))
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_lifecycle_contract!(package) }
      end
      File.write(path, JSON.generate('piLifecycle' => 1))
      WorkspaceSessionBackends.require_lifecycle_contract!(package)
    end
  end

  def test_pi_fork_target_requires_creation_roster_and_native_readers
    Dir.mktmpdir('pi-fork-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      complete = {'piFork' => 1, 'piCreationSchema' => 5, 'piTeamRecords' => {'roster' => 4}}
      File.write(path, JSON.generate(complete))
      WorkspaceSessionBackends.require_fork_contract!(package)
      [{'piFork' => 0}, {'piCreationSchema' => 4}, {'piTeamRecords' => {'roster' => 3}}].each do |delta|
        File.write(path, JSON.generate(complete.merge(delta)))
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_fork_contract!(package) }
      end
    end
  end

  def test_pi_fork_ready_and_later_add_join_real_creation_records
    [false, true].product([4,5]).each do |appended, version|
      Dir.mktmpdir('pi-fork-record-join-') do |root|
        workspace, state = File.join(root, 'workspace'), File.join(root, 'state')
        records = pi_fork_join_records(workspace)
        intent = records.fetch(:intent)
        roster = {'schema' => version, 'forkCreationId' => intent['receipt_id'], 'workspace' => workspace, 'slug' => 'example',
          'sessionIncarnation' => intent['session_incarnation'], 'leadIncarnation' => intent['conversations'][0]['incarnation'],
          'members' => [{'address' => 'implementer0', 'incarnation' => 'c' * 64, 'creationId' => '8' * 64,
            'fork' => {'rootCreationId' => intent['receipt_id'], 'nativeId' => intent['fork']['members'][1]['native_id']}}]}
        if version == 5
          roster['members'][0].merge!('state' => 'removed', 'removedAt' => Time.now.utc.iso8601)
        end
        if appended
          addition = {'address' => 'reviewer0', 'incarnation' => '7' * 64, 'creationId' => '9' * 64,
            'addition' => {'rootCreationId' => intent['receipt_id'], 'requestId' => '22222222-2222-4222-8222-222222222222',
              'priorBindingRevision' => 2, 'payloadDigest' => '9' * 64, 'generation' => 'e' * 64,
              'selection' => {'address' => 'reviewer0', 'incarnation' => '7' * 64}}}
          roster['members'] << addition
          records[:binding]['revision'] = 3
          records[:binding]['conversations'] << addition.slice('address', 'incarnation').merge('state' => 'creating')
        end
        paths = write_pi_creation_records(state, workspace, records)
        id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
        roster_path = File.join(state, 'codex-teams', id, 'example.json')
        FileUtils.mkdir_p(File.dirname(roster_path), mode: 0700)
        File.write(roster_path, JSON.generate(roster), mode: 'w', perm: 0600)
        before = (paths.values + [roster_path]).to_h { |path| [path, File.binread(path)] }
        assert(WorkspaceSessionBackends.inspect_pi_state!(state, workspace, 'example'))
        proof = WorkspaceSessionBackends.retained_creation_fence!(state, workspace, 'example')
        assert_equal(intent, proof.fetch(:intent))
        before.each { |path, bytes| assert_equal(bytes, File.binread(path)) }
        faults = %w[root_creation_id old_roster fork_root fork_native binding_native]
        faults += %w[addition_root addition_fork] if appended
        faults.each do |fault|
          changed = Marshal.load(Marshal.dump(roster))
          binding = Marshal.load(Marshal.dump(records[:binding]))
          case fault
          when 'root_creation_id' then changed['members'][0]['creationId'] = intent['receipt_id']
          when 'old_roster' then changed['schema'] = 3
          when 'fork_root' then changed['forkCreationId'] = '0' * 64
          when 'fork_native' then changed['members'][0]['fork']['nativeId'] = 'wrong-native'
          when 'binding_native' then binding['conversations'][1]['native_id'] = 'wrong-native'
          when 'addition_root' then changed['members'].last['addition']['rootCreationId'] = '0' * 64
          when 'addition_fork' then changed['members'].last['fork'] = changed['members'][0]['fork']
          end
          File.write(roster_path, JSON.generate(changed))
          File.write(paths.fetch(:binding), JSON.generate(binding))
          assert_raises(WorkspaceSessionBackends::Error, fault) { WorkspaceSessionBackends.inspect_pi_state!(state, workspace, 'example') }
          assert_raises(WorkspaceSessionBackends::Error, fault) { WorkspaceSessionBackends.retained_creation_fence!(state, workspace, 'example') }
        end
      end
    end
  end


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
      [nil, false, '2', 2.0, 0, 1, 4].each { |value| invalid << expected.merge('piPreparationSchema' => value) }
      invalid.each do |value|
        bytes = JSON.generate(value)
        File.write(path, bytes)
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_preparation_contract!(package) }
        assert_equal(bytes, File.binread(path))
      end
      File.write(path, JSON.generate(expected))
      WorkspaceSessionBackends.require_preparation_contract!(package)
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_preparation_contract!(package, required: 3) }
      File.write(path, JSON.generate(expected.merge('piPreparationSchema' => 3)))
      WorkspaceSessionBackends.require_preparation_contract!(package, required: 2)
      WorkspaceSessionBackends.require_preparation_contract!(package, required: 3)
    end
  end

  def test_current_runtime_projection_joins_original_or_successor_authority
    records = pi_creation_records('/fixture/workspace')
    selected, journal = records.values_at(:binding, :journal)
    scope = {'workspace' => selected.fetch('workspace'), 'slug' => selected.fetch('slug'), 'incarnation' => selected.fetch('session_incarnation')}
    original = {'session' => scope, 'generation' => journal.fetch('native_generation'), 'phase' => 'starting'}
    output = {'schema' => 3, 'required' => false, 'sessions' => [], 'piPreparations' => [], 'piRuntimes' => [original]}
    result = WorkspaceSessionBackends.team_preflight!(JSON.generate(output), root: '/unused', workspaces: ['/fixture/workspace'])
    assert_equal(output, result)
    WorkspaceSessionBackends.current_runtime_projection!(selected, journal, original, nil)
    authority = {'schema' => 2, 'backend' => 'pi', 'backend_contract' => 1, 'workspace' => selected.fetch('workspace'), 'slug' => selected.fetch('slug'),
      'session_incarnation' => selected.fetch('session_incarnation'), 'binding_revision' => selected.fetch('revision'), 'generation' => original.fetch('generation'), 'state' => 'ready'}
    WorkspaceSessionBackends.current_runtime_projection!(selected, journal, original, authority)
    successor = original.merge('generation' => '7' * 64)
    invalid = [[nil, nil], [original.merge('phase' => 'preparing'), nil], [successor, nil], [successor, authority],
      [successor, authority.merge('generation' => '7' * 64, 'state' => 'starting')],
      [successor.merge('session' => scope.merge('incarnation' => 'f' * 64)), authority.merge('generation' => '7' * 64)]]
    invalid.each do |reference, live|
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.current_runtime_projection!(selected, journal, reference, live) }
    end
    WorkspaceSessionBackends.current_runtime_projection!(selected, journal, successor, authority.merge('generation' => '7' * 64))
    conflicting = [original, original.merge('session' => scope.merge('incarnation' => 'f' * 64))].sort_by { |row| row.fetch('session').values_at('workspace', 'slug', 'incarnation') }
    [output.merge('piRuntimes' => [original, original]), output.merge('piRuntimes' => conflicting), output.merge('piRuntimes' => [original.merge('extra' => true)]),
      output.reject { |key, _| key == 'piRuntimes' }, output.merge('piRuntimes' => [original.merge('generation' => 'bad')]),
      output.merge('piRuntimes' => [original.merge('session' => scope.merge('workspace' => '/foreign'))])].each do |value|
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(JSON.generate(value), root: '/unused', workspaces: ['/fixture/workspace']) }
    end
  end

  def test_team_conflict_recovery_transition_is_exact_and_conditional
    records = pi_creation_records('/fixture/workspace')
    selected, journal = records.values_at(:binding, :journal)
    scope = {'workspace' => selected.fetch('workspace'), 'slug' => selected.fetch('slug'), 'incarnation' => selected.fetch('session_incarnation')}
    proof = {'requestId' => '22222222-2222-4222-8222-222222222222', 'sourceGeneration' => journal.fetch('native_generation'), 'bindingRevision' => selected.fetch('revision'), 'phase' => 'settled'}
    reference = {'session' => scope, 'generation' => '7' * 64, 'phase' => 'starting', 'teamConflictRecovery' => proof}
    authority = {'schema' => 2, 'backend' => 'pi', 'backend_contract' => 1, 'workspace' => scope.fetch('workspace'), 'slug' => scope.fetch('slug'),
      'session_incarnation' => scope.fetch('incarnation'), 'binding_revision' => selected.fetch('revision'), 'generation' => proof.fetch('sourceGeneration'), 'state' => 'ready'}
    output = {'schema' => 3, 'required' => false, 'sessions' => [], 'piPreparations' => [], 'piRuntimes' => [reference]}
    parse = ->(value) { WorkspaceSessionBackends.team_preflight!(JSON.generate(value), root: '/unused', workspaces: [scope.fetch('workspace')]) }
    assert_equal(output, parse.call(output))
    original = JSON.generate([selected, journal, reference, authority])
    assert(WorkspaceSessionBackends.current_runtime_projection!(selected, journal, reference, authority))
    refute(WorkspaceSessionBackends.current_runtime_projection!(selected, journal, reference, authority.merge('generation' => reference.fetch('generation'))))
    [nil, authority.merge('state' => 'starting'), authority.merge('generation' => '8' * 64), authority.merge('binding_revision' => 3)].each do |live|
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.current_runtime_projection!(selected, journal, reference, live) }
    end
    [nil, proof.merge('extra' => true), proof.reject { |key, _| key == 'requestId' }, proof.merge('requestId' => 'bad'),
     proof.merge('sourceGeneration' => reference.fetch('generation')), proof.merge('bindingRevision' => 2.0), proof.merge('bindingRevision' => 0), proof.merge('phase' => 'prepared'), proof.merge('phase' => 'complete')].each do |bad|
      assert_raises(WorkspaceSessionBackends::Error) { parse.call(output.merge('piRuntimes' => [reference.merge('teamConflictRecovery' => bad)])) }
    end
    assert_raises(WorkspaceSessionBackends::Error) { parse.call(output.merge('piRuntimes' => [reference.merge('phase' => 'preparing')])) }
    assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.current_runtime_projection!(selected, journal, reference.merge('teamConflictRecovery' => proof.merge('bindingRevision' => 3)), authority) }
    duplicate = JSON.generate(output).sub('"phase":"settled"', '"phase":"settled","phase":"settled"')
    assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.team_preflight!(duplicate, root: '/unused', workspaces: [scope.fetch('workspace')]) }
    Dir.mktmpdir('pi-transition-contract') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      [{}, *[nil, false, '1', 1.0, 0, 2].map { |v| {'piTeamConflictRecovery' => 1, 'piTeamConflictRecoveryTransition' => v} }, {'piTeamConflictRecoveryTransition' => 1}].each do |contract|
        File.write(path, JSON.generate(contract))
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_team_conflict_recovery_transition_contract!(package) }
      end
      File.write(path, JSON.generate('piTeamConflictRecovery' => 1, 'piTeamConflictRecoveryTransition' => 1))
      WorkspaceSessionBackends.require_team_conflict_recovery_transition_contract!(package)
    end
    assert_equal(original, JSON.generate([selected, journal, reference, authority]))
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
        [nil, false, '2', 2.0, 0, 3].each { |value| invalid << expected.merge(key => value) unless key == 'roster' && value == 3 }
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
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_team_contract!(package, required: 3) }
      File.write(path, JSON.generate('piTeamRecords' => expected.merge('roster' => 3)))
      WorkspaceSessionBackends.require_team_contract!(package, required: 3)
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_team_contract!(package, required: 5) }
      File.write(path, JSON.generate('piTeamRecords' => expected.merge('roster' => 5)))
      WorkspaceSessionBackends.require_team_contract!(package, required: 5)
    end
  end

  def test_added_roster_requires_exact_binding_prefix_and_original_creation
    Dir.mktmpdir('pi-added-roster-') do |root|
      workspace, state = File.join(root, 'workspace'), File.join(root, 'state')
      records = pi_creation_records(workspace)
      records[:binding]['revision'] = 3
      addition = {'address' => 'reviewer0', 'incarnation' => '7' * 64, 'creationId' => '8' * 64,
        'addition' => {'rootCreationId' => records[:intent]['receipt_id'], 'requestId' => '22222222-2222-4222-8222-222222222222',
          'priorBindingRevision' => 2, 'payloadDigest' => '9' * 64, 'generation' => 'e' * 64,
          'selection' => {'address' => 'reviewer0', 'incarnation' => '7' * 64}}}
      records[:binding]['conversations'] << addition.slice('address', 'incarnation').merge('state' => 'creating')
      paths = write_pi_creation_records(state, workspace, records)
      id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      path = File.join(state, 'codex-teams', id, 'example.json')
      FileUtils.mkdir_p(File.dirname(path), mode: 0700)
      roster = {'schema' => 3, 'workspace' => workspace, 'slug' => 'example', 'sessionIncarnation' => 'a' * 64, 'leadIncarnation' => 'b' * 64,
        'members' => [{'address' => 'implementer0', 'incarnation' => 'c' * 64, 'creationId' => 'd' * 64}, addition]}
      File.write(path, JSON.generate(roster), mode: 'w', perm: 0600)
      before = paths.to_h { |key, value| [key, File.binread(value)] }
      assert(WorkspaceSessionBackends.inspect_pi_state!(state, workspace, 'example'))
      WorkspaceSessionBackends.retained_creation_fence!(state, workspace, 'example')
      before.each { |key, bytes| assert_equal(bytes, File.binread(paths.fetch(key))) }
      roster['members'].last['addition']['rootCreationId'] = '0' * 64
      File.write(path, JSON.generate(roster))
      assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.inspect_pi_state!(state, workspace, 'example') }
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

class WorkspaceSessionBackendsTest
  def test_portal_delivery_target_contract_requires_exact_scalar_one
    Dir.mktmpdir('portal-delivery-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      [{}, *[nil, false, '1', 1.0, 0, 2, {}, []].map { |value| {'piPortalDelivery' => value} }].each do |contract|
        File.write(path, JSON.generate(contract))
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_portal_delivery_contract!(package) }
      end
      File.write(path, JSON.generate('piPortalDelivery' => 1))
      WorkspaceSessionBackends.require_portal_delivery_contract!(package)
    end
  end

  def test_native_input_delivery_target_contract_requires_exact_scalar_one
    Dir.mktmpdir('pi-input-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      [{}, *[nil, false, '1', 1.0, 0, 2, {}, []].map { |value| {'piInputDelivery' => value} }].each do |contract|
        File.write(path, JSON.generate(contract))
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_input_delivery_contract!(package) }
      end
      File.write(path, JSON.generate('piInputDelivery' => 1))
      WorkspaceSessionBackends.require_input_delivery_contract!(package)
    end
  end

  def test_input_recovery_target_contract_requires_exact_scalar_one
    Dir.mktmpdir('pi-recovery-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      [{}, *[nil, false, '1', 1.0, 0, 2, {}, []].map { |value| {'piInputRecovery' => value} }].each do |contract|
        File.write(path, JSON.generate(contract))
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_input_recovery_contract!(package) }
      end
      File.write(path, JSON.generate('piInputRecovery' => 1))
      WorkspaceSessionBackends.require_input_recovery_contract!(package)
    end
  end

  def test_team_conflict_recovery_target_contract_requires_exact_scalar_one
    Dir.mktmpdir('pi-recovery-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      [{}, *[nil, false, '1', 1.0, 0, 2, {}, []].map { |value| {'piTeamConflictRecovery' => value} }].each do |contract|
        File.write(path, JSON.generate(contract))
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_team_conflict_recovery_contract!(package) }
      end
      File.write(path, JSON.generate('piTeamConflictRecovery' => 1))
      WorkspaceSessionBackends.require_team_conflict_recovery_contract!(package)
    end
  end
  def test_team_conflict_terminal_refusal_requires_both_exact_reader_capabilities
    Dir.mktmpdir('pi-terminal-refusal-contract-') do |package|
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(path))
      assert_includes WorkspaceSessionBackends::TEAM_SOURCES, 'team_conflict_terminal_refusal'
      %w[piTeamConflictRecovery piTeamConflictTerminalRefusal].each do |key|
        [nil, false, '1', 1.0, 0, 2, {}, []].each do |value|
          contract = {'piTeamConflictRecovery' => 1, 'piTeamConflictTerminalRefusal' => 1}
          contract[key] = value
          File.write(path, JSON.generate(contract))
          assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_team_conflict_terminal_refusal_contract!(package) }
        end
        File.write(path, JSON.generate({'piTeamConflictRecovery' => 1, 'piTeamConflictTerminalRefusal' => 1}.reject { |k, _| k == key }))
        assert_raises(WorkspaceSessionBackends::Error) { WorkspaceSessionBackends.require_team_conflict_terminal_refusal_contract!(package) }
      end
      File.write(path, JSON.generate('piTeamConflictRecovery' => 1, 'piTeamConflictTerminalRefusal' => 1))
      WorkspaceSessionBackends.require_team_conflict_terminal_refusal_contract!(package)
    end
  end

end
