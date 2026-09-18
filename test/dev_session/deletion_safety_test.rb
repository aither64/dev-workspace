# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_remove_retries_after_tmux_was_killed
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = KillThenFailOnceTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      authority_dir = File.join(workspace, 'authority')
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      assert_includes(error.message, 'after tmux removal')
      assert(tmux.killed)
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('worktrees_removed', journal.fetch('phase'))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_recovers_a_tracking_move_before_its_phase_update
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      advance = runner.method(:advance_removal!)
      interrupted = false
      runner.define_singleton_method(:advance_removal!) do |current_slug, removal, phase|
        if phase == 'tracking_preserved' && !interrupted
          interrupted = true
          raise DevSession::Error, 'simulated interruption after tracking move'
        end
        advance.call(current_slug, removal, phase)
      end

      assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      recovery = removal_recovery(workspace, slug)
      assert(File.directory?(File.join(recovery, 'work')))
      refute(File.exist?(File.join(workspace, 'work', slug)))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('removed', JSON.parse(File.read(File.join(recovery, 'recovery.json'))).fetch('state'))
    end
  end

  def test_remove_commits_tracked_active_and_archived_sessions
    %w[work archive].each do |tracking_kind|
      with_workspace do |workspace|
        slug = "2026-06-06-#{tracking_kind}"
        runner = runner_for(workspace)
        runner.ensure_tracking_files(slug)
        commit_tracking(workspace, slug, lifecycle: 'active')
        if tracking_kind == 'archive'
          FileUtils.mkdir_p(File.join(workspace, 'archive'))
          File.rename(File.join(workspace, 'work', slug), File.join(workspace, 'archive', slug))
          commit_archive_move(workspace, slug)
        end
        configure_workspace_origin(workspace)
        unrelated = File.join(workspace, 'unrelated.txt')
        File.write(unrelated, "keep staged\n")
        assert_git_success('git', '-C', workspace, 'add', 'unrelated.txt')

        runner.delete(slug, as_is: true, force: false)

        relative = File.join(tracking_kind, slug)
        assert_equal('', git_capture_success('git', '-C', workspace, 'ls-files', '--', relative))
        assert_equal("workspace: delete #{slug}", git_capture_success(
          'git', '-C', workspace, 'log', '-1', '--format=%s'
        ).strip)
        assert_equal('A  unrelated.txt', git_capture_success(
          'git', '-C', workspace, 'status', '--short', '--', 'unrelated.txt'
        ).strip)
      end
    end
  end

  def test_remove_retries_a_failed_tracking_deletion_commit
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      marker = File.join(workspace, '.git', 'remove-hook-retried')
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      File.write(hook, <<~SH)
        #!/bin/sh
        if [ ! -e #{Shellwords.escape(marker)} ]; then
          : > #{Shellwords.escape(marker)}
          exit 1
        fi
      SH
      File.chmod(0o755, hook)

      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('tracking_preserved', journal.fetch('phase'))
      refute(File.exist?(File.join(workspace, 'work', slug)))

      [
        -> { runner.start(slug, as_is: true, new: false, attach: false, run_codex: false) },
        -> { runner.revive(slug, as_is: true) },
        -> {
          runner.worktree_add(
            slug, 'sample', as_is: true, name: nil, branch: nil,
            base: nil, fetch: false
          )
        }
      ].each do |operation|
        error = assert_raises(DevSession::Error, &operation)
      assert_includes(error.message, 'deletion is unfinished')
      end

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('', git_capture_success(
        'git', '-C', workspace, 'ls-files', '--', File.join('work', slug)
      ))
    end
  end

  def test_remove_keeps_its_journal_when_a_successful_hook_recreates_tracking
    with_workspace do |workspace|
      slug = '2026-06-06-hook-recreates-tracking'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      recreated = File.join(workspace, 'work', slug)
      File.write(hook, <<~SH)
        #!/bin/sh
        mkdir -p #{Shellwords.escape(recreated)}
        printf 'recreated by hook\n' > #{Shellwords.escape(File.join(recreated, 'unexpected.txt'))}
      SH
      File.chmod(0o755, hook)

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'tracking reappeared')
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('tracking_preserved', journal.fetch('phase'))
      assert(File.file?(File.join(recreated, 'unexpected.txt')))
      assert_equal('', git_capture_success(
        'git', '-C', workspace, 'ls-files', '--', File.join('work', slug)
      ))

      retry_error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      assert_includes(retry_error.message, 'tracking reappeared')
      retry_journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('tracking_preserved', retry_journal.fetch('phase'))
    end
  end

  def test_remove_rechecks_remote_master_immediately_before_deletion_commit
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      remote = File.join(workspace, '.git', 'test-origin.git')
      commit = runner.method(:commit_removed_tracking!)
      advanced = false
      runner.define_singleton_method(:commit_removed_tracking!) do |current_slug, removal|
        unless advanced
          advanced = true
          Dir.mktmpdir('dev-session-remote-advance') do |checkout|
            system('git', 'clone', remote, checkout, out: File::NULL, err: File::NULL) || raise('clone failed')
            system('git', '-C', checkout, 'config', 'user.email', 'test@example.invalid') || raise('config failed')
            system('git', '-C', checkout, 'config', 'user.name', 'Test User') || raise('config failed')
            File.write(File.join(checkout, 'remote.txt'), "advanced\n")
            system('git', '-C', checkout, 'add', 'remote.txt') || raise('add failed')
            system('git', '-C', checkout, 'commit', '-m', 'advance remote', out: File::NULL) || raise('commit failed')
            system('git', '-C', checkout, 'push', 'origin', 'master', out: File::NULL) || raise('push failed')
          end
        end
        commit.call(current_slug, removal)
      end

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'workspace master advanced')
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('tracking_preserved', journal.fetch('phase'))
      assert_equal('start initiative', git_capture_success(
        'git', '-C', workspace, 'log', '-1', '--format=%s'
      ).strip)
    end
  end

  def test_remove_uses_verified_worktrees_missing_from_the_portal_manifest
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-unregistered-worktree'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      head = git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['repositories'] = []
      File.write(manifest_path, YAML.dump(manifest))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(path))
      recovery = JSON.parse(File.read(File.join(
        removal_recovery(workspace, slug), 'recovery.json'
      )))
      assert_equal(
        [{
          'name' => 'sample',
          'project' => 'sample',
          'path' => path,
          'git_common_dir' => File.join(workspace, 'repos', 'sample.git'),
          'branch' => slug,
          'head_sha' => head,
          'dirty' => false,
          'removed' => true
        }],
        recovery.fetch('worktrees')
      )
      assert_git_success(
        'git', "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref', '--verify', "refs/heads/#{slug}"
      )
    end
  end

  def test_remove_ignores_a_stale_portal_worktree_identity
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      create_bare_repo(workspace, 'other')
      slug = '2026-06-06-stale-worktree-registration'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest.fetch('repositories').fetch(0)['project'] = 'other'
      File.write(manifest_path, YAML.dump(manifest))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      recovery = JSON.parse(File.read(File.join(
        removal_recovery(workspace, slug), 'recovery.json'
      )))
      assert_equal('sample', recovery.fetch('worktrees').fetch(0).fetch('project'))
    end
  end

  def test_remove_refuses_dirty_worktrees_without_force
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      path = File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
      File.write(File.join(path, 'dirty.txt'), "dirty\n")

      error = assert_raises(DevSession::Error) do
        runner.delete('demo', as_is: false, force: false)
      end

      assert_match(/uncommitted changes/, error.message)
      assert(File.exist?(path))
    end
  end

  def test_remove_force_cleans_dirty_worktrees
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      path = File.join(workspace, 'worktrees', slug, 'sample')
      File.write(File.join(path, 'dirty.txt'), "dirty\n")

      runner.delete('demo', as_is: false, force: true)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      refute(File.exist?(File.join(workspace, 'work', slug)))
      recovery = removal_recovery(workspace, slug)
      assert(File.directory?(recovery))
      metadata = JSON.parse(File.read(File.join(recovery, 'recovery.json')))
      assert_equal(true, metadata.fetch('worktrees').fetch(0).fetch('dirty'))
    end
  end

  def test_remove_kills_managed_tmux_session
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
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)
      assert(tmux_session_exists?(socket, slug))

      runner.delete('demo', as_is: false, force: false)

      refute(tmux_session_exists?(socket, slug))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_remove_refuses_unmanaged_tmux_session
    skip 'tmux cannot run in this environment' unless tmux_test_available?

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', slug))
      tmux_run(socket, 'new-session', '-d', '-s', slug, '-c', workspace)
      cluster_helper = cleanup_contract_helper(workspace, 'empty-cluster', [])

      runner = DevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        cluster_providers: { 'alpha' => cluster_helper, 'beta' => cluster_helper },
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )

      error = assert_raises(DevSession::Error) do
        runner.delete('demo', as_is: false, force: false)
      end

      assert_match(/not managed/, error.message)
      assert(tmux_session_exists?(socket, slug))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_finalize_archives_tracking_after_removing_worktrees
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      add_runner = runner_for(workspace)
      add_runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      commit_tracking(workspace, slug, lifecycle: 'active')
      state = File.join(workspace, 'work', slug, 'state.md')
      tracking_paths = [
        File.join('work', slug),
        File.join('archive', slug)
      ]
      set_lifecycle(workspace, slug, 'complete')
      File.write(state, "#{File.read(state)}\nFinal result: passed\n")
      assert_equal(
        '1',
        git_capture_success(
          'git', '-C', workspace, 'rev-list', '--count', 'HEAD', '--', *tracking_paths
        ).strip
      )
      out = StringIO.new
      tmux = ManagedTmux.new(slug, workspace:)

      runner = runner_for(workspace, tmux:, out:)
      merge_registered_branches(workspace, slug)
      finalize_core(runner, 'demo', as_is: false)

      refute(tmux.killed)
      assert_includes(out.string, File.join(workspace, 'archive', slug))
      assert_includes(
        out.string,
        "stop after committing: dev-session stop #{slug} --as-is"
      )
      assert_includes(
        File.read(File.join(workspace, 'archive', slug, 'state.md')),
        'lifecycle: complete'
      )
      assert_includes(
        File.read(File.join(workspace, 'archive', slug, 'state.md')),
        'Final result: passed'
      )
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref',
        '--verify',
        '--quiet',
        'refs/heads/2026-06-06-demo'
      )

      error = assert_raises(DevSession::Error) do
        runner.stop(slug, as_is: true)
      end
      assert_match(/must be committed before stopping/, error.message)
      refute(tmux.killed)

      commit_archive_move(workspace, slug)
      assert_equal(
        '2',
        git_capture_success(
          'git', '-C', workspace, 'rev-list', '--count', 'HEAD', '--', *tracking_paths
        ).strip
      )
      runner.stop(slug, as_is: true)
      assert(tmux.killed)
    end
  end

  def test_session_closing_rejects_ambiguous_or_missing_tracking
    skip 'git is not available' unless command_available?('git')

    %i[stop remove].each do |operation|
      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        base_runner = runner_for(workspace)
        base_runner.ensure_tracking_files(slug)
        commit_tracking(workspace, slug, lifecycle: 'complete')
        tmux = ManagedTmux.new(slug, workspace:)
        runner = runner_for(workspace, tmux:)
        finalize_core(runner, slug, as_is: true)
        FileUtils.mkdir_p(File.join(workspace, 'work', slug))

        error = assert_raises(DevSession::Error) do
          if operation == :stop
            runner.stop(slug, as_is: true)
          else
            runner.delete(slug, as_is: true, force: false)
          end
        end

        assert_match(/active and archived tracking both exist/, error.message)
        refute(tmux.killed)
      end

      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        tmux = ManagedTmux.new(slug, workspace:)
        runner = runner_for(workspace, tmux:)

        error = assert_raises(DevSession::Error) do
          if operation == :stop
            runner.stop(slug, as_is: true)
          else
            runner.delete(slug, as_is: true, force: false)
          end
        end

        assert_match(/session tracking is missing/, error.message)
        refute(tmux.killed)
      end
    end
  end

  def test_session_closing_rejects_invalid_active_tracking
    cases = %i[symlink file empty_directory].map { |kind| [:stop, kind] }
    cases += %i[symlink file].map { |kind| [:remove, kind] }
    cases.each do |operation, kind|
      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        path = File.join(workspace, 'work', slug)
        case kind
        when :symlink
          FileUtils.ln_s(File.join(workspace, 'missing-work'), path)
        when :file
          File.write(path, "not a tracking directory\n")
        when :empty_directory
          FileUtils.mkdir_p(path)
        end

        tmux = ManagedTmux.new(slug, workspace:)
        runner = runner_for(workspace, tmux:)
        error = assert_raises(DevSession::Error) do
          if operation == :stop
            runner.stop(slug, as_is: true)
          else
            runner.delete(slug, as_is: true, force: false)
          end
        end

        assert_match(/work directory|session tracking|missing tracking files/, error.message)
        refute(tmux.killed)
      end
    end
  end

  def test_remove_accepts_incomplete_empty_tracking
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      FileUtils.mkdir_p(File.join(workspace, 'work', slug))
      tmux = ManagedTmux.new(slug, workspace:)

      runner_for(workspace, tmux:).delete(slug, as_is: true, force: false)

      assert(tmux.killed)
      refute(File.exist?(File.join(workspace, 'work', slug)))
      assert(File.directory?(File.join(removal_recovery(workspace, slug), 'work')))
    end
  end

end
