# frozen_string_literal: true

# User-manager acceptance after independent review:
# ruby test/background_integration_systemd_test.rb -n /systemd/
require_relative 'integration_test'

class BackgroundIntegrationSystemdTest < IntegrationTest
  def wait_for_background
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 30
    until yield
      raise 'background worker did not reach the expected state' if Process.clock_gettime(Process::CLOCK_MONOTONIC) > deadline
      sleep 0.05
    end
  end

  def test_systemd_background_continuation_survives_its_submitter_and_worker_restart
    first = owner('systemd-background')
    integration = driver(first)
    payload = integration_request(first)
    integration.start(payload, background: true)
    hold = File.join(@tmp, 'hold-preparation')
    started = File.join(@tmp, 'preparation-started')
    File.write(hold, 'hold before preparation')
    wrapper = File.join(@package, 'bin/workspace-host')
    FileUtils.mkdir_p(File.dirname(wrapper))
    File.write(wrapper, <<~SCRIPT)
      #!#{RbConfig.ruby}
      require #{File.expand_path('../libexec/workspace-coordination-execution', __dir__).inspect}
      require #{File.expand_path('../libexec/workspace-integration', __dir__).inspect}
      request = WorkspaceCoordination.private_read(ARGV.last)
      engine = WorkspaceCoordination::Engine.new(state: #{@state.inspect}, package: request['package'], profile: request['profile'],
        workspace: request['workspace'], entry: request['entry'], fixtures: true)
      owner = engine.store.read['owners'].fetch(request['ownerId'])
      driver = WorkspaceCoordination::Integration.new(engine, owner, admission: ->(&block) { block.call }, prepare: ->(fields) {
        File.write(#{started.inspect}, Process.pid.to_s)
        sleep 0.05 while File.exist?(#{hold.inspect})
        engine.prepare_merge(owner, fields.fetch('repository'), fields.fetch('evidence'))
      })
      driver.define_singleton_method(:record_completion) { |_value| }
      driver.run_background(request.fetch('runId'))
    SCRIPT
    File.chmod(0o700, wrapper)
    submitter = fork { @engine.dispatch_integrations(first); exit! 0 }
    Process.wait(submitter)
    assert $?.success?
    unit = "workspace-integration-#{payload['requestId']}.service"
    wait_for_background { File.exist?(started) }
    before = fixture_systemctl('show', unit, '--property=MainPID', '--value')
    @engine.dispatch_integrations(first)
    assert_equal before, fixture_systemctl('show', unit, '--property=MainPID', '--value')
    fixture_systemctl('stop', unit)
    File.unlink(hold)
    @engine.dispatch_integrations(first)
    wait_for_background { integration.read(payload['requestId'])['phase'] == 'merging' }
    job = @engine.claim(owner_id: first['id']).first
    attempt_path = @engine.attempt_path(job['attempt'], 'request.json')
    WorkspaceCoordination.atomic(attempt_path, @engine.worker_request(job, first))
    WorkspaceCoordination::Worker.new(attempt_path, fixtures: true).run
    @engine.reconcile_attempts
    wait_for_background { !%w[active activating deactivating].include?(fixture_systemctl('show', unit, '--property=ActiveState', '--value').strip) }
    @engine.dispatch_integrations(first)
    wait_for_background do
      integration.read(payload['requestId'])['phase'] == 'finalizing' && @engine.store.read['outbox'].length == 1
    end
    assert_equal 1, @engine.store.read['outbox'].length
    deliver_background_notifications(first)
    wait_for_background { !%w[active activating deactivating].include?(fixture_systemctl('show', unit, '--property=ActiveState', '--value').strip) }
    @engine.dispatch_integrations(first)
    wait_for_background { integration.read(payload['requestId'])['phase'] == 'complete' }
    assert_equal 1, @engine.store.read['jobs'].length
    assert_equal 1, @engine.store.read['outbox'].length
  ensure
    Open3.capture3('systemctl', '--user', 'stop', unit) if unit
  end

  def fixture_systemctl(*args)
    output, error, status = Open3.capture3('systemctl', '--user', *args)
    raise error unless status.success?
    output
  end
end
