# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_finalize_runtime_output_uses_deployed_helper
    slug = '2026-06-06-demo'
    with_workspace do |workspace|
      runner_for(workspace).send(:ensure_tracking_files, slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      out = StringIO.new
      runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:),
        tmux_socket: '/run/workspace-tmux/tmux.sock',
        authority_dir: File.join(workspace, 'runtime-authority'),
        codex_socket: '/run/workspace-codex/app-server.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        portal_command: ['/run/current-system/sw/bin/workspace-portal'],
        cluster_providers: { 'alpha' => RbConfig.ruby, 'beta' => RbConfig.ruby },
        require_runtime: true,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      finalize_core(runner, slug, as_is: true)

      assert_includes(
        out.string,
        "stop after committing: dev-session stop #{slug} --as-is"
      )
    end
  end

  def test_finalize_keeps_real_tmux_session_until_explicit_stop
    skip 'git is not available' unless command_available?('git')
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      out = StringIO.new
      authority_dir = File.join(workspace, 'runtime-authority')
      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        authority_dir:,
        codex_command: 'false',
        out:,
        err: StringIO.new,
        today: TODAY
      )
      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      ordinary_runner = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      finalize_core(ordinary_runner, 'demo', as_is: false)

      assert(File.directory?(File.join(workspace, 'archive', slug)))
      assert(tmux_session_exists?(socket, slug))
      assert_includes(out.string, 'stop after committing')

      commit_archive_move(workspace, slug)
      ordinary_runner.stop(slug, as_is: true)
      refute(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_tmux_codex_runs_from_shell_and_leaves_shell_available
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      codex_probe = File.join(workspace, 'codex-ran.txt')
      shell_probe = File.join(workspace, 'shell-remained.txt')
      codex_command = "printf codex > #{Shellwords.escape(codex_probe)}"
      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command:,
        portal_command: [],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: true)
      wait_for_file(codex_probe)

      panes = tmux_capture(
        socket,
        'list-panes',
        '-t',
        "#{slug}:dev",
        '-F',
        "\#{pane_id}\t\#{pane_current_path}"
      ).lines.map { |line| line.chomp.split("\t", 2) }
      left = panes.find { |_id, path| path == workspace }.fetch(0)
      command = "printf shell > #{Shellwords.escape(shell_probe)}"

      tmux_run(socket, 'send-keys', '-t', left, '-l', command)
      tmux_run(socket, 'send-keys', '-t', left, 'Enter')
      wait_for_file(shell_probe)

      assert_equal('codex', File.read(codex_probe))
      assert_equal('shell', File.read(shell_probe))
      assert(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_shared_session_records_codex_endpoint_provenance
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'
    codex_socket = '/run/dev-workspace-codex/app-server.sock'

    with_workspace do |workspace|
      codex_executable = File.join(workspace, 'codex')
      codex_log = File.join(workspace, 'codex.log')
      File.write(codex_executable, <<~SH)
        #!/bin/sh
        [ "$1" = --version ] && { echo 'codex-cli 0.152.1'; exit 0; }
        printf '%s\n' "$@" > #{(codex_log + '.tmp').dump}
        mv #{(codex_log + '.tmp').dump} #{codex_log.dump}
        sleep 2
      SH
      File.chmod(0o755, codex_executable)
      portal_log = File.join(workspace, 'portal.log')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Investigate the reported issue.\n")
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{portal_log.dump}, 'a') { |file| file.puts ARGV.join(' ') }
        if ARGV[0, 2] == ['thread', 'create']
          puts JSON.generate(threadId: 'thread-123')
        end
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_socket:,
        codex_version: '0.152.1',
        codex_command: codex_executable,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal
      )

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-123', manifest.dig('codex', 'thread_id'))
      assert_equal(codex_socket, manifest.dig('codex', 'socket_path'))
      assert_equal('0.152.1', manifest.dig('codex', 'client_version'))
      refute(manifest.key?('tmux'))
      wait_for_file(codex_log)
      codex_arguments = File.readlines(codex_log, chomp: true)
      assert_includes(codex_arguments, '--remote')
      assert_includes(codex_arguments, "unix://#{codex_socket}")
      assert_includes(codex_arguments, 'resume')
      assert_includes(codex_arguments, 'thread-123')
      portal_commands = File.readlines(portal_log, chomp: true)
      assert(portal_commands[0].start_with?('thread create '))
      assert(portal_commands[1].start_with?('thread set-name '))
      assert(portal_commands[2].start_with?('thread ensure-initial '))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_codex_provenance_requires_the_configured_executable_version
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Investigate the reported issue.\n")
      codex_executable = File.join(workspace, 'codex')
      File.write(codex_executable, "#!/bin/sh\necho 'codex-cli 0.151.0'\n")
      File.chmod(0o755, codex_executable)
      session = DevSession::Tmux::Session.new(
        id: '$created', name: slug, mark: '1', slug:, workspace:
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **_keywords|
          session
        end
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/codex.sock',
        codex_version: '0.152.1',
        codex_command: codex_executable,
        portal_command: [
          RbConfig.ruby,
          '-e',
          "require 'json'; puts JSON.generate(threadId: 'thread-123')"
        ],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: goal
        )
      end
      assert_includes(error.message, 'does not report configured version')
    end
  end

  def test_tmux_start_and_sync_manage_only_worktree_windows
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      create_bare_repo(workspace, 'alpha')
      repository = File.join(workspace, 'repos', 'alpha.git')
      master = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      assert_git_success('git', "--git-dir=#{repository}", 'branch', slug, 'master')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'update-ref',
        'refs/remotes/origin/master', master
      )
      assert_git_success(
        'git', "--git-dir=#{repository}", 'symbolic-ref',
        'refs/remotes/origin/HEAD', 'refs/remotes/origin/master'
      )
      worktree = File.join(workspace, 'worktrees', slug, 'alpha')
      FileUtils.mkdir_p(File.dirname(worktree))
      assert_git_success(
        'git', "--git-dir=#{repository}", 'worktree', 'add', worktree, slug
      )

      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)

      session_env = tmux_capture(socket, 'show-environment', '-t', slug)
      assert_includes(session_env, "#{DevSession::ENV_SLUG}=#{slug}\n")
      assert_includes(
        session_env,
        "#{DevSession::ENV_WORKSPACE}=#{workspace}\n"
      )
      assert_includes(
        session_env,
        "#{DevSession::ENV_WORK_DIR}=#{File.join(workspace, 'work', slug)}\n"
      )
      assert_includes(
        session_env,
        "#{DevSession::ENV_WORKTREES_DIR}=#{File.join(workspace, 'worktrees', slug)}\n"
      )
      assert_includes(
        session_env,
        "#{DevSession::ENV_PORTAL_BASE_URL}=#{DevSession::DEFAULT_PORTAL_BASE_URL}\n"
      )
      assert_includes(
        session_env,
        "#{DevSession::ENV_PORTAL_URL}=#{DevSession::DEFAULT_PORTAL_BASE_URL}/#{slug}/\n"
      )

      panes = tmux_capture(socket, 'list-panes', '-t', "#{slug}:dev", '-F', '#{pane_current_path}')
              .lines
              .map(&:chomp)

      assert_equal(3, panes.length)
      assert_includes(panes, workspace)
      assert_includes(panes, File.join(workspace, 'work', slug))
      assert_includes(panes, File.join(workspace, 'worktrees', slug))

      windows = tmux_capture(
        socket,
        'list-windows',
        '-t',
        slug,
        '-F',
        '#{window_name}:#{@dev_session_window}'
      ).lines.map(&:chomp)

      assert_includes(windows, 'alpha:worktree')

      probe = File.join(workspace, 'alpha-env.txt')
      command = "printf '%s\\n' \"$#{DevSession::ENV_SLUG}\" > #{Shellwords.escape(probe)}"
      tmux_run(socket, 'send-keys', '-t', "#{slug}:alpha", command, 'Enter')
      wait_for_file(probe)
      assert_equal(slug, File.read(probe).strip)

      tmux_run(socket, 'new-window', '-d', '-t', slug, '-n', 'custom', '-c', workspace)
      FileUtils.rm_rf(worktree)
      runner.sync('demo', as_is: false)

      names = tmux_capture(socket, 'list-windows', '-t', slug, '-F', '#{window_name}')
              .lines
              .map(&:chomp)

      assert_includes(names, 'custom')
      refute_includes(names, 'alpha')
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_codex_endpoint_identity_survives_compatible_client_upgrade
    with_workspace do |workspace|
      runner = DevSession::Runner.new(
        workspace:,
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        out: StringIO.new,
        err: StringIO.new,
        env: {}
      )
      session = DevSession::Tmux::Session.new(
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.152.1'
      )

      assert(runner.send(:session_codex_provenance_matches?, session, 'thread-1'))
      session.codex_socket_path = '/run/workspace/other.sock'
      refute(runner.send(:session_codex_provenance_matches?, session, 'thread-1'))
    end
  end

  def test_start_rejects_a_ready_thread_from_another_app_server_runtime
    with_workspace do |workspace|
      slug = '2026-06-06-migrated'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(slug)
      manifest = setup.send(:ensure_portal_manifest, slug)
      manifest['codex'] = {
        'thread_id' => 'thread-1',
        'socket_path' => '/run/old/app-server.sock',
        'client_version' => '0.151.0'
      }
      setup.send(:write_portal_manifest, slug, manifest)
      codex = File.join(workspace, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.2'\n")
      File.chmod(0o755, codex)
      session = DevSession::Tmux::Session.new(
        id: '$migrated', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/new/tmux.sock', codex_thread_id: 'thread-1',
        codex_socket_path: '/run/new/app-server.sock',
        codex_client_version: '0.153.2', codex_pane_id: '%1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:sync_slug) { |*_args, **_options| session }
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/new/app-server.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        portal_command: [
          RbConfig.ruby,
          '-e',
          "require 'json'; puts JSON.generate(threadId: 'thread-1') if ARGV[0, 2] == ['thread', 'create']"
        ],
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )

      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: true)
      end

      assert_includes(error.message, 'belongs to another runtime')
      retained = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-1', retained.dig('codex', 'thread_id'))
      assert_equal('/run/old/app-server.sock', retained.dig('codex', 'socket_path'))
      assert_equal('0.151.0', retained.dig('codex', 'client_version'))
    end
  end

  def test_exclusive_replay_rejects_a_ready_thread_from_another_runtime
    with_workspace do |workspace|
      slug = '2026-06-06-old-runtime'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue the original request.\n")
      setup = runner_for(workspace)
      journal = setup.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      setup.ensure_tracking_files(slug)
      manifest = setup.send(:ensure_portal_manifest, slug, creation_journal: journal)
      manifest['schema'] = 1
      manifest['creation']['state'] = 'ready'
      manifest['creation']['initial_goal_sent'] = true
      manifest['creation'].delete('initial_goal_attempted')
      manifest['codex'] = {
        'thread_id' => 'thread-old',
        'socket_path' => '/run/old/app-server.sock',
        'client_version' => '0.151.0'
      }
      setup.send(:write_portal_manifest, slug, manifest)
      setup.send(:mark_creation_journal_ready, slug, journal)
      called = File.join(workspace, 'portal-called')
      portal = [RbConfig.ruby, '-e', "File.write(#{called.dump}, 'called'); exit 1"]
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.152.1',
        portal_command: portal,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: goal,
          json: true,
          exclusive: true
        )
      end

      assert_includes(error.message, 'belongs to another runtime')
      refute(File.exist?(called))
      retained = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('/run/old/app-server.sock', retained.dig('codex', 'socket_path'))
    end
  end

  def test_attach_restarts_terminal_client_after_app_server_disconnect
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      codex = File.join(workspace, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.2'\n")
      File.chmod(0o755, codex)
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.152.1',
        pane_current_command: 'sh'
      )
      errors = StringIO.new
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        out: StringIO.new,
        err: errors,
        env: { 'SHELL' => '/bin/sh' }
      )

      session = runner.send(:reconcile_native_client!, slug, tmux.session(slug))

      assert_equal('$managed', session.id)
      assert_equal('0.153.2', session.codex_client_version)
      assert_equal(1, tmux.sent_commands.length)
      assert_includes(tmux.sent_commands.first, codex)
      assert_includes(tmux.sent_commands.first, '--remote unix:///run/workspace/codex.sock')
      assert_includes(tmux.sent_commands.first, 'resume thread-1')
      assert_includes(errors.string, 'restarted terminal Codex client')

      running = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.153.2',
        pane_current_command: '.codex-wrapped'
      )
      running_runner = DevSession::Runner.new(
        workspace:,
        tmux: running,
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )
      running_runner.send(:reconcile_native_client!, slug, running.session(slug))
      assert_empty(running.sent_commands)
    end
  end

  def test_terminal_reconciliation_refuses_an_incomplete_creation
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      portal_log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal.rb')
      codex = File.join(workspace, 'codex')
      File.write(portal, <<~RUBY)
        File.write(#{portal_log.dump}, ARGV.join(' '))
      RUBY
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.2'\n")
      File.chmod(0o755, codex)
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-creating',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.153.2',
        pane_current_command: 'sh'
      )
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['schema'] = 2
      manifest['codex'] = {
        'thread_id' => 'thread-creating',
        'socket_path' => '/run/workspace/codex.sock',
        'client_version' => '0.153.2'
      }
      manifest['creation'] = {
        'state' => 'creating',
        'initial_goal_sent' => false,
        'initial_goal_attempted' => true,
        'goal_sha256' => Digest::SHA256.hexdigest('initial request')
      }
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::Error) do
        runner.send(:reconcile_native_client!, slug, tmux.session(slug))
      end

      assert_includes(error.message, 'session creation is incomplete')
      assert_empty(tmux.sent_commands)
      refute(File.exist?(portal_log))
    end
  end

  def test_concurrent_attach_cannot_launch_codex_during_initial_delivery
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'authority')
      goal = File.join(workspace, 'goal.txt')
      portal = File.join(workspace, 'portal.rb')
      codex = File.join(workspace, 'codex')
      File.write(goal, "Deliver the initial request.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        if ARGV[0, 2] == ['thread', 'create']
          puts JSON.generate(threadId: 'thread-concurrent')
        end
      RUBY
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.2'\n")
      File.chmod(0o755, codex)
      entered_delivery = Queue.new
      release_delivery = Queue.new
      created_session = DevSession::Tmux::Session.new(
        id: '$9', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-concurrent',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.153.2', codex_pane_id: '%1'
      )
      creator_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| created_session }
        define_method(:sync_slug) { |*_arguments, **_keywords| created_session }
        define_method(:revalidate_session!) { |_expected| created_session }
        define_method(:send_portal_goal) do |*_arguments, **_keywords|
          entered_delivery << true
          release_delivery.pop
        end
        define_method(:reconcile_native_client!) do |_slug, session, **_keywords|
          session
        end
      end
      creator = creator_class.new(
        workspace:,
        authority_dir:,
        tmux_socket: '/run/workspace/tmux.sock',
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        portal_command: [RbConfig.ruby, portal],
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )
      creator_thread = Thread.new do
        creator.start(
          slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, exclusive: true
        )
      end
      entered_delivery.pop

      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/workspace/tmux.sock',
        codex_thread_id: 'thread-concurrent',
        codex_socket_path: '/run/workspace/codex.sock',
        codex_client_version: '0.153.2',
        pane_current_command: 'sh',
        id: '$9'
      )
      attacher = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        tmux_socket: '/run/workspace/tmux.sock',
        codex_socket: '/run/workspace/codex.sock',
        codex_version: '0.153.2',
        codex_command: codex,
        portal_command: [RbConfig.ruby, portal],
        tmux:,
        process_exec: ->(*_arguments) { raise 'attach unexpectedly replaced the process' },
        out: StringIO.new,
        err: StringIO.new,
        env: { 'SHELL' => '/bin/sh' }
      )

      error = assert_raises(DevSession::Error) do
        attacher.attach(slug, as_is: true)
      end
      assert_includes(error.message, 'session creation is incomplete')
      assert_empty(tmux.sent_commands)
    ensure
      release_delivery << true if release_delivery
      creator_thread&.value
    end
  end

end
