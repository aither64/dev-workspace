# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class WorkspaceHostTest < Minitest::Test
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
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = QuiesceHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => runtime
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
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => runtime,
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      Dir.chdir(directory) do
        assert_equal(0, host.run('dev-session', ['--workspace', 'example-workspace', 'list']))
      end
      environment, command, arguments = host.captured
      assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
      assert_equal(File.join(directory, '.codex'), environment.fetch('DEV_WORKSPACE_CODEX_HOME'))
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, root)
      assert_includes(arguments, File.join(runtime, 'example-workspace', 'app-server.sock'))
      lock_index = arguments.index('--transition-lock')
      assert_operator(lock_index, :<, arguments.index('--'))
      assert_equal(File.join(state, 'transition.lock'), arguments.fetch(lock_index + 1))
      token_index = arguments.index('--expected-host-profile-token')
      assert_operator(token_index, :<, arguments.index('--'))
      assert_match(/\A[0-9a-f]{64}\z/, arguments.fetch(token_index + 1))
      assert_equal('list', arguments.last)
    end
  end

  def test_ordinary_dispatch_passes_the_selected_codex_home_and_inherited_environment
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      selected_home = File.join(directory, 'selected-codex-home')
      configured_home = File.join(directory, 'codex-home')
      FileUtils.mkdir_p(selected_home)
      File.symlink(selected_home, configured_home)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      inherited = install_source_profile(host_environment(directory, config:)).merge(
        'CODEX_HOME' => configured_home,
        'DEV_WORKSPACE_CODEX_HOME' => File.join(directory, 'stale-codex-home'),
        'DEV_SESSION_SLUG' => '2026-09-06-test',
        'DEV_SESSION_WORKSPACE' => root,
        'DISPATCH_MARKER' => 'inherited-value'
      )
      host = CapturingHost.new(env: inherited, out: StringIO.new, err: StringIO.new)
      package = File.realpath(inherited.fetch('DEV_WORKSPACES_PROFILE'))
      expected_options = {
        '--workspace' => root,
        '--expected-host-generation' => package,
        '--host-profile' => inherited.fetch('DEV_WORKSPACES_PROFILE'),
        '--expected-host-profile-token' => host.send(:profile_link_token),
        '--transition-lock' => File.join(directory, 'state/transition.lock'),
        '--codex-socket' => File.join(directory, 'runtime/example-workspace/app-server.sock'),
        '--portal-command' => File.join(package, 'bin/workspace-portal')
      }

      [['list'], ['archive', '2026-09-06-test', '--as-is']].each do |request|
        assert_equal(0, host.run('dev-session', ['--workspace', 'example-workspace', *request]))
        environment, command, arguments = host.captured
        assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
        assert_equal(selected_home, environment.fetch('DEV_WORKSPACE_CODEX_HOME'))
        inherited.each do |key, value|
          next if key == 'DEV_WORKSPACE_CODEX_HOME'

          assert_equal(value, environment.fetch(key), key)
        end
        assert_equal(File.join(package, 'libexec/workspace-portal/dev-session'), command)
        expected_options.each do |option, value|
          option_index = arguments.index(option)
          assert_operator(option_index, :<, arguments.index('--'), option)
          assert_equal(value, arguments.fetch(option_index + 1), option)
        end
        assert_equal(request, arguments.drop(arguments.index('--') + 1))
      end
    end
  end

  def test_exec_with_workspace_keeps_the_default_environment_for_existing_callers
    Dir.mktmpdir('workspace-host-test') do |directory|
      inherited = { 'HOME' => directory, 'DISPATCH_MARKER' => 'inherited-value' }
      host = CapturingHost.new(env: inherited, out: StringIO.new, err: StringIO.new)

      host.send(:exec_with_workspace, { 'name' => 'example-workspace' }, '/command', 'argument')

      assert_equal(
        [inherited.merge('DEV_WORKSPACE_NAME' => 'example-workspace'), '/command', ['argument']],
        host.captured
      )
    end
  end

  def test_cluster_commands_hold_the_shared_host_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      profile = File.join(directory, 'profile')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      File.symlink(File.expand_path('../..', __dir__), profile)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_PROFILE' => profile,
          'DEV_WORKSPACES_EXTENSION_CATALOG' => source_extension_catalog(directory)
        },
        out: StringIO.new,
        err: StringIO.new
      )
      result = Thread.new do
        host.run('alpha-devcluster', ['--workspace', 'example-workspace', 'status', '2026-09-06-test'])
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
        DevWorkspaceHost::Registry.new(config).register(
          name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
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
            'DEV_WORKSPACES_CONFIG' => config,
            'DEV_WORKSPACES_STATE' => state,
            'DEV_WORKSPACES_PROFILE' => profile
          },
          out: StringIO.new,
          err: error_output
        )
        result = Thread.new do
          host.run('alpha-devcluster', [
            '--workspace', 'example-workspace', 'status', '2026-09-06-test'
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
            'DEV_WORKSPACES_STATE' => state,
            'DEV_WORKSPACES_PROFILE' => profile
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
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      File.symlink(File.expand_path('../..', __dir__), profile)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_PROFILE' => profile,
          'DEV_WORKSPACES_EXTENSION_CATALOG' => source_extension_catalog(directory),
          'DEV_WORKSPACE_TRANSITION_HELD' => '1',
          'DEV_SESSION_LIFECYCLE_OPERATION' => 'archive'
        },
        out: StringIO.new,
        err: StringIO.new
      )

      result = Thread.new do
        host.run(
          'alpha-devcluster',
          ['--workspace', 'example-workspace', 'reset', '2026-09-06-test']
        )
      end
      sleep 0.05
      assert_nil(host.captured)
      owner.flock(File::LOCK_UN)

      assert_equal(0, result.value)
      refute_nil(host.captured)
      environment, = host.captured
      refute(environment.key?('DEV_WORKSPACE_TRANSITION_HELD'))
      refute(environment.key?('DEV_SESSION_LIFECYCLE_OPERATION'))
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
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => File.join(directory, 'state'),
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(
        0,
        host.run(
          'dev-session',
          ['--workspace', 'example-workspace', 'delete', '2026-09-06-test', '--as-is']
        )
      )
      environment, command, arguments = host.captured
      refute(environment.key?('DEV_WORKSPACE_TRANSITION_HELD'))
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
      selected_home = File.join(directory, 'selected-codex-home')
      configured_home = File.join(directory, 'codex-home')
      FileUtils.mkdir_p(selected_home)
      File.symlink(selected_home, configured_home)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
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
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex,
          'CODEX_HOME' => configured_home,
          'DEV_WORKSPACE_CODEX_HOME' => File.join(directory, 'stale-codex-home'),
          'DISPATCH_MARKER' => 'inherited-value',
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => owner.fileno.to_s
        },
        out: StringIO.new,
        err: error_output
      )

      assert_equal(0, host.run(
        'dev-session',
        ['--workspace', 'example-workspace', 'delete', '2026-09-06-test', '--as-is']
      ), error_output.string)
      environment, command, arguments = host.captured
      assert_equal(owner.fileno.to_s, environment.fetch('DEV_WORKSPACE_TRANSITION_LOCK_FD'))
      assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
      assert_equal(selected_home, environment.fetch('DEV_WORKSPACE_CODEX_HOME'))
      assert_equal(configured_home, environment.fetch('CODEX_HOME'))
      assert_equal('inherited-value', environment.fetch('DISPATCH_MARKER'))
      assert_equal('dev-session', File.basename(command))
      assert_equal(root, arguments.fetch(arguments.index('--workspace') + 1))
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
      host = DevWorkspaceHost::Host.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => impostor.fileno.to_s
        },
        out: StringIO.new,
        err: StringIO.new
      )

      refute(host.send(:inherited_exclusive_transition_lock?))

      host = DevWorkspaceHost::Host.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => owner.fileno.to_s
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
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(
        0,
        host.run(
          'dev-session',
          ['--workspace', 'example-workspace', 'archive', '2026-09-06-test', '--as-is']
        )
      )
      environment, command, arguments = host.captured
      assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
      assert_equal(File.join(directory, '.codex'), environment.fetch('DEV_WORKSPACE_CODEX_HOME'))
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
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )
      argv = [
        '--workspace', 'example-workspace', 'archive', '--abandoned',
        '2026-09-06-Foo_bar', '--as-is'
      ]

      assert_equal(0, host.run('dev-session', argv))
      _environment, command, arguments = host.captured
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, '--transition-lock')
      assert_equal(['archive', '--abandoned', '2026-09-06-Foo_bar', '--as-is'], arguments.last(4))
    end
  end

end
