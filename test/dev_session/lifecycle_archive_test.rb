# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_stop_rejects_an_archive_without_committed_active_history
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      archive = File.join(workspace, 'archive', slug)
      FileUtils.mkdir_p(archive)
      File.write(File.join(archive, 'plan.md'), "# Plan\n")
      File.write(
        File.join(archive, 'state.md'),
        "---\nlifecycle: complete\n---\n\n# #{slug}\n\n## Status\n"
      )
      assert_git_success('git', 'init', '-b', 'master', workspace)
      configure_git_identity(workspace)
      assert_git_success('git', '-C', workspace, 'add', File.join('archive', slug))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'terminal archive only')

      tmux = ManagedTmux.new(slug, workspace:)
      error = assert_raises(DevSession::Error) do
        runner_for(workspace, tmux:).stop(slug, as_is: true)
      end

      assert_match(/no committed active lifecycle/, error.message)
      refute(tmux.killed)
    end
  end

  def test_finalize_accepts_abandoned_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'abandoned')

      finalize_core(runner, 'demo', as_is: false)

      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_finalize_refuses_active_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/lifecycle is not terminal/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_missing_tracking_file
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      FileUtils.rm(File.join(workspace, 'work', slug, 'plan.md'))

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/missing tracking files/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_tracking_without_a_prior_commit
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      set_lifecycle(workspace, slug, 'complete')

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/tracking files have no prior commit/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_tracking_without_a_committed_active_state
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_terminal_tracking_only(workspace, slug, lifecycle: 'complete')

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/no committed active lifecycle/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_uses_only_front_matter_for_current_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      state = File.join(workspace, 'work', slug, 'state.md')
      File.write(
        state,
        state_with_body_lifecycle(File.read(state), 'complete')
      )

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
      end

      assert_match(/not terminal: active/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_missing_lifecycle_front_matter
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      state = File.join(workspace, 'work', slug, 'state.md')
      content = File.read(state).sub(/\A---\nlifecycle: active\n---\n\n/, '')
      File.write(state, "#{content}\n- Lifecycle: complete\n")

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
      end

      assert_match(/must start with lifecycle YAML front matter/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_lifecycle_front_matter_has_exact_boundaries
    with_workspace do |workspace|
      runner = runner_for(workspace)
      assert_nil(
        runner.send(
          :validate_terminal_lifecycle_content!,
          "---\r\nlifecycle: complete\r\n---\r\n\r\n# State\r\n"
        )
      )

      invalid = [
        " \n---\nlifecycle: complete\n---\n",
        "\uFEFF---\nlifecycle: complete\n---\n",
        "---\nlifecycle: complete\nowner: agent\n---\n",
        "---\nlifecycle: complete\n--- trailing\n"
      ]
      invalid.each do |content|
        error = assert_raises(DevSession::Error) do
          runner.send(:validate_terminal_lifecycle_content!, content)
        end
        assert_match(/must start with lifecycle YAML front matter/, error.message)
      end
    end
  end

  def test_ruby_lifecycle_parser_accepts_shared_valid_fixtures
    fixtures = Dir[File.expand_path('../fixtures/lifecycle-valid-*.md', __dir__)]
    refute_empty(fixtures)

    fixtures.each do |fixture|
      with_workspace do |workspace|
        lifecycle = runner_for(workspace).send(:lifecycle_state, File.binread(fixture))
        assert_includes(%w[active complete abandoned], lifecycle, File.basename(fixture))
      end
    end
  end

  def test_ruby_lifecycle_parser_rejects_shared_invalid_fixtures
    fixtures = Dir[File.expand_path('../fixtures/lifecycle-invalid-*.md', __dir__)]
    refute_empty(fixtures)

    fixtures.each do |fixture|
      with_workspace do |workspace|
        assert_raises(DevSession::Error, File.basename(fixture)) do
          runner_for(workspace).send(:lifecycle_state, File.binread(fixture))
        end
      end
    end
  end

  def test_lifecycle_parser_rejects_invalid_utf8_and_oversized_input
    with_workspace do |workspace|
      runner = runner_for(workspace)
      assert_raises(DevSession::Error) do
        runner.send(:lifecycle_state, "---\nlifecycle: active\n---\n\xff".b)
      end
      assert_raises(DevSession::Error) do
        runner.send(
          :lifecycle_state,
          "---\nlifecycle: active\n---\n" + ('x' * DevSession::TRACKING_MAX_SIZE)
        )
      end
    end
  end

  def test_finalize_uses_only_front_matter_for_active_history
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      state = File.join(workspace, 'work', slug, 'state.md')
      set_lifecycle(workspace, slug, 'complete')
      File.write(
        state,
        state_with_body_lifecycle(File.read(state), 'active')
      )
      assert_git_success('git', 'init', '-b', 'master', workspace)
      configure_git_identity(workspace)
      assert_git_success('git', '-C', workspace, 'add', File.join('work', slug))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'pseudo active state')

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
      end

      assert_match(/no committed active lifecycle/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_stop_uses_only_front_matter_for_archived_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      tmux = ManagedTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)
      finalize_core(runner, slug, as_is: true)
      state = File.join(workspace, 'archive', slug, 'state.md')
      content = File.read(state).sub('lifecycle: complete', 'lifecycle: active')
      File.write(state, state_with_body_lifecycle(content, 'complete'))
      commit_archive_move(workspace, slug)

      error = assert_raises(DevSession::Error) do
        runner.stop(slug, as_is: true)
      end

      assert_match(/not terminal: active/, error.message)
      refute(tmux.killed)
    end
  end

  def test_finalize_refuses_existing_archive
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      FileUtils.mkdir_p(File.join(workspace, 'archive', slug))

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/archive already exists/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_a_dangling_archive_symlink
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      FileUtils.mkdir_p(File.join(workspace, 'archive'))
      FileUtils.ln_s(
        File.join(workspace, 'missing-archive-target'),
        File.join(workspace, 'archive', slug)
      )

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/archive already exists/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_dirty_worktree_without_changing_session
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
      commit_tracking(workspace, slug, lifecycle: 'complete')
      path = File.join(workspace, 'worktrees', slug, 'sample')
      File.write(File.join(path, 'dirty.txt'), "dirty\n")
      tmux = ManagedTmux.new(slug, workspace:)

      error = assert_raises(DevSession::Error) do
        finalize_core(runner_for(workspace, tmux:), 'demo', as_is: false)
      end

      assert_match(/uncommitted changes/, error.message)
      refute(tmux.killed)
      assert(File.directory?(path))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_a_clean_detached_worktree_head
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
      commit_tracking(workspace, slug, lifecycle: 'complete')
      path = File.join(workspace, 'worktrees', slug, 'sample')
      assert_git_success('git', '-C', path, 'switch', '--detach')
      configure_git_identity(path)
      File.write(File.join(path, 'detached.txt'), "unique commit\n")
      assert_git_success('git', '-C', path, 'add', 'detached.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'detached work')
      detached_head = git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/detached HEAD/, error.message)
      assert(File.directory?(path))
      assert_equal(detached_head, git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip)
    end
  end

  def test_worktree_remove_refuses_a_per_worktree_symbolic_ref
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
      assert_git_success('git', '-C', path, 'symbolic-ref', 'HEAD', 'refs/worktree/private-save')
      configure_git_identity(path)
      File.write(File.join(path, 'saved.txt'), "saved commit\n")
      assert_git_success('git', '-C', path, 'add', 'saved.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'private worktree commit')
      head = git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip

      error = assert_raises(DevSession::Error) do
        runner.worktree_remove('demo', 'sample', as_is: false, force: false)
      end

      assert_match(/retained shared branch/, error.message)
      assert(File.directory?(path))
      assert_git_success('git', '-C', path, 'cat-file', '-e', "#{head}^{commit}")
    end
  end

  def test_finalize_refuses_a_symlinked_worktree_entry
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      outside = File.join(workspace, 'outside-sample')
      bare = File.join(workspace, 'repos', 'sample.git')
      assert_git_success(
        'git',
        "--git-dir=#{bare}",
        'worktree',
        'add',
        '-b',
        'outside',
        outside,
        'master'
      )
      FileUtils.ln_s(outside, File.join(workspace, 'worktrees', slug, 'sample'))

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/unmanaged entries/, error.message)
      assert(File.directory?(outside))
      assert_git_success('git', '-C', outside, 'status', '--short')
    end
  end

  def test_finalize_refuses_a_symlinked_work_root
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      work_root = File.join(workspace, 'work')
      outside_root = File.join(workspace, 'outside-work')
      FileUtils.mv(work_root, outside_root)
      FileUtils.ln_s(outside_root, work_root)

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/work root is a symlink/, error.message)
      assert(File.directory?(File.join(outside_root, slug)))
      assert(File.file?(File.join(outside_root, slug, 'state.md')))
      refute(File.exist?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_finalize_uses_an_atomic_no_clobber_archive_move
    skip 'git is not available' unless command_available?('git')

    %i[directory symlink].each do |collision|
      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        base_runner = runner_for(workspace)
        base_runner.ensure_tracking_files(slug)
        commit_tracking(workspace, slug, lifecycle: 'complete')
        source = File.join(workspace, 'work', slug)
        destination = File.join(workspace, 'archive', slug)

        command_runner = CallbackCommandRunner.new(
          out: StringIO.new,
          err: StringIO.new
        ) do |argv|
          next unless argv.first == 'mv' && argv.last != '--help'

          if collision == :directory
            FileUtils.mkdir_p(destination)
          else
            FileUtils.ln_s(File.join(workspace, 'collision-target'), destination)
          end
        end
        runner = DevSession::Runner.new(
          workspace:,
          command_runner:,
          tmux: NullTmux.new,
          out: StringIO.new,
          err: StringIO.new,
          today: TODAY
        )

        assert_raises(DevSession::Error) do
          finalize_core(runner, 'demo', as_is: false)
        end

        assert(File.directory?(source), "#{collision} collision moved the source")
        assert(File.file?(File.join(source, 'state.md')))
        if collision == :symlink
          assert(File.symlink?(destination))
        else
          assert_equal([], Dir.children(destination))
        end
      end
    end
  end

  def test_atomic_archive_move_syncs_both_parent_directories
    with_workspace do |workspace|
      source = File.join(workspace, 'work', '2026-06-06-demo')
      destination = File.join(workspace, 'archive', '2026-06-06-demo')
      FileUtils.mkdir_p(source)
      FileUtils.mkdir_p(File.dirname(destination))
      runner = runner_for(workspace)
      synced = []
      runner.define_singleton_method(:fsync_directory) { |path| synced << path }

      runner.send(:atomic_archive_move!, source, destination)

      assert_equal(
        [File.join(workspace, 'work'), File.join(workspace, 'archive')],
        synced
      )
      refute(File.exist?(source))
      assert(File.directory?(destination))
    end
  end

  def test_finalize_checks_atomic_move_support_before_worktree_cleanup
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      base_runner = runner_for(workspace)
      base_runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      commit_tracking(workspace, slug, lifecycle: 'complete')
      path = File.join(workspace, 'worktrees', slug, 'sample')
      command_runner = CallbackCommandRunner.new(
        out: StringIO.new,
        err: StringIO.new
      ) do |argv|
        next unless argv.first == 'mv' && argv.last == '--help'

        raise DevSession::Error, 'atomic move options unavailable'
      end
      runner = DevSession::Runner.new(
        workspace:,
        command_runner:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
      end

      assert(File.directory?(path))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_preflights_and_executes_one_archive_move_command
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      base_runner = runner_for(workspace)
      base_runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      move_commands = []
      command_runner = CallbackCommandRunner.new(
        out: StringIO.new,
        err: StringIO.new
      ) do |argv|
        move_commands << argv if argv.first == 'mv'
      end
      runner = DevSession::Runner.new(
        workspace:,
        command_runner:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      finalize_core(runner, slug, as_is: true)

      assert_equal(2, move_commands.length)
      move_commands.each do |argv|
        assert_equal(
          ['mv', *DevSession::ARCHIVE_MOVE_OPTIONS],
          argv.take(DevSession::ARCHIVE_MOVE_OPTIONS.length + 1)
        )
      end
      assert_equal('--help', move_commands.first.last)
      assert_equal(File.join(workspace, 'archive', slug), move_commands.last.last)
    end
  end

  def test_dev_session_serializes_commands_per_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-demo'
      other_slug = '2026-06-06-other'
      runner.ensure_tracking_files(slug)
      runner.ensure_tracking_files(other_slug)
      lock_root = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(lock_root)
      lock_path = File.join(lock_root, "#{slug}.lock")

      File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
        assert(lock.flock(File::LOCK_EX | File::LOCK_NB))

        error = assert_raises(DevSession::Error) do
          runner.delete(slug, as_is: true, force: false)
        end
        assert_match(/another dev-session command/, error.message)

        runner.delete(other_slug, as_is: true, force: false)
        refute(File.exist?(File.join(workspace, 'worktrees', other_slug)))
      end

      runner.delete(slug, as_is: true, force: false)
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_finalize_refuses_unmanaged_worktree_group_entries
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', slug, 'cache'))

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end

      assert_match(/contains unmanaged entries/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_a_registered_git_worktree_missing_from_manifest
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      path = File.join(workspace, 'worktrees', slug, 'sample')
      assert_git_success('git', "--git-dir=#{repository}", 'worktree', 'add', path, 'master')
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, slug, as_is: true)
      end

      assert_match(/missing from the portal manifest/, error.message)
      assert(File.directory?(path))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_a_worktree_from_outside_canonical_repositories
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      Dir.mktmpdir('external-dev-session-repository') do |external|
        FileUtils.mkdir_p(File.join(external, 'repos'))
        create_bare_repo(external, 'sample')
        bare = File.join(external, 'repos', 'sample.git')
        path = File.join(workspace, 'worktrees', slug, 'external')
        assert_git_success('git', "--git-dir=#{bare}", 'worktree', 'add', path, 'master')

        error = assert_raises(DevSession::Error) do
          finalize_core(runner, slug, as_is: true)
        end

        assert_match(/outside the canonical repository root/, error.message)
        assert(File.directory?(path))
      end
    end
  end

  def test_finalize_refuses_unmanaged_tmux_session
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(DevSession::Error) do
        finalize_core(
          runner_for(workspace, tmux: UnmanagedTmux.new(slug)),
          'demo',
          as_is: false
        )
      end

      assert_match(/not managed/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

end
