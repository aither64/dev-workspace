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
          'state' => 'ready'
        ) + "\n"
      )
      File.chmod(0o600, journal)

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
    end
  end

  def test_switch_allows_a_legacy_journal_only_creation_for_safe_retry
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
          'state' => 'creating'
        ) + "\n"
      )
      File.chmod(0o600, journal)

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
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
        'cannot inspect session operation state'
      )
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

  def test_rollback_refuses_a_cluster_contract_with_the_old_tracking_limit
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      old_contract = File.join(
        host.send(:profile_generation_path, 1),
        'share/workspace-portal/runtime-contract.json'
      )
      File.write(old_contract, JSON.generate(
        'developmentClusterStateSchema' => 1,
        'developmentClusterTransitionPolicy' =>
          DevWorkspaceHost::RUNTIME_CONTRACT.fetch(
            'developmentClusterTransitionPolicy'
          ),
        'trackingMaxBytes' => 1024 * 1024
      ))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(File.join(
        workspace, '.dev-clusters', 'beta', 'clusters',
        '2026-09-07-active-cluster'
      ))

      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(2, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'target package has no compatible cluster-state contract'
      )
    end
  end

  def test_rollback_refuses_a_package_without_the_identity_authority_contract
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      old_contract = File.join(
        host.send(:profile_generation_path, 1),
        'share/workspace-portal/runtime-contract.json'
      )
      contract = JSON.parse(File.read(old_contract))
      contract.delete('runtimeAuthorityIdentityPolicy')
      File.write(old_contract, JSON.generate(contract))
      runtime = host.send(:instance_runtime, host.send(:registry).entries.fetch(0))
      FileUtils.mkdir_p(runtime.fetch(:authority), mode: 0o700)
      File.write(
        File.join(runtime.fetch(:authority), '2026-09-07-active.json'),
        JSON.generate('tmux_identity' => 'a' * 64),
        mode: 'w'
      )
      File.chmod(0o600, File.join(runtime.fetch(:authority), '2026-09-07-active.json'))

      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(2, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'target package cannot validate their tmux identities'
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

  def test_rollback_refuses_the_permissive_cluster_transition_policy
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      old_contract = File.join(
        host.send(:profile_generation_path, 1),
        'share/workspace-portal/runtime-contract.json'
      )
      File.write(old_contract, JSON.generate(
        'developmentClusterStateSchema' =>
          DevWorkspaceHost::RUNTIME_CONTRACT.fetch('developmentClusterStateSchema'),
        'developmentClusterTransitionPolicy' => 1,
        'trackingMaxBytes' =>
          DevWorkspaceHost::RUNTIME_CONTRACT.fetch('trackingMaxBytes')
      ))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      cluster = File.join(
        workspace, '.dev-clusters', 'alpha', 'clusters',
        '2026-09-07-active-cluster'
      )
      FileUtils.mkdir_p(cluster)
      File.write(File.join(cluster, 'socket-dir'), "/tmp/workspace-scoped-socket\n")

      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(2, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'target package has no compatible cluster-state contract'
      )
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

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(File.realpath(paths.fetch(:old_codex)), File.realpath(host.send(:active_codex)))
      assert(host.send(:pending_codex_update?, File.realpath(paths.fetch(:system_codex))))
      refute_includes(host.events, [:consumers_restarted])

      host.busy = []
      assert_equal(0, host.run('workspace-host', ['reconcile-codex', '--pending-only']))
      assert_equal(File.realpath(paths.fetch(:system_codex)), File.realpath(host.send(:active_codex)))
      refute(host.send(:pending_codex_update?))
      assert_includes(host.events, [:consumers_restarted])

      checks = host.events.count { |event| event.first == :codex_checked }
      assert_equal(0, host.run('workspace-host', ['reconcile-codex', '--pending-only']))
      assert_equal(checks, host.events.count { |event| event.first == :codex_checked })
    end
  end

  def test_rollback_selects_the_retained_codex_for_the_previous_profile_generation
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      second_package = make_package(paths.fetch(:root), 'package-two')
      second_codex = make_codex(paths.fetch(:root), 'codex-two')
      host.candidate = second_package
      host.instance_variable_set(:@system_codex, second_codex)
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(2, host.send(:profile_generation))

      assert_equal(0, host.run('workspace-host', ['rollback']))
      assert_equal(1, host.send(:profile_generation))
      assert_equal(File.realpath(paths.fetch(:system_codex)), File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:router_restarted])
      assert_includes(host.events, [:consumers_restarted])
      assert_includes(
        host.events,
        [:sessions_restored, File.realpath(host.send(:profile_generation_path, 1))]
      )
    end
  end

  def test_rollback_keeps_the_selected_generation_when_terminal_restoration_fails
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      second_package = make_package(paths.fetch(:root), 'package-two')
      host.candidate = second_package
      host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.fail_restore = true

      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(1, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'workspace package rollback completed, but terminal clients need dev-session sync'
      )
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

  def test_rollback_refuses_unfinished_session_lifecycle_operations
    DevWorkspaceHost::LIFECYCLE_JOURNALS.each do |journal|
      kind = journal.fetch('name')
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        host.candidate = make_package(paths.fetch(:root), 'package-two')
        host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        workspace = host.send(:registry).entries.fetch(0).fetch('root')
        locks = File.join(workspace, 'worktrees', '.locks')
        FileUtils.mkdir_p(locks)
        File.write(File.join(locks, "2026-09-07-pending.#{kind}.json"), "{}\n")

        assert_equal(1, host.run('workspace-host', ['rollback']))
        assert_equal(2, host.send(:profile_generation))
      end
    end
  end

  def test_rollback_refuses_canonical_and_legacy_development_cluster_state
    [
      ['beta', '2026-09-07-canonical-cluster'],
      ['alpha', '2026-08-18-alpha-password-reset']
    ].each do |kind, slug|
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        File.unlink(
          File.join(
            host.send(:profile_generation_path, 1),
            'share/workspace-portal/runtime-contract.json'
          )
        )
        host.candidate = make_package(paths.fetch(:root), 'package-two')
        host.instance_variable_set(
          :@system_codex,
          make_codex(paths.fetch(:root), 'codex-two')
        )
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        workspace = host.send(:registry).entries.fetch(0).fetch('root')
        FileUtils.mkdir_p(File.join(workspace, '.dev-clusters', kind, 'clusters', slug))

        assert_equal(1, host.run('workspace-host', ['rollback']))
        assert_equal(2, host.send(:profile_generation))
        error_output = host.instance_variable_get(:@err).string
        assert_includes(error_output, "example-workspace/#{kind}/#{slug}")
        assert_includes(error_output, 'reset these clusters first')
      end
    end
  end

  def test_failed_switch_restores_the_previous_profile_and_codex_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      first_codex = File.realpath(host.send(:active_codex))

      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.fail_activation = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(1, host.send(:profile_generation))
      assert_equal(first_codex, File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:profile_selected, 1])
      assert_includes(host.events, [:consumers_restarted])
    end
  end

  def test_failed_link_install_restores_the_previous_profile_and_codex_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      first_codex = File.realpath(host.send(:active_codex))

      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.fail_links = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(1, host.send(:profile_generation))
      assert_equal(first_codex, File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:profile_selected, 1])
      assert_includes(host.events, [:consumers_restarted])
    end
  end

  def test_failed_switch_generation_is_not_eligible_for_later_rollback
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      host.candidate = make_package(paths.fetch(:root), 'failed-package')
      host.fail_activation = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.send(:profile_generation_path, 2)))
      assert_nil(host.send(:generation_codex, 2))

      host.candidate = make_package(paths.fetch(:root), 'working-package')
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(2, host.send(:profile_generation))

      assert_equal(0, host.run('workspace-host', ['rollback']))
      assert_equal(1, host.send(:profile_generation))
      assert_equal(File.realpath(paths.fetch(:system_codex)), File.realpath(host.send(:active_codex)))
    end
  end

  def test_failed_codex_adoption_restores_the_previous_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      previous = File.realpath(host.send(:active_codex))
      replacement = make_codex(paths.fetch(:root), 'codex-replacement')
      host.instance_variable_set(:@system_codex, replacement)
      host.fail_restart = true

      assert_equal(1, host.run('workspace-host', ['reconcile-codex']))

      assert_equal(previous, File.realpath(host.send(:active_codex)))
      assert_equal(previous, File.realpath(host.send(:generation_codex, 1)))
      assert_operator(host.events.count { |event| event == [:consumers_restarted] }, :>=, 2)
    end
  end

  def test_failed_rollback_restores_the_original_generation_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      second_codex = make_codex(paths.fetch(:root), 'codex-two')
      host.instance_variable_set(:@system_codex, second_codex)
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.fail_restart = true
      host.fail_restore = true

      assert_equal(1, host.run('workspace-host', ['rollback']))

      assert_equal(2, host.send(:profile_generation))
      assert_equal(File.realpath(second_codex), File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:profile_selected, 2])
      assert_includes(
        host.events,
        [:sessions_restored, File.realpath(host.send(:profile_generation_path, 2))]
      )
      error_output = host.instance_variable_get(:@err).string
      assert_includes(error_output, 'injected consumer restart failure')
      assert_includes(error_output, 'injected restoration failure')
    end
  end

end
