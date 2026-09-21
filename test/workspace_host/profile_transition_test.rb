# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class WorkspaceHostTest < Minitest::Test
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

end
