# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_delete_moves_direct_team_roster_to_private_recovery
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      roster = runner.send(:direct_team_state_path, slug)
      FileUtils.mkdir_p(File.dirname(roster), mode: 0o700)
      File.write(roster, "old-root-team\n")
      File.chmod(0o600, roster)

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(roster), 'the deleted slug must be reusable')
      assert_equal("old-root-team\n", File.read(File.join(removal_recovery(workspace, slug), 'team.json')))
    end
  end

  def test_quiesce_refuses_a_busy_direct_team_member
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        abort 'member has a pending request' if ARGV[0, 2] == ['team', 'require-idle']
      RUBY
      runner = runner_for(workspace, env: {
        DevSession::ENV_PORTAL_COMMAND => [RbConfig.ruby, portal].shelljoin
      })
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex'] = { 'thread_id' => 'root-thread' }
      runner.send(:write_portal_manifest, slug, manifest)
      roster = runner.send(:direct_team_state_path, slug)
      FileUtils.mkdir_p(File.dirname(roster), mode: 0o700)
      File.write(roster, "team\n")

      error = assert_raises(DevSession::CommandError) do
        runner.send(:quiesce_and_require_idle!, slug, nil)
      end
      assert_includes(error.message, 'member has a pending request')
    end
  end

  def test_force_delete_retires_busy_direct_member_after_non_force_retry
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      calls = File.join(workspace, 'portal-calls')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        exit 0 if ARGV[0, 2] == ['uploads', 'remove-session']
        File.open(#{calls.dump}, 'a') { |file| file.puts ARGV[0, 2].join(' ') }
        abort 'member has an active turn' if ARGV[0, 2] == ['team', 'require-idle']
        abort 'ordinary archive must stay idle-only' if ARGV[0, 2] == ['team', 'archive']
        abort 'forced root retirement lacks --force' if ARGV[0, 2] == ['thread', 'retire'] && !ARGV.include?('--force')
      RUBY
      runner = runner_for(workspace, env: {
        DevSession::ENV_PORTAL_COMMAND => [RbConfig.ruby, portal].shelljoin,
        DevSession::ENV_CODEX_SOCKET => '/run/test/codex.sock'
      })
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex'] = {'thread_id' => 'root-thread'}
      runner.send(:write_portal_manifest, slug, manifest)
      roster = runner.send(:direct_team_state_path, slug)
      FileUtils.mkdir_p(File.dirname(roster), mode: 0o700)
      File.write(roster, "team\n")

      assert_raises(DevSession::CommandError) { runner.delete(slug, as_is: true, force: false) }
      runner.delete(slug, as_is: true, force: true)

      assert_equal(['team require-idle', 'team retire', 'thread retire'], File.readlines(calls, chomp: true))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_discovers_a_thread_whose_start_result_was_lost
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'portal.rb')
      File.write(goal, "Create the session.\n")
      File.write(portal, <<~RUBY)
        exit 0 if ARGV[0, 2] == ['uploads', 'remove-session']
        case ARGV[1]
        when 'create'
          warn 'simulated loss after App Server committed thread/start'
          exit 19
        when 'retire'
          File.open(#{log.dump}, 'a') { |file| file.puts ARGV.join(' ') }
          abort 'cwd discovery unexpectedly supplied a thread ID' if ARGV.include?('--thread-id')
        else
          abort "unexpected portal action: \#{ARGV.join(' ')}"
        end
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )

      assert_raises(DevSession::CommandError) do
        runner.start(
          slug, as_is: true, new: false, attach: false, run_codex: true,
          goal_file: goal, json: true, exclusive: true
        )
      end
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_nil(manifest.dig('codex', 'thread_id'))

      runner.delete(slug, as_is: true, force: false)

      call = File.read(log)
      assert_includes(call, "thread retire --cwd #{File.join(workspace, 'work', slug)}")
      assert_includes(call, '--socket /run/test/codex.sock')
      refute_includes(call, '--thread-id')
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_can_upgrade_a_non_force_retirement_retry_to_force
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      calls = File.join(workspace, 'retire-calls')
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        exit 0 if ARGV[0, 2] == ['agent-teams', 'require-unmanaged']
        exit 0 if ARGV[0, 2] == ['uploads', 'remove-session']
        File.open(#{calls.dump}, 'a') { |file| file.puts ARGV.join(' ') }
        unless ARGV.include?('--force')
          warn 'Codex thread still has an active turn'
          exit 19
        end
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: ManagedTmux.new(
          slug,
          workspace:,
          codex_thread_id: 'thread-1',
          codex_socket_path: '/run/test/codex.sock'
        ),
        portal_command: [RbConfig.ruby, portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      runner.ensure_tracking_files(slug)

      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal(false, journal.fetch('force'))
      assert_equal('thread_retiring', journal.fetch('phase'))

      runner.delete(slug, as_is: true, force: true)

      lines = File.readlines(calls, chomp: true)
      refute_includes(lines.fetch(0), '--force')
      assert_includes(lines.fetch(1), '--force')
      recovery = JSON.parse(File.read(File.join(removal_recovery(workspace, slug), 'recovery.json')))
      assert_equal(true, recovery.fetch('force'))
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_tracking
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      state_home = File.join(workspace, 'work', slug, 'private-state')
      runner = runner_for(workspace, env: {'XDG_STATE_HOME' => state_home})
      runner.ensure_tracking_files(slug)

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(state_home))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_refuses_non_directory_state_root_before_recovery_setup
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Create the session.\n")
      creator = runner_for(workspace)
      creator.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      creator.ensure_tracking_files(slug)
      journal = creator.send(:creation_journal_file, slug)
      runner = runner_for(workspace, env: {'XDG_STATE_HOME' => journal})

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

	  assert_includes(error.message, 'session deletion recovery root is inside session state')
	  assert(File.file?(journal))
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_the_creation_journal
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Create the session.\n")
      creator = runner_for(workspace)
      creator.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      creator.ensure_tracking_files(slug)
      journal = creator.send(:creation_journal_file, slug)
      portal = File.join(workspace, 'portal.rb')
      File.write(portal, <<~RUBY)
        exit 0 if ARGV[0, 2] == ['agent-teams', 'require-unmanaged']

        abort "unexpected portal action: \#{ARGV.join(' ')}"
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        portal_command: [RbConfig.ruby, portal],
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => journal}
      )

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.file?(journal))
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_direct_team_state_probe_allows_an_operator_managed_symlinked_parent
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      state_home = File.join(workspace, 'private-state')
      runner = runner_for(workspace, env: {'XDG_STATE_HOME' => state_home})

      refute(runner.send(:direct_team_state_present?, slug))
      target = File.join(workspace, 'untrusted-state')
      FileUtils.mkdir_p(target)
      File.symlink(target, state_home)

      refute(runner.send(:direct_team_state_present?, slug))
    end
  end

  def test_remove_force_rejects_recovery_storage_nested_in_a_worktree
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      slug = '2026-06-06-demo'
      creator = runner_for(workspace)
      creator.worktree_add(
        'demo', 'sample', as_is: false, name: nil, branch: nil,
        base: 'master', fetch: false
      )
      worktree = File.join(workspace, 'worktrees', slug, 'sample')
      state_home = File.join(worktree, 'private-state')
      runner = runner_for(workspace, env: {'XDG_STATE_HOME' => state_home})

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: true)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.directory?(worktree))
      refute(File.exist?(state_home))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_absent_cluster_state
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      cluster = File.join(workspace, '.dev-clusters', 'alpha', 'clusters', slug)
      state_home = File.join(cluster, 'private-state')
      helper = cleanup_contract_helper(workspace, 'alpha', [cluster])
      runner = runner_for(
        workspace,
        env: {'XDG_STATE_HOME' => state_home},
        alpha_cluster: helper
      )
      runner.ensure_tracking_files(slug)

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(state_home))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_cluster_socket
    with_workspace do |workspace|
      {
        'alpha' => 'example-devcluster',
        'beta' => 'beta-devcluster'
      }.each do |kind, prefix|
        slug = "2026-06-06-demo-#{kind}"
        digest = Digest::SHA256.hexdigest("#{workspace}\0#{slug}")[0, 12]
        socket = File.join('/tmp', "#{prefix}-#{digest}")
        state_home = File.join(socket, 'private-state')
        helper = cleanup_contract_helper(workspace, kind, [socket])
        runner = runner_for(
          workspace,
          env: {'XDG_STATE_HOME' => state_home},
          alpha_cluster: kind == 'alpha' ? helper : nil,
          beta_cluster: kind == 'beta' ? helper : nil
        )
        runner.ensure_tracking_files(slug)

        error = assert_raises(DevSession::Error) do
          runner.delete(slug, as_is: true, force: false)
        end

        assert_includes(error.message, 'recovery root is inside session state')
        assert(File.directory?(File.join(workspace, 'work', slug)))
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      end
    end
  end

  def test_remove_rejects_recovery_storage_nested_in_legacy_cluster_socket
    with_workspace do |workspace|
      slug = '2026-08-18-alpha-password-reset'
      digest = Digest::SHA256.hexdigest(slug)[0, 12]
      socket = File.join('/tmp', "example-devcluster-#{digest}")
      state_home = File.join(socket, 'private-removal-state')
      helper = cleanup_contract_helper(workspace, 'alpha', [socket])
      runner = runner_for(
        workspace,
        env: {'XDG_STATE_HOME' => state_home},
        alpha_cluster: helper
      )
      runner.ensure_tracking_files(slug)

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'recovery root is inside session state')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_reconciles_an_archive_before_its_phase_update
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      calls = File.join(workspace, 'retire-calls')
      portal = File.join(workspace, 'portal')
      File.write(portal, <<~RUBY)
        exit 0 if ARGV[0, 2] == ['agent-teams', 'require-unmanaged']
        exit 0 if ARGV[0, 2] == ['uploads', 'remove-session']
        File.open(#{calls.dump}, 'a') { |file| file.puts ARGV.join(' ') }
        abort 'retirement lost its thread identity' unless ARGV.include?('--thread-id')
      RUBY
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        codex_thread_id: 'thread-1',
        codex_socket_path: '/run/test/codex.sock'
      )
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        portal_command: [RbConfig.ruby, portal],
        codex_socket: '/run/test/codex.sock',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      runner.ensure_tracking_files(slug)
      advance = runner.method(:advance_removal!)
      interrupted = false
      runner.define_singleton_method(:advance_removal!) do |current_slug, removal, phase|
        if phase == 'thread_retired' && !interrupted
          interrupted = true
          raise DevSession::Error, 'simulated interruption after thread archive'
        end
        advance.call(current_slug, removal, phase)
      end

      assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('thread_retiring', journal.fetch('phase'))
      assert(tmux.quiesced)

      runner.delete(slug, as_is: true, force: false)

      assert_equal(2, File.readlines(calls).length)
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_refuses_to_orphan_a_known_thread_when_runtime_is_unavailable
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['codex'] = {'thread_id' => 'thread-1'}
      File.write(manifest_path, YAML.dump(manifest))

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'workspace-portal is required')
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('thread_retiring', journal.fetch('phase'))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_retries_after_cluster_release_failure
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
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
        workspace:,
        tmux: ManagedTmux.new(slug, workspace:),
        cluster_providers: { 'alpha' => helper },
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')}
      )
      runner.ensure_tracking_files(slug)

      assert_raises(DevSession::CommandError) do
        runner.delete(slug, as_is: true, force: false)
      end
      journal = JSON.parse(File.read(runner.send(:lifecycle_journal_file, slug, 'delete')))
      assert_equal('thread_retired', journal.fetch('phase'))
      assert(File.directory?(File.join(workspace, 'work', slug)))

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_passes_its_exclusive_session_lock_to_cluster_reset
    with_workspace do |workspace|
      slug = '2026-06-06-cluster-lock-owner'
      cluster = File.join(workspace, '.dev-clusters', 'alpha', 'clusters', slug)
      helper = File.join(workspace, 'alpha-devcluster')
      File.write(helper, <<~SH)
        #!/bin/sh
        if [ "$1" = cleanup-paths ]; then
          printf '%s\n' #{Shellwords.escape(JSON.generate('schema' => 1, 'paths' => [cluster]))}
          exit 0
        fi
        test "$1" = reset
        test "$DEV_SESSION_LIFECYCLE_OPERATION" = delete
        test -n "$DEV_SESSION_LIFECYCLE_LOCK_FD"
        test -n "$DEV_SESSION_LIFECYCLE_LOCK_PATH"
        rm -rf -- #{Shellwords.escape(cluster)}
      SH
      File.chmod(0o755, helper)
      runner = runner_for(
        workspace,
        alpha_cluster: helper
      )
      runner.ensure_tracking_files(slug)
      FileUtils.mkdir_p(cluster)

      runner.delete(slug, as_is: true, force: false)

      refute(File.exist?(cluster))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_preflights_legacy_identity_before_creating_a_journal
    with_workspace do |workspace|
      slug = '2026-06-06-remove-legacy-identity'
      authority_dir = File.join(workspace, 'authority')
      tmux = RefusedIdentityInitializationTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')

      error = assert_raises(DevSession::Error) do
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(tmux.identity_initialization_attempted)
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_rejects_a_renamed_authority_session_before_creating_a_journal
    with_workspace do |workspace|
      slug = '2026-06-06-remove-renamed-session'
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
        runner.delete(slug, as_is: true, force: false)
      end

      assert_includes(error.message, 'does not match trusted authority')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
    end
  end

  def test_remove_resume_upgrades_a_legacy_identity_before_continuing
    with_workspace do |workspace|
      slug = '2026-06-06-remove-resume-legacy'
      authority_dir = File.join(workspace, 'authority')
      tmux = ManagedTmux.new(
        slug, workspace:, socket_path: '/run/test.sock', id: '$11',
        identity_token: nil
      )
      runner = runner_for(workspace, tmux:, authority_dir:)
      runner.ensure_tracking_files(slug)
      runner.send(:write_session_authority, slug, tmux.session(slug), state: 'ready')
      runner.send(:prepare_removal!, slug, force: false)

      runner.delete(slug, as_is: true, force: false)

      assert(tmux.killed)
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'delete')))
      refute(File.exist?(File.join(authority_dir, "#{slug}.json")))
      refute(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_removal_journal_is_bound_to_the_portal_operation_identity
    with_workspace do |workspace|
      slug = '2026-06-06-remove-operation-identity'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      operation_id = 'a' * 64

      removal = runner.send(
        :prepare_removal!, slug, force: false, operation_id:
      )
      assert_equal(operation_id, removal.fetch('operation_id'))
      assert_equal(
        operation_id,
        runner.send(:load_removal_journal, slug).fetch('operation_id')
      )

      error = assert_raises(DevSession::Error) do
        runner.send(
          :prepare_removal!, slug, force: false, operation_id: 'b' * 64
        )
      end
      assert_includes(error.message, 'session deletion operation changed')
    end
  end

  def test_archive_journal_is_bound_to_the_portal_operation_identity
    with_workspace do |workspace|
      slug = '2026-06-06-archive-operation-identity'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      operation_id = 'a' * 64
      plan = runner.send(:prepare_cleanup, slug, force: false)

      journal = runner.send(
        :prepare_archive_journal!,
        slug,
        'complete',
        {},
        plan,
        operation_id:
      )
      assert_equal(operation_id, journal.fetch('operation_id'))
      assert_equal(
        operation_id,
        runner.send(:load_archive_journal, slug).fetch('operation_id')
      )

      error = assert_raises(DevSession::Error) do
        runner.archive(
          slug,
          as_is: true,
          operation_id: 'b' * 64
        )
      end
      assert_includes(error.message, 'session archive operation changed')
    end
  end

  def test_revive_journal_is_bound_to_the_portal_operation_identity
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-revive-operation-identity'
      runner = archived_runner(workspace, slug)
      configure_workspace_origin(workspace)
      operation_id = 'a' * 64

      journal = runner.send(
        :prepare_revive_journal!,
        slug,
        'complete',
        operation_id:
      )
      assert_equal(operation_id, journal.fetch('operation_id'))
      assert_equal(
        operation_id,
        runner.send(:load_revive_journal, slug).fetch('operation_id')
      )

      error = assert_raises(DevSession::Error) do
        runner.revive(
          slug,
          as_is: true,
          operation_id: 'b' * 64
        )
      end
      assert_includes(error.message, 'session revive operation changed')
    end
  end

end
