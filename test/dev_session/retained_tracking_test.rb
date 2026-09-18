# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_start_adopts_committed_active_tracking_and_registers_existing_worktrees
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-retained'
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
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      FileUtils.mkdir_p(File.dirname(worktree))
      assert_git_success(
        'git', "--git-dir=#{repository}", 'worktree', 'add', worktree, slug
      )

      tracking = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(tracking)
      plan = "# Retained plan\n\nPokračovat v existující implementaci.\n"
      state = "---\nlifecycle: active\n---\n\n# Retained state\n\nPřipraveno.\n" + ('x' * 1_100_000)
      File.write(File.join(tracking, 'plan.md'), plan)
      File.write(File.join(tracking, 'state.md'), state)
      commit_tracking(workspace, slug, lifecycle: 'active')

      goal = File.join(workspace, 'goal.txt')
      portal = File.join(workspace, 'portal.rb')
      File.write(goal, "Read plan.md and state.md, then continue the retained initiative.\n")
      File.write(portal, <<~RUBY)
        require 'json'
        puts JSON.generate(threadId: 'thread-retained') if ARGV[0, 2] == ['thread', 'create']
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$retained', name: slug, mark: '1', slug:, workspace:,
        socket_path: '/run/current/tmux.sock', codex_thread_id: 'thread-retained',
        codex_socket_path: '/run/current/app-server.sock',
        codex_client_version: '0.152.1', codex_pane_id: '%1'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_args, **_options| session }
        define_method(:sync_slug) do |selected_slug, **_options|
          sync_portal_repositories(selected_slug, worktree_entries(selected_slug))
          session
        end
        define_method(:revalidate_session!) { |_selected| session }
        define_method(:reconcile_native_client!) { |_slug, selected, **_options| selected }
        define_method(:verify_codex_client!) {}
      end
      out = StringIO.new
      runner = runner_class.new(
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

      interrupted = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      assert_equal('retained', interrupted.fetch('tracking_origin'))
      refute(File.exist?(File.join(tracking, 'portal.yml')))

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

      assert_equal('thread-retained', JSON.parse(out.string).fetch('threadId'))
      assert_equal(plan.b, File.binread(File.join(tracking, 'plan.md')))
      assert_equal(state.b, File.binread(File.join(tracking, 'state.md')))
      manifest = YAML.safe_load(File.read(File.join(tracking, 'portal.yml')))
      assert_equal('thread-retained', manifest.dig('codex', 'thread_id'))
      assert_equal('sample', manifest.dig('repositories', 0, 'project'))
      assert_equal(slug, manifest.dig('repositories', 0, 'branch'))
      assert_equal('master', manifest.dig('repositories', 0, 'default_branch'))
      assert_equal(master, manifest.dig('repositories', 0, 'initial_base_sha'))
      refute(manifest.fetch('creation').key?('tracking_origin'))

      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      assert_equal('ready', journal.fetch('state'))
      assert_equal('retained', journal.fetch('tracking_origin'))
      assert_equal(Digest::SHA256.hexdigest(plan), journal.fetch('tracking_plan_sha256'))
      assert_equal(Digest::SHA256.hexdigest(state), journal.fetch('tracking_state_sha256'))
      refute(journal.key?('tracking_portal_sha256'))
    end
  end

  def test_start_refuses_uncommitted_retained_plan_or_state
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-dirty'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      File.write(File.join(workspace, 'work', slug, 'plan.md'), "uncommitted replacement\n")
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

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
      assert_match(/retained plan, state, and portal absence must match/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
    end
  end

  def test_start_refuses_a_deleted_committed_retained_portal
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-deleted-portal'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      File.unlink(File.join(workspace, 'work', slug, 'portal.yml'))
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

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
      assert_match(/retained plan, state, and portal absence must match/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
    end
  end

  def test_start_refuses_retained_tracking_without_codex_before_mutation
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-no-codex'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

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
      assert_match(/restarting retained tracking requires a Codex conversation/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
    end
  end

  def test_start_without_goal_refuses_retained_tracking_without_codex_before_mutation
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-no-goal-no-codex'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          json: true,
          exclusive: true
        )
      end
      assert_match(/restarting retained tracking requires a Codex conversation/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
      refute(runner.instance_variable_get(:@tmux).session(slug))
    end
  end

  def test_start_without_goal_refuses_deleted_committed_portal_without_codex_before_mutation
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-deleted-portal-no-codex'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      File.unlink(File.join(workspace, 'work', slug, 'portal.yml'))

      error = assert_raises(DevSession::Error) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: false,
          json: true,
          exclusive: true
        )
      end
      assert_match(/restarting retained tracking requires a Codex conversation/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'work', slug, 'portal.yml')))
      refute(runner.instance_variable_get(:@tmux).session(slug))
    end
  end

  def test_start_without_goal_allows_a_stopped_ready_session_without_codex
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-ready-no-codex'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(slug)
      setup.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      session = DevSession::Tmux::Session.new(
        id: '$restarted', name: slug, mark: '1', slug:, workspace:
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |expected| expected }
      end
      out = StringIO.new
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {}
      )

      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: false,
        json: true,
        exclusive: false
      )

      result = JSON.parse(out.string)
      assert_equal(slug, result.fetch('slug'))
      assert_nil(result.fetch('threadId'))
      assert_equal(['tmux', 'attach-session', '-t', '$restarted:'], result.fetch('attach'))
    end
  end

  def test_start_refuses_committed_empty_portal_as_retained_tracking
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-empty-portal'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

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
      assert_match(/retained active tracking with a portal manifest cannot be adopted/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
    end
  end

  def test_start_refuses_manifestless_tracking_with_archive_history
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-manually-restored-archive'
      runner = archived_runner(workspace, slug)
      FileUtils.mv(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )
      FileUtils.rm_f(File.join(workspace, 'work', slug, 'portal.yml'))
      set_lifecycle(workspace, slug, 'active')
      assert_git_success(
        'git', '-C', workspace, 'add', '-A', '--',
        File.join('work', slug), File.join('archive', slug)
      )
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'manually restore archive')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")

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
      assert_match(/archived slug must be restored with dev-session revive/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
    end
  end

  def test_retained_conversation_replay_rejects_changed_manifest_provenance
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-manifest-replay'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue this initiative.\n")
      journal = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: journal)
      manifest['creation']['tracking_state_sha256'] = '0' * 64
      runner.send(:write_portal_manifest, slug, manifest)

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
      assert_match(/preserved tracking provenance changed during creation/, error.message)
    end
  end

  def test_portal_sync_leaves_an_unproven_unregistered_worktree_untouched
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-unproven'
      path = File.join(workspace, 'worktrees', slug, 'legacy-clone')
      assert_git_success('git', 'init', '-b', slug, path)
      marker = File.join(path, 'uncommitted.txt')
      File.write(marker, "keep this work\n")
      plain_path = File.join(workspace, 'worktrees', slug, 'plain-directory')
      FileUtils.mkdir_p(plain_path)
      plain_marker = File.join(plain_path, 'keep.txt')
      File.write(plain_marker, "keep this too\n")
      err = StringIO.new
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err:, today: TODAY
      )
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)

      runner.send(
        :sync_portal_repositories,
        slug,
        runner.send(:worktree_entries, slug)
      )

      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_empty(manifest.fetch('repositories'))
      assert_equal("keep this work\n", File.read(marker))
      assert_equal("keep this too\n", File.read(plain_marker))
      assert_match(/did not register unproven worktree/, err.string)
      assert_match(/outside the canonical repository root/, err.string)
      assert_match(/not a canonical attached Git worktree/, err.string)
    end
  end

  def test_portal_sync_leaves_a_canonical_worktree_with_an_unsafe_name_unregistered
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      repository = File.join(workspace, 'repos', 'sample.git')
      slug = '2026-06-06-unsafe-worktree-name'
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
      path = File.join(workspace, 'worktrees', slug, 'legacy checkout')
      FileUtils.mkdir_p(File.dirname(path))
      assert_git_success('git', "--git-dir=#{repository}", 'worktree', 'add', path, slug)
      err = StringIO.new
      runner = DevSession::Runner.new(
        workspace:, tmux: NullTmux.new, out: StringIO.new, err:, today: TODAY
      )
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)

      proven = runner.send(
        :sync_portal_repositories,
        slug,
        runner.send(:worktree_entries, slug)
      )

      assert_empty(proven)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_empty(manifest.fetch('repositories'))
      assert_match(/worktree name is unsafe for a portal manifest/, err.string)
      assert(File.directory?(path))
    end
  end

  def test_portal_sync_rejects_a_registered_non_worktree_path
    with_workspace do |workspace|
      slug = '2026-06-06-registered-non-worktree'
      path = File.join(workspace, 'worktrees', slug, 'broken')
      FileUtils.mkdir_p(path)
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['repositories'] = [{
        'name' => 'broken',
        'project' => 'sample',
        'branch' => slug,
        'default_branch' => 'master'
      }]
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::Error) do
        runner.send(:sync_portal_repositories, slug, [])
      end
      assert_match(/registered portal worktree is not a canonical attached worktree/, error.message)
      assert(File.directory?(path))
    end
  end

  def test_sync_does_not_open_a_window_for_an_unproven_git_directory
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-unproven-window'
      path = File.join(workspace, 'worktrees', slug, 'standalone')
      assert_git_success('git', 'init', '-b', slug, path)
      tmux = WindowRecordingTmux.new(slug, workspace:)
      err = StringIO.new
      runner = DevSession::Runner.new(
        workspace:, tmux:, out: StringIO.new, err:, today: TODAY, env: {}
      )
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug)

      runner.send(:sync_slug, slug, require_session: true)

      assert_empty(tmux.captures)
      assert_match(/did not register unproven worktree/, err.string)
      assert(File.directory?(path))
    end
  end

  def test_fresh_conversation_rejects_terminal_revived_tracking
    %w[complete abandoned].each do |lifecycle|
      with_workspace do |workspace|
        slug = "2026-06-06-#{lifecycle}"
        tracking = File.join(workspace, 'work', slug)
        FileUtils.mkdir_p(tracking)
        FileUtils.mkdir_p(File.join(workspace, 'worktrees', slug))
        plan = "# Retained plan\n\nContinue this work.\n"
        state = "---\nlifecycle: #{lifecycle}\n---\n\n# Retained state\n"
        File.write(File.join(tracking, 'plan.md'), plan)
        File.write(File.join(tracking, 'state.md'), state)
        runner = runner_for(workspace)
        manifest = runner.send(
          :revived_portal_manifest,
          slug,
          plan_sha256: Digest::SHA256.hexdigest(plan),
          state_sha256: Digest::SHA256.hexdigest(state)
        )
        File.write(File.join(tracking, 'portal.yml'), YAML.dump(manifest))
        goal = File.join(workspace, 'goal.txt')
        File.write(goal, "Resume retained work.\n")

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
        assert_match(/cannot create a conversation for a #{lifecycle} initiative/, error.message)
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
      end
    end
  end

  def test_revived_conversation_replay_rejects_changed_tracking
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revived-replay'
      runner = archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      journal = runner.send(:prepare_revive_journal!, slug, 'complete')
      runner.send(:finish_revive_tracking!, slug, journal)
      File.unlink(runner.send(:lifecycle_journal_file, slug, 'revive'))
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume retained work.\n")
      journal = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      assert(journal.fetch('preserve_tracking'))
      File.write(File.join(workspace, 'work', slug, 'plan.md'), "x")

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
      assert_match(/preserved tracking changed before conversation creation/, error.message)
    end
  end

  def test_revived_conversation_rejects_self_asserted_uncommitted_provenance
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-self-asserted-revive'
      runner = archived_runner(workspace, slug)
      File.rename(
        File.join(workspace, 'archive', slug),
        File.join(workspace, 'work', slug)
      )
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', slug))
      plan = "# Replacement plan\n\nThis did not come from the archive.\n"
      state_path = File.join(workspace, 'work', slug, 'state.md')
      state = runner.send(:revived_state_content, File.read(state_path))
      File.write(File.join(workspace, 'work', slug, 'plan.md'), plan)
      File.write(state_path, state)
      manifest = runner.send(
        :revived_portal_manifest,
        slug,
        plan_sha256: Digest::SHA256.hexdigest(plan),
        state_sha256: Digest::SHA256.hexdigest(state)
      )
      File.write(File.join(workspace, 'work', slug, 'portal.yml'), YAML.dump(manifest))
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Resume retained work.\n")

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
      assert_match(/revived tracking provenance cannot be proven/, error.message)
      refute(File.exist?(runner.send(:creation_journal_file, slug)))
    end
  end

  def test_revive_refuses_abandoned_dirty_duplicate_and_live_states
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-abandoned'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'abandoned')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)
      error = assert_raises(DevSession::Error) { runner.revive(slug, as_is: true) }
      assert_includes(error.message, 'without confirmation')
      runner.send(:prepare_revive_journal!, slug, 'abandoned', abandoned_confirmed: true)
      runner.send(:finish_revive_tracking!, slug, runner.send(:load_revive_journal, slug))
      assert_match(/lifecycle: active/, File.read(File.join(workspace, 'work', slug, 'state.md')))
    end

    with_workspace do |workspace|
      slug = '2026-06-06-dirty'
      runner = archived_runner(workspace, slug)
      File.write(File.join(workspace, 'archive', slug, 'state.md'), "\nchanged\n", mode: 'a')
      error = assert_raises(DevSession::Error) do
        runner.send(:prepare_revive_journal!, slug, 'complete')
      end
      assert_includes(error.message, 'archive move must be committed')
    end

    with_workspace do |workspace|
      slug = '2026-06-06-duplicate'
      runner = archived_runner(workspace, slug)
      FileUtils.mkdir_p(File.join(workspace, 'work', slug))
      error = assert_raises(DevSession::Error) do
        runner.send(:prepare_revive_journal!, slug, 'complete')
      end
      assert_includes(error.message, 'active tracking already exists')
    end

    with_workspace do |workspace|
      slug = '2026-06-06-live'
      archived_runner(workspace, slug)
      tmux = ManagedTmux.new(slug, workspace:)
      error = assert_raises(DevSession::Error) do
        runner_for(workspace, tmux:).send(:prepare_revive_journal!, slug, 'complete')
      end
      assert_includes(error.message, 'live tmux session')
    end
  end

  private

  def test_lifecycle_phase_output_uses_the_persisted_phase_name
    Dir.mktmpdir do |workspace|
      out = StringIO.new
      runner = runner_for(workspace, out:)

      runner.send(:announce_lifecycle_phase, 'archive', 'clusters_released')

      assert_equal("archive: clusters released\n", out.string)
    end
  end

end
