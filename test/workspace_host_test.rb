# frozen_string_literal: true

require 'fileutils'
require 'minitest/autorun'
require 'stringio'
require 'tmpdir'

load File.expand_path('../libexec/workspace-host', __dir__)

class WorkspaceHostTest < Minitest::Test
  def test_lifecycle_journals_are_projected_from_the_shared_runtime_contract
    contract = JSON.parse(
      File.read(File.expand_path('../portal/internal/session/runtime-contract.json', __dir__))
    )

    assert_equal(
      contract.fetch('lifecycleJournals'),
      VpsfreeWorkspaceHost::LIFECYCLE_JOURNALS
    )
  end

  def test_registry_selects_the_longest_matching_root_and_requires_a_name_outside_multiple_roots
    Dir.mktmpdir('workspace-host-test') do |directory|
      first = make_workspace(directory, 'first')
      nested = make_workspace(first, 'nested')
      registry = registry_at(directory)
      registry.register(
        name: 'first', root: first, hostname: 'first.workspace.example.test',
        aliases: [], replace: false
      )
      registry.register(
        name: 'nested', root: nested, hostname: 'nested.workspace.example.test',
        aliases: [], replace: false
      )

      assert_equal('nested', registry.select(cwd: nested).fetch('name'))
      error = assert_raises(VpsfreeWorkspaceHost::Error) do
        registry.select(cwd: directory)
      end
      assert_includes(error.message, '--workspace NAME')
      assert_equal('first', registry.select(name: 'first', cwd: directory).fetch('name'))
    end
  end

  def test_registry_is_private_and_rejects_duplicate_hosts
    Dir.mktmpdir('workspace-host-test') do |directory|
      first = make_workspace(directory, 'first')
      second = make_workspace(directory, 'second')
      registry = registry_at(directory)
      registry.register(
        name: 'first', root: first, hostname: 'first.workspace.example.test',
        aliases: ['old.example.test'], replace: false
      )

      assert_equal(0o600, File.stat(registry.path).mode & 0o777)
      error = assert_raises(VpsfreeWorkspaceHost::Error) do
        registry.register(
          name: 'second', root: second, hostname: 'old.example.test',
          aliases: [], replace: false
        )
      end
      assert_includes(error.message, 'duplicate workspace hostname')

      File.chmod(0o644, registry.path)
      assert_raises(VpsfreeWorkspaceHost::Error) do
        VpsfreeWorkspaceHost::Registry.new(registry.path)
      end
    end
  end

  def test_registry_replace_keeps_the_workspace_root_immutable
    Dir.mktmpdir('workspace-host-test') do |directory|
      first = make_workspace(directory, 'first')
      second = make_workspace(directory, 'second')
      registry = registry_at(directory)
      registry.register(
        name: 'vpsfree-cz', root: first, hostname: 'old.workspace.example.test',
        aliases: [], replace: false
      )

      error = assert_raises(VpsfreeWorkspaceHost::Error) do
        registry.register(
          name: 'vpsfree-cz', root: second, hostname: 'new.workspace.example.test',
          aliases: [], replace: true
        )
      end

      assert_includes(error.message, 'unregister vpsfree-cz first')
      assert_equal(first, registry.find('vpsfree-cz').fetch('root'))
      updated = registry.register(
        name: 'vpsfree-cz', root: first, hostname: 'new.workspace.example.test',
        aliases: ['old.workspace.example.test'], replace: true
      )
      assert_equal(first, updated.fetch('root'))
      assert_equal('new.workspace.example.test', updated.fetch('hostname'))
    end
  end

  def test_unregister_stops_instance_services_and_removes_the_registry_entry
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      runtime = File.join(directory, 'runtime')
      authority = File.join(runtime, 'vpsfree-cz', 'authority')
      FileUtils.mkdir_p(authority)
      File.write(File.join(authority, 'old.json'), "old authority\n")
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      host = UnregisterHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACES_RUNTIME_DIR' => runtime
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(0, host.run('workspace-host', ['unregister', 'vpsfree-cz']))
      assert_empty(VpsfreeWorkspaceHost::Registry.new(config).entries)
      disable = host.commands.find do |command|
        command[0, 4] == ['systemctl', '--user', 'disable', '--now']
      end
      refute_nil(disable)
      assert_includes(disable, 'workspace-portal@vpsfree-cz.service')
      assert_includes(disable, 'workspace-codex@vpsfree-cz.service')
      assert_includes(disable, 'workspace-tmux@vpsfree-cz.service')
      refute(File.exist?(File.join(runtime, 'vpsfree-cz')))

      replacement = make_workspace(directory, 'replacement')
      registered = VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root: replacement,
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
        'VPSFREE_WORKSPACES_STATE' => state
      )
      host = BootstrappingHost.new(
        env: environment, out: StringIO.new, err: StringIO.new
      )

      assert_equal(0, host.run('workspace-host', [
        'register', 'vpsfree-cz', original,
        '--hostname', 'vpsfree-cz.workspace.example.test'
      ]))
      assert_equal(1, host.run('workspace-host', [
        'switch', '--source', File.join(directory, 'missing-source')
      ]))
      refute(File.exist?(File.join(state, 'profile')))

      assert_equal(0, host.run('workspace-host', ['unregister', 'vpsfree-cz']))
      assert_equal(0, host.run('workspace-host', [
        'register', 'vpsfree-cz', replacement,
        '--hostname', 'replacement.workspace.example.test'
      ]))
      assert_equal(
        replacement,
        VpsfreeWorkspaceHost::Registry.new(config).find('vpsfree-cz').fetch('root')
      )
    end
  end

  def test_unregister_restores_units_and_clients_after_a_partial_disable_failure
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      host = FailedUnregisterHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => state
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'vpsfree-cz']))
      refute_nil(VpsfreeWorkspaceHost::Registry.new(config).find('vpsfree-cz'))
      enable = host.commands.find do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end
      refute_nil(enable)
      assert_equal([:quiesced], host.restored)
    end
  end

  def test_unregister_refuses_workspace_with_development_cluster_state
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      cluster = File.join(
        root, '.dev-clusters', 'vpsadmin', 'clusters',
        '2026-09-07-active-cluster'
      )
      FileUtils.mkdir_p(cluster)
      config = File.join(directory, 'config', 'registry.json')
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      error_output = StringIO.new
      host = UnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:)),
        out: StringIO.new,
        err: error_output
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'vpsfree-cz']))
      refute_nil(VpsfreeWorkspaceHost::Registry.new(config).find('vpsfree-cz'))
      assert_includes(error_output.string, 'workspace unregister is blocked')
      assert_includes(error_output.string, 'vpsfree-cz/vpsadmin/2026-09-07-active-cluster')
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
      host = VpsfreeWorkspaceHost::Host.new(
        env: install_source_profile(host_environment(directory, config:)),
        out: StringIO.new, err: error_output
      )

      assert_equal(1, host.run('workspace-host', [
        'register', 'vpsfree-cz', root,
        '--hostname', 'vpsfree-cz.workspace.example.test'
      ]))
      assert_empty(VpsfreeWorkspaceHost::Registry.new(config).entries)
      assert_includes(error_output.string, 'workspace register is blocked')

      File.unlink(File.join(locks, '2026-09-07-pending.archive.json'))
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      File.write(File.join(locks, '2026-09-07-pending.revive.json'), "{}\n")
      error_output.truncate(0)
      error_output.rewind

      assert_equal(1, host.run('workspace-host', ['unregister', 'vpsfree-cz']))
      refute_nil(VpsfreeWorkspaceHost::Registry.new(config).find('vpsfree-cz'))
      assert_includes(error_output.string, 'workspace unregister is blocked')
    end
  end

  def test_quiesce_ignores_a_manifest_from_another_codex_runtime
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      runtime = File.join(directory, 'runtime')
      slug = '2026-09-06-old-runtime'
      manifest_dir = File.join(root, 'work', slug)
      FileUtils.mkdir_p(manifest_dir)
      File.write(
        File.join(manifest_dir, 'portal.yml'),
        portal_manifest('thread-old', '/run/old/app-server.sock', 'ready')
      )
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      host = QuiesceHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACES_RUNTIME_DIR' => runtime
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_empty(host.send(:quiesce_sessions))
      assert_empty(host.commands)
    end
  end

  def test_dev_session_dispatch_binds_the_registered_workspace_runtime
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      runtime = File.join(directory, 'runtime')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACES_RUNTIME_DIR' => runtime,
          'VPSFREE_WORKSPACES_SYSTEM_CODEX' => codex
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      Dir.chdir(directory) do
        assert_equal(0, host.run('dev-session', ['--workspace', 'vpsfree-cz', 'list']))
      end
      environment, command, arguments = host.captured
      assert_equal('vpsfree-cz', environment.fetch('VPSFREE_WORKSPACE_NAME'))
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, root)
      assert_includes(arguments, File.join(runtime, 'vpsfree-cz', 'app-server.sock'))
      lock_index = arguments.index('--transition-lock')
      assert_operator(lock_index, :<, arguments.index('--'))
      assert_equal(File.join(state, 'transition.lock'), arguments.fetch(lock_index + 1))
      token_index = arguments.index('--expected-host-profile-token')
      assert_operator(token_index, :<, arguments.index('--'))
      assert_match(/\A[0-9a-f]{64}\z/, arguments.fetch(token_index + 1))
      assert_equal('list', arguments.last)
    end
  end

  def test_cluster_commands_hold_the_shared_host_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      profile = File.join(directory, 'profile')
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      File.symlink(File.expand_path('..', __dir__), profile)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACES_PROFILE' => profile
        },
        out: StringIO.new,
        err: StringIO.new
      )
      result = Thread.new do
        host.run('vpsadmin-devcluster', ['--workspace', 'vpsfree-cz', 'status', '2026-09-06-test'])
      end
      sleep 0.05
      assert_nil(host.captured)
      owner.flock(File::LOCK_UN)

      assert_equal(0, result.value)
      refute_nil(host.captured)
    ensure
      owner&.close unless owner&.closed?
    end
  end

  def test_waiting_cluster_command_rejects_a_changed_package_generation
    %w[successful-switch compensated-switch].each do |scenario|
      Dir.mktmpdir("workspace-host-#{scenario}") do |directory|
        root = make_workspace(directory, 'workspace')
        config = File.join(directory, 'config', 'registry.json')
        state = File.join(directory, 'state')
        profile = File.join(directory, 'profile')
        expected = File.join(directory, 'old')
        selected = File.join(directory, 'new')
        FileUtils.mkdir_p(state)
        FileUtils.mkdir_p(expected)
        FileUtils.mkdir_p(selected)
        File.symlink(expected, profile)
        VpsfreeWorkspaceHost::Registry.new(config).register(
          name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
          aliases: [], replace: false
        )
        lock_path = File.join(state, 'transition.lock')
        owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
        owner.flock(File::LOCK_EX)
        error_output = StringIO.new
        host = GenerationClusterHost.new(
          package_root: expected,
          env: {
            'HOME' => directory,
            'PATH' => ENV.fetch('PATH'),
            'VPSFREE_WORKSPACES_CONFIG' => config,
            'VPSFREE_WORKSPACES_STATE' => state,
            'VPSFREE_WORKSPACES_PROFILE' => profile
          },
          out: StringIO.new,
          err: error_output
        )
        result = Thread.new do
          host.run('vpsadmin-devcluster', [
            '--workspace', 'vpsfree-cz', 'status', '2026-09-06-test'
          ])
        end
        sleep 0.05
        File.unlink(profile)
        File.symlink(selected, profile)
        if scenario == 'compensated-switch'
          File.unlink(profile)
          File.symlink(expected, profile)
        end
        owner.flock(File::LOCK_UN)

        assert_equal(1, result.value)
        assert_nil(host.captured)
        assert_includes(error_output.string, 'package transition completed')
      ensure
        owner&.flock(File::LOCK_UN)
        owner&.close
        result&.join
      end
    end
  end

  def test_waiting_host_mutation_rechecks_successful_and_compensated_switches
    %w[successful compensated].each do |scenario|
      Dir.mktmpdir("workspace-host-mutation-#{scenario}") do |directory|
        state = File.join(directory, 'state')
        profile = File.join(directory, 'profile')
        expected = File.join(directory, 'expected')
        candidate = File.join(directory, 'candidate')
        FileUtils.mkdir_p([state, expected, candidate])
        File.symlink(expected, profile)
        lock_path = File.join(state, 'transition.lock')
        owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
        owner.flock(File::LOCK_EX)
        error_output = StringIO.new
        host = GenerationMutationHost.new(
          package_root: expected,
          env: {
            'HOME' => directory,
            'PATH' => ENV.fetch('PATH'),
            'VPSFREE_WORKSPACES_STATE' => state,
            'VPSFREE_WORKSPACES_PROFILE' => profile
          },
          out: StringIO.new,
          err: error_output
        )
        result = Thread.new { host.run('workspace-host', ['suspend']) }
        sleep 0.05
        assert_nil(host.captured)

        File.unlink(profile)
        File.symlink(candidate, profile)
        if scenario == 'compensated'
          File.unlink(profile)
          File.symlink(expected, profile)
        end
        owner.flock(File::LOCK_UN)

        assert_equal(1, result.value)
        assert_nil(host.captured)
        assert_includes(error_output.string, 'package transition completed')
      ensure
        owner&.flock(File::LOCK_UN)
        owner&.close
        result&.join
      end
    end
  end

  def test_cluster_command_ignores_spoofed_transition_and_lifecycle_environment
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      profile = File.join(directory, 'profile')
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      File.symlink(File.expand_path('..', __dir__), profile)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACES_PROFILE' => profile,
          'VPSFREE_WORKSPACE_TRANSITION_HELD' => '1',
          'VPSFREE_DEV_SESSION_LIFECYCLE_OPERATION' => 'archive'
        },
        out: StringIO.new,
        err: StringIO.new
      )

      result = Thread.new do
        host.run(
          'vpsadmin-devcluster',
          ['--workspace', 'vpsfree-cz', 'reset', '2026-09-06-test']
        )
      end
      sleep 0.05
      assert_nil(host.captured)
      owner.flock(File::LOCK_UN)

      assert_equal(0, result.value)
      refute_nil(host.captured)
      environment, = host.captured
      refute(environment.key?('VPSFREE_WORKSPACE_TRANSITION_HELD'))
      refute(environment.key?('VPSFREE_DEV_SESSION_LIFECYCLE_OPERATION'))
    ensure
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_public_delete_executes_the_cli_with_its_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => File.join(directory, 'state'),
          'VPSFREE_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'VPSFREE_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(
        0,
        host.run(
          'dev-session',
          ['--workspace', 'vpsfree-cz', 'delete', '2026-09-06-test', '--as-is']
        )
      )
      environment, command, arguments = host.captured
      refute(environment.key?('VPSFREE_WORKSPACE_TRANSITION_HELD'))
      assert_equal('dev-session', File.basename(command))
      lock_index = arguments.index('--transition-lock')
      assert_operator(lock_index, :<, arguments.index('--'))
      assert_equal(File.join(directory, 'state', 'transition.lock'), arguments.fetch(lock_index + 1))
      assert_equal(
        ['delete', '2026-09-06-test', '--as-is'],
        arguments.last(3)
      )
    end
  end

  def test_portal_lifecycle_reuses_a_verified_inherited_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      error_output = StringIO.new
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'VPSFREE_WORKSPACES_SYSTEM_CODEX' => codex,
          'VPSFREE_WORKSPACE_TRANSITION_LOCK_FD' => owner.fileno.to_s
        },
        out: StringIO.new,
        err: error_output
      )

      assert_equal(0, host.run(
        'dev-session',
        ['--workspace', 'vpsfree-cz', 'delete', '2026-09-06-test', '--as-is']
      ), error_output.string)
      environment, command, arguments = host.captured
      assert_equal(owner.fileno.to_s, environment.fetch('VPSFREE_WORKSPACE_TRANSITION_LOCK_FD'))
      assert_equal('dev-session', File.basename(command))
      refute_includes(arguments, '--transition-lock')
    ensure
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_inherited_transition_lock_rejects_an_unlocked_descriptor_for_the_same_inode
    Dir.mktmpdir('workspace-host-test') do |directory|
      state = File.join(directory, 'state')
      FileUtils.mkdir_p(state)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      impostor = File.open(lock_path, File::RDWR)
      host = VpsfreeWorkspaceHost::Host.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACE_TRANSITION_LOCK_FD' => impostor.fileno.to_s
        },
        out: StringIO.new,
        err: StringIO.new
      )

      refute(host.send(:inherited_exclusive_transition_lock?))

      host = VpsfreeWorkspaceHost::Host.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACE_TRANSITION_LOCK_FD' => owner.fileno.to_s
        },
        out: StringIO.new,
        err: StringIO.new
      )
      assert(host.send(:inherited_exclusive_transition_lock?))
    ensure
      impostor&.close
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_public_archive_delegates_cluster_cleanup_to_the_session_command
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'VPSFREE_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(
        0,
        host.run(
          'dev-session',
          ['--workspace', 'vpsfree-cz', 'archive', '2026-09-06-test', '--as-is']
        )
      )
      _environment, command, arguments = host.captured
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, '--transition-lock')
      assert_equal(['archive', '2026-09-06-test', '--as-is'], arguments.last(3))
    end
  end

  def test_public_archive_passes_options_to_the_private_cli
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'VPSFREE_WORKSPACES_CONFIG' => config,
          'VPSFREE_WORKSPACES_STATE' => state,
          'VPSFREE_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'VPSFREE_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )
      argv = [
        '--workspace', 'vpsfree-cz', 'archive', '--abandoned',
        '2026-09-06-Foo_bar', '--as-is'
      ]

      assert_equal(0, host.run('dev-session', argv))
      _environment, command, arguments = host.captured
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, '--transition-lock')
      assert_equal(['archive', '--abandoned', '2026-09-06-Foo_bar', '--as-is'], arguments.last(4))
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
    VpsfreeWorkspaceHost::LIFECYCLE_JOURNALS.each do |journal|
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

  def test_switch_refuses_incompatible_cluster_helpers_while_cluster_state_exists
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'vpsadminos', 'clusters',
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
        'trackingMaxBytes' => 1024 * 1024
      ))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(File.join(
        workspace, '.dev-clusters', 'vpsadminos', 'clusters',
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

  def test_switch_refuses_unadoptable_precontract_cluster_state
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'vpsadmin', 'clusters',
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
          workspace, '.dev-clusters', 'vpsadmin', 'clusters',
          '2026-08-18-vpsadmin-password-reset'
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
          workspace, '.dev-clusters', 'vpsadmin', 'clusters',
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
        workspace, '.dev-clusters', 'vpsadmin', 'clusters',
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
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(
        File.join(
          root, '.dev-clusters', 'vpsadminos', 'clusters',
          '2026-09-07-precontract'
        )
      )
      package = make_package(directory, 'candidate')
      error_output = StringIO.new
      environment = host_environment(directory, config:).merge(
        'VPSFREE_WORKSPACE_ACTIVATION' => '1'
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
    with_transition_host(busy: ['vpsfree-cz/active']) do |host, paths|
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
    end
  end

  def test_rollback_refuses_unfinished_session_lifecycle_operations
    VpsfreeWorkspaceHost::LIFECYCLE_JOURNALS.each do |journal|
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
      ['vpsadminos', '2026-09-07-canonical-cluster'],
      ['vpsadmin', '2026-08-18-vpsadmin-password-reset']
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
        assert_includes(error_output, "vpsfree-cz/#{kind}/#{slug}")
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

      assert_equal(1, host.run('workspace-host', ['rollback']))

      assert_equal(2, host.send(:profile_generation))
      assert_equal(File.realpath(second_codex), File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:profile_selected, 2])
    end
  end

  def test_suspend_quiesces_sessions_before_disabling_the_user_services
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.events.clear

      assert_equal(0, host.run('workspace-host', ['suspend']))

      assert_equal(:sessions_quiesced, host.events.fetch(0).first)
      disable = host.events.find do |event|
        event[0, 4] == [:command, 'systemctl', '--user', 'disable']
      end
      refute_nil(disable)
      assert_includes(disable, '--now')
      assert_includes(disable, 'workspace-codex@vpsfree-cz.service')
      assert_includes(disable, 'workspace-tmux@vpsfree-cz.service')
    end
  end

  def test_suspend_and_codex_reconciliation_refuse_unfinished_lifecycle_operations
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-07-pending.removal.json'), "{}\n")
      host.events.clear

      assert_equal(1, host.run('workspace-host', ['suspend']))
      assert_equal(1, host.run('workspace-host', ['reconcile-codex']))
      refute(host.events.any? { |event| event.first == :sessions_quiesced })
      refute_includes(host.events, [:consumers_restarted])
    end
  end

  private

  class CapturingHost < VpsfreeWorkspaceHost::Host
    attr_reader :captured

    private

    def exec_with_workspace(entry, command, *arguments)
      @captured = [@env.to_h.merge('VPSFREE_WORKSPACE_NAME' => entry.fetch('name')), command, arguments]
    end
  end

  class ClusterHost < VpsfreeWorkspaceHost::Host
    attr_reader :captured

    private

    def system_env!(environment, command, *arguments)
      @captured = [environment, command, arguments]
    end
  end

  class GenerationClusterHost < ClusterHost
    def initialize(package_root:, **options)
      @test_package_root = package_root
      super(**options)
    end

    private

    def package_root
      @test_package_root
    end
  end

  class GenerationMutationHost < VpsfreeWorkspaceHost::Host
    attr_reader :captured

    def initialize(package_root:, **options)
      @test_package_root = package_root
      super(**options)
    end

    private

    def package_root
      @test_package_root
    end

    def suspend(argv)
      raise VpsfreeWorkspaceHost::Error, 'unexpected arguments' unless argv.empty?

      @captured = :suspended
    end
  end

  class FinalizeHost < VpsfreeWorkspaceHost::Host
    attr_reader :commands
    attr_accessor :cluster_active

    def initialize(**options)
      super
      @commands = []
      @cluster_active = true
    end

    private

    def capture_env!(_environment, command, *arguments)
      if File.basename(command).include?('dev-session')
        separator = arguments.index('--')
        request = arguments.drop(separator + 2)
        slug = request.find { |argument| !argument.start_with?('-') }
        "https://vpsfree-cz.workspace.example.test/#{slug}/\n"
      else
        found = cluster_active && File.basename(command) == 'vpsadmin-devcluster'
        JSON.generate('schema' => 1, 'found' => found)
      end
    end

    def system_env!(_environment, command, *arguments)
      @commands << [command, arguments]
    end
  end

  class UnregisterHost < VpsfreeWorkspaceHost::Host
    attr_reader :commands

    def initialize(**options)
      super
      @commands = []
    end

    private

    def system!(*argv)
      @commands << argv
    end

    def quiesce_sessions(_entries)
      []
    end
  end

  class BootstrappingHost < UnregisterHost
    private

    def capture!(*argv)
      if argv[0, 2] == ['nix', 'build']
        raise VpsfreeWorkspaceHost::Error, 'injected first switch failure'
      end

      super
    end
  end

  class QuiesceHost < VpsfreeWorkspaceHost::Host
    attr_reader :commands

    def initialize(**options)
      super
      @commands = []
    end

    private

    def capture_env!(environment, command, *arguments)
      @commands << [environment, command, arguments]
      "quiesced terminal: #{arguments[-2]}\n"
    end
  end

  class FailedUnregisterHost < VpsfreeWorkspaceHost::Host
    attr_reader :commands, :restored

    def initialize(**options)
      super
      @commands = []
    end

    private

    def quiesce_sessions(_entries)
      [:quiesced]
    end

    def restore_quiesced_sessions(sessions)
      @restored = sessions
    end

    def system!(*argv)
      @commands << argv
      if argv[0, 4] == ['systemctl', '--user', 'disable', '--now']
        raise VpsfreeWorkspaceHost::Error, 'injected partial disable failure'
      end
    end
  end

  class TransitionHost < VpsfreeWorkspaceHost::Host
    attr_accessor :busy, :candidate, :fail_activation, :fail_links, :fail_restart
    attr_reader :events

    def initialize(candidate:, busy:, **options)
      super(**options)
      @candidate = candidate
      @busy = busy
      @events = []
    end

    private

    # A real stable command invocation comes from the selected profile. Tests
    # reuse this host object across invocations, so follow the selected profile
    # to model the package that a new process would execute.
    def package_root
      File.symlink?(@profile) ? File.realpath(@profile) : super
    end

    def capture!(*argv)
      return "#{candidate}\n" if argv[0, 2] == ['nix', 'build']
      super
    end

    def system!(*argv)
      if argv[0, 3] == ['nix-env', '--profile', @profile]
        if argv[3] == '--set'
          generations = Dir["#{@profile}-*-link"].filter_map do |path|
            File.basename(path)[/-(\d+)-link\z/, 1]&.to_i
          end
          current = generations.max.to_i + 1
          generation = profile_generation_path(current)
          File.symlink(argv.fetch(4), generation)
          File.unlink(@profile) if File.symlink?(@profile)
          File.symlink(File.basename(generation), @profile)
          @events << [:profile_set, current]
          return
        elsif argv[3] == '--rollback'
          target = previous_profile_generation
          File.unlink(@profile)
          File.symlink(File.basename(profile_generation_path(target)), @profile)
          @events << [:profile_rolled_back, target]
          return
        elsif argv[3] == '--switch-generation'
          target = Integer(argv.fetch(4), 10)
          File.unlink(@profile)
          File.symlink(File.basename(profile_generation_path(target)), @profile)
          @events << [:profile_selected, target]
          return
        elsif argv[3] == '--delete-generations'
          target = Integer(argv.fetch(4), 10)
          File.unlink(profile_generation_path(target))
          @events << [:profile_deleted, target]
          return
        end
      end
      if argv[0, 4] == ['systemctl', '--user', 'restart', 'workspace-router.service']
        @events << [:router_restarted]
      else
        @events << [:command, *argv]
      end
    end

    def root_codex(command, root)
      package = File.dirname(File.dirname(File.realpath(command)))
      FileUtils.mkdir_p(File.dirname(root))
      File.unlink(root) if File.symlink?(root)
      File.symlink(package, root)
    end

    def install_links
      @events << [:links_installed]
      if fail_links
        self.fail_links = false
        raise VpsfreeWorkspaceHost::Error, 'injected link installation failure'
      end
    end

    def configure_user_services
      @events << [:configured]
    end

    def activate_installed(_command)
      configure_user_services
      if fail_activation
        self.fail_activation = false
        raise VpsfreeWorkspaceHost::Error, 'injected activation failure'
      end
      reconcile_codex_update(defer_busy: true)
    end

    def quiesce_sessions
      raise VpsfreeWorkspaceHost::Error, busy.join(', ') unless busy.empty?
      @events << [:sessions_quiesced]
      []
    end

    def wait_for_codex_sockets
      @events << [:codex_ready]
    end

    def restore_quiesced_sessions(_sessions)
      @events << [:sessions_restored]
    end

    def check_codex(command)
      @events << [:codex_checked, File.realpath(command)]
    end

    def busy_codex_sessions
      busy
    end

    def restart_codex_consumers
      @events << [:consumers_restarted]
      if fail_restart
        self.fail_restart = false
        raise VpsfreeWorkspaceHost::Error, 'injected consumer restart failure'
      end
    end
  end

  class ActivationGuardHost < VpsfreeWorkspaceHost::Host
    attr_reader :configured

    def initialize(package:, **options)
      super(**options)
      @package = package
      @configured = false
    end

    private

    def package_root
      @package
    end

    def configure_user_services
      @configured = true
    end

    def reconcile_codex_update(defer_busy:)
      defer_busy
    end
  end

  def registry_at(directory)
    VpsfreeWorkspaceHost::Registry.new(File.join(directory, 'config', 'registry.json'))
  end

  def make_workspace(parent, name)
    root = File.join(parent, name)
    %w[repos work worktrees].each { |item| FileUtils.mkdir_p(File.join(root, item)) }
    root
  end

  def make_codex(parent, name)
    package = File.join(parent, name)
    command = File.join(package, 'bin', 'codex')
    FileUtils.mkdir_p(File.dirname(command))
    File.write(command, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
    File.chmod(0o755, command)
    command
  end

  def install_source_profile(environment)
    profile = environment.fetch(
      'VPSFREE_WORKSPACES_PROFILE',
      File.join(environment.fetch('VPSFREE_WORKSPACES_STATE'), 'profile')
    )
    FileUtils.mkdir_p(File.dirname(profile))
    File.symlink(File.expand_path('..', __dir__), profile)
    environment.merge('VPSFREE_WORKSPACES_PROFILE' => profile)
  end

  def make_package(
    parent,
    name,
    cluster_contract: true,
    tracking_max: VpsfreeWorkspaceHost::RUNTIME_CONTRACT.fetch('trackingMaxBytes')
  )
    package = File.join(parent, name)
    command = File.join(package, 'bin', 'workspace-host')
    FileUtils.mkdir_p(File.dirname(command))
    File.write(command, "#!/bin/sh\nexit 0\n")
    File.chmod(0o755, command)
    if cluster_contract
      contract = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(contract))
      File.write(contract, JSON.generate(
        'developmentClusterStateSchema' => VpsfreeWorkspaceHost::RUNTIME_CONTRACT.fetch(
          'developmentClusterStateSchema'
        ),
        'trackingMaxBytes' => tracking_max
      ))
      %w[vpsadmin vpsadminos].each do |kind|
        helper = File.join(package, 'libexec/workspace-portal', "#{kind}-devcluster")
        FileUtils.mkdir_p(File.dirname(helper))
        File.write(helper, <<~SH)
          #!/bin/sh
          if [ "$1" = transition-adopt ] &&
             [ -f "$VPSFREE_DEVCLUSTER_WORKSPACE/.dev-clusters/#{kind}/clusters/$2/socket-dir" ]; then
            exit 0
          fi
          echo 'pre-contract cluster cannot be adopted' >&2
          exit 1
        SH
        File.chmod(0o755, helper)
      end
    end
    package
  end

  def host_environment(directory, config:, runtime: File.join(directory, 'runtime'), system_codex: nil)
    system_codex ||= make_codex(directory, 'codex-system')
    {
      'HOME' => directory,
      'PATH' => ENV.fetch('PATH'),
      'VPSFREE_WORKSPACES_CONFIG' => config,
      'VPSFREE_WORKSPACES_STATE' => File.join(directory, 'state'),
      'VPSFREE_WORKSPACES_RUNTIME_DIR' => runtime,
      'VPSFREE_WORKSPACES_PROFILE' => File.join(directory, 'state', 'profile'),
      'VPSFREE_WORKSPACES_SYSTEM_CODEX' => system_codex
    }
  end

  def with_transition_host(busy: [])
    Dir.mktmpdir('workspace-host-transition-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      VpsfreeWorkspaceHost::Registry.new(config).register(
        name: 'vpsfree-cz', root:, hostname: 'vpsfree-cz.workspace.example.test',
        aliases: [], replace: false
      )
      source = File.join(directory, 'source')
      FileUtils.mkdir_p(source)
      old_codex = make_codex(directory, 'codex-old')
      system_codex = make_codex(directory, 'codex-system')
      candidate = make_package(directory, 'package-one')
      environment = host_environment(directory, config:, system_codex:)
      host = TransitionHost.new(
        candidate:, busy:, env: environment, out: StringIO.new, err: StringIO.new
      )
      yield host, {
        root: directory,
        source:,
        old_codex:,
        system_codex:,
        current_root: File.join(directory, 'state', 'codex', 'current')
      }
    end
  end

  def portal_manifest(thread_id, socket, state)
    YAML.dump(
      'schema' => state == 'creating' ? 2 : 1,
      'slug' => 'ignored-by-host',
      'codex' => { 'thread_id' => thread_id, 'socket_path' => socket },
      'creation' => { 'state' => state }
    )
  end
end
