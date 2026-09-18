# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_exclusive_replay_publishes_authority_before_consuming_start_journal
    with_workspace do |workspace|
      slug = '2026-06-06-restart'
      goal = File.join(workspace, 'goal.txt')
      authority_dir = File.join(workspace, 'authority')
      File.write(goal, "Replay this completed request.\n")
      setup = runner_for(workspace, authority_dir:)
      creation = setup.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: false,
        model: nil,
        effort: nil
      )
      setup.ensure_tracking_files(slug)
      setup.send(:seed_goal, slug, goal)
      manifest = setup.send(:ensure_portal_manifest, slug, creation_journal: creation)
      manifest['creation']['state'] = 'ready'
      manifest['creation']['initial_goal_sent'] = true
      manifest['creation'].delete('initial_goal_attempted')
      manifest['schema'] = 1
      setup.send(:write_portal_manifest, slug, manifest)
      creation_identity = creation.fetch('tmux_identity')
      setup.send(:mark_creation_journal_ready, slug, creation)
      runtime = setup.send(
        :prepare_start_journal!,
        slug,
        preferred_identity: creation_identity
      )
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/test/tmux.sock',
        identity_token: runtime.fetch('tmux_identity'),
        id: '$12'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)

      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: false,
        goal_file: goal,
        exclusive: true
      )

      authority = JSON.parse(File.read(runner.send(:authority_file, slug)))
      assert_equal(runtime.fetch('tmux_identity'), authority.fetch('tmux_identity'))
      refute(File.exist?(setup.send(:start_journal_file, slug)))
    end
  end

  def test_worktree_mutations_use_the_slug_lock
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      lock_path = File.join(workspace, 'worktrees', '.locks', "#{slug}.lock")
      FileUtils.mkdir_p(File.dirname(lock_path))

      File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
        assert(lock.flock(File::LOCK_EX | File::LOCK_NB))

        add_error = assert_raises(DevSession::Error) do
          runner.worktree_add(
            slug,
            'sample',
            as_is: true,
            name: nil,
            branch: nil,
            base: nil,
            fetch: false
          )
        end
        assert_match(/another dev-session command/, add_error.message)

        remove_error = assert_raises(DevSession::Error) do
          runner.worktree_remove(slug, 'sample', as_is: true, force: false)
        end
        assert_match(/another dev-session command/, remove_error.message)
      end
    end
  end

  def test_stop_does_not_kill_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner_for(workspace).ensure_tracking_files(slug)
      tmux = ReplacedTmux.new(slug, workspace:)

      error = assert_raises(DevSession::Error) do
        runner_for(workspace, tmux:).stop(slug, as_is: true)
      end

      assert_match(/session changed during operation/, error.message)
      refute(tmux.kill_attempted)
    end
  end

  def test_stop_keeps_authority_when_identity_changes_at_the_kill_boundary
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ReplacedDuringConditionalKillTmux.new(
        slug, workspace:, socket_path: '/run/test/tmux.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')

      error = assert_raises(DevSession::Error) do
        runner.stop(slug, as_is: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(tmux.conditional_kill_attempted)
      refute(tmux.killed)
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_stop_upgrades_a_live_legacy_authority_before_retirement
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test/tmux.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      authority_path = File.join(authority_dir, "#{slug}.json")
      refute(JSON.parse(File.read(authority_path)).key?('tmux_identity'))

      runner.stop(slug, as_is: true)

      assert(tmux.killed)
      refute(File.exist?(authority_path))
    end
  end

  def test_stop_refuses_an_active_codex_turn_before_killing_tmux_or_authority
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.152.1',
        id: '$7'
      )
      failing_portal = [RbConfig.ruby, '-e', "warn 'thread is active'; exit 1"]
      runner = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        tmux:,
        portal_command: failing_portal,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-1',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::Error) do
        runner.stop(slug, as_is: true)
      end
      assert_includes(error.message, 'unable to restore terminal Codex client')
      refute(tmux.killed)
      assert(tmux.quiesced)
      assert_empty(tmux.sent_commands)
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))

      idle_runner = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        tmux:,
        portal_command: ['true'],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      idle_runner.stop(slug, as_is: true)
      assert(tmux.killed)
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_idle_check_ignores_a_manifest_without_current_socket_provenance
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal')
      File.write(portal, <<~RUBY)
        File.write(#{log.dump}, ARGV.join(' '))
      RUBY
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new,
        codex_socket: '/run/test/codex.sock', codex_version: '0.152.1',
        portal_command: [RbConfig.ruby, portal], out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = { 'thread_id' => 'thread-legacy' }
      runner.send(:write_portal_manifest, slug, manifest)

      runner.send(:ensure_portal_thread_idle!, slug, nil)

      refute(File.exist?(log))
    end
  end

  def test_idle_check_ignores_a_manifest_from_another_runtime
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal')
      File.write(portal, <<~RUBY)
        File.write(#{log.dump}, ARGV.join(' '))
      RUBY
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new,
        codex_socket: '/run/test/codex.sock', codex_version: '0.152.1',
        portal_command: [RbConfig.ruby, portal], out: StringIO.new,
        err: StringIO.new, today: TODAY, env: {}
      )
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-old',
        'socket_path' => '/run/old/app-server.sock',
        'client_version' => '0.151.0'
      }
      runner.send(:write_portal_manifest, slug, manifest)

      runner.send(:ensure_portal_thread_idle!, slug, nil)

      refute(File.exist?(log))
    end
  end

  def test_stop_quiesces_terminal_before_the_authoritative_idle_check
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      active = File.join(workspace, 'turn-active')
      portal = File.join(workspace, 'portal')
      codex = File.join(workspace, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 0.152.1'\n")
      File.chmod(0o755, codex)
      File.write(portal, <<~RUBY)
        #!/usr/bin/env ruby
        if ARGV.include?('require-idle') && File.exist?(#{active.dump})
          abort 'turn became active while terminal was quiesced'
        end
      RUBY
      File.chmod(0o755, portal)
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        on_kill: -> { File.write(active, "active\n") },
        socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.152.1',
        id: '$7'
      )
      runner = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: codex,
        tmux:,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-1',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::CommandError) do
        runner.stop(slug, as_is: true)
      end
      assert_includes(error.message, 'turn became active')
      refute(tmux.killed)
      refute(tmux.quiesced, 'terminal Codex client was not restored')
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_quiesce_does_not_restore_a_client_that_was_already_stopped
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        codex_thread_id: 'thread-1',
        codex_pane_id: '%1',
        pane_current_command: File.basename(ENV.fetch('SHELL', '/bin/sh'))
      )
      runner = runner_for(workspace, tmux:)

      assert_nil(runner.send(:quiesce_native_client!, slug, tmux.session(slug)))
      refute(tmux.quiesced)
      assert_empty(tmux.sent_commands)
    end
  end

  def test_recover_stale_removes_only_idle_identity_validated_authority
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      authority_dir = File.join(workspace, 'runtime-authority')
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        socket_path: '/run/test/tmux.sock',
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/test/codex.sock',
        codex_client_version: '0.152.1',
        id: '$7'
      )
      runner = DevSession::Runner.new(
        workspace:,
        authority_dir:,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        tmux:,
        portal_command: ['true'],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      assert_raises(DevSession::Error) do
        runner.recover_stale(slug, as_is: true)
      end

      tmux.run('kill-session', '-t', '$7:')
      runner.recover_stale(slug, as_is: true)
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_recover_stale_accepts_an_id_reused_with_a_different_identity
    with_workspace do |workspace|
      slug = '2026-06-06-stale-reused-id'
      authority_dir = File.join(workspace, 'runtime-authority')
      replacement = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test/tmux.sock', id: '$7',
        identity_token: 'b' * 64
      )
      runner = runner_for(workspace, tmux: replacement, authority_dir:)
      runner.ensure_tracking_files(slug)
      original = DevSession::Tmux::Session.new(
        id: '$7', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test/tmux.sock',
        identity_token: 'a' * 64
      )
      runner.send(:write_session_authority, slug, original, state: 'ready')

      runner.recover_stale(slug, as_is: true)

      refute(replacement.killed)
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_remove_does_not_kill_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner_for(workspace).ensure_tracking_files(slug)
      tmux = ReplacedTmux.new(slug, workspace:)

      error = assert_raises(DevSession::Error) do
        runner_for(workspace, tmux:).delete(slug, as_is: true, force: false)
      end

      assert_match(/session changed during operation/, error.message)
      refute(tmux.kill_attempted)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_managed_tmux_session_is_bound_to_its_workspace
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )
      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)
      pane = tmux_capture(socket, 'list-panes', '-t', slug, '-F', '#{pane_id}')
             .lines
             .first
             .strip

      Dir.mktmpdir('other-dev-session-workspace') do |other_workspace|
        other_out = StringIO.new
        other_runner = DevSession::Runner.new(
          workspace: other_workspace,
          tmux_socket: socket,
          out: other_out,
          err: StringIO.new,
          today: TODAY,
          env: { 'TMUX' => 'socket', 'TMUX_PANE' => pane }
        )

        current_error = assert_raises(DevSession::Error) do
          other_runner.current
        end
        assert_match(/not managed by this workspace/, current_error.message)

        other_runner.list
        assert_equal('', other_out.string)
        lookup_error = assert_raises(DevSession::Error) do
          other_runner.lookup_slug('demo', as_is: false)
        end
        assert_match(/no slug found/, lookup_error.message)

        error = assert_raises(DevSession::Error) do
          other_runner.stop(slug, as_is: true)
        end

        assert_match(/not managed by this workspace/, error.message)
      end

      assert(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

end
