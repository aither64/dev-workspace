# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_revive_commits_tracking_before_starting_the_runtime
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      starts = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:start) do |input, **options|
          starts << [input, options]
        end
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      runner.revive(slug, as_is: true)

      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(File.join(workspace, 'archive', slug)))
      assert_match(
        /\A---\nlifecycle: active\n---/,
        File.read(File.join(workspace, 'work', slug, 'state.md'))
      )
      assert_equal(1, starts.length)
      assert_equal(true, starts.fetch(0).fetch(1).fetch(:allow_empty_thread))
      assert_equal(false, starts.fetch(0).fetch(1).fetch(:exclusive))
      assert_equal(
        "workspace: revive #{slug}",
        git_capture_success('git', '-C', workspace, 'log', '-1', '--format=%s').strip
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_revive_retains_its_journal_until_the_automatic_archive_reset_succeeds
    with_workspace do |workspace|
      slug = '2026-06-06-revive-sidecar'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runner = runner_for(workspace)
      runner.define_singleton_method(:start) { |*, **| nil }
      store = runner.auto_archive_store
      previous = { 'slug' => slug, 'identity' => 'retained-thread', 'hold' => true }
      store.write("session-#{slug}", previous)
      path = File.join(store.root, "session-#{slug}.json")
      File.write(path, 'invalid JSON')
      assert_raises(WorkspaceAutoArchive::Error) { runner.revive(slug, as_is: true) }
      journal = runner.send(:lifecycle_journal_file, slug, 'revive')
      assert_equal('runtime_started', JSON.parse(File.read(journal))['phase'])
      store.write("session-#{slug}", previous)
      runner.revive(slug, as_is: true)
      refute(File.exist?(journal))
      assert(store.session(slug)['hold'])
      assert(store.session(slug)['reset_at'])
    end
  end

  def test_revive_does_not_create_a_blank_thread_for_a_current_manifest
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-current-no-thread'
      base = runner_for(workspace)
      base.ensure_tracking_files(slug)
      base.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(base, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)
      starts = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:start) { |input, **options| starts << [input, options] }
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      runner.revive(slug, as_is: true)

      assert_empty(starts)
      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'work', slug, 'portal.yml'))
      )
      assert_nil(manifest.dig('codex', 'thread_id'))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_revive_resumes_an_interrupted_archive_to_work_transition
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-interrupted'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runner_class = Class.new(DevSession::Runner) do
        define_method(:start) { |_input, **_options| nil }
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )
      runner.send(:prepare_revive_journal!, slug, 'complete')
      File.rename(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )

      runner.revive(slug, as_is: true)

      assert_match(
        /\A---\nlifecycle: active\n---/,
        File.read(File.join(workspace, 'work', slug, 'state.md'))
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
      assert_equal(
        "workspace: revive #{slug}",
        git_capture_success('git', '-C', workspace, 'log', '-1', '--format=%s').strip
      )
    end
  end

  def test_revive_retry_uses_durable_abandoned_confirmation_after_tracking_move
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-abandoned-retry'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'abandoned')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)

      journal = runner.send(
        :prepare_revive_journal!, slug, 'abandoned', abandoned_confirmed: true
      )
      runner.send(:finish_revive_tracking!, slug, journal)

      assert_equal(
        { lifecycle: 'abandoned', pending: true },
        runner.revive_confirmation(slug, as_is: true)
      )
      runner.revive(slug, as_is: true, allow_abandoned: false)

      assert_match(
        /\A---\nlifecycle: active\n---/,
        File.read(File.join(workspace, 'work', slug, 'state.md'))
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_revive_rejects_tracking_edits_after_the_restore_move
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-tree-change'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runner = runner_for(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.write(File.join(workspace, 'work', slug, 'unexpected.txt'), "changed\n")

      error = assert_raises(DevSession::Error) do
        runner.revive(slug, as_is: true)
      end

      assert_includes(error.message, 'revived tracking changed during recovery')
      assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_revive_recovery_rejects_dirty_tracking_after_commit
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-dirty-commit'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runner = runner_for(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      runner.send(
        :commit_tracking_transition!, slug, direction: 'revive', mode: 'complete'
      )
      runner.send(:advance_revive!, slug, journal, 'tracking_committed')
      File.open(File.join(workspace, 'work', slug, 'plan.md'), 'a') do |file|
        file.write("\nUncommitted change.\n")
      end

      error = assert_raises(DevSession::Error) do
        runner.revive(slug, as_is: true)
      end

      assert_includes(error.message, 'committed revive tracking differs')
      assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_finalize_rejects_an_unmerged_legacy_worktree_without_a_manifest
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-legacy'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      File.unlink(File.join(workspace, 'work', slug, 'portal.yml'))
      configure_git_identity(path)
      File.write(File.join(path, 'feature.txt'), "unmerged legacy feature\n")
      assert_git_success('git', '-C', path, 'add', 'feature.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'unmerged legacy feature')
      repository = File.join(workspace, 'repos', 'sample.git')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'push', 'origin',
        "refs/heads/#{slug}:refs/heads/#{slug}"
      )
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
      end

      assert_includes(error.message, 'feature head is not merged')
      assert_includes(error.message, "#{slug} -> origin/master")
    end
  end

  def test_archive_refuses_an_active_codex_turn_without_quiescing
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-active-turn'
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
        portal_command: [RbConfig.ruby, '-e', "warn 'thread is active'; exit 1"],
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
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(DevSession::CommandError) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'thread is active')
      refute(tmux.quiesced)
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_finalize_accepts_an_unmerged_abandoned_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'discarded.txt'), "discarded feature\n")
      assert_git_success('git', '-C', path, 'add', 'discarded.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'discarded feature')
      commit_tracking(workspace, slug, lifecycle: 'abandoned')

      finalize_core(runner, slug, as_is: true)

      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_revive_current_archive_clears_terminal_metadata
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      merge_registered_branches(workspace, slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)

      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))

      state = File.read(File.join(workspace, 'work', slug, 'state.md'))
      assert_match(/\A---\nlifecycle: active\n---\n/, state)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      refute(manifest.key?('finalized_at'))
      refute(manifest.dig('repositories', 0).key?('final_head_sha'))
      assert_equal('revived', manifest.dig('creation', 'tracking_origin'))
      refute(File.exist?(File.join(workspace, 'archive', slug)))

      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: nil, fetch: false
      )
      assert(File.directory?(File.join(workspace, 'worktrees', slug, 'sample')))
    end
  end

  def test_revive_journal_repeats_parent_sync_after_an_interrupted_move
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-interrupted'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runtime = File.join(workspace, 'runtime-authority')
      runner = runner_for(workspace, authority_dir: runtime)
      runner.send(:prepare_revive_journal!, slug, 'complete')
      File.rename(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )

      FileUtils.rm_rf(runtime)
      synced = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:fsync_directory) do |path|
          synced << path
          super(path)
        end
      end
      retry_runner = runner_class.new(
        workspace:,
        authority_dir: runtime,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )
      journal = retry_runner.send(:load_revive_journal, slug)
      retry_runner.send(:finish_revive_tracking!, slug, journal)

      state = File.read(File.join(workspace, 'work', slug, 'state.md'))
      assert_match(/\A---\nlifecycle: active\n---\n/, state)
      refute(File.exist?(File.join(workspace, 'archive', slug)))
      assert(File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.revive.json")))
      assert_includes(synced, File.join(workspace, 'archive'))
      assert_includes(synced, File.join(workspace, 'work'))
    end
  end

  def test_revive_journal_rejects_a_changed_plan_after_an_interrupted_move
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-interrupted-plan'
      archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      runner = runner_for(workspace)
      runner.send(:prepare_revive_journal!, slug, 'complete')
      File.rename(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )
      File.write(File.join(workspace, 'work', slug, 'plan.md'), "truncated\n")

      error = assert_raises(DevSession::Error) do
        journal = runner.send(:load_revive_journal, slug)
        runner.send(:finish_revive_tracking!, slug, journal)
      end
      assert_match(/revived plan changed after revive was prepared/, error.message)
      assert(File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.revive.json")))
    end
  end

  def test_revive_with_an_existing_thread_records_exact_recovery_provenance
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-existing-thread'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex']['thread_id'] = 'thread-existing'
      manifest['creation']['initial_goal_sent'] = true
      runner.send(:write_portal_manifest, slug, manifest)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)

      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)

      revived = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-existing', revived.dig('codex', 'thread_id'))
      assert_equal('revived', revived.dig('creation', 'tracking_origin'))
      assert_match(/\A[0-9a-f]{64}\z/, revived.dig('creation', 'tracking_plan_sha256'))
      assert_match(/\A[0-9a-f]{64}\z/, revived.dig('creation', 'tracking_state_sha256'))
    end
  end

  def test_start_revived_existing_thread_uses_exact_archived_recovery_without_a_goal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-existing-thread-recovery'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex'] = {
        'thread_id' => 'thread-existing',
        'socket_path' => '/run/current/app-server.sock',
        'client_version' => '0.152.1'
      }
      manifest['creation']['initial_goal_sent'] = true
      runner.send(:write_portal_manifest, slug, manifest)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))

      calls = File.join(workspace, 'portal-calls')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{calls.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        puts JSON.generate(threadId: 'thread-existing') if ARGV[0, 2] == ['thread', 'create']
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$revived', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/current/tmux.sock', codex_thread_id: 'thread-existing',
        codex_socket_path: '/run/current/app-server.sock',
        codex_client_version: '0.152.1', codex_pane_id: '%1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:sync_slug) { |*_args, **_options| session }
        define_method(:revalidate_session!) { |_selected| session }
        define_method(:reconcile_native_client!) { |_slug, selected, **_options| selected }
        define_method(:verify_codex_client!) {}
      end
      out = StringIO.new
      starter = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.152.1',
        portal_command: [RbConfig.ruby, portal],
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        json: true,
        exclusive: false
      )

      assert_equal('thread-existing', JSON.parse(out.string).fetch('threadId'))
      recorded = File.read(calls)
      assert_includes(recorded, 'thread create')
      assert_includes(recorded, '--thread-id thread-existing')
      assert_includes(recorded, '--recover-archived')
      refute_includes(recorded, 'ensure-initial')
      updated = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-existing', updated.dig('codex', 'thread_id'))
      refute(updated.dig('creation').key?('tracking_origin'))
      refute(updated.dig('creation').key?('tracking_plan_sha256'))
      refute(updated.dig('creation').key?('tracking_state_sha256'))
    end
  end

  def test_revived_thread_recovery_defers_version_refresh_until_authority_is_ready
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-existing-thread-retry'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex'] = {
        'thread_id' => 'thread-existing',
        'socket_path' => '/run/current/app-server.sock',
        'client_version' => '0.152.1'
      }
      manifest['creation']['initial_goal_sent'] = true
      runner.send(:write_portal_manifest, slug, manifest)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))

      calls = File.join(workspace, 'portal-calls')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        File.open(#{calls.dump}, 'a') { |file| file.puts(ARGV.join(' ')) }
        puts JSON.generate(threadId: 'thread-existing') if ARGV[0, 2] == ['thread', 'create']
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$revived', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/current/tmux.sock', codex_thread_id: 'thread-existing',
        codex_socket_path: '/run/current/app-server.sock',
        codex_client_version: '0.153.4', codex_pane_id: '%1'
      )
      authority_attempts = 0
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:write_session_authority) do |*_args, **_options|
          authority_attempts += 1
          if authority_attempts == 1
            raise DevSession::Error, 'simulated authority publication failure'
          end
        end
        define_method(:sync_slug) { |*_args, **_options| session }
        define_method(:revalidate_session!) { |_selected| session }
        define_method(:reconcile_native_client!) { |_slug, selected, **_options| selected }
        define_method(:verify_codex_client!) {}
      end
      out = StringIO.new
      starter = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.153.4',
        portal_command: [RbConfig.ruby, portal],
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      error = assert_raises(DevSession::Error) do
        starter.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          json: true,
          exclusive: false
        )
      end
      assert_match(/simulated authority publication failure/, error.message)
      interrupted = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('0.152.1', interrupted.dig('codex', 'client_version'))
      assert_equal('revived', interrupted.dig('creation', 'tracking_origin'))

      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        json: true,
        exclusive: false
      )

      assert_equal('thread-existing', JSON.parse(out.string).fetch('threadId'))
      assert_equal(2, authority_attempts)
      assert_equal(2, File.readlines(calls).count { |line| line.start_with?('thread create ') })
      recovered = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('0.153.4', recovered.dig('codex', 'client_version'))
      refute(recovered.dig('creation').key?('tracking_origin'))
      refute(recovered.dig('creation').key?('tracking_plan_sha256'))
      refute(recovered.dig('creation').key?('tracking_state_sha256'))
    end
  end

  def test_revive_legacy_archive_reuses_retained_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-demo'
      assert_git_success('git', "--git-dir=#{repository}", 'branch', slug, 'master')
      master = git_capture_success('git', "--git-dir=#{repository}", 'rev-parse', 'master').strip
      assert_git_success(
        'git', "--git-dir=#{repository}", 'update-ref',
        'refs/remotes/origin/master', master
      )
      assert_git_success(
        'git', "--git-dir=#{repository}", 'symbolic-ref',
        'refs/remotes/origin/HEAD', 'refs/remotes/origin/master'
      )
      tracking = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(tracking)
      runner = runner_for(workspace)
      File.write(
        File.join(tracking, 'plan.md'),
        <<~PLAN
          # Retained legacy plan

          This substantive plan predates the current tracking template.
        PLAN
      )
      File.write(
        File.join(tracking, 'state.md'),
        <<~STATE
          ---
          lifecycle: active
          ---

          # Retained legacy state

          This substantive state predates the current tracking template.
        STATE
      )
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)

      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))
      revived = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('revived', revived.dig('creation', 'tracking_origin'))
      assert_equal(
        Digest::SHA256.hexdigest(File.read(File.join(workspace, 'work', slug, 'plan.md'))),
        revived.dig('creation', 'tracking_plan_sha256')
      )
      assert_equal(
        Digest::SHA256.hexdigest(File.read(File.join(workspace, 'work', slug, 'state.md'))),
        revived.dig('creation', 'tracking_state_sha256')
      )
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: nil, fetch: false
      )

      path = File.join(workspace, 'worktrees', slug, 'sample')
      assert_equal(slug, git_capture_success('git', '-C', path, 'branch', '--show-current').strip)
      assert_equal(master, git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal(master, manifest.dig('repositories', 0, 'initial_base_sha'))

      plan_before = File.read(File.join(workspace, 'work', slug, 'plan.md'))
      state_before = File.read(File.join(workspace, 'work', slug, 'state.md'))
      goal = File.join(workspace, 'goal.txt')
      portal = File.join(workspace, 'portal.rb')
      File.write(goal, "Continue the retained initiative.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        puts JSON.generate(threadId: 'thread-fresh') if ARGV[0, 2] == ['thread', 'create']
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$fresh', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/current/tmux.sock', codex_thread_id: 'thread-fresh',
        codex_socket_path: '/run/current/app-server.sock',
        codex_client_version: '0.152.1', codex_pane_id: '%1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:sync_slug) { |*_args, **_options| session }
        define_method(:revalidate_session!) { |_selected| session }
        define_method(:reconcile_native_client!) { |_slug, selected, **_options| selected }
        define_method(:verify_codex_client!) {}
      end
      out = StringIO.new
      starter = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/current/app-server.sock',
        codex_version: '0.152.1',
        portal_command: [RbConfig.ruby, portal],
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )

      assert_equal('thread-fresh', JSON.parse(out.string).fetch('threadId'))
      assert_equal(plan_before, File.read(File.join(workspace, 'work', slug, 'plan.md')))
      assert_equal(state_before, File.read(File.join(workspace, 'work', slug, 'state.md')))
      updated = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal(master, updated.dig('repositories', 0, 'initial_base_sha'))
      assert_equal('thread-fresh', updated.dig('codex', 'thread_id'))
      assert_equal('ready', updated.dig('creation', 'state'))
      refute(updated.dig('creation').key?('tracking_origin'))
      refute(updated.dig('creation').key?('tracking_plan_sha256'))
      refute(updated.dig('creation').key?('tracking_state_sha256'))

      out.truncate(0)
      out.rewind
      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )
      assert_equal('thread-fresh', JSON.parse(out.string).fetch('threadId'))

      journal_path = starter.send(:creation_journal_file, slug)
      interrupted = JSON.parse(File.read(journal_path)).merge('state' => 'creating')
      interrupted['tmux_identity'] = 'a' * 64
      File.write(journal_path, JSON.generate(interrupted))
      File.write(
        File.join(workspace, 'work', slug, 'plan.md'),
        "#{plan_before}\nFollow-up recorded after the initial turn.\n"
      )
      out.truncate(0)
      out.rewind
      starter.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: nil,
        json: true,
        exclusive: false
      )
      assert_equal('thread-fresh', JSON.parse(out.string).fetch('threadId'))
      completed_journal = JSON.parse(File.read(journal_path))
      assert_equal('ready', completed_journal.fetch('state'))
      refute(completed_journal.key?('tmux_identity'))
    end
  end

end
