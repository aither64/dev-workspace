# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class WorkspaceHostTest < Minitest::Test
  def test_tmux_keeper_adopts_an_existing_server
    skip 'real tmux tests are disabled' if ENV['DEV_SESSION_SKIP_REAL_TMUX_TESTS'] == '1'

    Dir.mktmpdir('workspace-host-tmux-adoption-test') do |directory|
      socket = File.join(directory, 'tmux.sock')
      system('tmux', '-S', socket, 'new-session', '-d', '-s', '__workspace_portal_keeper')
      host = DevWorkspaceHost::Host.new(
        env: host_environment(directory, config: File.join(directory, 'registry.json')),
        out: StringIO.new,
        err: StringIO.new
      )

      refute(host.send(:ensure_tmux_server, socket, 'tmux'))
      assert(system('tmux', '-S', socket, 'has-session', '-t', '__workspace_portal_keeper'))
    ensure
      system('tmux', '-S', socket, 'kill-server', out: File::NULL, err: File::NULL) if socket
    end
  end

  def test_tmux_shutdown_cleanup_removes_a_socket_left_by_a_directory_rename
    skip 'real tmux tests are disabled' if ENV['DEV_SESSION_SKIP_REAL_TMUX_TESTS'] == '1'

    Dir.mktmpdir('workspace-host-tmux-rename-test') do |directory|
      old_directory = File.join(directory, 'old')
      new_directory = File.join(directory, 'new')
      FileUtils.mkdir_p(old_directory)
      old_socket = File.join(old_directory, 'tmux.sock')
      new_socket = File.join(new_directory, 'tmux.sock')
      system('tmux', '-S', old_socket, 'new-session', '-d', '-s', '__workspace_portal_keeper')
      File.rename(old_directory, new_directory)
      host = DevWorkspaceHost::Host.new(
        env: host_environment(directory, config: File.join(directory, 'registry.json')),
        out: StringIO.new,
        err: StringIO.new
      )

      assert(system('tmux', '-S', new_socket, 'kill-server'))
      assert(File.socket?(new_socket))
      host.send(:remove_stopped_tmux_socket, new_socket)
      refute(File.exist?(new_socket))
    ensure
      system('tmux', '-S', new_socket, 'kill-server', out: File::NULL, err: File::NULL) if new_socket
      File.unlink(new_socket) if new_socket && File.socket?(new_socket)
    end
  end

  def test_tmux_shutdown_cleanup_preserves_a_live_server_without_the_keeper
    skip 'real tmux tests are disabled' if ENV['DEV_SESSION_SKIP_REAL_TMUX_TESTS'] == '1'

    Dir.mktmpdir('workspace-host-live-tmux-test') do |directory|
      socket = File.join(directory, 'tmux.sock')
      system('tmux', '-S', socket, 'new-session', '-d', '-s', 'unrelated')
      host = DevWorkspaceHost::Host.new(
        env: host_environment(directory, config: File.join(directory, 'registry.json')),
        out: StringIO.new,
        err: StringIO.new
      )

      host.send(:remove_stopped_tmux_socket, socket)
      assert(File.socket?(socket))
      assert(system('tmux', '-S', socket, 'has-session', '-t', 'unrelated'))
    ensure
      system('tmux', '-S', socket, 'kill-server', out: File::NULL, err: File::NULL) if socket
    end
  end

  def test_run_portal_applies_workspace_display_and_provider_configuration
    Dir.mktmpdir('workspace-host-portal-config-test') do |directory|
      root = make_workspace(directory, 'workspace')
      File.write(File.join(root, '.dev-workspace.json'), JSON.generate(
        'schema' => 2, 'displayLabel' => 'example organization development',
        'hostLabel' => 'build-host', 'sshHost' => 'build-host.int.example.cz',
        'developmentClusterProviders' => %w[alpha],
        'portal' => { 'hostname' => 'workspace.example.test', 'aliases' => [] }
      ))
      config = File.join(directory, 'config/registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'workspace.example.test', aliases: [], replace: false
      )
      package = make_package(directory, 'package')
      profile = File.join(directory, 'state/profile')
      FileUtils.mkdir_p(File.dirname(profile))
      File.symlink(package, profile)
      host = PortalArgumentHost.new(
        package_root: package, env: host_environment(directory, config:),
        out: StringIO.new, err: StringIO.new
      )
      host.send(:run_portal, ['example-workspace'])
      arguments = host.execution.drop(2)
      assert_equal('example organization development', arguments[arguments.index('--display-label') + 1])
      assert_equal('build-host', arguments[arguments.index('--host-label') + 1])
      assert_equal('build-host.int.example.cz', arguments[arguments.index('--ssh-host') + 1])
      assert_equal(
        File.join(directory, 'state'),
        arguments[arguments.index('--user-state-root') + 1]
      )
      provider_index = arguments.index('--cluster-provider')
      refute_nil(provider_index)
      assert_match(/\Aalpha=Alpha=/, arguments.fetch(provider_index + 1))
      refute(arguments.any? { |value| value.match?(/\Abeta=Beta=/) })
    end
  end

  def test_unregister_stops_instance_services_and_removes_the_registry_entry
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      runtime = File.join(directory, 'runtime')
      authority = File.join(runtime, 'example-workspace', 'authority')
      FileUtils.mkdir_p(authority)
      File.write(File.join(authority, 'old.json'), "old authority\n")
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = UnregisterHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => runtime
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(0, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_empty(DevWorkspaceHost::Registry.new(config).entries)
      disable = host.commands.find do |command|
        command[0, 4] == ['systemctl', '--user', 'disable', '--now']
      end
      refute_nil(disable)
      assert_includes(disable, 'workspace-portal@example-workspace.service')
      assert_includes(disable, 'workspace-codex@example-workspace.service')
      assert_includes(disable, 'workspace-tmux@example-workspace.service')
      refute(File.exist?(File.join(runtime, 'example-workspace')))

      replacement = make_workspace(directory, 'replacement')
      registered = DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root: replacement,
        hostname: 'replacement.workspace.example.test', aliases: [], replace: false
      )
      assert_equal(replacement, registered.fetch('root'))
    end
  end

  def test_unregister_recovers_registration_after_failed_first_switch
    Dir.mktmpdir('workspace-host-bootstrap-test') do |directory|
      original = make_workspace(directory, 'original')
      replacement = make_workspace(directory, 'replacement')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      environment = host_environment(directory, config:).merge(
        'DEV_WORKSPACES_STATE' => state
      )
      host = BootstrappingHost.new(
        env: environment, out: StringIO.new, err: StringIO.new
      )

      assert_equal(0, host.run('workspace-host', [
        'register', 'example-workspace', original,
        '--hostname', 'example-workspace.workspace.example.test'
      ]))
      assert_equal(1, host.run('workspace-host', [
        'switch', '--source', File.join(directory, 'missing-source')
      ]))
      refute(File.exist?(File.join(state, 'profile')))

      assert_equal(0, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_equal(0, host.run('workspace-host', [
        'register', 'example-workspace', replacement,
        '--hostname', 'replacement.workspace.example.test'
      ]))
      assert_equal(
        replacement,
        DevWorkspaceHost::Registry.new(config).find('example-workspace').fetch('root')
      )
    end
  end

  def test_unregister_restores_units_and_clients_after_a_partial_disable_failure
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = FailedUnregisterHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      refute_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      enable = host.commands.find do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end
      refute_nil(enable)
      assert_equal([:quiesced], host.restored)
      error_output = host.instance_variable_get(:@err).string
      primary = error_output.index('injected partial disable failure')
      units = error_output.index('failed to re-enable workspace services: injected unit recovery failure')
      clients = error_output.index('failed to restore terminal clients: injected terminal recovery failure')
      refute_nil(primary)
      refute_nil(units)
      refute_nil(clients)
      assert_operator(primary, :<, units)
      assert_operator(units, :<, clients)
    end
  end

  def test_unregister_reconciles_persisted_registration_after_a_post_commit_failure
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      runtime = File.join(directory, 'runtime')
      state = File.join(directory, 'state')
      entry = PostCommitFailureRegistry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      runtime_root = File.join(runtime, 'example-workspace')
      FileUtils.mkdir_p(runtime_root)
      host = PostCommitFailureUnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:, runtime:).merge(
          'DEV_WORKSPACES_STATE' => state
        )),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_equal(entry, DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      assert(File.directory?(runtime_root))
      assert(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_equal(
        1,
        host.commands.count do |command|
          command == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
        end
      )
      assert_equal([:quiesced], host.restored)
      assert_includes(
        host.instance_variable_get(:@err).string,
        'injected failure after the registry replacement'
      )
    end
  end

  def test_unregister_skips_dependent_recovery_when_prerequisites_cannot_be_restored
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      replacement = make_workspace(directory, 'replacement')
      config = File.join(directory, 'config', 'registry.json')
      runtime = File.join(directory, 'runtime')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(File.join(runtime, 'example-workspace'))
      host = FailedLateUnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:, runtime:)),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      refute(File.exist?(File.join(runtime, 'example-workspace')))
      assert_equal(1, Dir[File.join(runtime, '.retired-example-workspace-*')].length)
      refute(host.runtime_restore_attempted)
      refute(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_equal(
        1,
        host.commands.count do |command|
          command == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
        end
      )
      assert_nil(host.restored)
      error_output = host.instance_variable_get(:@err).string
      primary = error_output.index('injected router failure')
      registration = error_output.index(
        'failed to restore workspace registration: injected registration recovery failure'
      )
      refute_nil(primary)
      refute_nil(registration)
      assert_operator(primary, :<, registration)

      registered = DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root: replacement,
        hostname: 'replacement.workspace.example.test', aliases: [], replace: false
      )
      assert_equal(replacement, registered.fetch('root'))
      refute(File.exist?(File.join(runtime, 'example-workspace')))
      assert_equal(1, Dir[File.join(runtime, '.retired-example-workspace-*')].length)
    end
  end

  def test_unregister_keeps_old_runtime_quarantined_for_a_replacement_registration
    Dir.mktmpdir('workspace-host-test') do |directory|
      original = make_workspace(directory, 'original')
      replacement = make_workspace(directory, 'replacement')
      config = File.join(directory, 'config', 'registry.json')
      runtime = File.join(directory, 'runtime')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root: original,
        hostname: 'example-workspace.workspace.example.test', aliases: [], replace: false
      )
      runtime_root = File.join(runtime, 'example-workspace')
      FileUtils.mkdir_p(runtime_root)
      File.write(File.join(runtime_root, 'original-authority'), "original\n")
      host = ReplacementDuringUnregisterHost.new(
        replacement:,
        env: install_source_profile(host_environment(directory, config:, runtime:)),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      registered = DevWorkspaceHost::Registry.new(config).find('example-workspace')
      assert_equal(replacement, registered.fetch('root'))
      refute(File.exist?(runtime_root))
      retired = Dir[File.join(runtime, '.retired-example-workspace-*')]
      assert_equal(1, retired.length)
      assert(File.file?(File.join(retired.fetch(0), 'original-authority')))
      refute(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_equal(
        1,
        host.commands.count do |command|
          command == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
        end
      )
      assert_includes(
        host.instance_variable_get(:@err).string,
        'workspace registration changed during unregister recovery: example-workspace'
      )
    end
  end

  def test_unregister_keeps_runtime_quarantined_after_an_ambiguous_recovery_write
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      runtime = File.join(directory, 'runtime')
      entry = PostCommitFailureRegistry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      runtime_root = File.join(runtime, 'example-workspace')
      FileUtils.mkdir_p(runtime_root)
      File.write(File.join(runtime_root, 'original-authority'), "original\n")
      host = AmbiguousRecoveryWriteUnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:, runtime:)),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_equal(entry, DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      refute(File.exist?(runtime_root))
      retired = Dir[File.join(runtime, '.retired-example-workspace-*')]
      assert_equal(1, retired.length)
      assert(File.file?(File.join(retired.fetch(0), 'original-authority')))
      refute(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_nil(host.restored)
      error_output = host.instance_variable_get(:@err).string
      primary = error_output.index('injected failure after the registry replacement')
      recovery = error_output.index('injected failure after the recovery replacement')
      refute_nil(primary)
      refute_nil(recovery)
      assert_operator(primary, :<, recovery)
    end
  end

  def test_unregister_does_not_compensate_after_committed_success_output_failure
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = UnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:)),
        out: FailedOutput.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      refute(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_includes(host.instance_variable_get(:@err).string, 'Broken pipe')
    end
  end

  def test_unregister_refuses_workspace_with_development_cluster_state
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      cluster = File.join(
        root, '.dev-clusters', 'alpha', 'clusters',
        '2026-09-07-active-cluster'
      )
      FileUtils.mkdir_p(cluster)
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      error_output = StringIO.new
      host = UnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:)),
        out: StringIO.new,
        err: error_output
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      refute_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      assert_includes(error_output.string, 'workspace unregister is blocked')
      assert_includes(error_output.string, 'example-workspace/alpha/2026-09-07-active-cluster')
      assert_empty(host.commands)
    end
  end

  def test_register_and_unregister_refuse_unfinished_lifecycle_operations
    Dir.mktmpdir('workspace-host-test') do |directory|
      config = File.join(directory, 'config', 'registry.json')
      root = make_workspace(directory, 'workspace')
      locks = File.join(root, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-07-pending.archive.json'), "{}\n")
      error_output = StringIO.new
      host = DevWorkspaceHost::Host.new(
        env: install_source_profile(host_environment(directory, config:)),
        out: StringIO.new, err: error_output
      )

      assert_equal(1, host.run('workspace-host', [
        'register', 'example-workspace', root,
        '--hostname', 'example-workspace.workspace.example.test'
      ]))
      assert_empty(DevWorkspaceHost::Registry.new(config).entries)
      assert_includes(error_output.string, 'workspace register is blocked')

      File.unlink(File.join(locks, '2026-09-07-pending.archive.json'))
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      File.write(File.join(locks, '2026-09-07-pending.revive.json'), "{}\n")
      error_output.truncate(0)
      error_output.rewind

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      refute_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      assert_includes(error_output.string, 'workspace unregister is blocked')
    end
  end

end
