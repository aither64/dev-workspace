# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_archive_rejects_an_unmerged_registered_branch_before_mutating_state
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
      File.write(File.join(path, 'feature.txt'), "unmerged\n")
      assert_git_success('git', '-C', path, 'add', 'feature.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'unmerged feature')
      repository = File.join(workspace, 'repos', 'sample.git')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'push', 'origin',
        "refs/heads/#{slug}:refs/heads/#{slug}"
      )
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'feature head is not merged')
      assert_includes(error.message, "#{slug} -> origin/master")
      assert(File.directory?(File.join(workspace, 'work', slug)))
      assert(File.directory?(File.join(workspace, 'worktrees', slug, 'sample')))
      assert_match(
        /\A---\nlifecycle: active\n---/,
        File.read(File.join(workspace, 'work', slug, 'state.md'))
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_accepts_an_unchanged_unpushed_feature_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-unchanged-unpushed'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      repository = File.join(workspace, 'repos', 'sample.git')
      base = git_capture_success(
        'git', "--git-dir=#{repository}", 'rev-parse', 'refs/heads/master'
      ).strip
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      runner.archive(slug, as_is: true)

      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'archive', slug, 'portal.yml'))
      )
      repository_entry = manifest.fetch('repositories').fetch(0)
      assert_equal(base, repository_entry.fetch('initial_base_sha'))
      assert_equal(base, repository_entry.fetch('final_head_sha'))
      refute_git_success(
        'git', "--git-dir=#{repository}", 'ls-remote', '--exit-code', '--heads',
        'origin', "refs/heads/#{slug}"
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_retry_accepts_an_unchanged_unpushed_feature_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-retry-unchanged-unpushed'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      File.write(hook, "#!/bin/sh\nexit 1\n")
      File.chmod(0o755, hook)

      assert_raises(DevSession::CommandError) do
        runner.archive(slug, as_is: true)
      end
      File.unlink(hook)

      runner.archive(slug, as_is: true)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_archive_rejects_a_changed_unpushed_feature_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-changed-unpushed'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'feature.txt'), "unpushed\n")
      assert_git_success('git', '-C', path, 'add', 'feature.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'unpushed feature')
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'feature branch is not present on origin')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_closes_and_commits_a_coordination_only_session
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-coordination'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      File.write(File.join(workspace, 'staged.txt'), "staged\n")
      File.write(File.join(workspace, 'unstaged.txt'), "unstaged\n")
      assert_git_success('git', '-C', workspace, 'add', 'staged.txt')

      runner.archive(slug, as_is: true)

      refute(File.exist?(File.join(workspace, 'work', slug)))
      assert(File.directory?(File.join(workspace, 'archive', slug)))
      assert_match(
        /\A---\nlifecycle: complete\n---/,
        File.read(File.join(workspace, 'archive', slug, 'state.md'))
      )
      assert_equal(
        "workspace: archive #{slug}",
        git_capture_success('git', '-C', workspace, 'log', '-1', '--format=%s').strip
      )
      changed = git_capture_success(
        'git', '-C', workspace, 'show', '--format=', '--name-only', 'HEAD'
      ).lines.map(&:strip).reject(&:empty?)
      refute_includes(changed, 'staged.txt')
      refute_includes(changed, 'unstaged.txt')
      assert_equal('A  staged.txt', git_capture_success(
        'git', '-C', workspace, 'status', '--short', '--', 'staged.txt'
      ).strip)
      assert_equal('?? unstaged.txt', git_capture_success(
        'git', '-C', workspace, 'status', '--short', '--', 'unstaged.txt'
      ).strip)
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_retries_a_failed_exact_tracking_commit
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-retry'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      File.write(hook, "#!/bin/sh\nexit 1\n")
      File.chmod(0o755, hook)

      assert_raises(DevSession::CommandError) do
        runner.archive(slug, as_is: true)
      end
      assert(File.directory?(File.join(workspace, 'archive', slug)))
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_equal('tracking_archived', journal.fetch('phase'))

      File.unlink(hook)
      runner.archive(slug, as_is: true)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_equal(
        "workspace: archive #{slug}",
        git_capture_success('git', '-C', workspace, 'log', '-1', '--format=%s').strip
      )
    end
  end

  def test_archive_retry_does_not_kill_a_same_name_replacement
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-stale-runtime'
      state_home = File.join(workspace, '.xdg-state')
      authority_dir = File.join(workspace, 'authority')
      base = runner_for(workspace)
      base.ensure_tracking_files(slug)
      base.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      interrupted = false
      runner_class = Class.new(DevSession::Runner) do
        define_method(:advance_archive!) do |current_slug, journal, phase|
          super(current_slug, journal, phase)
          if phase == 'thread_retired' && !interrupted
            interrupted = true
            raise DevSession::Error, 'injected pre-runtime interruption'
          end
        end
      end
      first = runner_class.new(
        workspace:, tmux: NullTmux.new, authority_dir:,
        out: StringIO.new, err: StringIO.new, today: TODAY,
        env: { 'XDG_STATE_HOME' => state_home }
      )

      assert_raises(DevSession::Error) do
        first.archive(slug, as_is: true)
      end
      journal_path = first.send(:lifecycle_journal_file, slug, 'archive')
      assert_equal('thread_retired', JSON.parse(File.read(journal_path)).fetch('phase'))

      replacement = ReplacedTmux.new(slug, workspace:)
      second = runner_for(
        workspace,
        tmux: replacement,
        authority_dir:,
        env: { 'XDG_STATE_HOME' => state_home }
      )
      stale = DevSession::Tmux::Session.new(
        id: '$11', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test.sock',
        identity_token: 'a' * 64
      )
      second.send(:write_session_authority, slug, stale, state: 'ready')

      error = assert_raises(DevSession::Error) do
        second.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      refute(replacement.kill_attempted)
      assert(File.file?(File.join(authority_dir, "#{slug}.json")))
      assert(File.file?(journal_path))
    end
  end

  def test_archive_preflights_legacy_identity_before_creating_a_journal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-legacy-identity'
      authority_dir = File.join(workspace, 'authority')
      tmux = RefusedIdentityInitializationTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(tmux.identity_initialization_attempted)
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_rejects_a_renamed_authority_session_before_creating_a_journal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-renamed-session'
      authority_dir = File.join(workspace, 'authority')
      tmux = RenamedManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11'
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      session = DevSession::Tmux::Session.new(
        id: '$11', name: slug, mark: '1', slug:, workspace:,
        environment_slug: slug, socket_path: '/run/test.sock',
        identity_token: 'a' * 64
      )
      runner.send(:write_session_authority, slug, session, state: 'ready')
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_resume_upgrades_a_legacy_identity_before_continuing
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-resume-legacy'
      authority_dir = File.join(workspace, 'authority')
      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      plan = runner.send(:prepare_cleanup, slug, force: false)
      runner.send(:prepare_archive_journal!, slug, 'complete', {}, plan)

      runner.archive(slug, as_is: true)

      assert(tmux.killed)
      assert(File.directory?(File.join(workspace, 'archive', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
    end
  end

  def test_archive_retry_rejects_mutated_tracking_before_the_commit_phase
    skip 'git is not available' unless command_available?('git')

    %i[state thread].each do |mutation|
      with_workspace do |workspace|
        slug = "2026-06-06-archive-#{mutation}"
        base = runner_for(workspace)
        base.ensure_tracking_files(slug)
        base.send(:ensure_portal_manifest, slug)
        commit_tracking(workspace, slug, lifecycle: 'active')
        configure_workspace_origin(workspace)
        fail_once = true
        runner_class = Class.new(DevSession::Runner) do
          define_method(:advance_archive!) do |current_slug, journal, phase|
            if phase == 'tracking_archived' && fail_once
              fail_once = false
              raise DevSession::Error, 'injected post-move failure'
            end

            super(current_slug, journal, phase)
          end
        end
        runner = runner_class.new(
          workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
          today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
        )

        assert_raises(DevSession::Error) do
          runner.archive(slug, as_is: true)
        end
        assert(File.directory?(File.join(workspace, 'archive', slug)))
        journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
        assert_equal('clusters_released', journal.fetch('phase'))

        if mutation == :state
          state = File.join(workspace, 'archive', slug, 'state.md')
          File.write(state, File.read(state).sub('lifecycle: complete', 'lifecycle: active'))
        else
          portal = File.join(workspace, 'archive', slug, 'portal.yml')
          manifest = YAML.safe_load(File.read(portal))
          manifest['codex']['thread_id'] = 'replacement-thread'
          File.write(portal, YAML.dump(manifest))
        end

        error = assert_raises(DevSession::Error) do
          runner.archive(slug, as_is: true)
        end
        assert_includes(error.message, 'archived tracking changed during recovery')
        assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      end
    end
  end

  def test_unfinished_archive_reserves_the_slug_until_its_matching_retry
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-owned'
      base = runner_for(workspace)
      base.ensure_tracking_files(slug)
      base.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      fail_retirement = true
      runner_class = Class.new(DevSession::Runner) do
        define_method(:retire_portal_thread!) do |*arguments, **options|
          raise DevSession::Error, 'injected retirement failure' if fail_retirement

          super(*arguments, **options)
        end
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )

      assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_equal('tracking_committed', journal.fetch('phase'))

      conflicts = [
        -> { runner.delete(slug, as_is: true, force: false) },
        -> { runner.revive(slug, as_is: true) },
        -> do
          runner.start(
            slug, as_is: true, new: false, attach: false, run_codex: false
          )
        end,
        -> do
          runner.worktree_add(
            slug, 'sample', as_is: true, name: nil, branch: nil,
            base: 'master', fetch: false
          )
        end
      ]
      conflicts.each do |operation|
        error = assert_raises(DevSession::Error, &operation)
        assert_includes(error.message, 'session archive is unfinished')
      end

      fail_retirement = false
      runner.archive(slug, as_is: true)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert(File.directory?(File.join(workspace, 'archive', slug)))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_archive_retry_rejects_dirty_tracking_after_the_commit_phase
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-archive-dirty-commit'
      base = runner_for(workspace)
      base.ensure_tracking_files(slug)
      base.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      runner_class = Class.new(DevSession::Runner) do
        define_method(:retire_portal_thread!) do |*_arguments, **_options|
          raise DevSession::Error, 'injected retirement failure'
        end
      end
      runner = runner_class.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err: StringIO.new,
        today: TODAY, env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      )
      assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_equal('tracking_committed', journal.fetch('phase'))
      File.open(File.join(workspace, 'archive', slug, 'plan.md'), 'a') do |file|
        file.write("\nUncommitted change.\n")
      end

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'committed archive tracking differs')
      assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_archive_retry_reproves_the_retained_feature_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-reprove'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'feature.txt'), "merged\n")
      assert_git_success('git', '-C', path, 'add', 'feature.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'merged feature')
      commit_tracking(workspace, slug, lifecycle: 'active')
      merge_registered_branches(workspace, slug)
      configure_workspace_origin(workspace)
      hook = File.join(workspace, '.git', 'hooks', 'pre-commit')
      File.write(hook, "#!/bin/sh\nexit 1\n")
      File.chmod(0o755, hook)

      assert_raises(DevSession::CommandError) do
        runner.archive(slug, as_is: true)
      end
      File.unlink(hook)
      repository = File.join(workspace, 'repos', 'sample.git')
      temporary = File.join(workspace, 'advanced-feature')
      assert_git_success(
        'git', "--git-dir=#{repository}", 'worktree', 'add', temporary, slug
      )
      configure_git_identity(temporary)
      File.write(File.join(temporary, 'later.txt'), "not merged\n")
      assert_git_success('git', '-C', temporary, 'add', 'later.txt')
      assert_git_success('git', '-C', temporary, 'commit', '-m', 'later feature')
      assert_git_success('git', '-C', temporary, 'push', 'origin', slug)

      error = assert_raises(DevSession::Error) do
        runner.archive(slug, as_is: true)
      end

      assert_includes(error.message, 'feature branch changed after merge proof')
      assert(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_archive_reproves_exact_heads_before_each_destructive_retry_phase
    skip 'git is not available' unless command_available?('git')

    %w[quiesced clusters_released].each do |phase|
      with_workspace do |workspace|
        create_bare_repo(workspace, 'sample')
        slug = "2026-06-06-reprove-#{phase.tr('_', '-')}"
        runner = runner_for(workspace)
        runner.worktree_add(
          slug, 'sample', as_is: true, name: nil, branch: nil,
          base: 'master', fetch: false
        )
        path = File.join(workspace, 'worktrees', slug, 'sample')
        configure_git_identity(path)
        File.write(File.join(path, 'feature.txt'), "first merged head\n")
        assert_git_success('git', '-C', path, 'add', 'feature.txt')
        assert_git_success('git', '-C', path, 'commit', '-m', 'first merged feature')
        commit_tracking(workspace, slug, lifecycle: 'active')
        merge_registered_branches(workspace, slug)
        configure_workspace_origin(workspace)

        plan = runner.send(:prepare_cleanup, slug, force: false)
        heads = runner.send(:prove_registered_branches_merged!, slug, plan)
        journal = runner.send(:prepare_archive_journal!, slug, 'complete', heads, plan)
        runner.send(:advance_archive!, slug, journal, 'quiesced')
        if phase == 'clusters_released'
          runner.send(:advance_archive!, slug, journal, 'clusters_released')
        end

        File.write(File.join(path, 'later.txt'), "second merged head\n")
        assert_git_success('git', '-C', path, 'add', 'later.txt')
        assert_git_success('git', '-C', path, 'commit', '-m', 'second merged feature')
        assert_git_success('git', '-C', path, 'push', 'origin', slug)
        assert_git_success('git', '-C', path, 'push', 'origin', "#{slug}:master")

        error = assert_raises(DevSession::Error) do
          runner.archive(slug, as_is: true)
        end
        assert_includes(error.message, 'feature heads changed during archival')
        assert(File.directory?(File.join(workspace, 'work', slug)))
        assert(File.directory?(path))
        persisted = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'archive')))
        assert_equal(phase, persisted.fetch('phase'))
      end
    end
  end

  def test_archive_accepts_the_exact_feature_head_after_it_is_merged
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-merged'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'feature.txt'), "merged\n")
      assert_git_success('git', '-C', path, 'add', 'feature.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'merged feature')
      commit_tracking(workspace, slug, lifecycle: 'active')
      merge_registered_branches(workspace, slug)
      configure_workspace_origin(workspace)

      runner.archive(slug, as_is: true)

      manifest = YAML.safe_load(
        File.read(File.join(workspace, 'archive', slug, 'portal.yml'))
      )
      repository = manifest.fetch('repositories').fetch(0)
      assert_equal(
        git_capture_success(
          'git', "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
          'rev-parse', "refs/heads/#{slug}"
        ).strip,
        repository.fetch('final_head_sha')
      )
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_archive_abandoned_skips_the_merge_requirement
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-discarded'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      File.write(File.join(path, 'discarded.txt'), "discarded\n")
      assert_git_success('git', '-C', path, 'add', 'discarded.txt')
      assert_git_success('git', '-C', path, 'commit', '-m', 'discarded')
      commit_tracking(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)

      runner.archive(slug, as_is: true, abandoned: true)

      assert(File.directory?(File.join(workspace, 'archive', slug)))
      assert_match(
        /\A---\nlifecycle: abandoned\n---/,
        File.read(File.join(workspace, 'archive', slug, 'state.md'))
      )
    end
  end

end
