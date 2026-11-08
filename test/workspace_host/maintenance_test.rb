# frozen_string_literal: true

require_relative 'suspension_test'

class WorkspaceHostTest < Minitest::Test
  def test_maintenance_control_refusal_retains_safe_request_correlation
    identity = '11223344-1122-4122-8122-112233441122'
    requests = [
      [{'action' => 'startup-finish', 'operationId' => identity}, 'startup-finish', identity],
      [{}, 'unknown', 'unknown'],
      [{'action' => "unsafe\nPRIVATE", 'operationId' => '../PRIVATE'}, 'unknown', 'unknown'],
      [{'action' => 'a' * 65, 'operationId' => 42}, 'unknown', 'unknown'],
      [{'action' => 42, 'operationId' => 'ABCDEF12-1122-4122-8122-112233441122'}, 'unknown', 'unknown']
    ]
    requests.each do |request, action, operation|
      response = {'schema' => 1, 'status' => 'refused', 'reason' => 'startup-grant-has-not-reached-native-launch'}
      with_maintenance_control_transport(JSON.generate(response)) do |host, record, witness|
        request['private'] = 'PRIVATE-BODY'
        error = assert_raises(DevWorkspaceHost::Error) { host.send(:maintenance_control, request) }
        assert_equal("maintenance control refused: #{response.fetch('reason')} (action=#{action} operation=#{operation})", error.message)
        %w[PRIVATE-BODY PRIVATE-STDERR PRIVATE-INVOCATION].each { |value| refute_includes(error.message, value) }
        sent = JSON.parse(File.read(record))
        assert_equal(request.merge('schema' => 1, 'invocation' => 'PRIVATE-INVOCATION'), sent.fetch('request'))
        assert_includes(sent.fetch('argv'), '--worker')
        assert_equal('retained evidence', File.read(witness))
        assert_empty(host.events)
      end
    end
  end

  def test_maintenance_control_transport_preserves_success_and_validation
    response = {'schema' => 1, 'status' => 'pending', 'startup' => {'pending' => true}}
    with_maintenance_control_transport(JSON.generate(response)) do |host, _record, witness|
      assert_equal(response, host.send(:maintenance_control, {'action' => 'startup-status'}, worker: false))
      assert_equal('retained evidence', File.read(witness))
      assert_empty(host.events)
    end
    [[JSON.generate(response), 3, DevWorkspaceHost::Error, 'maintenance control failed'],
     ['not JSON', 0, JSON::ParserError, nil],
     ['{"schema":2,"status":"pending"}', 0, DevWorkspaceHost::Error, 'invalid response'],
     ['x' * (4 * 1024 * 1024 + 1), 0, DevWorkspaceHost::Error, 'exceeds bounds']].each do |output, status, type, message|
      with_maintenance_control_transport(output, status: status) do |host, _record, witness|
        error = assert_raises(type) { host.send(:maintenance_control, {'action' => 'startup-status'}) }
        assert_includes(error.message, message) if message
        refute_includes(error.message, 'PRIVATE-STDERR')
        assert_equal('retained evidence', File.read(witness))
        assert_empty(host.events)
      end
    end
  end

  def test_held_target_admission_refuses_before_maintenance_effects
    [:absent, 0, 2, '1', 1.0, true, nil].each do |value|
      with_maintenance_transition_host do |host, paths, _|
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
        host.define_singleton_method(:codex_restart_required?) { |*args, **kwargs| true }
        prepared = host.send(:prepare_profile, ['--source', paths.fetch(:source), '--enable-maintenance'])
        contract_path = File.join(host.candidate, 'share/workspace-portal/runtime-contract.json')
        contract = JSON.parse(File.read(contract_path))
        value == :absent ? contract.delete('maintenanceHeldCompletionPolicy') : contract['maintenanceHeldCompletionPolicy'] = value
        File.write(contract_path, JSON.generate(contract))
        effects = []
        host.define_singleton_method(:maintenance_control) { |*args, **| effects << args; raise 'maintenance effect before admission' }
        before = host.events.length
        assert_raises(DevWorkspaceHost::Error) { host.send(:prepare_profile, ['--source', paths.fetch(:source), '--enable-maintenance']) }
        assert_raises(DevWorkspaceHost::Error) { host.send(:start_maintenance_switch, prepared) }
        assert_raises(DevWorkspaceHost::Error) { host.send(:validate_candidate_preparation!, host.candidate, prepared: prepared) }
        assert_empty(effects)
        refute(host.events.drop(before).any? { |event| event.first == :profile_set || event.last.to_s.start_with?('workspace-maintenance@') })
        host.define_singleton_method(:maintenance_completion_held?) { true }
        host.define_singleton_method(:install_links) { |**| raise 'links before target admission' }
        timer = File.join(host.instance_variable_get(:@home), '.config/systemd/user/workspace-auto-archive@.timer')
        FileUtils.mkdir_p(File.dirname(timer))
        saved_timer = File.join(paths.fetch(:root), 'retained-auto-archive.timer')
        File.write(saved_timer, 'original timer')
        File.symlink(saved_timer, timer)
        before_activation = host.events.length
        assert_raises(DevWorkspaceHost::Error) { host.send(:activate, []) }
        assert(File.symlink?(timer), 'preflight refusal removed retained auto-archive timer')
        assert_equal(saved_timer, File.readlink(timer))
        refute(host.events.drop(before_activation).any? { |event| event.first == :command }, 'preflight refusal entered cleanup/service effects')
      end
    end
  end

  def test_maintenance_first_candidate_stage_keeps_reporting_three_and_busy_refusal
    with_maintenance_transition_host do |host, paths, predecessor|
      error = assert_raises(DevWorkspaceHost::Error) do
        host.send(:prepare_profile, ['--source', paths.fetch(:source), '--from-candidate', '--enable-maintenance'], from_candidate: true)
      end
      assert_includes(error.message, 'install bounded maintenance support')
      assert_equal(predecessor, File.realpath(host.instance_variable_get(:@profile)))

      host.busy = ['active native turn']
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
      assert_equal(predecessor, File.realpath(host.instance_variable_get(:@profile)))
      host.busy = []
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']), host.instance_variable_get(:@err).string)
      assert_equal(host.candidate, File.realpath(host.instance_variable_get(:@profile)))
      entry = host.send(:registry).entries.first
      argv = host.send(:load_registration_marker, entry).fetch('launch').fetch('argv')
      assert_includes(argv, 'mcp_servers.team_reporting.enabled_tools=["report_to_lead","session_communication","monitor_operation"]')
      refute(argv.any? { |value| value.include?('workspace_maintenance') })
      refute_path_exists(File.join(host.instance_variable_get(:@state), 'maintenance'))
    end
  end

  def test_maintenance_same_package_enable_needs_no_mcp_restart
    with_maintenance_transition_host do |host, paths, _|
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
      prepared = host.send(:prepare_profile, ['--source', paths.fetch(:source), '--enable-maintenance', '--pause-timeout', '7'])
      assert_equal(7, prepared.fetch('pauseTimeout'))
      refute(host.send(:maintenance_switch_required?, prepared))
      startup = {'enabled' => false, 'pending' => false}
      observed = []
      host.define_singleton_method(:maintenance_control) do |request, **|
        case request.fetch('action')
        when 'active' then {'status' => 'none'}
        when 'startup-status' then {'startup' => JSON.parse(JSON.generate(startup))}
        when 'startup-stage'
          startup.merge!(request.fetch('startup'), 'operationId' => request.fetch('operationId'), 'pending' => true)
          path = File.join(@state, 'maintenance/startup.json')
          FileUtils.mkdir_p(File.dirname(path))
          File.write(path, '{}')
          {'status' => 'pending'}
        when 'startup-authorize'
          startup.merge!(request.fetch('startup'))
          {'status' => 'pending'}
        when 'startup-reconcile' then {'status' => 'startup-reconciled'}
        when 'startup-observe-existing'
          observed << request.fetch('grant')
          raise 'missing exact marker digest' unless request.fetch('markerSHA256').match?(/\A[0-9a-f]{64}\z/)
          {'status' => 'startup-observed'}
        when 'startup-finish'
          raise 'unobserved grant' unless observed == startup.fetch('launchGrants')
          startup['pending'] = false
          startup['enabled'] = startup.fetch('targetEnabled')
          {'status' => 'enabled'}
        else raise "unexpected startup action #{request.fetch('action')}"
        end
      end
      host.define_singleton_method(:portal_service_state) do |_unit|
        {'ActiveState' => 'active', 'MainPID' => Process.pid.to_s, 'ControlPID' => '0'}
      end
      before = host.events.length
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--enable-maintenance']), host.instance_variable_get(:@err).string)
      assert(startup.fetch('enabled'))
      refute(startup.fetch('pending'))
      assert_equal(1, observed.length)
      refute(host.events.drop(before).any? { |event| %i[sessions_quiesced consumers_restarted profile_set].include?(event.first) })
      assert_raises(DevWorkspaceHost::Error) { host.send(:prepare_profile, ['--source', paths.fetch(:source), '--pause-timeout', '0']) }
      assert_raises(OptionParser::InvalidArgument) { host.send(:prepare_profile, ['--source', paths.fetch(:source), '--pause-timeout', 'no']) }
    end
  end

  def test_maintenance_rejects_stale_source_token_and_claim_before_operation_effects
    with_maintenance_transition_host do |host, paths, _predecessor|
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
      host.define_singleton_method(:codex_restart_required?) { |*args, **kwargs| true }
      prepared = host.send(:prepare_profile, ['--source', paths.fetch(:source), '--enable-maintenance'])
      requests = []
      host.define_singleton_method(:maintenance_control) { |request, **_options| requests << request; raise 'operation effect before admission' }
      [['profileToken', 'stale-token'], ['sourceIdentity', {}]].each do |key, value|
        error = assert_raises(DevWorkspaceHost::Error) { host.send(:start_maintenance_switch, prepared.merge(key => value)) }
        assert_includes(error.message, 'maintenance source changed')
      end
      host.define_singleton_method(:require_coordination_profile_claim!) do |_candidate|
        raise WorkspaceCoordination::Error.new('exact machine claim refused', 4)
      end
      error = assert_raises(WorkspaceCoordination::Error) { host.send(:start_maintenance_switch, prepared) }
      assert_includes(error.message, 'exact machine claim refused')
      assert_empty(requests)
      refute(host.events.any? { |event| event.last.to_s.start_with?('workspace-maintenance@') })
    end
  end

  def test_maintenance_registration_has_no_tool_dependency
    with_maintenance_transition_host do |host, paths, _|
      assert_equal(0,host.run('workspace-host',['switch','--source',paths.fetch(:source),'--from-candidate']))
      plans=host.send(:registration_plans,package:host.candidate)
      refute(plans.any? { |_,plan| plan.argv.any? { |value| value.include?('workspace_maintenance') } })
    end
  end

  def test_maintenance_startup_grants_include_registered_inactive_units_without_fake_sources
    [false, true].each do |all_inactive|
      with_maintenance_transition_host do |host, paths, _predecessor|
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--from-candidate']))
        host.send(:registry).register(name: 'inactive-workspace', root: make_workspace(paths.fetch(:root), 'inactive-workspace'),
                                     hostname: 'inactive.workspace.example.test', aliases: [], replace: false)
        prepared = host.send(:prepare_profile, ['--source', paths.fetch(:source), '--enable-maintenance'])
        host.define_singleton_method(:codex_restart_required?) { |*args, **kwargs| true }
        operation, grants = nil, nil
        host.define_singleton_method(:portal_service_state) do |unit|
          running = !all_inactive && unit == 'workspace-codex@example-workspace.service'
          {'ActiveState' => running ? 'active' : 'inactive', 'MainPID' => running ? Process.pid.to_s : '0', 'ControlPID' => '0'}
        end
        host.define_singleton_method(:maintenance_control) do |request, **_options|
          case request.fetch('action')
          when 'prepare'
            operation = request.fetch('operation').merge('phase' => 'complete')
            {'status' => 'prepared'}
          when 'inspect'
            {'operation' => operation}
          when 'startup-authorize'
            grants = request.fetch('startup').fetch('launchGrants')
            {'status' => 'authorized'}
          when 'startup-reconcile'
            {'status' => 'startup-reconciled'}
          else
            raise "unexpected startup action #{request.fetch('action')}"
          end
        end
        assert(host.send(:start_maintenance_switch, prepared))
        assert_equal(2, operation.fetch('schema'))
        assert_equal(all_inactive ? [] : ['workspace-codex@example-workspace.service'], operation.fetch('services').map { |source| source.fetch('unit') })
        host.define_singleton_method(:maintenance_startup_selection) do
          {'pending' => true, 'enabled' => false, 'operationId' => prepared.fetch('switchOperationId'), 'targetEnabled' => true}
        end
        host.send(:authorize_maintenance_startup, prepared)
        assert_equal(%w[workspace-codex@example-workspace.service workspace-codex@inactive-workspace.service], grants.map { |grant| grant.fetch('unit') })
        assert(grants.all? { |grant| grant.fetch('native') == prepared.fetch('candidateCodex') && !grant.fetch('argvDigest').empty? })
        assert(grants.none? { |grant| grant.key?('pid') || grant.key?('socketEpoch') })
      end
    end
  end

  def test_maintenance_owned_activation_waits_for_its_completion_not_generic_restart
    [false, true].each do |matching_marker|
      with_owned_maintenance_activation(matching_marker: matching_marker) do |host, operation, events, fixture|
        pending = host.send(:pending_codex_update)
        fixture[:after_activation] = lambda do
          assert_equal(pending, host.send(:pending_codex_update))
          assert(fixture.fetch(:startup).fetch('pending'))
          refute_includes(events, :continuation)
          refute_includes(events, :terminals_restored)
          refute_includes(events, :writers_restored)
        end
        host.send(:maintenance_worker_command, [operation.fetch('operationId')])
        assert_equal('complete', operation.fetch('phase'))
        assert_nil(host.send(:pending_codex_update))
        refute(fixture.fetch(:startup).fetch('pending'))
        %i[native_started activation_returned socket_ready registration_verified startup_ready startup_finished destination_ready continuation terminals_restored writers_restored].each_cons(2) do |first, second|
          assert_operator(events.index(first), :<, events.index(second), "#{first} before #{second}")
        end
        assert_equal(operation.fetch('operationId'), fixture.fetch(:environment).fetch('DEV_WORKSPACE_MAINTENANCE_ACTIVATION'))
        assert_equal('1', fixture.fetch(:environment).fetch('DEV_WORKSPACE_ACTIVATION'))
        assert_empty(fixture.fetch(:forbidden))
      end
    end
  end

  def test_maintenance_owned_activation_completion_refusals_never_resume
    %i[marker startup profile pending destination].each do |failure|
      with_owned_maintenance_activation(failure: failure) do |host, operation, events, fixture|
        error = assert_raises(DevWorkspaceHost::Error) { host.send(:maintenance_worker_command, [operation.fetch('operationId')]) }
        assert_match(/markers do not match|readiness remains unproved|completion was superseded|destination native service/, error.message)
        assert_equal('recovery-required', operation.fetch('phase'))
        refute_includes(events, :continuation)
        refute_includes(events, :terminals_restored)
        refute_includes(events, :writers_restored)
        refute_includes(events, :destination_ready)
        assert_empty(fixture.fetch(:forbidden))
      end
    end
  end

  def test_maintenance_owned_activation_authority_refuses_before_effects
    mutations = {
      missing_owner: ->(f, o, e) { e.delete('DEV_WORKSPACE_MAINTENANCE_ACTIVATION') },
      wrong_owner: ->(f, o, e) { e['DEV_WORKSPACE_MAINTENANCE_ACTIVATION'] = 'foreign' },
      private_activation: ->(f, o, e) { e.delete('DEV_WORKSPACE_ACTIVATION') },
      startup_owner: ->(f, o, e) { f.fetch(:startup)['operationId'] = 'foreign' },
      missing_selection: ->(f, o, e) { f.fetch(:startup).replace('pending' => false, 'enabled' => false) },
      target: ->(f, o, e) { o['activationPackage'] = '/foreign-target' },
      startup_target: ->(f, o, e) { f.fetch(:startup)['candidatePackage'] = '/foreign-target' },
      token: ->(f, o, e) { f.fetch(:startup)['authorizedProfileToken'] = 'stale' },
      source_token: ->(f, o, e) { f.fetch(:startup)['sourceProfileToken'] = 'stale' },
      profile: ->(f, o, e) { o['profile'] = '/foreign-profile' },
      phase: ->(f, o, e) { o['phase'] = 'stopping-native' },
      cutover: ->(f, o, e) { o['cutoverIntent'] = false },
      source_stop: ->(f, o, e) { o['sourcesStopped'] = false },
      activation_intent: ->(f, o, e) { o['activationIntent'] = false }
    }
    mutations.each do |name, mutate|
      with_owned_maintenance_activation do |host, operation, events, fixture|
        fixture[:mutate_environment] = ->(environment) { mutate.call(fixture, operation, environment) }
        pending = host.send(:pending_codex_update)
        token = host.send(:profile_link_token)
        error = assert_raises(DevWorkspaceHost::Error, name.to_s) { fixture.fetch(:activate).call }
        assert_match(/owning activation|activation is private|maintenance must finish or recover/, error.message, name.to_s)
        assert_equal(pending, host.send(:pending_codex_update), name.to_s)
        assert_equal(token, host.send(:profile_link_token), name.to_s)
        assert_equal(fixture.fetch(:grant_before_invocation), fixture.fetch(:startup), name.to_s)
        assert_empty(fixture.fetch(:candidate).events, name.to_s)
        refute_includes(events, :native_started)
      end
    end
  end

  def test_maintenance_owned_activation_accepts_completed_retry_and_admitted_source_recovery
    [[true, false], [false, true], [true, true]].each do |completed, source_recovered|
      with_owned_maintenance_activation(completed: completed, source_recovered: source_recovered) do |host, operation, events, fixture|
        host.send(:maintenance_worker_command, [operation.fetch('operationId')])
        assert_equal(source_recovered ? 'cancelled' : 'complete', operation.fetch('phase'))
        assert_equal(operation.fetch('activationPackage'), fixture.fetch(:startup).fetch('candidatePackage'))
        if source_recovered
          assert_equal(operation.fetch('sourcePackage'), operation.fetch('activationPackage'))
          refute_equal(operation.fetch('candidatePackage'), operation.fetch('activationPackage'))
        end
        assert_equal(completed ? 0 : 1, events.count(:startup_finished))
        assert_includes(events, :destination_ready)
        assert_empty(fixture.fetch(:forbidden))
      end
    end
  end

  def test_ordinary_startup_selection_does_not_hold_portals_without_an_operation
    with_transition_host do |host, paths|
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      directory = File.join(host.instance_variable_get(:@state), 'maintenance')
      FileUtils.mkdir_p(directory)
      File.write(File.join(directory, 'startup.json'), '{}')
      host.define_singleton_method(:require_compatible_maintenance!) { |*| }
      host.define_singleton_method(:maintenance_control) { |_request, **| {'status' => 'none'} }
      host.define_singleton_method(:maintenance_startup_selection) { {'pending' => true, 'enabled' => false} }
      host.define_singleton_method(:maintenance_activation_target!) { |**| false }
      host.define_singleton_method(:configure_user_services) { |defer_portals:| events << [:configured_deferred, defer_portals] }
      host.define_singleton_method(:codex_restart_required?) { |*args, **kwargs| true }
      host.define_singleton_method(:reconcile_codex_update) { |**| true }
      host.events.clear
      host.send(:activate, [])
      assert_includes(host.events, [:command, 'systemctl', '--user', 'start', 'workspace-portal@example-workspace.service'])
    end
  end

  def test_maintenance_reconciliation_refuses_mismatched_registration_before_effects
    [false, true].each do |defer_busy|
      with_transition_host do |host, paths|
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        host.events.clear
        host.define_singleton_method(:maintenance_completion_held?) { true }
        host.define_singleton_method(:codex_restart_required?) { |*args, **kwargs| true }
        pending = host.send(:pending_codex_update)
        directory = File.join(host.instance_variable_get(:@state), 'maintenance')
        FileUtils.mkdir_p(directory)
        grant = File.join(directory, 'startup.json')
        File.write(grant, '{"owned":"unchanged-grant-evidence"}')
        error = assert_raises(DevWorkspaceHost::Error) { host.send(:reconcile_codex_update, defer_busy: defer_busy) }
        assert_includes(error.message, 'owning maintenance recover')
        if pending.nil?
          assert_nil(host.send(:pending_codex_update))
        else
          assert_equal(pending, host.send(:pending_codex_update))
        end
        assert_equal('{"owned":"unchanged-grant-evidence"}', File.read(grant))
        assert_empty(host.events)
      end
    end
  end

  def test_maintenance_reconciliation_matching_registration_is_passive
    [false, true].each do |defer_busy|
      with_transition_host do |host, paths|
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        host.define_singleton_method(:maintenance_completion_held?) { true }
        plans = host.send(:registration_plans)
        host.send(:write_pending_codex_update, host.candidate, File.realpath(host.send(:active_codex)), plans)
        host.events.clear
        assert(host.send(:reconcile_codex_update, defer_busy: defer_busy))
        assert_nil(host.send(:pending_codex_update))
        assert_empty(host.events)
      end
    end
  end

  def test_maintenance_worker_restart_routes_to_retained_source_before_claim
    with_maintenance_worker do |host, operation, events|
      owner = File.join(host.instance_variable_get(:@state), 'retained-owner')
      FileUtils.mkdir_p(owner)
      operation['sourcePackage'] = owner
      dispatched = nil
      host.define_singleton_method(:exec) do |*args|
        dispatched = args
        raise DevWorkspaceHost::Error, 'owner dispatch boundary'
      end
      error = assert_raises(DevWorkspaceHost::Error) { host.send(:maintenance_worker_command, [operation.fetch('operationId')]) }
      assert_equal('owner dispatch boundary', error.message)
      assert_equal([File.join(owner, 'bin/workspace-host'), 'maintenance-worker', operation.fetch('operationId')], dispatched.drop(1))
      refute_includes(events, :wait)
      refute_includes(events, :profile)
    end
  end

  def test_maintenance_all_inactive_worker_completes_without_source_drain
    with_maintenance_worker do |host, operation, events|
      operation['services'] = []
      operation['sourcesStopped'] = false
      host.send(:maintenance_worker_command, [operation.fetch('operationId')])
      assert_equal('complete', operation.fetch('phase'))
      assert_includes(events, :native_stop)
      assert_includes(events, :successful_exit)
      assert_includes(events, :profile)
      assert(operation.fetch('restoration').fetch('completed'))
    end
  end

  def test_maintenance_worker_awaits_without_exclusion_and_admits_activation_after_exit
    with_maintenance_worker do |host, operation, events|
      host.send(:maintenance_worker_command, [operation.fetch('operationId')])
      assert_equal('complete', operation.fetch('phase'))
      assert_operator(events.index(:wait), :<, events.index(:writers_joined))
      assert_operator(events.index(:writers_joined), :<, events.index(:native_stop))
      assert_operator(events.index(:successful_exit), :<, events.index(:activation_intent))
      assert_operator(events.index(:activation_intent), :<, events.index(:profile))
      assert_operator(events.index(:ready), :<, events.index(:continuation))
      assert_operator(events.index(:continuation), :<, events.rindex(:terminals_restored))
    end
  end

  def test_maintenance_worker_rechecks_claim_after_readiness_before_draining
    valid = true
    after_ready = ->(_host, _paths, _operation) { valid = false }
    with_maintenance_worker(after_ready: after_ready) do |host, operation, events|
      host.define_singleton_method(:require_coordination_profile_claim!) do |_candidate|
        raise WorkspaceCoordination::Error.new('original claim is no longer held', 4) unless valid
      end
      error = assert_raises(WorkspaceCoordination::Error) do
        host.send(:maintenance_worker_command, [operation.fetch('operationId')])
      end
      assert_includes(error.message, 'original claim is no longer held')
      assert_includes(events, :wait)
      assert_empty(events & %i[observer_drain writers_joined terminals_quiesced native_stop activation_intent profile])
    end
  end

  def test_maintenance_worker_rechecks_prepared_inputs_after_readiness_before_draining
    %i[source registry registration].each do |change|
      after_ready = lambda do |host, paths, _operation|
        case change
        when :source
          File.write(File.join(paths.fetch(:source), 'changed.nix'), '{ changed = true; }')
        when :registry
          host.send(:registry).register(name: 'new-workspace', root: make_workspace(paths.fetch(:root), 'new-workspace'),
                                       hostname: 'new.workspace.example.test', aliases: [], replace: false)
        when :registration
          host.registration_digest = 'b' * 64
        end
      end
      with_maintenance_worker(after_ready: after_ready) do |host, operation, events|
        error = assert_raises(DevWorkspaceHost::Error) do
          host.send(:maintenance_worker_command, [operation.fetch('operationId')])
        end
        assert_match(/source changed|registry changed|registration changed/, error.message)
        assert_includes(events, :wait)
        assert_empty(events & %i[observer_drain writers_joined terminals_quiesced native_stop activation_intent profile])
      end
    end
  end

  def test_maintenance_cancel_before_shutdown_restores_only_owned_state
    with_maintenance_worker(cancel: true) do |host, operation, events|
      host.send(:maintenance_worker_command, [operation.fetch('operationId')])
      assert_equal('cancelled', operation.fetch('phase'))
      refute_includes(events, :native_stop)
      refute_includes(events, :profile)
      assert_equal(2, events.count(:exclusion))
      assert_includes(events, :terminals_restored)
    end
  end

  def test_restoration_failures_and_worker_loss_retry_owned_tail_before_terminal
    [:terminal, :portal, :before_finalize].each do |fault|
      [false, true].each do |cancel|
        with_maintenance_worker(cancel: cancel, fault: fault) do |host, operation, events|
          assert_raises(DevWorkspaceHost::Error) { host.send(:maintenance_worker_command, [operation.fetch('operationId')]) }
          refute_includes(%w[complete cancelled], operation.fetch('phase'))
          refute(operation.fetch('restoration').fetch('completed'))
          profiles = events.count(:profile)
          continuations = events.count(:continuation)
          drains = events.count(:native_stop)
          host.send(:maintenance_worker_command, [operation.fetch('operationId')])
          assert_equal(cancel ? 'cancelled' : 'complete', operation.fetch('phase'))
          assert(operation.fetch('restoration').fetch('completed'))
          assert_equal(profiles, events.count(:profile))
          assert_equal(continuations, events.count(:continuation))
          assert_equal(drains, events.count(:native_stop))
        end
      end
    end
  end

  def test_source_recovered_outcome_waits_for_owned_restoration
    with_maintenance_worker(source_recovered: true, fault: :before_finalize) do |host, operation, events|
      assert_raises(DevWorkspaceHost::Error) { host.send(:maintenance_worker_command, [operation.fetch('operationId')]) }
      assert_equal('recovery-required', operation.fetch('phase'))
      host.send(:maintenance_worker_command, [operation.fetch('operationId')])
      assert_equal('cancelled', operation.fetch('phase'))
      assert_equal('source-recovered', operation.fetch('outcome'))
      assert_equal(1, events.count(:continuation))
      assert(operation.fetch('restoration').fetch('completed'))
    end
  end

  def test_maintenance_shutdown_and_activation_failures_never_resume
    %i[stop shutdown activation].each do |fault|
      with_maintenance_worker do |host, operation, events|
        if fault == :stop
          original = host.method(:system!)
          host.define_singleton_method(:system!) do |*argv|
            raise DevWorkspaceHost::Error, 'owned service stop failed' if argv[0, 3] == ['systemctl', '--user', 'stop']
            original.call(*argv)
          end
        elsif fault == :shutdown
          host.define_singleton_method(:maintenance_verify_source_exit!) { |_| raise DevWorkspaceHost::Error, 'source processes remain' }
        else
          host.define_singleton_method(:complete_candidate_activation) { |_| raise DevWorkspaceHost::Error, 'activation failed' }
        end
        assert_raises(DevWorkspaceHost::Error) { host.send(:maintenance_worker_command, [operation.fetch('operationId')]) }
        assert_equal('recovery-required', operation.fetch('phase'))
        refute_includes(events, :continuation)
        refute_includes(events, :terminals_restored)
        refute_includes(events, :writers_restored)
      end
    end
  end

  private

  def with_maintenance_control_transport(output, status: 0)
    with_transition_host do |host, paths|
      host.executing_package = host.candidate
      host.instance_variable_set(:@env, host.instance_variable_get(:@env).merge('INVOCATION_ID' => 'PRIVATE-INVOCATION'))
      record = File.join(paths.fetch(:root), 'request.json')
      witness = File.join(paths.fetch(:root), 'retained-evidence')
      File.write(witness, 'retained evidence')
      helper = File.join(host.candidate, 'bin/workspace-portal')
      File.write(helper, <<~RUBY)
        #!#{RbConfig.ruby}
        require 'json'
        File.write(#{record.dump}, JSON.generate('request' => JSON.parse(STDIN.read), 'argv' => ARGV))
        STDERR.write('PRIVATE-STDERR is not JSON')
        STDOUT.write(#{output.dump})
        exit #{status}
      RUBY
      File.chmod(0o755, helper)
      yield host, record, witness
    end
  end

  def maintenance_test_argv(package, state, workspace, profile, policy, enabled)
    tools = ['report_to_lead']
    tools << 'session_communication' if policy >= 2
    tools << 'monitor_operation' if policy == 3
    values = [
      ['team_reporting.command', File.join(package, 'bin/workspace-portal')],
      ['team_reporting.args', ['team-mcp', '--user-state-root', state, '--workspace', workspace, '--host-profile', profile]],
      ['team_reporting.required', true], ['team_reporting.enabled_tools', tools],
      ['team_reporting.tools.report_to_lead.approval_mode', 'approve']
    ]
    values << ['team_reporting.tools.session_communication.approval_mode', 'approve'] if policy >= 2
    values << ['team_reporting.tools.monitor_operation.approval_mode', 'approve'] if policy == 3
    values.flat_map { |key, value| ['-c', "mcp_servers.#{key}=#{JSON.generate(value)}"] }
  end

  def with_maintenance_transition_host
    with_transition_host do |host, paths|
      predecessor = host.candidate
      path = File.join(predecessor, 'share/workspace-portal/runtime-contract.json')
      File.write(path, JSON.generate(JSON.parse(File.read(path)).merge('workspaceReportingPolicy' => 2)))
      builder = method(:maintenance_test_argv)
      host.define_singleton_method(:registration_plan_for) do |entry, package: package_root|
        policy = JSON.parse(File.read(File.join(package, 'share/workspace-portal/runtime-contract.json'))).fetch('workspaceReportingPolicy')
        argv = builder.call(package, @state, entry.fetch('root'), @profile, policy, maintenance_registration_target)
        DevWorkspaceHost::RegistrationPlan.new(package_root: package, argv: argv,
          digest: Digest::SHA256.hexdigest(JSON.generate(argv)), policy: 1, required_native_child_threads: 0, states: [])
      end
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.candidate = make_package(paths.fetch(:root), 'maintenance-final')
      path = File.join(host.candidate, 'share/workspace-portal/runtime-contract.json')
      File.write(path, JSON.generate(JSON.parse(File.read(path)).merge('workspaceReportingPolicy' => 3,
        'maintenanceStateSchema' => 2, 'maintenancePolicy' => 2, 'maintenanceHeldCompletionPolicy' => 1)))
      host.executing_package = host.candidate
      yield host, paths, predecessor
    end
  end

  # Exercise candidate dispatch with its actual environment, and keep only
  # external controller/systemd observations local to this composition fixture.
  def with_owned_maintenance_activation(matching_marker: false, completed: false, source_recovered: false, failure: nil)
    with_maintenance_worker(source_recovered: source_recovered) do |host, operation, events|
      runtime = Dir.mktmpdir('ma-', '/tmp')
      host.instance_variable_set(:@runtime, runtime)
      host.instance_variable_set(:@env, host.instance_variable_get(:@env).merge('DEV_WORKSPACES_RUNTIME_DIR' => runtime))
      startup_path = File.join(host.instance_variable_get(:@state), 'maintenance', 'startup.json')
      selected = host.candidate
      operation.merge!('phase' => 'activating', 'cutoverIntent' => true, 'sourcesStopped' => true,
                       'activationIntent' => true, 'activationPackage' => selected, 'candidatePackage' => selected)
      operation['candidatePackage'] = make_package(File.dirname(selected), 'unselected-original-candidate') if source_recovered
      startup = {'pending' => !completed, 'enabled' => completed, 'targetEnabled' => true,
                 'operationId' => operation.fetch('operationId'), 'candidatePackage' => selected,
                 'sourceProfileToken' => operation.fetch('sourceProfileToken'),
                 'authorizedProfileToken' => host.send(:profile_link_token)}
      File.write(startup_path, JSON.generate(startup))
      host.instance_variable_set(:@maintenance_operation, operation.fetch('operationId'))
      plans = host.send(:registration_plans, package: selected)
      native = File.realpath(host.send(:active_codex))
      host.send(:write_pending_codex_update, selected, native, plans)
      plans.each { |entry, plan| host.send(:write_registration_marker, entry, native, plan) } if matching_marker
      fixture = {startup: startup, forbidden: []}
      sockets = []
      original_control = host.method(:maintenance_control)
      control = lambda do |request, **options|
        case request.fetch('action')
        when 'active', 'inspect'
          {'status' => operation.fetch('phase'), 'operation' => JSON.parse(JSON.generate(operation))}
        when 'startup-status'
          {'startup' => JSON.parse(JSON.generate(startup))}
        when 'startup-reconcile'
          events << :startup_ready
          {'status' => failure == :startup ? 'retry-wait' : 'startup-reconciled'}
        when 'startup-finish'
          events << :startup_finished
          startup['pending'] = false
          startup['enabled'] = startup.fetch('targetEnabled')
          {'status' => 'startup-finished'}
        when 'destination-ready'
          events << :destination_ready
          {'status' => 'destination-ready'}
        else
          original_control.call(request, **options)
        end
      end
      host.define_singleton_method(:maintenance_control, &control)
      host.define_singleton_method(:system_env!) do |environment, command, *arguments|
        raise 'unexpected candidate invocation' unless command == File.join(selected, 'bin/workspace-host') && arguments == ['_activate']
        fixture[:mutate_environment]&.call(environment)
        fixture[:environment] = environment.dup
        fixture[:grant_before_invocation] = JSON.parse(JSON.generate(startup))
        candidate = TransitionHost.new(candidate: selected, busy: [], env: environment, out: StringIO.new, err: StringIO.new)
        fixture[:candidate] = candidate
        candidate.define_singleton_method(:maintenance_control, &control)
        configure = DevWorkspaceHost::Host.instance_method(:configure_user_services).bind(candidate)
        candidate.define_singleton_method(:configure_user_services) { |**options| configure.call(**options) }
        system = candidate.method(:system!)
        candidate.define_singleton_method(:system!) do |*argv|
          system.call(*argv)
          if argv[0, 4] == ['systemctl', '--user', 'enable', '--now'] && argv.include?('workspace-codex@example-workspace.service')
            events << :native_started
            entry = send(:registry).entries.first
            socket = send(:instance_runtime, entry).fetch(:codex)
            send(:ensure_private_instance_runtime!, entry)
            sockets << UNIXServer.new(socket) unless File.socket?(socket)
          end
          if argv[0] == 'systemctl' && (argv.include?('start') || argv.include?('--now') || argv.include?('try-restart')) && argv.any? { |arg| arg.start_with?('workspace-portal@') }
            fixture.fetch(:forbidden) << argv
          end
        end
        %i[drain_portal_services quiesce_sessions probe_registration_plans! restart_codex_consumers].each do |method|
          candidate.define_singleton_method(method) { |*args, **options| fixture.fetch(:forbidden) << method; raise "unexpected generic #{method}" }
        end
        candidate.send(:workspace_host, arguments)
      end
      invoke = DevWorkspaceHost::Host.instance_method(:activate_installed).bind(host)
      fixture[:activate] = -> { invoke.call(File.join(selected, 'bin/workspace-host')) }
      host.define_singleton_method(:activate_candidate) do |candidate, **|
        raise 'unadmitted target' unless candidate == operation.fetch('activationPackage')
        unless matching_marker
          plans.each { |entry, _plan| File.unlink(send(:registration_marker_path, entry)) if File.exist?(send(:registration_marker_path, entry)) }
        end
        fixture.fetch(:activate).call
        events << :activation_returned
        fixture[:after_activation]&.call
        plans.each { |entry, plan| send(:write_registration_marker, entry, native, plan) } unless failure == :marker
        if failure == :profile
          File.unlink(@profile)
          File.symlink(File.dirname(selected), @profile)
        elsif failure == :pending
          changed = send(:pending_codex_update).merge('workspaces' => [])
          File.write(send(:pending_codex_file), JSON.generate(changed))
        end
        {'candidate' => selected, 'plans' => plans, 'codex' => native,
         'profileToken' => startup.fetch('authorizedProfileToken'), 'pending' => fixture.fetch(:pending),
         'quiesced' => [], 'restartRequired' => true}
      end
      fixture[:pending] = host.send(:pending_codex_update)
      wait = DevWorkspaceHost::Host.instance_method(:wait_for_codex_sockets).bind(host)
      host.define_singleton_method(:wait_for_codex_sockets) { wait.call; events << :socket_ready }
      verify = host.method(:verify_registration_semantics!)
      host.define_singleton_method(:verify_registration_semantics!) { |*args| verify.call(*args); events << :registration_verified }
      complete = DevWorkspaceHost::Host.instance_method(:complete_candidate_activation).bind(host)
      host.define_singleton_method(:complete_candidate_activation) { |activation| complete.call(activation) }
      if failure == :destination
        host.define_singleton_method(:portal_service_state) { |_| {'ActiveState' => 'inactive', 'MainPID' => '0'} }
      end
      yield host, operation, events, fixture
    ensure
      sockets&.each(&:close)
      FileUtils.remove_entry(runtime) if runtime
    end
  end

  def with_maintenance_worker(cancel: false, retry_once: false, fault: nil, source_recovered: false, after_ready: nil)
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      identity = '12345678-1234-4234-8234-123456789012'
      directory = File.join(host.instance_variable_get(:@state), 'maintenance', identity)
      FileUtils.mkdir_p(directory, mode: 0o700)
      entry = host.send(:registry).entries.fetch(0)
      prepared = host.send(:prepare_profile, ['--source', paths.fetch(:source)])
      contract_path = File.join(host.candidate, 'share/workspace-portal/runtime-contract.json')
      File.write(contract_path, JSON.generate(JSON.parse(File.read(contract_path)).merge('maintenanceHeldCompletionPolicy' => 1, 'maintenancePolicy' => 2, 'maintenanceStateSchema' => 2)))
      prepared = prepared.merge('switchOperationId' => identity, 'maintenanceTarget' => true,
                                'sourcePlansDigest' => prepared.fetch('plansDigest'), 'sourceMaintenanceTarget' => true)
      operation = {'operationId' => identity, 'phase' => 'prepared', 'cutoverIntent' => false,
                   'preparation' => prepared, 'sourcePackage' => host.candidate,
                   'profile' => host.instance_variable_get(:@profile), 'sourceProfileToken' => prepared.fetch('profileToken'), 'sourceRecovered' => source_recovered,
                   'services' => [{'workspace' => entry.fetch('root'), 'unit' => 'workspace-codex@example-workspace.service', 'pid' => Process.pid}],
                   'terminalIntents' => [], 'drainedPortals' => []}
      events = []
      excluded = false
      retried = false
      fault_raised = false
      host.define_singleton_method(:with_transition_lock) do |&block|
        raise 'nested package exclusion' if excluded
        events << :exclusion
        excluded = true
        begin
          block.call
        ensure
          excluded = false
        end
      end
      host.define_singleton_method(:maintenance_control) do |request, **_options|
        case request.fetch('action')
        when 'terminal-restoration'
          raise 'terminal identity observed without exclusion' unless excluded
          {'operation' => JSON.parse(JSON.generate(operation))}
        when 'inspect'
          {'operation' => JSON.parse(JSON.generate(operation))}
        when 'worker-claim'
          {'status' => 'worker-owned'}
        when 'pause'
          raise 'ack wait holds package exclusion' if excluded
          events << :wait
          after_ready&.call(host, paths, operation)
          {'status' => cancel ? 'cancelled' : 'ready'}
        when 'writers-drained'
          raise 'writer intent outside exclusion' unless excluded
          operation['drainedPortals'] = request.fetch('operation').fetch('drainedPortals')
          {'status' => 'draining-writers'}
        when 'source-current'
          {'status' => 'source-current'}
        when 'stop-intent'
          raise 'native stop outside exclusion' unless excluded
          events << :native_stop
          operation['cutoverIntent'] = true
          {'status' => 'stopping-native'}
        when 'sources-stopped'
          operation['sourcesStopped'] = true
          {'status' => 'sources-stopped'}
        when 'activation-intent'
          raise 'target admitted before source exit' unless operation.fetch('sourcesStopped')
          events << :activation_intent
          operation['activationIntent'] = true
          {'status' => 'activation-admitted'}
        when 'continue'
          raise 'continuation lacks exclusion' unless excluded
          events << :continuation
          operation['phase'] = 'resuming'
          operation['continuationsSettled'] = true
          {'status' => 'continued'}
        when 'cancelled'
          raise 'cancel after native intent' if operation['cutoverIntent']
          operation['phase'] = 'resuming'
          operation['cancelRequested'] = true
          operation['noticeCleanupComplete'] = true
          {'status' => 'cancelling'}
        when 'cancel-notices'
          {'status' => 'cancelling'}
        when 'restoration-begin'
          intent = request.fetch('restoration').merge('portals' => operation.fetch('drainedPortals'),
                                                     'terminals' => operation.fetch('terminalIntents'), 'completed' => false)
          raise 'restoration intent changed' if operation['restoration'] && operation['restoration'] != intent
          operation['restoration'] = intent
          operation['phase'] = 'resuming'
          events << :restoration_intent
          {'status' => 'restoring'}
        when 'restoration-complete'
          raise 'finalize before owned effects' unless events.include?(:terminals_restored) && events.include?(:writers_restored)
          if fault == :before_finalize && !fault_raised
            fault_raised = true
            raise DevWorkspaceHost::Error, 'interrupted after restoration effects'
          end
          operation.fetch('restoration')['completed'] = true
          result = operation.fetch('restoration').fetch('outcome')
          operation['phase'] = result == 'updated' ? 'complete' : 'cancelled'
          operation['outcome'] = result == 'cancelled' ? '' : result
          events << :finalized
          {'status' => operation.fetch('phase')}
        when 'recovery-required'
          operation['phase'] = 'recovery-required' unless %w[complete cancelled].include?(operation.fetch('phase'))
          {'status' => 'recovery-required'}
        else
          raise "unexpected control action #{request['action']}"
        end
      end
      host.define_singleton_method(:require_no_unfinished_lifecycle_operations!) { |**_options| nil }
      host.define_singleton_method(:portal_service_state) { |_unit| {'ActiveState' => 'active', 'MainPID' => Process.pid.to_s} }
      host.define_singleton_method(:drain_portal_services) do |except: []|
        raise 'lost restoration set' unless operation.fetch('drainedPortals').include?('workspace-portal@example-workspace.service')
        events << :writers_joined
        operation.fetch('drainedPortals')
      end
      host.define_singleton_method(:quiesce_sessions) do |**options|
        raise 'terminal missing maintenance authority' unless excluded && options[:maintenance_operation] == identity
        events << :terminals_quiesced
        []
      end
      host.define_singleton_method(:maintenance_verify_source_exit!) { |_operation| events << :successful_exit }
      host.define_singleton_method(:activate_candidate) do |_candidate, **_options|
        raise 'profile selected without durable target intent' unless operation['activationIntent']
        events << :profile
        {'profileToken' => send(:profile_link_token)}
      end
      host.define_singleton_method(:complete_candidate_activation) { |_activation| events << :ready }
      host.define_singleton_method(:maintenance_restore_writers) do |owned|
        raise 'foreign portal restoration' unless owned.fetch('drainedPortals', []) == operation.fetch('drainedPortals')
        events << :writers_restored
        if fault == :portal && !fault_raised
          fault_raised = true
          raise DevWorkspaceHost::Error, 'portal restoration interrupted'
        end
      end
      host.define_singleton_method(:restore_quiesced_sessions) do |_sessions, **_options|
        events << :terminals_restored
        if fault == :terminal && !fault_raised
          fault_raised = true
          raise DevWorkspaceHost::Error, 'terminal restoration interrupted'
        end
      end
      yield host, operation, events
    end
  end
end

class WorkspaceHostTest < Minitest::Test
  def test_maintenance_holds_inactive_native_until_exact_target_grant
    with_transition_host do |host, paths|
      entry = host.send(:registry).entries.first
      directory = File.join(host.instance_variable_get(:@state), 'maintenance')
      FileUtils.mkdir_p(directory)
      File.write(File.join(directory, 'startup.json'), '{}')
      operation = {'schema' => 2, 'operationId' => 'own', 'phase' => 'pausing', 'services' => [], 'activationIntent' => false}
      startup = {'pending' => false}
      host.define_singleton_method(:maintenance_control) { |_, **| {'status' => 'pausing', 'operation' => operation} }
      host.define_singleton_method(:maintenance_startup_selection) { startup }
      assert_raises(DevWorkspaceHost::Error) { host.send(:maintenance_native_admission!, entry) }
      operation['activationIntent'] = true
      startup = {'pending' => true, 'operationId' => 'wrong'}
      assert_raises(DevWorkspaceHost::Error) { host.send(:maintenance_native_admission!, entry) }
      startup = {'pending' => true, 'operationId' => 'own'}
      host.send(:maintenance_native_admission!, entry)
      operation['phase'] = 'complete'
      startup = {'pending' => false}
      host.send(:maintenance_native_admission!, entry)
      # Old owner semantics remain service-scoped rather than adopting schema2.
      operation['schema'] = 1
      operation['phase'] = 'awaiting-ready'
      host.send(:maintenance_native_admission!, entry)
      refute(host.events.any? { |event| event.first == :profile_set })
    end
  end
end

class WorkspaceHostTest < Minitest::Test
  def test_cancellation_resume_serializes_native_stop_after_admitted_dispatch
    with_maintenance_worker(cancel: true) do |host, operation, _events|
      original_lock = host.method(:with_transition_lock)
      real_lock = DevWorkspaceHost::Host.instance_method(:with_transition_lock).bind(host)
      host.define_singleton_method(:with_transition_lock) do |mode = File::LOCK_EX, &block|
        real_lock.call(mode) { original_lock.call(&block) }
      end
      entered, release, stop_recorded, stop_blocked = Queue.new, Queue.new, Queue.new, Queue.new
      native = []
      original_control = host.method(:maintenance_control)
      host.define_singleton_method(:maintenance_control) do |request, **options|
        if request.fetch('action') == 'continue'
          # This is after admission/final StopRevision read, before native Send.
          entered << true
          release.pop
          native << :resume_dispatch
        end
        original_control.call(request, **options)
      end
      worker = Thread.new { host.send(:maintenance_cancel_restore, JSON.parse(JSON.generate(operation))) }
      Timeout.timeout(3) { entered.pop }
      stop = Thread.new do
        stop_recorded << true # Stop intent stays ahead of shared exclusion.
        path = host.send(:transition_lock_file)
        File.open(path, File::RDWR) do |file|
          acquired = file.flock(File::LOCK_SH | File::LOCK_NB)
          stop_blocked << !acquired
          file.flock(File::LOCK_SH) unless acquired
          native << :stop_interrupt
        end
      end
      Timeout.timeout(3) { stop_recorded.pop }
      assert(Timeout.timeout(3) { stop_blocked.pop }, 'Stop native interrupt escaped the cancellation exclusion')
      assert_empty(native)
      release << true
      worker.value
      stop.value
      assert_equal(%i[resume_dispatch stop_interrupt], native)
      assert_equal('cancelled', operation.fetch('phase'))
    ensure
      release << true if release && worker&.alive?
      worker&.join
      stop&.join
    end
  end

  def test_cancellation_resume_rechecks_source_and_token_before_dispatch
    %w[sourcePackage sourceProfileToken profile operationId].each do |field|
      with_maintenance_worker(cancel: true) do |host, operation, events|
        original = host.method(:maintenance_control)
        host.define_singleton_method(:maintenance_control) do |request, **options|
          result = original.call(request, **options)
          if request.fetch('action') == 'inspect'
            result.fetch('operation')[field] = field == 'sourcePackage' ? '/wrong-source' : 'wrong-identity'
          end
          result
        end
        assert_raises(DevWorkspaceHost::Error) { host.send(:maintenance_cancel_restore, operation) }
        refute_includes(events, :continuation)
        refute_includes(events, :terminals_restored)
      end
    end
  end
end

require_relative '../support/coordination_fixture'

class WorkspaceHostTest < Minitest::Test
  def test_maintenance_source_recovery_validates_retained_attempt_before_intent
    with_maintenance_worker(source_recovered: true) do |host, operation, events|
      fixture = Object.new.extend(CoordinationFixture)
      fixture.setup
      source = operation.fetch('sourcePackage')
      entry = host.send(:registry).entries.first
      fixture.instance_variable_set(:@state, host.instance_variable_get(:@state))
      fixture.instance_variable_set(:@workspace, entry.fetch('root'))
      fixture.instance_variable_set(:@package, source)
      fixture.instance_variable_set(:@profile, host.instance_variable_get(:@profile))
      fixture.instance_variable_set(:@entry, entry.merge('socket' => '/fixture/codex.sock', 'authority' => File.join(source, 'authority'), 'portalUrl' => 'https://fixture.test'))
      catalog = fixture.instance_variable_get(:@catalog)
      adapter = File.realpath(RbConfig.ruby)
      catalog['adapters'] = {'fixture' => {'protocol' => 1, 'command' => adapter}}
      catalog['machines'] = {'machine' => {'id' => 'fixture:machine', 'aliases' => ['alias'], 'adapter' => 'fixture', 'configurationRepository' => 'code', 'selector' => 'fixture'}}
      catalog['profileMachine'] = 'fixture:machine'
      fixture.install_catalog
      engine = fixture.engine
      fixture.instance_variable_set(:@engine, engine)
      fixture.git('clone', '--bare', fixture.instance_variable_get(:@remote), File.join(entry.fetch('root'), 'repos/code.git'))
      person = fixture.owner('maintenance-claim')
      target = make_package(File.dirname(source), 'original-target')
      prepared = operation.fetch('preparation').merge('candidatePackage' => target, 'coverage' => [])
      artifact = engine.save_artifact(prepared)
      snapshot = engine.prepare_snapshot(person, 'profile_activate', {'preparedArtifact' => artifact.fetch('id'), 'candidatePackage' => target,
        'expectedProfileToken' => artifact.fetch('profileToken'), 'coverage' => []}, fixture.evidence(person))
      approval = fixture.approval(person, snapshot, kind: 'deploy', scope: [{'machine' => 'fixture:machine', 'actions' => ['profile_activate'], 'reboot' => false}])
      reservation = engine.mutate(person, 'reserve', fixture.request('target' => 'alias', 'mode' => 'deploy_first', 'approvalId' => approval, 'integrationObligations' => []))
      engine.mutate(person, 'submit', fixture.request('kind' => 'profile_activate', 'dependsOn' => [], 'approvalId' => approval, 'reservationId' => reservation.fetch('id'), 'snapshot' => snapshot))
      job = engine.claim(owner_id: person.fetch('id')).first
      operation['preparation'] = artifact.merge('coordinationAttempt' => job.fetch('attempt'))
      operation['candidatePackage'] = target
      operation['cutoverIntent'] = true
      operation['activationIntent'] = false
      operation['sourcesStopped'] = false # validation must not depend on this later receipt
      host.define_singleton_method(:coordination_store) { engine.store }
      host.define_singleton_method(:coordination_catalog) { engine.catalog }
      host.define_singleton_method(:coordination_engine) { |_| engine }
      host.instance_variable_set(:@coordination_attempt, job.fetch('attempt'))
      host.instance_variable_set(:@maintenance_operation, operation.fetch('operationId'))
      refute(host.instance_variable_get(:@env).key?('DEV_WORKSPACE_CLAIM_ID'))
      contract = File.join(source, 'share/dev-workspace/coordination-contract.json')
      File.write(contract, JSON.generate(WorkspaceCoordination::CONTRACT))
      source_view = artifact.merge('candidatePackage' => source, 'candidateCodex' => artifact.fetch('sourceCodex'),
        'maintenanceTarget' => artifact.fetch('sourceMaintenanceTarget'), 'plansDigest' => artifact.fetch('sourcePlansDigest'), 'coordinationAttempt' => job.fetch('attempt'))
      # The real package compatibility reader requires canonical production
      # metadata; no remote command is executed by this profile-only fixture.
      catalog['repositories']['code']['remote'] = 'git@github.com:example/maintenance-fixture.git'
      fixture.install_catalog
      original = JSON.parse(JSON.generate(operation))
      original_ledger = engine.store.read
      host.send(:with_transition_lock) { host.send(:validate_candidate_preparation!, source, prepared: source_view) }
      mutations = [
        ->(o) { o['sourceRecovered'] = false }, ->(o) { o['cutoverIntent'] = false },
        ->(o) { o['sourcePackage'] = '/wrong-source' }, ->(o) { o['sourceProfileToken'] = 'wrong-token' },
        ->(o) { o['operationId'] = SecureRandom.uuid }, ->(o) { o['profile'] = '/wrong-profile' },
        ->(o) { o['preparation']['coordinationAttempt'] = SecureRandom.uuid },
        ->(o) { o['preparation']['id'] = 'sha256:' + 'f' * 64 },
        ->(o) { o['preparation']['pauseTimeout'] = 123 },
        ->(o) { o['activationNative'] = '/conflicting-intent' }
      ]
      mutations.each do |mutate|
        operation.replace(JSON.parse(JSON.generate(original))); mutate.call(operation)
        error = assert_raises(WorkspaceCoordination::Error) do
          host.send(:with_transition_lock) { host.send(:validate_candidate_preparation!, source, prepared: source_view) }
        end
        assert_match(/exact admitted machine claim|prepared artifact/, error.message)
        refute_includes(events, :activation_intent)
      end
      operation.replace(JSON.parse(JSON.generate(original)))
      artifact_path = File.join(engine.store.root, 'sources', artifact.fetch('id').delete_prefix('sha256:'), 'artifact.json')
      artifact_bytes = File.binread(artifact_path)
      File.write(artifact_path, JSON.generate(artifact.merge('pauseTimeout' => 999)))
      assert_raises(WorkspaceCoordination::Error) do
        host.send(:with_transition_lock) { host.send(:validate_candidate_preparation!, source, prepared: source_view) }
      end
      File.binwrite(artifact_path, artifact_bytes)
      control = host.method(:maintenance_control)
      host.define_singleton_method(:maintenance_control) do |request, **options|
        if request.fetch('action') == 'source-current'
          events << :validated_before_stop
        elsif request.fetch('action') == 'activation-intent'
          raise 'source target changed' unless request.fetch('operation').fetch('activationPackage') == source
          raise 'intent before validation/stop' unless events.include?(:validated_before_stop) && events.include?(:successful_exit)
        end
        control.call(request, **options)
      end
      host.send(:maintenance_worker_command, [operation.fetch('operationId')])
      assert_equal('cancelled', operation.fetch('phase'))
      assert_equal('source-recovered', operation.fetch('outcome'))
      assert_operator(events.index(:validated_before_stop), :<, events.index(:activation_intent))
      assert_equal(original_ledger, engine.store.read, 'recovery rewrote the attempt or reservation')
      assert_equal(artifact, engine.artifact!(artifact.fetch('id')))
      assert_equal(target, operation.fetch('preparation').fetch('candidatePackage'))
      operation.replace(JSON.parse(JSON.generate(original)))
      host.instance_variable_set(:@maintenance_operation, operation.fetch('operationId'))
      host.instance_variable_set(:@coordination_attempt, job.fetch('attempt'))
      File.unlink(host.instance_variable_get(:@profile))
      File.symlink(target, host.instance_variable_get(:@profile))
      assert_raises(WorkspaceCoordination::Error) do
        host.send(:with_transition_lock) { host.send(:validate_candidate_preparation!, source, prepared: source_view) }
      end
      assert_equal(original_ledger, engine.store.read)

    ensure
      fixture&.teardown
    end
  end
end
