# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
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
        assert_equal('replacement-thread', observed['identity'])
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
