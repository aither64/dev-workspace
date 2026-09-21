# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_portal_fork_rechecks_current_receipt_after_source_validation
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination = '2026-06-06-queued-fork'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, manifest)
      options = portal_creation_expectation(workspace, source_slug, destination: destination)
      current_path = File.join(File.dirname(options.fetch(:creation_evidence)), "#{destination}.json")
      runner_class = Class.new(DevSession::Runner) do
        define_method(:resolve_portal_fork_settings) do |*_args, **_kwargs|
          File.unlink(current_path)
          ['model-1', 'high']
        end
        define_method(:create_portal_fork) { |*_args, **_kwargs| raise 'retired fork reached thread creation' }
      end
      runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                err: StringIO.new, today: TODAY, env: {})
      error = assert_raises(DevSession::Error) do
        runner.fork(source_slug, destination, as_is: true, json: true,
                    model: 'model-1', effort: 'high', **options)
      end
      assert_includes(error.message, 'no longer current')
      refute(File.exist?(runner.send(:fork_journal_file, destination)))
      refute(File.exist?(File.join(workspace, 'work', destination)))
    end
  end

  def test_fork_creates_a_conversation_only_session
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup_runner = runner_for(workspace)
      setup_runner.ensure_tracking_files(source_slug)
      source_manifest = setup_runner.send(:ensure_portal_manifest, source_slug)
      source_manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup_runner.send(:write_portal_manifest, source_slug, source_manifest)

      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{log.dump}, 'a') { |file| file.puts ARGV.join(' ') }
        case ARGV[0, 2]
        when ['thread', 'resolve-fork-settings']
          puts JSON.generate(model: 'gpt-test', reasoningEffort: 'xhigh')
        when ['thread', 'fork']
          puts JSON.generate(threadId: 'thread-fork')
        end
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$fork', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_args, **kwargs|
          session.identity_token = kwargs.fetch(:identity_token)
          session
        end
        define_method(:sync_slug) { |*_args, **_kwargs| session }
      end
      out = StringIO.new
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, portal_command: [RbConfig.ruby, portal],
        out:, err: StringIO.new, today: TODAY, env: {}
      )

      runner.fork(
        source_slug, 'alternative', as_is: false, json: true,
        model: 'gpt-test', effort: 'xhigh'
      )

      result = JSON.parse(out.string)
      assert_equal(destination_slug, result.fetch('slug'))
      assert_equal(source_slug, result.fetch('forkedFrom'))
      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      assert_equal(source_slug, manifest.fetch('forked_from'))
      assert_equal('thread-fork', manifest.dig('codex', 'thread_id'))
      assert_empty(manifest.fetch('repositories'))
      assert_empty(manifest.fetch('artifacts'))
      assert_empty(Dir.children(File.join(workspace, 'worktrees', destination_slug)))
      commands = File.readlines(log, chomp: true)
      preflight = commands.find { |line| line.start_with?('thread resolve-fork-settings ') }
      assert_includes(preflight, '--thread-id thread-source')
      command = commands.find { |line| line.start_with?('thread fork ') }
      assert_includes(command, '--thread-id thread-source')
      assert_includes(command, '--model gpt-test')
      assert_includes(command, '--effort xhigh')
    end
  end

  def test_fork_refuses_an_existing_destination
    with_workspace do |workspace|
      runner = runner_for(workspace)
      runner.ensure_tracking_files('2026-06-05-source')
      manifest = runner.send(:ensure_portal_manifest, '2026-06-05-source')
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      runner.send(:write_portal_manifest, '2026-06-05-source', manifest)
      runner.ensure_tracking_files('2026-06-06-taken')

      error = assert_raises(DevSession::Error) do
        runner.fork('2026-06-05-source', 'taken', as_is: false, json: true)
      end
      assert_includes(error.message, 'already exists')
    end
  end

  def test_fork_rejects_a_source_conversation_from_another_runtime_without_mutation
    with_workspace do |workspace|
      source_slug = '2026-06-05-old-runtime'
      destination_slug = '2026-06-06-fork'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = {
        'thread_id' => 'thread-old',
        'socket_path' => '/run/old/app-server.sock',
        'client_version' => '0.151.0'
      }
      setup.send(:write_portal_manifest, source_slug, manifest)
      called = File.join(workspace, 'portal-called')
      portal = [RbConfig.ruby, '-e', "exit 0 if ARGV[0, 2] == ['agent-teams', 'require-unmanaged']; File.write(#{called.dump}, 'called'); exit 1"]
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
        runner.fork(source_slug, 'fork', as_is: false, json: true)
      end

      assert_includes(error.message, 'belongs to another runtime')
      refute(File.exist?(called))
      refute(File.exist?(File.join(workspace, 'work', destination_slug)))
      refute(File.exist?(File.join(workspace, 'worktrees', destination_slug)))
    end
  end

  def test_fork_resumes_after_thread_creation_and_tmux_failure
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-retry'
      setup_runner = runner_for(workspace)
      setup_runner.ensure_tracking_files(source_slug)
      manifest = setup_runner.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup_runner.send(:write_portal_manifest, source_slug, manifest)
      calls = File.join(workspace, 'fork-calls')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        if ARGV[0, 2] == ['thread', 'resolve-fork-settings']
          puts JSON.generate(model: 'source-model', reasoningEffort: 'medium')
        elsif ARGV[0, 2] == ['thread', 'fork']
          File.open(#{calls.dump}, 'a') { |file| file.puts 'fork' }
          puts JSON.generate(threadId: 'thread-fork')
        end
      RUBY
      failing_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_args, **_kwargs|
          raise DevSession::Error, 'tmux failed'
        end
      end
      failing = failing_class.new(
        workspace:, tmux: NullTmux.new, portal_command: [RbConfig.ruby, portal],
        out: StringIO.new, err: StringIO.new, today: TODAY, env: {}
      )
      assert_raises(DevSession::Error) do
        failing.fork(source_slug, 'retry', as_is: false, json: true)
      end

      session = DevSession::Tmux::Session.new(
        id: '$fork', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      retry_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_args, **kwargs|
          session.identity_token = kwargs.fetch(:identity_token)
          session
        end
        define_method(:sync_slug) { |*_args, **_kwargs| session }
      end
      retry_runner = retry_class.new(
        workspace:, tmux: NullTmux.new, portal_command: [RbConfig.ruby, portal],
        out: StringIO.new, err: StringIO.new, today: TODAY, env: {}
      )
      retry_runner.fork(source_slug, 'retry', as_is: false, json: true)
      assert_equal(["fork\n"], File.readlines(calls))
    end
  end

  def test_fork_resumes_after_manifest_crash_on_the_next_day_without_source_tracking
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-retry'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      crashing_class = Class.new(DevSession::Runner) do
        define_method(:create_portal_fork) do |*_arguments, **_keywords|
          raise DevSession::Error, 'simulated crash before forked thread creation'
        end
      end
      crashing = crashing_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      assert_raises(DevSession::Error) do
        crashing.fork(
          source_slug,
          'retry',
          as_is: false,
          json: true,
          model: 'gpt-test',
          effort: 'xhigh'
        )
      end
      journal_path = setup.send(:fork_journal_file, destination_slug)
      assert(File.file?(journal_path))
      journal = JSON.parse(File.read(journal_path))
      assert_equal('thread-source', journal.fetch('source_thread_id'))
      assert_equal('gpt-test', journal.fetch('model'))
      assert_equal('xhigh', journal.fetch('effort'))
      partial = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      assert_equal(source_slug, partial.fetch('forked_from'))
      assert_nil(partial.dig('codex', 'thread_id'))
      FileUtils.rm_r(File.join(workspace, 'work', source_slug))

      session = DevSession::Tmux::Session.new(
        id: '$12', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      forked_from_thread = nil
      retry_class = Class.new(DevSession::Runner) do
        define_method(:create_portal_fork) do |_slug, source_thread_id, **_keywords|
          forked_from_thread = source_thread_id
          'thread-fork'
        end
        define_method(:name_portal_thread) { |*_arguments| nil }
        define_method(:create_tmux_session) do |*_arguments, **keywords|
          session.identity_token = keywords.fetch(:identity_token)
          session
        end
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
      end
      out = StringIO.new
      retry_runner = retry_class.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY.next_day,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::Error) do
        retry_runner.fork(
          'source',
          'retry',
          as_is: false,
          json: true,
          model: 'gpt-other',
          effort: 'xhigh'
        )
      end
      assert_includes(error.message, 'options do not match')
      assert(File.file?(journal_path))
      retry_runner.fork(
        'source',
        'retry',
        as_is: false,
        json: true,
        model: 'gpt-test',
        effort: 'xhigh'
      )

      result = JSON.parse(out.string)
      assert_equal(destination_slug, result.fetch('slug'))
      assert_equal(source_slug, result.fetch('forkedFrom'))
      assert_equal('thread-source', forked_from_thread)
      refute(File.exist?(journal_path))
      refute(File.exist?(File.join(workspace, 'work', '2026-06-07-retry')))
    end
  end

  def test_fork_recovers_a_journal_owned_partial_tracking_skeleton
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-retry'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      crashing_class = Class.new(DevSession::Runner) do
        def ensure_fork_tracking_file!(path, expected, label)
          super
          if label == 'plan.md'
            raise DevSession::Error, 'simulated crash between tracking files'
          end
        end
      end
      crashing = crashing_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      assert_raises(DevSession::Error) do
        crashing.fork(source_slug, 'retry', as_is: false, json: true)
      end
      assert(File.file?(setup.send(:fork_journal_file, destination_slug)))
      destination_work = File.join(workspace, 'work', destination_slug)
      plan_path = File.join(destination_work, 'plan.md')
      state_path = File.join(destination_work, 'state.md')
      assert(File.file?(plan_path))
      refute(File.exist?(state_path))
      File.link(plan_path, File.join(destination_work, '.plan.md.123.tmp'))
      previous_umask = File.umask(0o077)
      begin
        File.write(File.join(destination_work, '.state.md.123.tmp'), "partial\n")
      ensure
        File.umask(previous_umask)
      end
      assert_equal(
        0o600,
        File.stat(File.join(destination_work, '.state.md.123.tmp')).mode & 0o777
      )
      FileUtils.rm_r(File.join(workspace, 'work', source_slug))
      retry_class = Class.new(DevSession::Runner) do
        define_method(:create_portal_fork) do |*_arguments, **_keywords|
          raise DevSession::Error, 'reached thread creation'
        end
      end
      retry_runner = retry_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY.next_day,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::Error) do
        retry_runner.fork('source', 'retry', as_is: false, json: true)
      end

      assert_equal('reached thread creation', error.message)
      state = File.read(state_path)
      assert_equal(setup.send(:state_skeleton, destination_slug), state)
      refute(File.exist?(File.join(destination_work, '.plan.md.123.tmp')))
      refute(File.exist?(File.join(destination_work, '.state.md.123.tmp')))
      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      assert_equal(source_slug, manifest.fetch('forked_from'))
    end
  end

  def test_fork_recovers_a_journal_owned_default_manifest
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-retry'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      crashing_class = Class.new(DevSession::Runner) do
        def ensure_fork_portal_manifest(slug, _source_slug)
          write_portal_manifest(slug, new_portal_manifest(slug))
          raise DevSession::Error, 'simulated crash before fork provenance'
        end
      end
      crashing = crashing_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      assert_raises(DevSession::Error) do
        crashing.fork(source_slug, 'retry', as_is: false, json: true)
      end
      partial = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      refute(partial.key?('forked_from'))
      portal_temporary = File.join(
        workspace, 'work', destination_slug, '.portal.yml.456.tmp'
      )
      File.write(portal_temporary, "partial: true\n")
      FileUtils.rm_r(File.join(workspace, 'work', source_slug))
      retry_class = Class.new(DevSession::Runner) do
        define_method(:create_portal_fork) do |*_arguments, **_keywords|
          raise DevSession::Error, 'reached thread creation'
        end
      end
      retry_runner = retry_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY.next_day,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::Error) do
        retry_runner.fork('source', 'retry', as_is: false, json: true)
      end

      assert_equal('reached thread creation', error.message)
      recovered = YAML.safe_load(
        File.read(File.join(workspace, 'work', destination_slug, 'portal.yml'))
      )
      assert_equal(source_slug, recovered.fetch('forked_from'))
      refute(File.exist?(portal_temporary))
    end
  end

  def test_fork_revalidates_options_when_a_journal_appears_under_lock
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      load_count = 0
      racing_class = Class.new(DevSession::Runner) do
        define_method(:load_fork_journal) do |slug, expected_source = nil|
          load_count += 1
          if load_count == 2
            prepare_fork_journal!(
              destination_slug,
              source_slug,
              'thread-source',
              model: 'gpt-first',
              effort: 'xhigh'
            )
          end
          super(slug, expected_source)
        end
      end
      runner = racing_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::Error) do
        runner.fork(
          source_slug,
          'alternative',
          as_is: false,
          json: true,
          model: 'gpt-second',
          effort: 'xhigh'
        )
      end

      assert_includes(error.message, 'options do not match')
      journal = JSON.parse(File.read(setup.send(:fork_journal_file, destination_slug)))
      assert_equal('gpt-first', journal.fetch('model'))
      refute(File.exist?(File.join(workspace, 'work', destination_slug)))
    end
  end

  def test_fork_rejects_archive_history_before_publishing_its_journal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      archive = File.join(workspace, 'archive', destination_slug)
      FileUtils.mkdir_p(archive)
      File.write(File.join(archive, 'plan.md'), "# Archived\n")
      File.write(
        File.join(archive, 'state.md'),
        "---\nlifecycle: complete\n---\n\n# #{destination_slug}\n"
      )
      assert_git_success('git', 'init', '-b', 'master', workspace)
      configure_git_identity(workspace)
      assert_git_success('git', '-C', workspace, 'add', File.join('archive', destination_slug))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'record archived slug')
      FileUtils.rm_r(archive)
      assert_git_success(
        'git', '-C', workspace, 'add', '-A', '--', File.join('archive', destination_slug)
      )
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'remove archive checkout')

      error = assert_raises(DevSession::Error) do
        setup.fork(source_slug, 'alternative', as_is: false, json: true)
      end

      assert_includes(error.message, 'archived slug cannot be reused')
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      refute(File.exist?(File.join(workspace, 'work', destination_slug)))
    end
  end

  def test_fork_rejects_invalid_settings_before_publishing_its_journal
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        warn 'Codex model is unavailable'
        exit 1
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      error = assert_raises(DevSession::CommandError) do
        runner.fork(
          source_slug,
          'alternative',
          as_is: false,
          json: true,
          model: 'missing-model',
          effort: 'xhigh'
        )
      end

      assert_includes(error.message, 'Codex model is unavailable')
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      refute(File.exist?(File.join(workspace, 'work', destination_slug)))
    end
  end

  def test_fork_recovery_rejects_a_renamed_authority_session
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      authority_dir = File.join(workspace, 'authority')
      setup = runner_for(workspace, authority_dir:)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', destination_slug))
      original = DevSession::Tmux::Session.new(
        id: '$11', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, environment_slug: destination_slug,
        socket_path: '/run/test.sock', identity_token: 'a' * 64
      )
      setup.send(:write_session_authority, destination_slug, original, state: 'ready')
      tmux = RenamedManagedTmux.new(
        destination_slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)

      error = assert_raises(DevSession::Error) do
        runner.fork(source_slug, 'alternative', as_is: false, json: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(File.file?(File.join(authority_dir, "#{destination_slug}.json")))
    end
  end

  def test_fork_recovery_replaces_only_the_journal_bound_partial_session
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      authority_dir = File.join(workspace, 'authority')
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      journal = setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      tmux = PartialManagedTmux.new(
        destination_slug, workspace:, identity_token: journal.fetch('tmux_identity')
      )
      created = DevSession::Tmux::Session.new(
        id: '$12', name: destination_slug, mark: '1', slug: destination_slug,
        workspace:, environment_slug: destination_slug,
        socket_path: '/run/test.sock', codex_thread_id: 'thread-fork',
        codex_socket_path: '/run/test/codex.sock', codex_client_version: '0.152.1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |*_arguments, **keywords|
          created.identity_token = keywords.fetch(:identity_token)
          created
        end
        define_method(:sync_slug) { |*_arguments, **_keywords| created }
      end
      runner = runner_class.new(
        workspace:, tmux:, authority_dir:, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      runner.fork('source', 'alternative', as_is: false, json: true)

      assert(tmux.killed)
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      authority = JSON.parse(
        File.read(File.join(authority_dir, "#{destination_slug}.json"))
      )
      assert_equal(journal.fetch('tmux_identity'), authority.fetch('tmux_identity'))
    end
  end

  def test_fork_recovery_refuses_a_partial_session_with_another_identity
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      tmux = PartialManagedTmux.new(
        destination_slug, workspace:, identity_token: 'b' * 64
      )
      runner = runner_for(workspace, tmux:)

      error = assert_raises(DevSession::Error) do
        runner.fork(source_slug, 'alternative', as_is: false, json: true)
      end

      assert_includes(error.message, 'does not match fork identity')
      refute(tmux.killed)
      assert(File.exist?(setup.send(:fork_journal_file, destination_slug)))
    end
  end

  def test_completed_fork_recovery_does_not_require_source_tracking
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      source = setup.send(:ensure_portal_manifest, source_slug)
      source['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, source)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      journal = setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      tmux = ManagedTmux.new(
        destination_slug,
        workspace:,
        identity_token: journal.fetch('tmux_identity'),
        codex_thread_id: 'thread-fork'
      )
      FileUtils.rm_r(File.join(workspace, 'work', source_slug))
      out = StringIO.new
      runner = runner_for(workspace, tmux:, out:)

      runner.fork('source', 'alternative', as_is: false, json: true)

      refute(tmux.killed)
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      assert_equal('thread-fork', JSON.parse(out.string).fetch('threadId'))
    end
  end

  def test_completed_fork_recovery_uses_the_journal_source_when_suffix_is_reused
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      newer_source_slug = '2026-06-07-source'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      journal = setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      setup.ensure_tracking_files(newer_source_slug)
      tmux = ManagedTmux.new(
        destination_slug,
        workspace:,
        identity_token: journal.fetch('tmux_identity'),
        codex_thread_id: 'thread-fork'
      )
      out = StringIO.new
      runner = runner_for(workspace, tmux:, out:)

      runner.fork('source', 'alternative', as_is: false, json: true)

      result = JSON.parse(out.string)
      assert_equal(source_slug, result.fetch('forkedFrom'))
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
    end
  end

  def test_completed_fork_recovery_reuses_the_journal_destination_after_midnight
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination_slug = '2026-06-06-alternative'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(destination_slug)
      destination = setup.send(:ensure_portal_manifest, destination_slug)
      destination['forked_from'] = source_slug
      destination['codex'] = { 'thread_id' => 'thread-fork' }
      setup.send(:write_portal_manifest, destination_slug, destination)
      journal = setup.send(
        :prepare_fork_journal!, destination_slug, source_slug, 'thread-source',
        model: nil, effort: nil
      )
      tmux = ManagedTmux.new(
        destination_slug,
        workspace:,
        identity_token: journal.fetch('tmux_identity'),
        codex_thread_id: 'thread-fork'
      )
      out = StringIO.new
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        out:,
        err: StringIO.new,
        today: TODAY.next_day,
        env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      runner.fork(source_slug, 'alternative', as_is: false, json: true)

      result = JSON.parse(out.string)
      assert_equal(destination_slug, result.fetch('slug'))
      refute(File.exist?(setup.send(:fork_journal_file, destination_slug)))
      refute(File.exist?(File.join(workspace, 'work', '2026-06-07-alternative')))
    end
  end

end
