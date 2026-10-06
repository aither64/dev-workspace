# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_automatic_observation_forwards_selected_tmux_socket_to_child
    with_workspace do |workspace|
      slug = '2026-06-06-selected-tmux'
      socket = File.join(workspace, 'selected-tmux.sock')
      portal = File.join(workspace, 'socket-observer.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        abort 'selected tmux socket is missing or wrong' unless ENV['DEV_SESSION_TMUX_SOCKET'] == #{socket.inspect}
        puts JSON.generate('schema' => 1, 'workspace' => #{workspace.inspect}, 'slug' => #{slug.inspect},
          'identity' => 'fixture-identity', 'activityKnown' => true, 'activityToken' => 'a' * 64,
          'lastActivityAt' => nil, 'idle' => true, 'subjects' => [], 'diagnostics' => [])
      RUBY
      invocation = <<~'RUBY'
        require 'json'
        load ARGV.shift
        workspace, socket, slug = ARGV
        runner = DevSession::Runner.new(workspace: workspace, tmux_socket: socket.empty? ? nil : socket)
        activity = runner.send(:auto_archive_session_observation, slug, 'fixture-identity')
        puts JSON.generate('activity' => activity, 'ambient_socket' => ENV['DEV_SESSION_TMUX_SOCKET'])
      RUBY
      environment = {
        'XDG_STATE_HOME' => File.join(workspace, '.xdg-state'),
        'DEV_WORKSPACES_STATE' => File.join(workspace, '.xdg-state', 'dev-workspaces'),
        DevSession::ENV_REQUIRE_RUNTIME => '0',
        DevSession::ENV_PORTAL_COMMAND => [RbConfig.ruby, portal].shelljoin,
        DevSession::ENV_CODEX_SOCKET => '/run/test/codex.sock',
        DevSession::ENV_AUTHORITY_DIR => File.join(workspace, '.authority'),
        'DEV_WORKSPACE_CODEX_HOME' => File.join(workspace, '.codex')
      }
      [nil, File.join(workspace, 'ambient-tmux.sock')].each do |ambient|
        environment[DevSession::ENV_TMUX_SOCKET] = ambient
        output, error, status = Open3.capture3(environment, RbConfig.ruby, '-e', invocation,
          File.expand_path('../../libexec/dev-session', __dir__), workspace, socket, slug)
        assert(status.success?, error)
        result = JSON.parse(output)
        assert(result.fetch('activity').fetch('activityKnown'))
        assert(result.fetch('activity').fetch('idle'))
        ambient ? assert_equal(ambient, result.fetch('ambient_socket')) : assert_nil(result.fetch('ambient_socket'))
      end
      environment[DevSession::ENV_TMUX_SOCKET] = nil
      _output, error, status = Open3.capture3(environment, RbConfig.ruby, '-e', invocation,
        File.expand_path('../../libexec/dev-session', __dir__), workspace, '', slug)
      refute(status.success?)
      assert_includes(error, 'Conversation activity cannot be verified.')
    end
  end

  def test_workspace_auto_archive_status_is_cached_sorted_and_isolates_legacy_and_moved_rows
    with_workspace do |workspace|
      slug = '2026-06-06-cached'
      runner = automatic_fixture(workspace, slug)
      automatic_scan(runner)
      runner.auto_archive_hold(slug, true, as_is: true)
      broken = File.join(workspace, 'work', '2026-06-06-broken')
      FileUtils.mkdir_p(broken)
      File.write(File.join(broken, 'state.md'), 'legacy prose only')
      moved = '2026-06-06-moved'
      runner.ensure_tracking_files(moved)
      File.write(File.join(workspace, 'work', moved, 'portal.yml'), YAML.dump(
        'schema' => 1, 'slug' => moved, 'repositories' => [], 'artifacts' => [],
        'finalized_at' => Time.now.utc.iso8601
      ))
      set_lifecycle(workspace, moved, 'complete')
      FileUtils.mkdir_p(File.join(workspace, 'archive'))
      FileUtils.mv(File.join(workspace, 'work', moved), File.join(workspace, 'archive', moved))
      runner.auto_archive_store.write("session-#{moved}", 'slug' => moved, 'hold' => false,
        'operation' => { 'id' => 'a' * 64, 'mode' => 'complete', 'tier' => 'complete', 'identity' => 'pending' })
      cached = Dir.glob(File.join(runner.auto_archive_store.root, '*.json')).to_h { |path| [path, File.binread(path)] }
      refs = git_capture_success('git', '-C', workspace, 'for-each-ref', '--format=%(refname):%(objectname)')

      result = runner.auto_archive_workspace_status

      assert_equal(1, result.fetch('schema'))
      assert_equal(workspace, result.fetch('workspace'))
      assert_equal([File.basename(broken), slug, moved], result.fetch('sessions').map { |row| row.fetch('slug') })
      assert(result.fetch('sessions').first.fetch('repair_needed'))
      assert_equal('legacy_format', result.fetch('sessions').first.fetch('diagnostics').first.fetch('category'))
      assert(result.fetch('sessions')[1].fetch('hold'))
      assert(result.fetch('sessions').last.fetch('operation'))
      refute(result.fetch('sessions').last.fetch('eligible'))
      assert_equal(cached, cached.keys.to_h { |path| [path, File.binread(path)] })
      assert_equal(refs, git_capture_success('git', '-C', workspace, 'for-each-ref', '--format=%(refname):%(objectname)'))
    end
  end
  def test_automatic_archive_inventory_preserves_undated_tracking_and_ignores_unrelated_folders
    with_workspace do |workspace|
      runner = automatic_fixture(workspace, 'undated-session')
      FileUtils.mkdir_p(File.join(workspace, 'work', 'logs'))
      File.write(File.join(workspace, 'work', 'logs', 'output.log'), 'not session tracking')
      FileUtils.mkdir_p(File.join(workspace, 'work', 'vpsadmin'))
      File.write(File.join(workspace, 'work', 'vpsadmin', 'state.md'), 'unresolved legacy state')
      FileUtils.mkdir_p(File.join(workspace, 'work', 'pending-creation'))
      lock_root = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(lock_root)
      File.write(File.join(lock_root, 'pending-creation.creation.json'), 'malformed pending evidence')
      assert_equal(%w[pending-creation undated-session vpsadmin], runner.auto_archive_session_slugs.sort)
      rows = runner.auto_archive_workspace_status.fetch('sessions')
      assert_equal(%w[pending-creation undated-session vpsadmin], rows.map { |row| row.fetch('slug') })
      refute(rows.any? { |row| row.fetch('slug') == 'logs' })
      assert(rows.find { |row| row.fetch('slug') == 'vpsadmin' }.fetch('repair_needed'))
      scanned = runner.auto_archive_scan(dry_run: true, transition: ->(&block) { block.call })
      assert_equal(%w[pending-creation undated-session vpsadmin], scanned.map { |row| row.fetch('slug') })
    end
  end

  def subprocess_can_lock?(path, mode)
    _output, _error, status = Open3.capture3(
      RbConfig.ruby, '-e',
      'File.open(ARGV[0], File::RDWR) { |file| exit(file.flock(ARGV[1].to_i | File::LOCK_NB) ? 0 : 2) }',
      path, mode.to_s
    )
    status.success?
  end

  def test_automatic_observation_shares_session_lock_without_mutation_authority
    with_workspace do |workspace|
      slug = '2026-06-06-shared-observation'
      runner = automatic_fixture(workspace, slug)
      path = runner.send(:session_lock_file, slug)
      runner.send(:with_slug_observation_lock, slug) do
        assert(subprocess_can_lock?(path, File::LOCK_SH))
        refute(subprocess_can_lock?(path, File::LOCK_EX))
        assert_nil(runner.instance_variable_get(:@held_slug_lock))
        assert_raises(DevSession::Error) do
          runner.send(:release_development_clusters!, slug, operation: 'archive')
        end
      end
      runner.send(:with_slug_lock, slug) do
        refute(subprocess_can_lock?(path, File::LOCK_SH))
        refute(subprocess_can_lock?(path, File::LOCK_EX))
      end
    end
  end

  def test_automatic_observation_uses_shared_transition_lock
    Dir.mktmpdir('automatic-transition-observation') do |directory|
      path = File.join(directory, 'transition.lock')
      command = Object.new
      test = self
      command.define_singleton_method(:auto_archive_scan) do |dry_run:, transition:, observation:|
        observation.call do
          test.assert(test.subprocess_can_lock?(path, File::LOCK_SH))
          test.refute(test.subprocess_can_lock?(path, File::LOCK_EX))
        end
        []
      end
      cli = DevSession::CLI.new(
        ['--transition-lock', path, 'auto-archive', 'scan', '--dry-run', '--json'],
        out: StringIO.new, err: StringIO.new
      )
      cli.define_singleton_method(:runner) { command }
      assert_equal(0, cli.run)
    end
  end

  def test_automatic_archive_uses_completed_and_empty_session_modes
    { 'complete' => [86_400, 'complete'], 'active' => [1_209_600, 'abandoned'] }.each do |lifecycle, (period, outcome)|
      with_workspace do |workspace|
        slug = '2026-06-06-automatic'
        runner = automatic_fixture(workspace, slug, lifecycle:)
        first = automatic_scan(runner).fetch(0)
        assert_empty(first['blockers'])
        refute(first['eligible'])
        age_automatic_session(runner, slug, period + 1)
        File.write(File.join(workspace, 'unrelated.txt'), 'preserve')
        result = automatic_scan(runner).fetch(0)
        assert_equal('archived', result['result'], result.inspect)
        assert_equal(outcome, result['archive_mode'])
        assert_includes(File.read(File.join(workspace, 'archive', slug, 'state.md')), "lifecycle: #{outcome}")
        assert_equal('preserve', File.read(File.join(workspace, 'unrelated.txt')))
        assert_empty(automatic_scan(runner))
      end
    end
  end

  def test_automatic_dry_run_does_not_update_sidecars_or_archive
    with_workspace do |workspace|
      slug = '2026-06-06-dry-run'
      runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
      automatic_scan(runner)
      age_automatic_session(runner, slug, 86_401)
      before = Dir.glob(File.join(runner.auto_archive_store.root, '*.json')).to_h { |path| [path, File.read(path)] }
      assert(automatic_scan(runner, dry_run: true).fetch(0)['eligible'])
      assert(File.directory?(File.join(workspace, 'work', slug)))
      assert_equal(before, before.keys.to_h { |path| [path, File.read(path)] })
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_automatic_cli_archive_emits_one_json_value
    with_workspace do |workspace|
      slug = '2026-06-06-json'
      out = StringIO.new
      runner = automatic_fixture(workspace, slug, lifecycle: 'complete', out:)
      automatic_scan(runner)
      age_automatic_session(runner, slug, 86_401)
      out.truncate(0)
      out.rewind
      cli = DevSession::CLI.new(['auto-archive', 'scan', '--json'], out:, err: StringIO.new)
      cli.define_singleton_method(:runner) { runner }
      assert_equal(0, cli.run)
      assert_equal('archived', JSON.parse(out.string).fetch(0)['result'])
      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_automatic_activity_reader_failure_restarts_the_period
    [['complete', 86_400], ['active', 604_800], ['active', 1_209_600]].each do |lifecycle, period|
      with_workspace do |workspace|
        slug = '2026-06-06-observation-outage'
        runner = automatic_fixture(workspace, slug, lifecycle:)
        if period == 604_800
          create_bare_repo(workspace, 'sample')
          runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false)
          merge_registered_branches(workspace, slug)
        end
        automatic_scan(runner)
        age_automatic_session(runner, slug, period + 1)
        portal = File.join(workspace, 'automatic-portal.rb')
        original = File.read(portal)
        File.write(portal, "abort 'Activity reader unavailable'\n")
        failed = automatic_scan(runner).fetch(0)
        assert_equal('deferred', failed['result'])
        File.write(portal, original)
        recovered = automatic_scan(runner).fetch(0)
        refute(recovered['eligible'])
        assert_empty(recovered['blockers'])
        assert(File.directory?(File.join(workspace, 'work', slug)))
      end
    end
  end

  def test_automatic_observation_rejects_missing_or_inconsistent_fields
    with_workspace do |workspace|
      slug = '2026-06-06-invalid-observation'
      runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
      automatic_scan(runner)
      portal = File.join(workspace, 'automatic-portal.rb')
      original = File.read(portal)
      path = File.join(workspace, 'work', slug)
      stat = File.lstat(path)
      valid = {
        'schema' => 1, 'workspace' => workspace, 'slug' => slug,
        'identity' => Digest::SHA256.hexdigest(JSON.generate(['session-observation-v1', workspace, slug, stat.dev, stat.ino, "thread-#{slug}"])),
        'activityKnown' => true, 'activityToken' => 'a' * 64, 'lastActivityAt' => Time.now.to_i,
        'idle' => true, 'subjects' => [], 'diagnostics' => []
      }
      [
        valid.except('idle'),
        valid.merge('idle' => false),
        valid.merge('diagnostics' => [{ 'code' => 'queued_input', 'category' => 'busy', 'message' => 'Queued message.' }]),
        valid.merge('diagnostics' => [{ 'code' => 'queued_input', 'category' => 'busy', 'message' => 'x' * 241 }]),
        valid.merge('lastActivityAt' => Time.now.to_i + 120),
        valid.merge('workspace' => '/foreign'),
      ].each do |response|
        age_automatic_session(runner, slug, 86_401)
        File.write(portal, "puts #{JSON.generate(response).inspect}\n")
        result = automatic_scan(runner).fetch(0)
        assert_equal('deferred', result['result'])
        refute(result['eligible'])
        assert(runner.auto_archive_store.session(slug)['continuity_lost'])
      end
      File.write(portal, original)
      recovered = automatic_scan(runner).fetch(0)
      refute(recovered['eligible'])
      assert_empty(recovered['blockers'])
    end
  end

  def test_automatic_hold_and_results_belong_to_the_conversation_identity
    [true, false].each do |held|
      with_workspace do |workspace|
        slug = '2026-06-06-reused'
        runner = automatic_fixture(workspace, slug)
        # Holds set before the first scan must already identify the conversation.
        runner.auto_archive_hold(slug, held, as_is: true)
        first = automatic_scan(runner).fetch(0)
        assert_equal(held, first['hold'])
        runner.auto_archive_reset(slug)
        assert_equal(held, runner.auto_archive_status(slug)['hold'])
        store = runner.auto_archive_store
        store.write("session-#{slug}", store.session(slug).merge('result' => 'archived', 'archive_mode' => 'complete'))
        manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
        manifest = YAML.safe_load(File.read(manifest_path))
        runner.delete(slug, as_is: true, force: false)
        runner.ensure_tracking_files(slug)
        manifest['codex']['thread_id'] = 'replacement-thread'
        File.write(manifest_path, YAML.dump(manifest))
        current = runner.auto_archive_status(slug)
        refute(current['hold'])
        assert_nil(current['result'])
        assert_nil(current['archive_mode'])
        observed = automatic_scan(runner).fetch(0)
        assert_equal('replacement-thread', observed['root_thread_id'])
        refute_equal(first['identity'], observed['identity'])
        refute(observed['hold'])
        refute(observed['eligible'])
        runner.auto_archive_hold(slug, true, as_is: true)
        assert(runner.auto_archive_status(slug)['hold'])
      end
    end
  end

  def test_automatic_archive_completes_the_merged_branch_tier
    with_workspace do |workspace|
      slug = '2026-06-06-merged'
      runner = automatic_fixture(workspace, slug)
      create_bare_repo(workspace, 'sample')
      runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false)
      merge_registered_branches(workspace, slug)
      assert_equal('merged', automatic_scan(runner).fetch(0)['tier'])
      age_automatic_session(runner, slug, 604_801)
      result = automatic_scan(runner).fetch(0)
      assert_equal('archived', result['result'], result.inspect)
      assert_equal('complete', result['archive_mode'])
      refute(File.exist?(File.join(workspace, 'worktrees', slug, 'sample')))
      assert_git_success('git', "--git-dir=#{workspace}/repos/sample.git", 'show-ref', '--verify', "refs/heads/#{slug}")
    end
  end

  def test_automatic_snapshot_ignores_touches_and_resets_after_content_or_queue_changes
    with_workspace do |workspace|
      slug = '2026-06-06-activity'
      runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
      first = automatic_scan(runner).fetch(0)
      path = File.join(workspace, 'work', slug, 'plan.md')
      File.utime(Time.now + 100, Time.now + 100, path)
      assert_equal(first['fingerprint'], automatic_scan(runner).fetch(0)['fingerprint'])
      age_automatic_session(runner, slug, 86_401)
      File.write(path, File.read(path) + "\nMore work.\n")
      changed = automatic_scan(runner).fetch(0)
      refute(changed['eligible'])
      refute_equal(first['fingerprint'], changed['fingerprint'])
      busy = File.join(workspace, 'work', slug, 'busy')
      File.write(busy, 'queued')
      age_automatic_session(runner, slug, 86_401)
      assert_includes(automatic_scan(runner).fetch(0)['blockers'].join, 'queued message')
      File.unlink(busy)
      resumed = automatic_scan(runner).fetch(0)
      assert_empty(resumed['blockers'])
      refute(resumed['eligible'])
    end
  end

  def test_automatic_hold_is_rechecked_before_preparing_the_archive
    with_workspace do |workspace|
      slug = '2026-06-06-hold-race'
      runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
      automatic_scan(runner)
      age_automatic_session(runner, slug, 86_401)
      original = runner.method(:archive)
      runner.define_singleton_method(:archive) do |input, **keywords|
        auto_archive_hold(input, true, as_is: true)
        original.call(input, **keywords)
      end
      result = automatic_scan(runner).fetch(0)
      assert_equal('deferred', result['result'])
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      assert_nil(runner.auto_archive_store.session(slug)['operation'])
      runner.auto_archive_hold(slug, false, as_is: true)
      refute(automatic_scan(runner, dry_run: true).fetch(0)['eligible'])
    end
  end

  def test_automatic_archive_rechecks_activity_and_worktrees_before_journal
    {
      'activity' => ->(workspace, slug) { File.write(File.join(workspace, 'work', slug, 'busy'), 'queued') },
      'worktree' => lambda do |workspace, slug|
        directory = File.join(workspace, 'worktrees', slug, 'unexpected')
        FileUtils.mkdir_p(directory)
        File.write(File.join(directory, 'unmanaged.txt'), 'new unmanaged content')
      end
    }.each do |change, mutate|
      with_workspace do |workspace|
        slug = "2026-06-06-#{change}-race"
        runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
        automatic_scan(runner)
        age_automatic_session(runner, slug, 86_401)
        original = runner.method(:archive)
        runner.define_singleton_method(:archive) do |input, **keywords|
          mutate.call(workspace, slug)
          original.call(input, **keywords)
        end
        result = automatic_scan(runner).fetch(0)
        assert_equal('deferred', result['result'])
        assert(File.directory?(File.join(workspace, 'work', slug)))
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      end
    end
  end

  def test_automatic_archive_checks_team_idle_before_journal
    with_workspace do |workspace|
      slug = '2026-06-06-member-race'
      runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
      automatic_scan(runner)
      age_automatic_session(runner, slug, 86_401)
      runner.define_singleton_method(:sync_team_lifecycle!) do |_slug, action|
        raise DevSession::Error, 'member is active' if action == 'require-idle'
      end
      result = automatic_scan(runner).fetch(0)
      assert_equal('deferred', result['result'])
      assert_includes(result['blockers'].join, 'member is active')
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_automatic_archive_rechecks_branch_head_before_journal
    with_workspace do |workspace|
      slug = '2026-06-06-head-race'
      runner = automatic_fixture(workspace, slug)
      create_bare_repo(workspace, 'sample')
      runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false)
      merge_registered_branches(workspace, slug)
      automatic_scan(runner)
      age_automatic_session(runner, slug, 604_801)
      path = File.join(workspace, 'worktrees', slug, 'sample')
      configure_git_identity(path)
      previous_head = git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip
      original = runner.method(:archive)
      runner.define_singleton_method(:archive) do |input, **keywords|
        File.write(File.join(path, 'new-work.txt'), 'changed head')
        system('git', '-C', path, 'add', 'new-work.txt', exception: true)
        system('git', '-C', path, 'commit', '-m', 'new work', exception: true, out: File::NULL)
        original.call(input, **keywords)
      end
      result = automatic_scan(runner).fetch(0)
      refute_equal(previous_head, git_capture_success('git', '-C', path, 'rev-parse', 'HEAD').strip,
                   'archive race callback must commit a new feature HEAD')
      assert_equal('deferred', result['result'])
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_automatic_archive_resumes_its_exact_journal_after_disable
    with_workspace do |workspace|
      slug = '2026-06-06-automatic-retry'
      runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
      automatic_scan(runner)
      age_automatic_session(runner, slug, 86_401)
      hook = File.join(workspace, '.git/hooks/pre-commit')
      File.write(hook, "#!/bin/sh\nexit 1\n")
      File.chmod(0o755, hook)
      assert_equal('deferred', automatic_scan(runner).fetch(0)['result'])
      journal = runner.send(:lifecycle_journal_file, slug, 'archive')
      assert_equal('tracking_archived', JSON.parse(File.read(journal))['phase'])
      receipt = runner.auto_archive_store.session(slug)['operation']
      assert_equal(receipt['id'], JSON.parse(File.read(journal))['operation_id'])
      runner.auto_archive_configure(false)
      File.unlink(hook)
      assert_equal('archived', automatic_scan(runner).fetch(0)['result'])
      refute(File.exist?(journal))
    end
  end

  def test_automatic_archive_keeps_registered_branches_after_worktree_removal
    with_workspace do |workspace|
      slug = '2026-06-06-retained'
      runner = automatic_fixture(workspace, slug)
      create_bare_repo(workspace, 'sample')
      runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false)
      path = File.join(workspace, 'worktrees', slug, 'sample')
      assert_git_success('git', '-C', path, 'worktree', 'remove', path)
      first = automatic_scan(runner).fetch(0)
      assert_equal('merged', first['tier'])
      age_automatic_session(runner, slug, 1_209_601)
      result = automatic_scan(runner).fetch(0)
      refute(result['eligible'])
      assert_includes(result['blockers'].join, 'cannot be completed')
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_retirement_uses_its_own_deadline_and_restores_the_scan_deadline
    assert_equal(210, DevSession::THREAD_RETIRE_TIMEOUT)
    [0.5, 3].each do |retirement_delay|
      with_workspace do |workspace|
        slug = '2026-06-06-retirement-deadline'
        runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
        portal = File.join(workspace, 'automatic-portal.rb')
        File.write(portal, "sleep #{retirement_delay} if ARGV[1] == 'retire'\n")
        command_runner = runner.instance_variable_get(:@command_runner)
        # Scale deadlines, not subprocess behavior: use the real timeout command.
        real_with_timeout = command_runner.method(:with_timeout)
        seen = []
        command_runner.define_singleton_method(:with_timeout) do |seconds, &block|
          seen << seconds
          real_with_timeout.call(seconds == DevSession::THREAD_RETIRE_TIMEOUT ? 1 : 0.1, &block)
        end
        command_runner.with_timeout(60) do
          if retirement_delay < 1
            runner.send(:retire_portal_thread!, slug, force: false)
          else
            error = assert_raises(DevSession::CommandError) do
              runner.send(:retire_portal_thread!, slug, force: false)
            end
            assert_equal(124, error.status.exitstatus)
          end
          error = assert_raises(DevSession::CommandError) do
            command_runner.capture([RbConfig.ruby, '-e', 'sleep 0.5'])
          end
          assert_equal(124, error.status.exitstatus, 'ordinary command deadline was not restored')
        end
        assert_equal([60, 210], seen)
        command_runner.capture([RbConfig.ruby, '-e', 'sleep 0.2'])
      end
    end
  end

  def test_automatic_retirement_failure_resumes_committed_tracking_without_another_commit
    with_workspace do |workspace|
      slug = '2026-06-06-retirement-retry'
      runner = automatic_fixture(workspace, slug, lifecycle: 'complete')
      automatic_scan(runner)
      age_automatic_session(runner, slug, 86_401)
      portal = File.join(workspace, 'automatic-portal.rb')
      original = File.read(portal)
      File.write(portal, original + "\nif ARGV[1] == 'retire'; warn 'find session conversation: context deadline exceeded'; exit 1; end\n")
      failure = automatic_scan(runner).fetch(0)
      assert_equal('deferred', failure['result'])
      assert_includes(failure['blockers'].join, 'find session conversation: context deadline exceeded')
      journal = runner.send(:lifecycle_journal_file, slug, 'archive')
      assert_equal('tracking_committed', JSON.parse(File.read(journal))['phase'])
      head = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD')
      pending = runner.auto_archive_status(slug)
      refute(pending['repair_needed'])
      assert_equal('tracking_committed', pending.fetch('journal').fetch('phase'))
      assert(pending.fetch('diagnostics').any? { |entry| entry['code'] == 'archive_operation_pending' })
      refute(pending.fetch('diagnostics').any? { |entry| entry['category'] == 'legacy_format' })
      assert_equal(head, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD'))
      File.write(portal, original)
      assert_equal('archived', automatic_scan(runner).fetch(0)['result'])
      assert_equal(head, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD'))
      refute(File.exist?(journal))
      assert_empty(runner.auto_archive_status(slug)['blockers'])
    end
  end

  class TTYInput < StringIO
    def tty?
      true
    end
  end

  class SignalingTTYInput < TTYInput
    def initialize(value, read:)
      super(value)
      @read = read
    end

    def gets(*arguments)
      value = super
      @read << true
      value
    end
  end

  class LockingCLI < DevSession::CLI
    def initialize(*args, entered:, release:, **options)
      super(*args, **options)
      @entered = entered
      @release = release
    end

    private

    def run_command(_command)
      @entered << true
      @release.pop
    end
  end

end
