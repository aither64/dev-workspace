# frozen_string_literal: true

require 'timeout'
require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  class LifecycleAccessProbe < StandardError; end

  def with_blocked_lifecycle(operation, kind: 'session', mode: File::LOCK_SH)
    with_workspace do |workspace|
      slug = '2026-06-06-lifecycle-access'
      output = StringIO.new
      runner = runner_for(workspace, out: output)
      runner.ensure_tracking_files(slug)
      path = runner.send(:session_lock_file, slug, kind:)
      waiting = Queue.new
      runner.define_singleton_method(:lifecycle_access_waiting) do |value|
        super(value)
        waiting << true
      end
      runner.define_singleton_method(:select_tmux_for_slug!) { |_value| raise LifecycleAccessProbe }
      holder = File.open(path, File::RDWR | File::CREAT, 0o600)
      holder.flock(mode)
      worker = Thread.new do
        case operation
        when 'archive' then runner.archive(slug, as_is: true, explicit: true, operation_id: 'a' * 64)
        when 'delete' then runner.delete(slug, as_is: true, force: false)
        when 'revive' then runner.revive(slug, as_is: true)
        end
      rescue StandardError => error
        error
      end
      Timeout.timeout(5) { waiting.pop }
      yield workspace, slug, runner, holder, worker, output
    ensure
      holder&.flock(File::LOCK_UN)
      holder&.close
      worker&.join(5)
      worker&.kill if worker&.alive?
    end
  end

  def test_explicit_lifecycle_waits_for_creation_shared_and_exclusive_session_locks
    %w[archive revive delete].each do |operation|
      [['creation', File::LOCK_EX], ['session', File::LOCK_SH], ['session', File::LOCK_EX]].each do |kind, mode|
        with_blocked_lifecycle(operation, kind:, mode:) do |workspace, slug, _runner, holder, worker, output|
          assert(worker.alive?, "#{operation} did not wait for #{kind}")
          refute(File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.archive.json")))
          if operation == 'archive'
            assert_includes(output.string, 'Waiting for session access')
            path = Dir.glob(File.join(workspace, '.xdg-state', 'dev-workspaces', 'archive-progress', '*', "#{slug}.json")).fetch(0)
            assert_equal('a' * 64, JSON.parse(File.read(path)).fetch('operation_id'))
          end
          holder.flock(File::LOCK_UN)
          assert_instance_of(LifecycleAccessProbe, Timeout.timeout(5) { worker.value })
        end
      end
    end
  end

  def test_cli_revive_confirmation_waits_for_session_access_before_prompting_once
    [['creation', File::LOCK_EX], ['session', File::LOCK_SH]].each do |kind, mode|
      with_workspace do |workspace|
        slug = '2026-06-06-cli-revive-access'
        runner = runner_for(workspace)
        runner.ensure_tracking_files(slug)
        FileUtils.mkdir_p(File.join(workspace, 'archive'))
        File.rename(File.join(workspace, 'work', slug), File.join(workspace, 'archive', slug))
        waiting, prompts, calls = Queue.new, Queue.new, []
        runner.define_singleton_method(:lifecycle_access_waiting) { |_value| waiting << true }
        runner.define_singleton_method(:revive) { |value, **options| calls << value }
        errors = StringIO.new
        cli = DevSession::CLI.new(['revive', slug, '--as-is'],
          input: SignalingTTYInput.new("yes\n", read: prompts), out: StringIO.new, err: errors)
        cli.define_singleton_method(:runner) { runner }
        holder = File.open(runner.send(:session_lock_file, slug, kind:), File::RDWR | File::CREAT, 0o600)
        holder.flock(mode)
        worker = Thread.new { cli.run }
        Timeout.timeout(5) { waiting.pop }
        assert worker.alive?
        assert prompts.empty?, 'confirmation ran before lifecycle access was obtained'
        holder.flock(File::LOCK_UN)
        assert_equal 0, Timeout.timeout(5) { worker.value }
        assert_equal [slug], calls
        assert_equal 1, errors.string.scan("Revive session #{slug}?").length
      ensure
        holder&.close
        worker&.join(5)
      end
    end
  end

  def test_interrupted_archive_wait_releases_lifecycle_admission_without_a_journal
    with_blocked_lifecycle('archive') do |_workspace, slug, runner, _holder, worker, _output|
      worker.kill
      Timeout.timeout(5) { worker.join }
      assert_nil(runner.instance_variable_get(:@lifecycle_access))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      File.open(runner.send(:session_lock_file, slug, kind: 'creation'), File::RDWR) do |probe|
        assert(probe.flock(File::LOCK_EX | File::LOCK_NB))
      end
    end
  end

  def test_waiting_lifecycle_rejects_a_replaced_tracking_directory
    with_blocked_lifecycle('archive') do |workspace, slug, runner, holder, worker, _output|
      directory = File.join(workspace, 'work', slug)
      File.rename(directory, directory + '.old')
      runner.ensure_tracking_files(slug)
      holder.flock(File::LOCK_UN)
      error = Timeout.timeout(5) { worker.value }
      assert_instance_of(DevSession::Error, error)
      assert_includes(error.message, 'session changed while waiting')
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_waiting_lifecycle_rejects_a_new_conflicting_journal
    with_blocked_lifecycle('archive') do |_workspace, slug, runner, holder, worker, _output|
      path = runner.send(:lifecycle_journal_file, slug, 'delete')
      FileUtils.mkdir_p(File.dirname(path))
      File.write(path, '{}')
      holder.flock(File::LOCK_UN)
      error = Timeout.timeout(5) { worker.value }
      assert_instance_of(DevSession::Error, error)
      assert_includes(error.message, 'unfinished')
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
    end
  end

  def test_waiting_lifecycle_rejects_a_changed_retained_root
    with_blocked_lifecycle('archive') do |workspace, slug, _runner, holder, worker, _output|
      path = File.join(workspace, 'work', slug, 'portal.yml')
      manifest = { 'schema' => 1, 'slug' => slug, 'repositories' => [], 'artifacts' => [] }
      manifest['codex'] = { 'thread_id' => 'different-root', 'socket_path' => '/run/test/codex.sock', 'client_version' => '0.160.0' }
      File.write(path, YAML.dump(manifest))
      holder.flock(File::LOCK_UN)
      error = Timeout.timeout(5) { worker.value }
      assert_instance_of(DevSession::Error, error)
      assert_includes(error.message, 'session changed while waiting')
    end
  end

  def test_lifecycle_reacquisitions_keep_waiting_without_retargeting_restored_tracking
    with_workspace do |workspace|
      slug = '2026-06-06-revive-access'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      waiting = Queue.new
      runner.define_singleton_method(:lifecycle_access_waiting) { |_value| waiting << true }
      holder = worker = nil
      runner.send(:with_lifecycle_access, slug, 'revive', operation_id: nil) do
        runner.send(:with_creation_lock, slug) { runner.send(:with_slug_lock, slug) {} }
        path = runner.send(:session_lock_file, slug)
        File.open(path, File::RDWR) do |holder|
          holder.flock(File::LOCK_SH)
          worker = Thread.new { runner.send(:with_creation_lock, slug) { runner.send(:with_slug_lock, slug) { :resumed } } }
          Timeout.timeout(5) { waiting.pop }
          assert(worker.alive?)
          holder.flock(File::LOCK_UN)
          assert_equal(:resumed, Timeout.timeout(5) { worker.value })
        end
      ensure
        holder&.flock(File::LOCK_UN) unless holder&.closed?
        worker&.kill if worker&.alive?
      end
      assert_nil(runner.instance_variable_get(:@lifecycle_access))
    end
  end

  def test_portal_authorization_is_rechecked_after_waiting
    with_workspace do |workspace|
      slug = '2026-06-06-portal-access'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      receipt = { 'receiptId' => 'b' * 64, 'attempt' => 1, 'targetIdentityVersion' => 2,
                  'options' => { 'journalId' => 'a' * 64, 'targetId' => runner.send(:lifecycle_access_target, slug) } }
      runner.instance_variable_set(:@require_runtime, true)
      runner.define_singleton_method(:portal_lifecycle_receipt) { |*_args| Marshal.load(Marshal.dump(receipt)) }
      runner.define_singleton_method(:select_tmux_for_slug!) { |_value| raise LifecycleAccessProbe }
      waiting = Queue.new
      runner.define_singleton_method(:lifecycle_access_waiting) { |_value| waiting << true }
      holder = worker = nil
      File.open(runner.send(:session_lock_file, slug), File::RDWR | File::CREAT, 0o600) do |holder|
        holder.flock(File::LOCK_SH)
        worker = Thread.new do
          runner.archive(slug, as_is: true, explicit: true, operation_id: 'a' * 64)
        rescue StandardError => error
          error
        end
        Timeout.timeout(5) { waiting.pop }
        receipt['attempt'] = 2
        holder.flock(File::LOCK_UN)
        error = Timeout.timeout(5) { worker.value }
        assert_instance_of(DevSession::Error, error)
        assert_includes(error.message, 'portal lifecycle authorization changed')
      ensure
        holder&.flock(File::LOCK_UN) unless holder&.closed?
        worker&.kill if worker&.alive?
      end
    end
  end

  def test_waiting_archive_rejects_a_replaced_recovery_operation
    with_workspace do |workspace|
      slug = '2026-06-06-recovery-access'
      runner = runner_for(workspace)
      path = runner.send(:lifecycle_journal_file, slug, 'archive')
      FileUtils.mkdir_p(File.dirname(path))
      File.write(path, JSON.generate('operation_id' => 'a' * 64))
      File.chmod(0o600, path)
      waiting = Queue.new
      runner.define_singleton_method(:lifecycle_access_waiting) { |_value| waiting << true }
      File.open(runner.send(:session_lock_file, slug), File::RDWR | File::CREAT, 0o600) do |holder|
        holder.flock(File::LOCK_SH)
        worker = Thread.new do
          runner.archive(slug, as_is: true, explicit: true, operation_id: 'a' * 64)
        rescue StandardError => error
          error
        end
        Timeout.timeout(5) { waiting.pop }
        File.write(path, JSON.generate('operation_id' => 'b' * 64))
        holder.flock(File::LOCK_UN)
        error = Timeout.timeout(5) { worker.value }
        assert_instance_of(DevSession::Error, error)
        assert_includes(error.message, 'another lifecycle operation started')
      end
    end
  end

  def test_automatic_scan_defers_busy_session_without_changing_cached_activity
    with_workspace do |workspace|
      slug = '2026-06-06-busy-observation'
      runner = automatic_fixture(workspace, slug)
      automatic_scan(runner)
      path = File.join(runner.auto_archive_store.root, "session-#{slug}.json")
      before = File.binread(path)
      File.open(runner.send(:session_lock_file, slug), File::RDWR) do |holder|
        holder.flock(File::LOCK_EX)
        result = automatic_scan(runner).fetch(0)
        assert_equal('deferred', result.fetch('result'))
        assert_includes(result.fetch('blockers').join, 'busy')
        assert_equal(before, File.binread(path))
      end
    end
  end
end
