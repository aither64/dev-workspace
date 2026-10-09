# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class WorkspaceHostTest < Minitest::Test
  def test_recovery_state_blocks_a_predecessor_that_ignores_holds_and_receipts
    with_transition_host do |host, paths|
      state = host.instance_variable_get(:@state)
      predecessor = make_package(paths.fetch(:root), 'predecessor', recovery_policy: nil)
      compatible = make_package(paths.fetch(:root), 'compatible')
      host.send(:require_compatible_session_recovery!, predecessor)
      %w[session-recovery submissions].each do |kind|
        directory = File.join(state, kind, 'scope')
        FileUtils.mkdir_p(directory)
        record = File.join(directory, 'example.json')
        File.write(record, '{}')
        error = assert_raises(DevWorkspaceHost::Error) do
          host.send(:require_compatible_session_recovery!, predecessor)
        end
        assert_includes(error.message, 'execution holds and durable submission receipts')
        host.send(:require_compatible_session_recovery!, compatible)
        File.unlink(record)
      end
    end
  end

  def test_switch_retains_codex_with_the_profile_generation_and_restarts_as_one_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(1, host.send(:profile_generation))
      assert_equal(
        File.realpath(paths.fetch(:system_codex)),
        File.realpath(host.send(:generation_codex, 1))
      )
      assert_equal(File.realpath(paths.fetch(:system_codex)), File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:configured])
      assert_includes(host.events, [:consumers_restarted])
    end
  end

  def test_candidate_switch_uses_the_exact_source_and_accepts_a_ready_expanded_team
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      previous = File.realpath(host.instance_variable_get(:@profile))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.executing_package = host.candidate
      host.registration_argv = [] # This case exercises the idle predecessor dispatcher.

      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-ready.creation.json')
      preset = {
        'id' => 'delegated', 'name' => 'Full team', 'description' => 'Direct threads',
        'catalogDigest' => 'a' * 64, 'teamDigest' => 'b' * 64,
        'leadModel' => 'gpt-6-sol', 'leadEffort' => 'high',
        'leadInstructions' => "Coordinate the team.\n", 'roles' => %w[lead architect0],
        'members' => [{
          'role' => 'architect', 'address' => 'architect0', 'behavior' => 'designer',
          'purpose' => 'design', 'access' => 'read_only',
          'instructions' => 'Write assigned design artifacts.',
          'model' => 'gpt-6-sol', 'reasoningEffort' => 'xhigh'
        }]
      }
      File.write(journal, JSON.generate(
        'schema' => 3, 'slug' => '2026-09-07-ready', 'goal_sha256' => 'c' * 64,
        'run_codex' => true, 'state' => 'ready', 'model' => 'gpt-6-sol',
        'effort' => 'high', 'direct_team' => preset
      ))
      File.chmod(0o600, journal)
      manifest_directory = File.join(workspace, 'work', '2026-09-07-ready')
      FileUtils.mkdir_p(manifest_directory)
      socket = host.send(:instance_runtime, host.send(:registry).entries.fetch(0)).fetch(:codex)
      File.write(File.join(manifest_directory, 'portal.yml'), YAML.dump(
        'codex' => { 'thread_id' => 'thread-one', 'socket_path' => socket },
        'creation' => { 'state' => 'ready' }
      ))
      host.delegate_quiesce = true

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(previous, File.realpath(host.instance_variable_get(:@profile)))
      host.quiesce_output = "quiesced terminal: 2026-09-07-ready\n"
      host.fail_set_before_profile = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
      assert_equal(previous, File.realpath(host.instance_variable_get(:@profile)))
      assert_includes(host.events, [:sessions_restored, previous])
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
      invocation = host.events.find { |event| event.first == :quiesce_invocation }.fetch(1)
      assert_equal(File.join(previous, 'libexec/workspace-portal/dev-session'), invocation.fetch(0))
      assert_equal(previous, invocation.fetch(invocation.index('--expected-host-generation') + 1))
      assert_equal(File.realpath(host.candidate), File.realpath(host.instance_variable_get(:@profile)))
      assert_equal(2, host.send(:profile_generation))
    end
  end

  def test_candidate_switch_rejects_a_different_source_package
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      previous = File.realpath(host.instance_variable_get(:@profile))
      host.executing_package = make_package(paths.fetch(:root), 'unmatched-package')
      host.candidate = make_package(paths.fetch(:root), 'package-two')

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
      assert_equal(previous, File.realpath(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, 'exact unselected source package')
    end
  end

  def test_candidate_switch_requires_an_installed_profile
    with_transition_host do |host, paths|
      host.executing_package = host.candidate

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, 'selected workspace package')
    end
  end

  def test_candidate_switch_still_refuses_an_unfinished_creation
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      previous = File.realpath(host.instance_variable_get(:@profile))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.executing_package = host.candidate
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-pending.creation.json')
      File.write(journal, JSON.generate(
        'schema' => 1, 'slug' => '2026-09-07-pending',
        'goal_sha256' => 'b' * 64, 'run_codex' => true,
        'state' => 'creating', 'tmux_identity' => 'a' * 64
      ))
      File.chmod(0o600, journal)

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
      assert_equal(previous, File.realpath(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, '(creation)')
    end
  end

  def test_switch_rejects_a_nonactivating_profile_change
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))

      assert_equal(
        1,
        host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--no-start'])
      )
      refute(File.exist?(host.instance_variable_get(:@profile)))
    end
  end

  def test_public_rollback_refuses_the_selected_forward_generation
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(1, host.send(:profile_generation))
      assert_includes(host.instance_variable_get(:@err).string, 'forward-only')
    end
  end

  def test_switch_refuses_unfinished_session_lifecycle_operations
    DevWorkspaceHost::LIFECYCLE_JOURNALS.each do |journal|
      kind = journal.fetch('name')
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        workspace = host.send(:registry).entries.fetch(0).fetch('root')
        locks = File.join(workspace, 'worktrees', '.locks')
        FileUtils.mkdir_p(locks)
        File.write(File.join(locks, "2026-09-07-pending.#{kind}.json"), "{}\n")

        assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        refute(File.exist?(host.instance_variable_get(:@profile)))
      end
    end
  end

  def test_switch_refuses_an_unfinished_session_creation
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-pending.creation.json')
      File.write(
        journal,
        JSON.generate(
          'schema' => 1,
          'slug' => '2026-09-07-pending',
          'goal_sha256' => 'b' * 64,
          'run_codex' => true,
          'state' => 'creating',
          'tmux_identity' => 'a' * 64
        ) + "\n"
      )
      File.chmod(0o600, journal)

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, '(creation)')
    end
  end

  def test_switch_refuses_an_unfinished_managed_session_creation
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-managed.creation.json')
      binding_digest = 'c' * 64
      File.write(
        journal,
        JSON.generate(
          'schema' => 2,
          'slug' => '2026-09-07-managed',
          'goal_sha256' => 'b' * 64,
          'run_codex' => true,
          'state' => 'creating',
          'tmux_identity' => 'a' * 64,
          'model' => 'gpt-6-astra',
          'effort' => 'xhigh',
          'agent_team_binding' => "v1.fixture.#{binding_digest}",
          'agent_team_binding_digest' => binding_digest
        ) + "\n"
      )
      File.chmod(0o600, journal)

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, '(creation)')
    end
  end

  def test_switch_allows_a_completed_session_creation_journal
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-ready.creation.json')
      File.write(
        journal,
        JSON.generate(
          'schema' => 1,
          'slug' => '2026-09-07-ready',
          'goal_sha256' => 'b' * 64,
          'run_codex' => true,
          'state' => 'ready'
        ) + "\n"
      )
      File.chmod(0o600, journal)

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
    end
  end

  def test_switch_fails_closed_on_a_creating_journal_without_a_tmux_identity
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-legacy.creation.json')
      File.write(
        journal,
        JSON.generate(
          'schema' => 1,
          'slug' => '2026-09-07-legacy',
          'goal_sha256' => 'b' * 64,
          'run_codex' => true,
          'state' => 'creating'
        ) + "\n"
      )
      File.chmod(0o600, journal)

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, 'creation journal has an invalid shape')
    end
  end

  def test_switch_refuses_an_unfinished_session_fork
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-07-fork.fork.json'), "{}\n")

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, '(fork)')
    end
  end

  def test_switch_refuses_an_unfinished_session_start
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-07-restart.start.json'), "{}\n")

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, '(start)')
    end
  end

  def test_switch_fails_closed_on_a_malformed_creation_journal
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-broken.creation.json')
      File.write(journal, "{\n")
      File.chmod(0o600, journal)

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'invalid creation journal'
      )
    end
  end

  def test_switch_refuses_a_reserved_agent_team_migration_journal
    with_transition_host do |host, paths|
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(
        File.join(locks, '2026-09-22-pending.agent-teams-migration.json'),
        "reserved for the forward migration\n"
      )

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_includes(host.instance_variable_get(:@err).string, '(agent-teams migration)')
      refute(File.exist?(host.instance_variable_get(:@profile)))
    end
  end

  def test_switch_quiesces_before_changing_the_profile_generation
    with_transition_host do |host, paths|
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      probe = host.events.index { |event| event.first == :registration_probed }
      quiesce = host.events.index([:sessions_quiesced])
      profile_set = host.events.index { |event| event.first == :profile_set }
      refute_nil(probe)
      refute_nil(quiesce)
      refute_nil(profile_set)
      assert_operator(probe, :<, quiesce)
      assert_operator(quiesce, :<, profile_set)
    end
  end

  def test_switch_refuses_incompatible_cluster_helpers_while_cluster_state_exists
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'beta', 'clusters',
          '2026-09-07-active-cluster'
        )
      )
      incompatible = make_package(paths.fetch(:root), 'package-incompatible', cluster_contract: false)
      host.candidate = incompatible

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'target package has no compatible cluster-state contract'
      )
    end
  end

  def test_switch_accepts_a_stricter_cluster_transition_policy
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      cluster = File.join(
        workspace, '.dev-clusters', 'beta', 'clusters',
        '2026-09-07-active-cluster'
      )
      FileUtils.mkdir_p(cluster)
      File.write(File.join(cluster, 'socket-dir'), "/tmp/workspace-scoped-socket\n")
      host.candidate = make_package(
        paths.fetch(:root),
        'package-stricter-policy',
        transition_policy: DevWorkspaceHost::RUNTIME_CONTRACT.fetch(
          'developmentClusterTransitionPolicy'
        ) + 1
      )

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
    end
  end

  def test_policy_two_predecessor_adopts_a_schema_one_policy_three_package
    with_cluster_transition_policy(2) do
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        cluster = recorded_cluster!(host)
        host.candidate = make_package(paths.fetch(:root), 'package-policy-three', transition_policy: 3)
        probe_cluster_adoption!(host.candidate)

        assert_equal(1, DevWorkspaceHost::RUNTIME_CONTRACT.fetch('developmentClusterStateSchema'))
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        assert_equal(1, host.send(:profile_generation))
        assert(File.file?(File.join(cluster, 'adoption-called')))
      end
    end
  end

  def test_policy_three_refuses_policy_two_before_adoption_or_profile_changes
    [false, true].each do |held|
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        previous = File.realpath(host.instance_variable_get(:@profile))
        cluster = recorded_cluster!(host)
        if held
          File.write(File.join(cluster, 'maintenance-hold.json'), JSON.generate(
            'version' => 1, 'mode' => 'maintenance', 'phase' => 'held'
          ))
        end
        original_files = Dir.children(cluster).to_h { |name| [name, File.binread(File.join(cluster, name))] }
        host.candidate = make_package(paths.fetch(:root), 'package-old-policy', transition_policy: 2)
        probe_cluster_adoption!(host.candidate)
        host.events.clear

        assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        assert_equal(previous, File.realpath(host.instance_variable_get(:@profile)))
        assert_equal(1, host.send(:profile_generation))
        refute(host.events.any? { |event| %i[sessions_quiesced profile_set consumers_restarted].include?(event.first) })
        refute(File.exist?(File.join(cluster, 'adoption-called')))
        assert_equal(original_files, Dir.children(cluster).to_h { |name| [name, File.binread(File.join(cluster, name))] })
        assert_includes(host.instance_variable_get(:@err).string, 'no compatible cluster-state contract')
        assert_includes(host.instance_variable_get(:@err).string, 'preserve retained cluster state and select a reviewed compatible package')
        refute_includes(host.instance_variable_get(:@err).string, 'reset these clusters first')
      end
    end
  end

  def test_policy_three_still_defers_equal_and_newer_candidates_to_provider_adoption
    [3, 4].each do |policy|
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        cluster = recorded_cluster!(host)
        host.candidate = make_package(paths.fetch(:root), 'package-refused-adoption', transition_policy: policy)
        probe_cluster_adoption!(host.candidate, exit_status: 1)

        assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        assert(File.file?(File.join(cluster, 'adoption-called')))
        refute(File.exist?(host.instance_variable_get(:@profile)))
        refute_includes(host.events, [:sessions_quiesced])
      end
    end
  end

  def test_candidate_activation_refuses_policy_two_before_provider_adoption
    with_transition_host do |host, paths|
      cluster = recorded_cluster!(host)
      package = make_package(paths.fetch(:root), 'package-old-activation', transition_policy: 2)
      probe_cluster_adoption!(package)
      environment = host.instance_variable_get(:@env).merge('DEV_WORKSPACE_ACTIVATION' => '1')
      error_output = StringIO.new
      activation = ActivationGuardHost.new(package:, env: environment, out: StringIO.new, err: error_output)

      assert_equal(1, activation.run('workspace-host', ['_activate']))
      refute(activation.configured)
      refute(File.exist?(File.join(cluster, 'adoption-called')))
      assert_includes(error_output.string, 'no compatible cluster-state contract')
    end
  end

  def test_policy_three_does_not_restrict_old_policy_when_no_cluster_state_exists
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      host.candidate = make_package(paths.fetch(:root), 'package-no-clusters', transition_policy: 2)

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
    end
  end

  def test_policy_three_keeps_schema_metadata_and_malformed_contract_refusals
    [
      '{',
      { 'developmentClusterStateSchema' => 2 },
      { 'trackingMaxBytes' => 1 },
      { 'developmentClusterTransitionPolicy' => '3' }
    ].each do |invalid|
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        cluster = recorded_cluster!(host)
        package = make_package(paths.fetch(:root), 'package-invalid-contract', transition_policy: 3)
        contract = File.join(package, 'share/workspace-portal/runtime-contract.json')
        value = invalid.is_a?(Hash) ? JSON.generate(JSON.parse(File.binread(contract)).merge(invalid)) : invalid
        File.write(contract, value)
        probe_cluster_adoption!(package)
        host.candidate = package

        assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        refute(File.exist?(host.instance_variable_get(:@profile)))
        refute(File.exist?(File.join(cluster, 'adoption-called')))
        refute_includes(host.events, [:sessions_quiesced])
      end
    end
  end

  def test_switch_refuses_unadoptable_precontract_cluster_state
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'alpha', 'clusters',
          '2026-09-07-active-cluster'
        )
      )

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'pre-contract cluster cannot be adopted'
      )
    end
  end

  def test_switch_refuses_the_password_cluster_without_recorded_socket_identity
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'alpha', 'clusters',
          '2026-08-18-alpha-password-reset'
        )
      )

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'pre-contract cluster cannot be adopted'
      )
    end
  end

  def test_switch_uses_the_installed_predecessor_contract_not_the_invoking_package
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      host.candidate = make_package(paths.fetch(:root), 'package-precontract', cluster_contract: false)
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'alpha', 'clusters',
          '2026-09-07-unadoptable'
        )
      )
      host.candidate = make_package(paths.fetch(:root), 'package-contract')

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'pre-contract cluster cannot be adopted'
      )
    end
  end

  def test_switch_validates_cluster_state_owned_by_a_contract_predecessor
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      cluster = File.join(
        workspace, '.dev-clusters', 'alpha', 'clusters',
        '2026-09-07-contract-state'
      )
      FileUtils.mkdir_p(cluster)
      File.write(File.join(cluster, 'socket-dir'), "/tmp/workspace-scoped-socket\n")
      host.candidate = make_package(paths.fetch(:root), 'package-next-contract')

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(2, host.send(:profile_generation))
    end
  end

  def test_candidate_activation_rechecks_precontract_cluster_adoption
    Dir.mktmpdir('workspace-host-activation-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(
        File.join(
          root, '.dev-clusters', 'beta', 'clusters',
          '2026-09-07-precontract'
        )
      )
      package = make_package(directory, 'candidate')
      error_output = StringIO.new
      environment = host_environment(directory, config:).merge(
        'DEV_WORKSPACE_ACTIVATION' => '1'
      )
      host = ActivationGuardHost.new(
        package:, env: environment, out: StringIO.new, err: error_output
      )

      assert_equal(1, host.run('workspace-host', ['_activate']))
      refute(host.configured)
      assert_includes(error_output.string, 'pre-contract cluster cannot be adopted')
    end
  end

  def test_busy_switch_keeps_the_old_codex_and_retries_only_the_pending_update
    with_transition_host(busy: ['example-workspace/active']) do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(File.realpath(paths.fetch(:old_codex)), File.realpath(host.send(:active_codex)))
      assert_equal(File.realpath(host.candidate), host.send(:pending_codex_update).fetch('package_root'))
      refute_includes(host.events, [:consumers_restarted])

      host.busy = []
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(File.realpath(paths.fetch(:system_codex)), File.realpath(host.send(:active_codex)))
      assert_nil(host.send(:pending_codex_update))
      assert_includes(host.events, [:consumers_restarted])
    end
  end

  def test_terminal_restoration_attempts_every_session_with_one_package_generation
    Dir.mktmpdir('workspace-host-restoration-test') do |directory|
      first = {
        'name' => 'first', 'root' => make_workspace(directory, 'first'),
        'hostname' => 'first.workspace.example.test'
      }
      second = {
        'name' => 'second', 'root' => make_workspace(directory, 'second'),
        'hostname' => 'second.workspace.example.test'
      }
      config = File.join(directory, 'config', 'registry.json')
      target = make_package(directory, 'target-package')
      environment = host_environment(directory, config:)
      profile = environment.fetch('DEV_WORKSPACES_PROFILE')
      FileUtils.mkdir_p(File.dirname(profile))
      File.symlink(target, profile)
      host = RestorationHost.new(
        fail_slug: 'broken', env: environment,
        out: StringIO.new, err: StringIO.new
      )

      error = assert_raises(DevWorkspaceHost::Error) do
        host.send(
          :restore_quiesced_sessions,
          [[first, 'broken'], [second, 'restored']],
          package: target
        )
      end

      assert_includes(error.message, 'first/broken: injected sync failure')
      assert_equal(2, host.invocations.length)
      host.invocations.each do |_environment, command, arguments|
        assert_equal(File.join(target, 'libexec/workspace-portal/dev-session'), command)
        assert_equal(target, arguments.fetch(arguments.index('--expected-host-generation') + 1))
        assert_equal(
          File.join(target, 'bin/workspace-portal'),
          arguments.fetch(arguments.index('--portal-command') + 1)
        )
        configured = arguments.each_index.filter_map do |index|
          arguments[index + 1] if arguments[index] == '--cluster-provider'
        end
        assert_equal(
          [
            "alpha=#{File.join(target, 'libexec/workspace-portal/alpha-devcluster')}",
            "beta=#{File.join(target, 'libexec/workspace-portal/beta-devcluster')}"
          ],
          configured
        )
        assert_equal(
          DevWorkspaceProfileIdentity.token(profile),
          arguments.fetch(arguments.index('--expected-host-profile-token') + 1)
        )
      end
    end
  end

  def test_terminal_restoration_preserves_the_primary_failure
    Dir.mktmpdir('workspace-host-restoration-error-test') do |directory|
      entry = {
        'name' => 'first', 'root' => make_workspace(directory, 'first'),
        'hostname' => 'first.workspace.example.test'
      }
      config = File.join(directory, 'config', 'registry.json')
      target = make_package(directory, 'target-package')
      host = RestorationHost.new(
        fail_slug: 'broken', env: host_environment(directory, config:),
        out: StringIO.new, err: StringIO.new
      )
      primary = DevWorkspaceHost::Error.new('injected primary failure')

      error = assert_raises(DevWorkspaceHost::Error) do
        host.send(
          :reraise_after_terminal_restoration,
          primary,
          [[entry, 'broken']],
          package: target
        )
      end

      assert_equal(
        'injected primary failure; unable to restore terminal clients: ' \
        'first/broken: injected sync failure',
        error.message
      )
    end
  end

  def test_failed_switch_keeps_the_selected_forward_generation_and_pending_evidence
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      first_codex = File.realpath(host.send(:active_codex))

      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.fail_activation = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(2, host.send(:profile_generation))
      assert_equal(first_codex, File.realpath(host.send(:active_codex)))
      refute_includes(host.events, [:profile_selected, 1])
      refute_nil(host.send(:pending_codex_update))
    end
  end

  def test_failed_link_install_keeps_the_selected_forward_generation_and_pending_evidence
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      first_codex = File.realpath(host.send(:active_codex))

      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.fail_links = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(2, host.send(:profile_generation))
      assert_equal(first_codex, File.realpath(host.send(:active_codex)))
      refute_includes(host.events, [:profile_selected, 1])
      refute_nil(host.send(:pending_codex_update))
    end
  end

  def test_forward_retry_preserves_pending_evidence_when_the_candidate_was_already_selected
    with_transition_host do |host, paths|
      host.fail_activation = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
      first_pending = host.send(:pending_codex_update)
      refute_nil(first_pending)
      restored = host.events.count { |event| event.first == :sessions_restored }

      host.fail_links = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(1, host.send(:profile_generation))
      assert_equal(first_pending.fetch('package_root'), host.send(:pending_codex_update).fetch('package_root'))
      assert_equal(restored, host.events.count { |event| event.first == :sessions_restored })
    end
  end

  def test_post_commit_profile_selection_failure_preserves_the_forward_candidate_and_pending_evidence
    with_transition_host do |host, paths|
      host.fail_set_after_profile = true

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(1, host.send(:profile_generation))
      assert_equal(File.realpath(host.candidate), File.realpath(host.instance_variable_get(:@profile)))
      refute_nil(host.send(:pending_codex_update))
      refute(host.events.any? { |event| event.first == :sessions_restored })
    end
  end

  def test_failed_codex_adoption_keeps_the_forward_target_and_pending_evidence
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      previous = File.realpath(host.send(:active_codex))
      replacement = make_codex(paths.fetch(:root), 'codex-replacement')
      host.instance_variable_set(:@system_codex, replacement)
      host.fail_restart = true
      restarts_before = host.events.count { |event| event == [:consumers_restarted] }

      assert_equal(1, host.run('workspace-host', ['reconcile-codex']))

      assert_equal(File.realpath(replacement), File.realpath(host.send(:active_codex)))
      refute_equal(previous, File.realpath(host.send(:active_codex)))
      refute_nil(host.send(:pending_codex_update))
      assert_equal(restarts_before + 1, host.events.count { |event| event == [:consumers_restarted] })
    end
  end

  private

  def recorded_cluster!(host)
    workspace = host.send(:registry).entries.fetch(0).fetch('root')
    cluster = File.join(workspace, '.dev-clusters', 'beta', 'clusters', '2026-09-07-contract-state')
    FileUtils.mkdir_p(cluster)
    File.write(File.join(cluster, 'socket-dir'), "/tmp/workspace-scoped-socket\n")
    cluster
  end

  def probe_cluster_adoption!(package, exit_status: 0)
    helper = File.join(package, 'libexec/workspace-portal/beta-devcluster')
    File.write(helper, <<~SH)
      #!/bin/sh
      [ "$1" = transition-adopt ] || exit 1
      printf 'adoption attempted\\n' > "$DEVCLUSTER_WORKSPACE/.dev-clusters/beta/clusters/$2/adoption-called"
      exit #{exit_status}
    SH
  end

  # Model the predecessor's declared policy with the unchanged host algorithm.
  def with_cluster_transition_policy(policy)
    original = DevWorkspaceHost::RUNTIME_CONTRACT
    DevWorkspaceHost.send(:remove_const, :RUNTIME_CONTRACT)
    DevWorkspaceHost.const_set(:RUNTIME_CONTRACT, original.merge('developmentClusterTransitionPolicy' => policy).freeze)
    yield
  ensure
    DevWorkspaceHost.send(:remove_const, :RUNTIME_CONTRACT)
    DevWorkspaceHost.const_set(:RUNTIME_CONTRACT, original)
  end

end
