# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  class MonitorArchiveTransport
    attr_reader :calls
    def initialize(test, hard_refusal: false)
      @test, @hard_refusal, @calls, @completes = test, hard_refusal, [], 0
    end
    def capture(argv, env:, pass_fds:)
      flag = ->(name) { argv.fetch(argv.index(name) + 1) }
      file = pass_fds.fetch(0)
      @test.assert_equal('archive', env.fetch('DEV_SESSION_LIFECYCLE_OPERATION'))
      @test.assert_equal(file.fileno.to_s, env.fetch('DEV_SESSION_LIFECYCLE_LOCK_FD'))
      path = env.fetch('DEV_SESSION_LIFECYCLE_LOCK_PATH')
      @test.assert_equal([file.stat.dev, file.stat.ino], [File.stat(path).dev, File.stat(path).ino])
      File.open(path, File::RDWR) { |probe| @test.refute(probe.flock(File::LOCK_SH | File::LOCK_NB)) }
      @test.assert(file.flock(File::LOCK_EX | File::LOCK_NB)) # Never unlock the inherited OFD.
      action = argv.fetch(argv.index('monitor-lifecycle') + 1)
      selected = argv.include?('--archive-operation-id') ? flag.call('--archive-operation-id') : nil
      @calls << [action, selected]
      authority = {'slug' => flag.call('--session-slug'), 'rootThreadId' => flag.call('--root-thread-id'),
                   'operationId' => selected || 'd' * 64, 'receiptId' => flag.call('--receipt-id'),
                   'targetId' => flag.call('--target-id'), 'mode' => flag.call('--archive-mode')}
      @completes += 1 if action == 'complete-archive'
      pending = action == 'complete-archive' && @completes == 1 && !@hard_refusal
      state = @hard_refusal || pending ? 'blocked' : (action == 'begin-archive' ? 'quiesceable' : 'quiescent')
      local = {'state' => state, 'operationIds' => ['1' * 32],
               'reason' => @hard_refusal ? 'Native receipt is unknown.' : 'Waiting for the original joined receipt.'}
      [JSON.generate('authority' => authority, 'local' => local, 'pending' => pending), '', nil]
    end
  end

  def with_monitor_archive_parent(hard_refusal: false, threadless: false)
    with_workspace do |workspace|
      slug = '2026-11-13-monitor-parent'
      runner = archive_runner_for(workspace)
      runner.ensure_tracking_files(slug)
      File.write(File.join(workspace, 'work', slug, 'portal.yml'), YAML.dump(
        'schema' => 1, 'slug' => slug, 'repositories' => [], 'artifacts' => []))
      archive_retained_identity_fixture(workspace, slug) unless threadless
      directory = File.join(runner.instance_variable_get(:@workspace_state_root), 'monitor-operations', Digest::SHA256.hexdigest(workspace))
      FileUtils.mkdir_p(directory)
      transport = MonitorArchiveTransport.new(self, hard_refusal:)
      runner.instance_variable_set(:@portal_command, ['portal-fixture'])
      runner.instance_variable_set(:@command_runner, transport)
      admitted = []
      selected = []
      runner.define_singleton_method(:archive_with_access) do |input, **options|
        admitted << @lifecycle_access.fetch(:admitted)
        with_creation_lock(input) do
          with_slug_lock(input) do
            selected << quiesce_archive_observers!(input, mode: 'archived', operation_id: options[:operation_id])
          end
        end
      end
      yield runner, workspace, slug, transport, admitted, selected
    end
  end

  def test_monitor_pending_releases_both_locks_and_replays_exact_binding
    with_monitor_archive_parent do |runner, _workspace, slug, transport, admitted, selected|
      waits = 0
      runner.define_singleton_method(:sleep) do |delay|
        raise 'unexpected wait' unless delay == 0.1
        %w[creation session].each do |kind|
          File.open(session_lock_file(slug, kind:), File::RDWR) do |probe|
            raise 'wait retained a lifecycle lock' unless probe.flock(File::LOCK_EX | File::LOCK_NB)
          end
        end
        waits += 1
      end
      runner.archive(slug, as_is: true, explicit: true)
      assert_equal(1, waits)
      assert_equal([false, false], admitted)
      assert_equal(['d' * 64], selected)
      assert_equal([['begin-archive', nil], ['complete-archive', 'd' * 64],
                    ['begin-archive', 'd' * 64], ['complete-archive', 'd' * 64]], transport.calls)
    end
  end

  def test_monitor_pending_rechecks_target_after_releasing_locks
    with_monitor_archive_parent do |runner, workspace, slug, transport, _admitted, _selected|
      runner.define_singleton_method(:sleep) do |_delay|
        path = File.join(workspace, 'work', slug)
        File.rename(path, path + '.original')
        FileUtils.cp_r(path + '.original', path)
      end
      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
      assert_includes(error.message, 'session changed while waiting')
      assert_equal(2, transport.calls.length)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_monitor_native_refusal_is_not_a_join_wait
    with_monitor_archive_parent(hard_refusal: true) do |runner, _workspace, slug, transport, _admitted, _selected|
      runner.define_singleton_method(:sleep) { |_| raise 'native refusal waited' }
      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
      assert_includes(error.message, 'Native receipt is unknown')
      assert_equal(2, transport.calls.length)
    end
  end
end
