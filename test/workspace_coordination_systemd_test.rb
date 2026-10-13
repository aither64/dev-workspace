# frozen_string_literal: true

# Run explicitly from a user manager after review:
# ruby test/workspace_coordination_systemd_test.rb -n /systemd/
# All ledger, Git and package fixtures remain in this test's private directory.
require_relative 'workspace_coordination_test'

class WorkspaceCoordinationTest
  def wait_for_fixture(path)
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 20
    until File.file?(path)
      raise "fixture did not start: #{path}" if Process.clock_gettime(Process::CLOCK_MONOTONIC) > deadline
      sleep 0.05
    end
  end

  def test_systemd_worker_survives_scheduler_exit_and_stalled_recovery_allows_other_workspace
    systemctl = lambda do |*args|
      out, error, status = Open3.capture3('systemctl', '--user', *args)
      raise error unless status.success?
      out
    end
    systemctl.call('show-environment')
    first = owner('systemd-deployer')
    submitted = submit_merge(first)
    claimed = @engine.claim(owner_id: first['id']).first
    hold = File.join(@tmp, 'hold-worker')
    started = File.join(@tmp, 'worker-started')
    recovery_hold = File.join(@tmp, 'hold-recovery')
    recovery_started = File.join(@tmp, 'recovery-started')
    source = File.expand_path('../libexec/workspace-coordination-execution', __dir__)
    wrapper = File.join(@package, 'bin/workspace-host')
    FileUtils.mkdir_p(File.dirname(wrapper))
    File.write(wrapper, <<~SCRIPT)
      #!#{RbConfig.ruby}
      require #{source.inspect}
      if ARGV.first == 'coordination-worker'
        WorkspaceCoordination::Worker.class_eval do
          alias_method :fixture_merge, :merge
          def merge
            File.write(#{started.inspect}, Process.pid.to_s)
            sleep 0.05 while File.exist?(#{hold.inspect})
            fixture_merge
          end
        end
        WorkspaceCoordination::Worker.new(ARGV.last, fixtures: true).run
      elsif ARGV.first == 'coordination-reconcile'
        WorkspaceCoordination::Engine.class_eval do
          alias_method :fixture_remote_head, :remote_head
          def remote_head(*args)
            File.write(#{recovery_started.inspect}, Process.pid.to_s)
            sleep 0.05 while File.exist?(#{recovery_hold.inspect})
            fixture_remote_head(*args)
          end
        end
        WorkspaceCoordination::Worker.new(ARGV.last, fixtures: true).reconcile
      end
    SCRIPT
    File.chmod(0o700, wrapper)
    File.write(hold, 'hold mutation before intent')
    scheduler = fork { @engine.dispatch(claimed); exit! 0 }
    Process.wait(scheduler)
    assert $?.success?
    wait_for_fixture(started)
    unit = "workspace-operation-#{claimed['attempt']}.service"
    assert_equal 'active', systemctl.call('show', unit, '--property=ActiveState', '--value').strip
    assert Process.kill(0, Integer(File.read(started)))
    systemctl.call('stop', unit)
    @engine.reconcile_attempts
    assert_equal 'reconciling', @engine.store.read['jobs'][submitted['id']]['state']
    File.write(recovery_hold, 'stalled read probe')
    @engine.dispatch_reconciliation(claimed)
    wait_for_fixture(recovery_started)
    recovery_unit = "workspace-reconcile-#{claimed['attempt']}.service"
    assert_equal 'active', systemctl.call('show', recovery_unit, '--property=ActiveState', '--value').strip
    # A duplicate scheduler pass cannot launch another reconciler.
    recovery_pid = systemctl.call('show', recovery_unit, '--property=MainPID', '--value')
    @engine.dispatch_reconciliation(claimed)
    assert_equal recovery_pid, systemctl.call('show', recovery_unit, '--property=MainPID', '--value')

    configure_machine
    @workspace = File.join(@tmp, 'healthy-workspace')
    FileUtils.mkdir_p(@workspace)
    @engine = engine
    second = owner('healthy')
    _reservation, queued = deployment(second)
    assert_equal [queued['id']], @engine.claim(owner_id: second['id']).map { |job| job['id'] }
    delivered = []
    @engine.define_singleton_method(:notification_call) do |recipient, event, action|
      if action == 'prepare'
        {'options' => {'model' => 'retained'}}
      else
        delivered << recipient['id']
        {'receipt' => {'threadId' => event['thread']}}
      end
    end
    @engine.deliver_notifications(owner_id: second['id'])
    assert_includes delivered, second['id']
    assert_equal 'reconciling', @engine.store.read['jobs'][submitted['id']]['state']
    assert_equal @base, git('--git-dir', @remote, 'rev-parse', 'master').strip
  ensure
    [unit, recovery_unit].compact.each do |name|
      Open3.capture3('systemctl', '--user', 'stop', name)
      Open3.capture3('systemctl', '--user', 'reset-failed', name)
    end
  end
end
