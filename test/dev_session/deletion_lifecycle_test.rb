# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_remove_discards_session_to_private_recovery_and_keeps_branch
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
      runner.delete('demo', as_is: false, force: false)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      refute(File.exist?(File.join(workspace, 'work', slug)))
      recovery = removal_recovery(workspace, slug)
      assert(File.exist?(File.join(recovery, 'work', 'plan.md')))
      assert_equal('removed', JSON.parse(File.read(File.join(recovery, 'recovery.json')))['state'])
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref',
        '--verify',
        '--quiet',
        'refs/heads/2026-06-06-demo'
      )
    end
  end

  def test_remove_records_heads_in_recovered_tracking
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
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
      path = File.join(workspace, 'worktrees', slug, 'sample')
      expected_head = git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip

      runner.delete('demo', as_is: false, force: false)
      recovery = removal_recovery(workspace, slug)
      manifest = YAML.safe_load(File.read(File.join(recovery, 'work', 'portal.yml')))
      assert_equal(expected_head, manifest.dig('repositories', 0, 'final_head_sha'))
    end
  end

  def test_finalize_rejects_worktree_swapped_from_another_canonical_repository
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      create_bare_repo(workspace, 'other')
      slug = '2026-06-06-demo'
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
      path = File.join(workspace, 'worktrees', slug, 'sample')
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'worktree',
        'remove',
        path
      )
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'other.git')}",
        'worktree',
        'add',
        '-b',
        'replacement',
        path,
        'master'
      )
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(DevSession::Error) do
        finalize_core(runner, 'demo', as_is: false)
      end
      assert_match(/repository identity does not match/, error.message)
      assert(File.directory?(path))
      refute(File.exist?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_remove_kills_session_after_worktrees_are_removed
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
      path = File.join(workspace, 'worktrees', slug, 'sample')
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        on_kill: lambda {
          refute(File.exist?(path))
          assert(File.exist?(File.join(workspace, 'work', slug)))
        }
      )
      remove_runner = runner_for(workspace, tmux:)

      remove_runner.delete('demo', as_is: false, force: false)

      assert(tmux.killed)
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      refute(File.exist?(File.join(workspace, 'work', slug)))
      assert(File.directory?(removal_recovery(workspace, slug)))
    end
  end

  def test_delete_all_is_rejected
    with_workspace do |workspace|
      err = StringIO.new
      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', 'demo', '--all'],
        out: StringIO.new,
        err:
      ).run

      assert_equal(1, status)
      assert_match(/invalid option: --all/, err.string)
    end
  end

  def test_delete_yes_bypass_is_rejected
    with_workspace do |workspace|
      runner_for(workspace).ensure_tracking_files('2026-06-06-demo')
      err = StringIO.new
      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', '2026-06-06-demo', '--as-is', '--yes'],
        input: StringIO.new,
        out: StringIO.new,
        err:
      ).run

      assert_equal(1, status)
      assert_match(/invalid option: --yes/, err.string)
      assert(File.directory?(File.join(workspace, 'work', '2026-06-06-demo')))
    end
  end

  def test_delete_requires_confirmation_in_noninteractive_cli
    with_workspace do |workspace|
      runner_for(workspace).ensure_tracking_files('2026-06-06-demo')
      err = StringIO.new

      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', '2026-06-06-demo', '--as-is'],
        input: StringIO.new,
        out: StringIO.new,
        err:
      ).run

      assert_equal(1, status)
      assert_includes(err.string, 'requires an interactive terminal')
      assert(File.directory?(File.join(workspace, 'work', '2026-06-06-demo')))
    end
  end

  def test_delete_uses_a_simple_interactive_confirmation
    with_workspace do |workspace|
      runner_for(workspace).ensure_tracking_files('2026-06-06-demo')
      err = StringIO.new

      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', '2026-06-06-demo', '--as-is'],
        input: TTYInput.new("no\n"),
        out: StringIO.new,
        err:,
        env: ENV.to_h.merge('XDG_STATE_HOME' => File.join(workspace, '.xdg-state'))
      ).run

      assert_equal(1, status)
      assert_includes(err.string, 'was not confirmed')
      assert(File.directory?(File.join(workspace, 'work', '2026-06-06-demo')))

      status = DevSession::CLI.new(
        ['--workspace', workspace, 'delete', '2026-06-06-demo', '--as-is'],
        input: TTYInput.new("yes\n"),
        out: StringIO.new,
        err: StringIO.new,
        env: ENV.to_h.merge('XDG_STATE_HOME' => File.join(workspace, '.xdg-state'))
      ).run

      assert_equal(0, status)
      refute(File.exist?(File.join(workspace, 'work', '2026-06-06-demo')))
    end
  end

  def test_remove_discards_an_archived_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      FileUtils.mkdir_p(File.join(workspace, 'archive'))
      File.rename(File.join(workspace, 'work', slug), File.join(workspace, 'archive', slug))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(File.join(workspace, 'archive', slug)))
      assert(File.directory?(File.join(removal_recovery(workspace, slug), 'archive')))
    end
  end

  def test_remove_preserves_the_creation_journal
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Fix the session.\n")
      runner = runner_for(workspace)
      runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      runner.ensure_tracking_files(slug)

      runner.delete(slug, as_is: true, force: false)

      recovery = removal_recovery(workspace, slug)
      assert(File.file?(File.join(recovery, 'creation.json')))
      refute(File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.creation.json")))
    end
  end

  def test_remove_force_refuses_unmanaged_worktree_entries
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      File.write(File.join(workspace, 'worktrees', slug, 'unmanaged.txt'), "keep me\n")

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'contains unmanaged entries')
      assert_equal(
        "keep me\n",
        File.read(File.join(workspace, 'worktrees', slug, 'unmanaged.txt'))
      )
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_force_refuses_a_symlinked_worktree_entry
    with_workspace do |workspace|
      slug = '2026-06-06-symlinked-worktree'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      outside = File.join(workspace, 'outside-worktree')
      FileUtils.mkdir_p(outside)
      FileUtils.ln_s(outside, File.join(workspace, 'worktrees', slug, 'linked'))

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'contains unmanaged entries')
      assert(File.symlink?(File.join(workspace, 'worktrees', slug, 'linked')))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_force_refuses_a_worktree_from_a_foreign_repository
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-foreign-worktree'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      Dir.mktmpdir('external-dev-session-repository') do |external|
        FileUtils.mkdir_p(File.join(external, 'repos'))
        create_bare_repo(external, 'sample')
        bare = File.join(external, 'repos', 'sample.git')
        path = File.join(workspace, 'worktrees', slug, 'sample')
        assert_git_success('git', "--git-dir=#{bare}", 'worktree', 'add', path, 'master')

        error = assert_raises(DevSession::Error) do
          runner.delete(slug, as_is: true, force: true)
        end

        assert_includes(error.message, 'outside the canonical repository root')
        assert(File.directory?(path))
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      end
    end
  end

  def test_remove_releases_both_development_cluster_types
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      helpers = File.join(workspace, 'helpers')
      log = File.join(workspace, 'cluster.log')
      FileUtils.mkdir_p(helpers)
      %w[alpha-devcluster beta-devcluster].each do |name|
        path = File.join(helpers, name)
        script = <<~'SH'
          #!/bin/sh
          if [ "$1" = cleanup-paths ]; then
            printf '%s\n' '{"schema":1,"paths":[]}'
            exit 0
          fi
          name=${0##*/}
          printf '%s:%s:%s\n' "$DEVCLUSTER_WORKSPACE" "$name" "$*" >> "$CLUSTER_LOG"
        SH
        File.write(path, script)
        File.chmod(0o755, path)
      end
      runner = runner_for(
        workspace,
        env: {'PATH' => helpers, 'CLUSTER_LOG' => log},
        alpha_cluster: File.join(helpers, 'alpha-devcluster'),
        beta_cluster: File.join(helpers, 'beta-devcluster')
      )
      runner.ensure_tracking_files(slug)

      runner.delete(slug, as_is: true, force: false)

      assert_equal(
        [
          "#{workspace}:alpha-devcluster:reset #{slug}",
          "#{workspace}:beta-devcluster:reset #{slug}"
        ],
        File.readlines(log, chomp: true)
      )
    end
  end

  def test_remove_retries_upload_cleanup_before_finalizing_the_journal
    with_workspace do |workspace|
      slug = '2026-06-06-upload-cleanup'
      creator = runner_for(workspace)
      creator.ensure_tracking_files(slug)
      manifest = creator.send(:ensure_portal_manifest, slug)
      manifest['codex'] = {'thread_id' => 'thread-inputs'}
      creator.send(:write_portal_manifest, slug, manifest)
      log = File.join(workspace, 'upload-cleanup.log')
      marker = File.join(workspace, 'upload-cleanup-retry')
      portal = File.join(workspace, 'portal')
      File.write(portal, <<~SH)
        #!/bin/sh
        if [ "$1" = uploads ]; then
          printf '%s\n' "$*" >> #{Shellwords.escape(log)}
          if [ ! -e #{Shellwords.escape(marker)} ]; then
            touch #{Shellwords.escape(marker)}
            exit 19
          fi
        fi
      SH
      File.chmod(0o755, portal)
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new, portal_command: [portal],
        codex_socket: '/run/test/codex.sock', out: StringIO.new, err: StringIO.new,
        today: TODAY, env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      assert_raises(DevSession::CommandError) { runner.delete(slug, as_is: true, force: true) }
      journal_path = runner.send(:lifecycle_journal_file, slug, 'delete')
      assert_equal('tracking_committed', JSON.parse(File.read(journal_path)).fetch('phase'))
      refute(File.exist?(File.join(workspace, 'work', slug)))
      runner.delete(slug, as_is: true, force: true)
      refute(File.exist?(journal_path))
      calls = File.readlines(log, chomp: true)
      assert_equal(2, calls.length)
      assert_equal(calls.first, calls.last)
      assert_includes(calls.first, "--thread-id thread-inputs")
      assert_includes(calls.first, "--session-slug #{slug}")
    end
  end

  def test_remove_retains_recovery_without_touching_worktrees_when_thread_retirement_fails
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      portal = File.join(workspace, 'portal')
      File.write(portal, "#!/bin/sh\n[ \"$1\" = agent-teams ] && [ \"$2\" = require-unmanaged ] && exit 0\nexit 19\n")
      File.chmod(0o755, portal)
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        portal_command: [portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['codex'] = {
        'thread_id' => 'thread-1',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.153.4'
      }
      File.write(manifest_path, YAML.dump(manifest))

      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert(File.directory?(File.join(workspace, 'worktrees', slug, 'sample')))
      assert(File.directory?(File.join(workspace, 'work', slug)))
      recovery = removal_recovery(workspace, slug)
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('thread_retiring', journal.fetch('phase'))
      assert(File.file?(File.join(recovery, 'recovery.json')))
    end
  end

  def test_remove_seals_the_head_written_by_an_active_turn_before_cleanup
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-active-turn-head'
      creator = runner_for(workspace)
      creator.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        exit 0 if ARGV[0, 2] == ['agent-teams', 'require-unmanaged']
        exit 0 if ARGV[0, 2] == ['uploads', 'remove-session']
        File.write(File.join(#{worktree.dump}, 'from-active-turn'), "changed\n")
        system(
          {'GIT_AUTHOR_NAME' => 'Test', 'GIT_AUTHOR_EMAIL' => 'test@example.invalid',
           'GIT_COMMITTER_NAME' => 'Test', 'GIT_COMMITTER_EMAIL' => 'test@example.invalid'},
          'git', '-C', #{worktree.dump}, 'add', 'from-active-turn'
        ) or abort 'unable to stage active-turn change'
        system(
          {'GIT_AUTHOR_NAME' => 'Test', 'GIT_AUTHOR_EMAIL' => 'test@example.invalid',
           'GIT_COMMITTER_NAME' => 'Test', 'GIT_COMMITTER_EMAIL' => 'test@example.invalid'},
          'git', '-C', #{worktree.dump}, 'commit', '-m', 'active turn change'
        ) or abort 'unable to commit active-turn change'
      RUBY
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['codex'] = {
        'thread_id' => 'thread-1', 'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.153.4'
      }
      File.write(manifest_path, YAML.dump(manifest))
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new, err: StringIO.new, today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )

      runner.delete(slug, as_is: true, force: true)

      recovery = JSON.parse(File.read(File.join(removal_recovery(workspace, slug), 'recovery.json')))
      recorded = recovery.fetch('worktrees').fetch(0)
      assert_equal(
        git_capture_success(
          'git', "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
          'rev-parse', slug
        ).strip,
        recorded.fetch('head_sha')
      )
      assert_equal(false, recorded.fetch('dirty'))
      assert_equal(true, recorded.fetch('removed'))
    end
  end

  def test_remove_retry_refuses_a_worktree_added_after_deletion_started
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      create_bare_repo(workspace, 'other')
      slug = '2026-06-06-added-worktree'
      creator = runner_for(workspace)
      creator.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      portal = File.join(workspace, 'portal')
      File.write(portal, "#!/bin/sh\n[ \"$1\" = agent-teams ] && [ \"$2\" = require-unmanaged ] && exit 0\nexit 19\n")
      File.chmod(0o755, portal)
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['codex'] = {
        'thread_id' => 'thread-1', 'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.153.4'
      }
      File.write(manifest_path, YAML.dump(manifest))
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new, portal_command: [portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new, err: StringIO.new, today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: true)
      end
      added = File.join(workspace, 'worktrees', slug, 'other')
      assert_git_success(
        'git', "--git-dir=#{File.join(workspace, 'repos', 'other.git')}",
        'worktree', 'add', '-b', slug, added, 'master'
      )
      File.write(portal, "#!/bin/sh\nexit 0\n")

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'worktrees were added after deletion started')
      assert(File.directory?(added))
      assert(File.directory?(File.join(workspace, 'worktrees', slug, 'sample')))
    end
  end

  def test_remove_retry_refuses_head_drift_after_inventory_is_sealed
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-sealed-head-drift'
      creator = runner_for(workspace)
      creator.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      helper = File.join(workspace, 'alpha-devcluster')
      marker = File.join(workspace, 'cluster-retried')
      File.write(helper, <<~SH)
        #!/bin/sh
        if [ "$1" = cleanup-paths ]; then
          printf '%s\n' '{"schema":1,"paths":[]}'
          exit 0
        fi
        if [ ! -e #{Shellwords.escape(marker)} ]; then
          : > #{Shellwords.escape(marker)}
          exit 19
        fi
      SH
      File.chmod(0o755, helper)
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new, cluster_providers: { 'alpha' => helper },
        out: StringIO.new, err: StringIO.new, today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: true)
      end
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      File.write(File.join(worktree, 'later'), "changed\n")
      assert_git_success('git', '-C', worktree, 'add', 'later')
      assert_git_success(
        'git', '-C', worktree,
        '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid',
        'commit', '-m', 'later change'
      )

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'worktree head sha changed during deletion')
      assert(File.directory?(worktree))
    end
  end

  def test_remove_retry_proves_a_worktree_removed_before_its_journal_update
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      create_bare_repo(workspace, 'other')
      slug = '2026-06-06-partial-worktree-removal'
      runner = runner_for(workspace)
      %w[sample other].each do |project|
        runner.worktree_add(
          slug, project, as_is: true, name: nil, branch: nil,
          base: 'master', fetch: false
        )
      end
      remove = runner.method(:remove_worktree_path)
      interrupted = false
      runner.define_singleton_method(:remove_worktree_path) do |*arguments, **options|
        remove.call(*arguments, **options)
        unless interrupted
          interrupted = true
          raise DevSession::Error, 'simulated interruption after worktree removal'
        end
      end

      assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal(0, journal.fetch('worktrees').count { |entry| entry.fetch('removed', false) })

      runner.delete(slug, as_is: true, force: false)

      recovery = JSON.parse(File.read(File.join(removal_recovery(workspace, slug), 'recovery.json')))
      assert(recovery.fetch('worktrees').all? { |entry| entry.fetch('removed') })
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_remove_rechecks_the_head_immediately_before_worktree_removal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-worktree-removal-race'
      runner = runner_for(workspace)
      runner.worktree_add(
        slug, 'sample', as_is: true, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      remove = runner.method(:remove_worktree_path)
      changed = false
      runner.define_singleton_method(:remove_worktree_path) do |*arguments, **options|
        unless changed
          changed = true
          File.write(File.join(worktree, 'raced'), "changed\n")
          system('git', '-C', worktree, 'add', 'raced') or raise 'unable to stage race'
          system(
            'git', '-C', worktree,
            '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid',
            'commit', '-m', 'raced change'
          ) or raise 'unable to commit race'
        end
        remove.call(*arguments, **options)
      end

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'worktree head sha changed during cleanup')
      assert(File.directory?(worktree))
    end
  end

end
