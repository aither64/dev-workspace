# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_creation_retry_preserves_previous_tracking
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume the recorded request.\n")
      crashing_class = Class.new(DevSession::Runner) do
        def ensure_tracking_files(_slug, **)
          raise DevSession::Error, 'simulated crash before tracking creation'
        end
      end
      crashing = crashing_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      assert_raises(DevSession::Error) do
        crashing.start(slug, as_is: true, new: false, attach: false,
                       run_codex: false, goal_file: goal, json: true, exclusive: true)
      end
      directory = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(directory)
      previous_plan = previous_tracking_template('plan', slug)
      File.write(File.join(directory, 'plan.md'), previous_plan)
      previous_state = previous_tracking_template('state', slug)
      File.write(File.join(directory, 'state.md'), previous_state)

      retry_class = Class.new(DevSession::Runner) do
        def create_tmux_session(*, **)
          raise DevSession::Error, 'reached tmux creation'
        end
      end
      runner = retry_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false,
                     run_codex: false, goal_file: goal, json: true, exclusive: true)
      end
      assert_equal('reached tmux creation', error.message)

      assert_equal(
        previous_plan.sub("## Goal\n\n", "## Goal\n\nResume the recorded request.\n\n"),
        File.read(File.join(directory, 'plan.md'))
      )
      assert_equal(
        previous_state.sub("## Status\n\n", "## Status\n\n- Session created with an initial request.\n\n"),
        File.read(File.join(directory, 'state.md'))
      )
    end
  end

  def test_exclusive_browser_start_retries_partial_creation_without_duplicate_thread
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      log = File.join(workspace, 'portal.log')
      failure_marker = File.join(workspace, 'send-failed')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(goal, "Implement a retry-safe feature.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{log.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        case ARGV[1]
        when 'create'
          puts JSON.generate(threadId: 'thread-retry')
        when 'ensure-initial'
          unless File.exist?(#{failure_marker.dump})
            File.write(#{failure_marker.dump}, "failed\n")
            warn 'simulated send failure'
            exit 1
          end
        end
      RUBY
      out = StringIO.new
      created_session = DevSession::Tmux::Session.new(
        id: '$created', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **_keywords|
          created_session
        end

        define_method(:sync_slug) do |*_arguments, **_keywords|
          created_session
        end

        define_method(:revalidate_session!) do |_expected|
          created_session
        end
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {},
        codex_socket: '/run/test/codex.sock',
        portal_command: [RbConfig.ruby, portal]
      )

      assert_raises(DevSession::CommandError) do
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
      partial = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-retry', partial.dig('codex', 'thread_id'))
      assert_equal(2, partial.fetch('schema'))
      assert_equal('creating', partial.dig('creation', 'state'))
      refute(partial.dig('creation', 'initial_goal_sent'))
      assert(partial.dig('creation', 'initial_goal_attempted'))

      creation_identity = JSON.parse(
        File.read(runner.send(:creation_journal_file, slug))
      ).fetch('tmux_identity')
      runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:, identity_token: creation_identity),
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
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
      commands = File.readlines(log, chomp: true)
      creation_commands = commands.select { |line| line.start_with?('thread create ') }
      assert_equal(2, creation_commands.length)
      creation_commands.each { |line| assert_includes(line, '--recover-creating') }
      refute_includes(creation_commands.fetch(0), '--thread-id')
      assert_includes(creation_commands.fetch(1), '--thread-id thread-retry')
      assert_equal(1, commands.count { |line| line.start_with?('thread set-name ') })
      initial_commands = commands.select { |line| line.start_with?('thread ensure-initial ') }
      assert_equal(2, initial_commands.length)
      assert_includes(initial_commands.fetch(0), '--start-unmaterialized')
      refute_includes(initial_commands.fetch(1), '--start-unmaterialized')
      complete = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('ready', complete.dig('creation', 'state'))
      assert(complete.dig('creation', 'initial_goal_sent'))
      assert_equal(1, complete.fetch('schema'))
      refute(complete.fetch('creation').key?('initial_goal_attempted'))

      out.truncate(0)
      out.rewind
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
      assert_equal('thread-retry', JSON.parse(out.string).fetch('threadId'))
      replayed_commands = File.readlines(log, chomp: true)
      assert_equal(2, replayed_commands.count { |line| line.start_with?('thread create ') })
      assert_equal(1, replayed_commands.count { |line| line.start_with?('thread set-name ') })
      assert_equal(2, replayed_commands.count { |line| line.start_with?('thread ensure-initial ') })
      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      assert_equal('ready', journal.fetch('state'))

      replay_out = StringIO.new
      replay_runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out: replay_out,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      replay_runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )
      replay = JSON.parse(replay_out.string)
      assert_equal('thread-retry', replay.fetch('threadId'))
      assert_nil(replay.fetch('attach'))
      assert_equal(replayed_commands, File.readlines(log, chomp: true))

      stopped_session = DevSession::Tmux::Session.new(
        id: '$restarted', name: slug, mark: '1', slug:, workspace:
      )
      restart_runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| stopped_session }
        define_method(:sync_slug) { |*_arguments, **_keywords| stopped_session }
        define_method(:revalidate_session!) { |_expected| stopped_session }
      end
      restart_out = StringIO.new
      restart_runner = restart_runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: restart_out,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      restart_runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        json: true
      )
      assert_equal('thread-retry', JSON.parse(restart_out.string).fetch('threadId'))
      restarted_commands = File.readlines(log, chomp: true)
      assert_equal(3, restarted_commands.count { |line| line.start_with?('thread create ') })
      assert_equal(2, restarted_commands.count { |line| line.start_with?('thread ensure-initial ') })
    end
  end

  def test_start_discards_a_stale_authority_thread_before_recreating_tmux
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      portal_log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal')
      codex = File.join(workspace, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.152.1'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{portal_log.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        puts JSON.generate(threadId: 'thread-manifest') if ARGV[1] == 'create'
      RUBY

      setup = DevSession::Runner.new(
        workspace:, authority_dir:, tmux: NullTmux.new,
        out: StringIO.new, err: StringIO.new, today: TODAY, env: {}
      )
      setup.ensure_tracking_files(slug)
      manifest = setup.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-manifest',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      setup.send(:write_portal_manifest, slug, manifest)
      stale = DevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-stale',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      setup.send(:write_session_authority, slug, stale, state: 'ready')

      created_threads = []
      created = stale.dup
      created.id = '$8'
      created.codex_thread_id = 'thread-manifest'
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |_slug, thread_id:, **_keywords|
          created_threads << thread_id
          created
        end
        define_method(:sync_slug) { |*_arguments, **_keywords| created }
        define_method(:revalidate_session!) { |_expected| created }
      end
      out = StringIO.new
      runner = runner_class.new(
        workspace:, authority_dir:, tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock', codex_version: '0.152.1',
        codex_command: codex, tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal], out:, err: StringIO.new,
        today: TODAY, env: {}
      )

      runner.start(
        slug, as_is: true, new: false, attach: false, run_codex: true, json: true
      )

      assert_equal(['thread-manifest'], created_threads)
      assert_equal('thread-manifest', JSON.parse(out.string).fetch('threadId'))
      commands = File.readlines(portal_log, chomp: true)
      assert(commands.any? { |line| line.start_with?('thread require-materialized ') })
      create = commands.find { |line| line.start_with?('thread create ') }
      assert_includes(create, '--thread-id thread-manifest')
      {
        '--session-slug' => slug,
        '--workspace' => workspace,
        '--cwd' => File.join(workspace, 'work', slug),
        '--worktrees-dir' => File.join(workspace, 'worktrees', slug),
        '--authority-dir' => authority_dir,
        '--tmux-socket' => '/run/test/tmux.sock',
        '--codex-command' => codex,
        '--socket' => '/run/test/codex.sock',
        '--codex-version' => '0.152.1',
        '--portal-command' => RbConfig.ruby
      }.each do |argument, value|
        assert_includes(create, "#{argument} #{value}")
      end
      refute_includes(create, 'thread-stale')
      authority = JSON.parse(File.read(File.join(authority_dir, "#{slug}.json")))
      assert_equal('thread-manifest', authority.fetch('codex_thread_id'))
      assert_equal('$8', authority.fetch('tmux_session_id'))
    end
  end

  def test_start_rejects_a_renamed_authority_session
    with_workspace do |workspace|
      slug = '2026-06-06-renamed-start'
      authority_dir = File.join(workspace, 'authority')
      tmux = RenamedManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      session = DevSession::Tmux::Session.new(
        id: '$11', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test.sock',
        identity_token: 'a' * 64
      )
      runner.send(:write_session_authority, slug, session, state: 'ready')

      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_initial_turn_starts_after_private_local_creation_and_without_the_slug_lock
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      authority_dir = File.join(workspace, 'runtime-authority')
      observation = File.join(workspace, 'initial-turn-observation')
      portal = File.join(workspace, 'fake-portal.rb')
      codex = File.join(workspace, 'codex')
      File.write(goal, "Implement after local setup.\n")
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.152.1'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        require 'json'
        require 'yaml'
        case ARGV[1]
        when 'create'
          puts JSON.generate(threadId: 'thread-123')
        when 'ensure-initial'
          authority = JSON.parse(File.read(#{File.join(authority_dir, "#{slug}.json").dump}))
          manifest = YAML.safe_load(File.read(#{File.join(workspace, 'work', slug, 'portal.yml').dump}))
          exit 2 unless authority['state'] == 'creating'
          exit 3 unless manifest.dig('creation', 'state') == 'creating'
          File.open(#{File.join(authority_dir, "#{slug}.lock").dump}, File::RDWR) do |lock|
            exit 4 unless lock.flock(File::LOCK_EX | File::LOCK_NB)
          end
          File.write(#{observation.dump}, "ready for initial turn\n")
        end
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-123',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |_expected| session }
        define_method(:reconcile_native_client!) do |_slug, expected, **_keywords|
          raise 'initial request was not persisted before Codex launch' unless File.file?(observation)

          expected
        end
      end
      runner = runner_class.new(
        workspace:,
        authority_dir:,
        tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: codex,
        tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      runner.start(
        slug, as_is: true, new: false, attach: false, run_codex: true,
        goal_file: goal, json: true, exclusive: true
      )

      assert_equal("ready for initial turn\n", File.read(observation))
      authority = JSON.parse(File.read(File.join(authority_dir, "#{slug}.json")))
      assert_equal('ready', authority.fetch('state'))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('ready', manifest.dig('creation', 'state'))
      assert(manifest.dig('creation', 'initial_goal_sent'))
    end
  end

  def test_exclusive_browser_retry_replaces_unmaterialized_thread_after_app_server_restart
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'fake-portal.rb')
      codex = File.join(workspace, 'codex')
      authority_dir = File.join(workspace, 'runtime-authority')
      File.write(goal, "Recover after an App Server restart.\n")
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.153.0'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{log.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        case ARGV[1]
        when 'create'
          id = ARGV.include?('--thread-id') ? 'thread-replacement' : 'thread-vanished'
          puts JSON.generate(threadId: id)
        when 'ensure-initial'
          id = ARGV.fetch(ARGV.index('--thread-id') + 1)
          exit 1 if id == 'thread-vanished'
        end
      RUBY
      original = DevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-vanished', codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.153.0', codex_pane_id: '%1'
      )
      first_runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **keywords|
          original.identity_token = keywords.fetch(:identity_token)
          original
        end
        define_method(:sync_slug) { |*_arguments, **_keywords| original }
        define_method(:revalidate_session!) { |_expected| original }
      end
      first = first_runner_class.new(
        workspace:, authority_dir:, tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock', codex_version: '0.153.0',
        codex_command: codex, tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal], out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      assert_raises(DevSession::CommandError) do
        first.start(
          slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, exclusive: true
        )
      end

      creation_identity = JSON.parse(
        File.read(first.send(:creation_journal_file, slug))
      ).fetch('tmux_identity')

      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-vanished', codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.153.0', codex_pane_id: '%1', id: '$7',
        identity_token: creation_identity
      )
      replacement = original.dup
      replacement.id = '$8'
      replacement.codex_thread_id = 'thread-replacement'
      second_runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **keywords|
          replacement.identity_token = keywords.fetch(:identity_token)
          replacement
        end
        define_method(:sync_slug) { |*_arguments, **_keywords| replacement }
        define_method(:revalidate_session!) { |expected| expected }
        define_method(:reconcile_native_client!) { |_slug, expected, **_keywords| expected }
      end
      second = second_runner_class.new(
        workspace:, authority_dir:, tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock', codex_version: '0.153.0',
        codex_command: codex, tmux:,
        portal_command: [RbConfig.ruby, portal], out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      second.start(
        slug, as_is: true, new: false, attach: false, run_codex: true,
        goal_file: goal, json: true, exclusive: true
      )

      assert(tmux.killed)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-replacement', manifest.dig('codex', 'thread_id'))
      assert_equal('ready', manifest.dig('creation', 'state'))
      assert(manifest.dig('creation', 'initial_goal_sent'))
      authority = JSON.parse(File.read(File.join(authority_dir, "#{slug}.json")))
      assert_equal('$8', authority.fetch('tmux_session_id'))
      assert_equal('thread-replacement', authority.fetch('codex_thread_id'))
      assert_equal('ready', authority.fetch('state'))
      commands = File.readlines(log, chomp: true)
      assert_equal(2, commands.count { |line| line.start_with?('thread create ') })
      commands.select { |line| line.start_with?('thread create ') }.each do |line|
        assert_includes(line, '--recover-creating')
      end
      assert(commands.any? do |line|
        line.start_with?('thread ensure-initial ') &&
          line.include?('--thread-id thread-replacement') &&
          line.include?("--cwd #{File.join(workspace, 'work', slug)}")
      end)
    end
  end

  def test_exclusive_browser_replay_completes_terminal_journal_after_authority_ready
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      authority_dir = File.join(workspace, 'runtime-authority')
      portal = File.join(workspace, 'fake-portal.rb')
      codex = File.join(workspace, 'codex')
      File.write(goal, "Complete the recoverable initial turn.\n")
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.152.1'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        require 'json'
        if ARGV[0, 2] == ['thread', 'create']
          puts JSON.generate(threadId: 'thread-crash')
        end
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$8', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-crash',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      crashing_runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |_expected| session }
        define_method(:reconcile_native_client!) { |_slug, expected, **_keywords| expected }

        def mark_creation_journal_ready(_slug, _journal)
          raise DevSession::Error, 'simulated crash before terminal journal'
        end
      end
      runner = crashing_runner_class.new(
        workspace:,
        authority_dir:,
        tmux_socket: '/run/test/tmux.sock',
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: codex,
        tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      assert_raises(DevSession::Error) do
        runner.start(
          slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, exclusive: true
        )
      end
      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      authority_path = File.join(authority_dir, "#{slug}.json")
      authority = JSON.parse(File.read(authority_path))
      assert_equal('creating', journal.fetch('state'))
      assert_equal('ready', manifest.dig('creation', 'state'))
      assert_equal('ready', authority.fetch('state'))
      creation_identity = journal.fetch('tmux_identity')

      out = StringIO.new
      replay = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        tmux: ManagedTmux.new(
          slug,
          workspace:,
          socket_path: '/run/test/tmux.sock',
          codex_thread_id: 'thread-crash',
          codex_socket_path: '/run/test/codex.sock',
          codex_client_version: '0.152.1',
          id: '$8', identity_token: creation_identity
        ),
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      replay.start(
        slug, as_is: true, new: false, attach: false, run_codex: true,
        goal_file: goal, json: true, exclusive: true
      )

      assert_equal('thread-crash', JSON.parse(out.string).fetch('threadId'))
      repaired = JSON.parse(File.read(authority_path))
      assert_equal('ready', repaired.fetch('state'))
    end
  end

  def test_exclusive_browser_start_recovers_a_lost_thread_result_and_rejects_goal_changes
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      changed_goal = File.join(workspace, 'changed-goal.txt')
      thread_marker = File.join(workspace, 'thread-created')
      invocation_log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(goal, "Implement the original request.\n")
      File.write(changed_goal, "Implement a different request.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{invocation_log.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        case ARGV[1]
        when 'create'
          unless File.exist?(#{thread_marker.dump})
            File.write(#{thread_marker.dump}, "thread-recovered\n")
            warn 'simulated loss after App Server committed thread/start'
            exit 1
          end
          puts JSON.generate(threadId: File.read(#{thread_marker.dump}).strip)
        end
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        codex_socket: '/run/test/codex.sock',
        portal_command: [RbConfig.ruby, portal]
      )

      assert_raises(DevSession::CommandError) do
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
      journal = runner.send(:creation_journal_file, slug)
      assert(File.file?(journal))
      partial = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('creating', partial.dig('creation', 'state'))
      assert_nil(partial.dig('codex', 'thread_id'))

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: changed_goal,
          json: true,
          exclusive: true
        )
      end
      assert_includes(error.message, 'does not match the recorded goal')

      creation_identity = JSON.parse(File.read(journal)).fetch('tmux_identity')
      retry_runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:, identity_token: creation_identity),
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      retry_runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )

      complete = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-recovered', complete.dig('codex', 'thread_id'))
      assert_equal('ready', complete.dig('creation', 'state'))
      assert_equal('ready', JSON.parse(File.read(journal)).fetch('state'))
      creation_commands = File.readlines(invocation_log, chomp: true)
                              .select { |line| line.include?('thread create') }
      assert_equal(2, creation_commands.length)
      creation_commands.each do |command|
        assert_includes(command, '--recover-creating')
        assert_includes(command, "--cwd #{File.join(workspace, 'work', slug)}")
        assert_includes(command, "--workspace #{workspace}")
        assert_includes(command, "--session-slug #{slug}")
        assert_includes(command, "--worktrees-dir #{File.join(workspace, 'worktrees', slug)}")
        assert_includes(
          command,
          "--portal-base-url #{DevSession::DEFAULT_PORTAL_BASE_URL}"
        )
        assert_includes(
          command,
          "--portal-url #{DevSession::DEFAULT_PORTAL_BASE_URL}/#{slug}/"
        )
      end
      assert_equal('thread-recovered', File.read(thread_marker).strip)
    end
  end

  def test_exclusive_browser_start_journals_before_tracking_files
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume after an early crash.\n")
      crashing_runner_class = Class.new(DevSession::Runner) do
        def ensure_tracking_files(slug, **)
          super
          raise DevSession::Error, 'simulated crash after tracking creation'
        end
      end
      crashing_runner = crashing_runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        codex_socket: '/run/test/codex.sock',
        portal_command: [RbConfig.ruby, '-e', "require 'json'; puts JSON.generate(threadId: 'thread-early')"]
      )

      assert_raises(DevSession::Error) do
        crashing_runner.start(
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
      journal = crashing_runner.send(:creation_journal_file, slug)
      assert(File.file?(journal))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))

      creation_identity = JSON.parse(File.read(journal)).fetch('tmux_identity')
      runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:, identity_token: creation_identity),
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, '-e', "require 'json'; puts JSON.generate(threadId: 'thread-early')"]
      )
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
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('ready', manifest.dig('creation', 'state'))
      assert_equal('thread-early', manifest.dig('codex', 'thread_id'))
      assert_equal('ready', JSON.parse(File.read(journal)).fetch('state'))
    end
  end

  def test_exclusive_browser_retry_refuses_partial_tracking_content
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume after an early crash.\n")
      crashing_runner_class = Class.new(DevSession::Runner) do
        def ensure_tracking_files(_slug, **)
          raise DevSession::Error, 'simulated crash before tracking creation'
        end
      end
      crashing_runner = crashing_runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      assert_raises(DevSession::Error) do
        crashing_runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          goal_file: goal,
          json: true,
          exclusive: true
        )
      end

      runner = runner_for(workspace)
      tracking = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(tracking)
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', slug))
      File.write(File.join(tracking, 'plan.md'), "# #{slug}\n\n## Goal\n")
      File.write(File.join(tracking, 'state.md'), runner.send(:state_skeleton, slug))

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          goal_file: goal,
          json: true,
          exclusive: true
        )
      end
      assert_match(/incomplete and cannot be reconciled/, error.message)
    end
  end

end
