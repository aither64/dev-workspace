# frozen_string_literal: true

require 'json'
require 'open3'
require 'securerandom'

module DevWorkspaceHost
  # The Go controller alone interprets and writes private maintenance state.
  # Ruby retains the existing profile/systemd/claim ownership and lock ordering.
  module WorkspaceMaintenanceHost
    private

    def maintenance_control(request, worker: true, workspace: nil)
      workspace ||= registry.entries.first&.fetch('root')
      raise Error, 'maintenance requires a registered workspace' unless workspace

      command = [File.join(package_root, 'bin/workspace-portal'), 'maintenance-control',
                 '--user-state-root', @state, '--host-profile', @profile, '--workspace', workspace]
      command << '--worker' if worker
      output = nil
      status = nil
      Open3.popen3(*command) do |input, result, errors, waiter|
        input.write(JSON.generate(request.merge('schema' => 1, 'invocation' => @env.fetch('INVOCATION_ID', ''))))
        input.close
        diagnostic = Thread.new { errors.read(16 * 1024); errors.each_line { |_line| } }
        output = result.read(4 * 1024 * 1024 + 1) || ''
        status = waiter.value
        diagnostic.value
      end
      raise Error, 'maintenance control failed; retain its journal for recovery' unless status.success?
      raise Error, 'maintenance control response exceeds bounds' if output.bytesize > 4 * 1024 * 1024

      response = JSON.parse(output)
      unless response['schema'] == 1 && response['status'].is_a?(String)
        raise Error, 'maintenance control returned an invalid response'
      end
      if response['status'] == 'refused'
        action = request['action']
        action = 'unknown' unless action.is_a?(String) && action.match?(/\A[a-z][a-z0-9-]{0,63}\z/)
        operation = request['operationId']
        operation = 'unknown' unless operation.is_a?(String) && operation.match?(/\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/)
        raise Error, "maintenance control refused: #{response.fetch('reason', 'unknown')} (action=#{action} operation=#{operation})"
      end

      response
    end

    def maintenance_command(argv)
      action = argv.shift
      identity = argv.shift
      if action == 'recover-startup'
        return recover_maintenance_startup(identity, argv)
      end
      unless %w[status startup-status cancel recover].include?(action) && argv.empty?
        raise Error, 'maintenance takes status, startup-status, cancel, recover or recover-startup'
      end
      if action == 'status' && !identity
        @out.puts JSON.generate(maintenance_control({'action' => 'active'}))
        return
      end
      require_startup_uuid!(identity)
      if action == 'recover'
        system! 'systemctl', '--user', 'start', "workspace-maintenance@#{identity}.service"
      elsif action == 'startup-status'
        response = maintenance_control({'action' => 'startup-status', 'operationId' => identity})
        raise Error, 'startup status belongs to another operation' unless response.fetch('startup').fetch('operationId') == identity
        @out.puts JSON.generate(response)
      else
        @out.puts JSON.generate(maintenance_control({'action' => action, 'operationId' => identity}, worker: false))
      end
    end

    def require_startup_uuid!(identity)
      raise Error, 'maintenance requires an exact operation UUID' unless identity&.match?(/\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/)
    end

    def require_startup_only!(identity)
      # A partial, symlinked or malformed full owner is never startup-only.
      path = File.join(@state, 'maintenance', identity)
      raise Error, 'startup recovery belongs to its full operation owner' if File.exist?(path) || File.symlink?(path)
      require_no_foreign_maintenance!
    end

    def startup_grants(plans, native)
      plans.map do |entry, plan|
        {'unit' => "workspace-codex@#{entry.fetch('name')}.service", 'workspace' => entry.fetch('root'),
         'native' => native, 'argvDigest' => plan.digest}
      end
    end

    def observe_existing_startup(selection, plans, native, require_running: false)
      plans.each do |entry, plan|
        unit = "workspace-codex@#{entry.fetch('name')}.service"
        state = portal_service_state(unit)
        if %w[inactive failed].include?(state.fetch('ActiveState')) && state.fetch('MainPID') == '0'
          raise Error, 'startup recovery requires the existing ready native without restart' if require_running
          next
        end
        saved = selection.fetch('launchGrants').find { |grant| grant.fetch('unit') == unit }
        raise Error, 'registered native has no saved startup grant' unless saved
        # Real launches retain their consumed-before-exec attempt and use the
        # existing reconciler. An observation must not replace that evidence.
        next if !require_running && saved.fetch('invocation', '') != ''
        marker_path = registration_marker_path(entry)
        bytes = File.binread(marker_path, REGISTRATION_OUTPUT_MAX_BYTES + 1)
        marker = load_registration_marker(entry)
        unless state.fetch('ActiveState') == 'active' && marker_has_semantics?(marker, native, plan) &&
               live_package_switch_contract?(marker.dig('launch', 'package_root'))
          raise Error, 'existing native registration is not ready for startup observation'
        end
        unless File.binread(marker_path, REGISTRATION_OUTPUT_MAX_BYTES + 1) == bytes
          raise Error, 'existing native registration changed during startup observation'
        end
        maintenance_control({'action' => 'startup-observe-existing', 'operationId' => selection.fetch('operationId'),
                             'grant' => startup_grants([[entry, plan]], native).first,
                             'markerSHA256' => Digest::SHA256.hexdigest(bytes),
                             'startup' => {'candidatePackage' => selection.fetch('candidatePackage'),
                                           'authorizedProfileToken' => profile_link_token}})
      end
    end

    def verify_startup_timers!(package)
      %w[auto-archive tracking-sync].each do |family|
        next unless File.file?(File.join(package, "share/systemd/user/workspace-#{family}@.timer"))
        registry.entries.each do |entry|
          unit = "workspace-#{family}@#{entry.fetch('name')}.timer"
          unless capture!('systemctl', '--user', 'is-enabled', unit).strip == 'enabled' &&
                 capture!('systemctl', '--user', 'is-active', unit).strip == 'active'
            raise Error, 'completed startup timer effects changed'
          end
        end
      end
    end

    def recover_maintenance_startup(identity, argv)
      require_startup_uuid!(identity)
      options = {}
      OptionParser.new do |opts|
        opts.on('--source PATH') { |value| options[:source] = value }
        opts.on('--from-candidate') { options[:candidate] = true }
      end.parse!(argv)
      raise Error, 'recover-startup requires --source PATH' unless argv.empty? && options[:source]
      source = File.realpath(File.expand_path(options.fetch(:source)))
      source_identity = coordination_source_identity(source)
      outputs = coordination_build(source).lines.map(&:strip).reject(&:empty?).uniq
      unless outputs.length == 1 && File.realpath(outputs.first) == File.realpath(package_root)
        raise Error, 'startup recovery executor must match the exact source build'
      end
      action = lambda do
        require_no_unfinished_lifecycle_operations!(operation: 'maintenance startup recovery')
        require_startup_only!(identity)
        raise Error, 'startup recovery source changed' unless coordination_source_identity(source) == source_identity
        selection = maintenance_startup_selection
        selected = File.realpath(@profile)
        token = profile_link_token
        unless selection.fetch('operationId') == identity && selection.fetch('targetEnabled') &&
               selection.fetch('candidatePackage') == selected && token &&
               selection.fetch('sourceProfileToken') == token && selection.fetch('authorizedProfileToken') == token &&
               (selection.fetch('pending') || selection.fetch('enabled'))
          raise Error, 'startup recovery requires the exact same-package enable intent'
        end
        require_coordination_profile_claim!(selected)
        native = File.realpath(codex_for_package(selected))
        plans = with_maintenance_registration_target(selection.fetch('targetEnabled')) { registration_plans(package: selected) }
        saved_grants = selection.fetch('launchGrants').map { |g| g.slice('unit', 'workspace', 'native', 'argvDigest') }
        raise Error, 'startup recovery registered plans changed' unless saved_grants == startup_grants(plans, native)
        expected_pending = {'schema' => AGENT_TEAM_REGISTRATION.fetch('pendingSchema'), 'package_root' => selected,
                            'codex_path' => native, 'workspaces' => plans.map { |entry, plan|
                              {'name' => entry.fetch('name'), 'root' => entry.fetch('root'), 'registration_digest' => plan.digest}
                            }.sort_by { |entry| entry.fetch('name') }}
        pending = pending_codex_update
        raise Error, 'startup recovery pending target changed' if pending && pending != expected_pending
        verify_registration_semantics!(plans, native)
        observe_existing_startup(selection, plans, native, require_running: true)
        response = maintenance_control({'action' => 'startup-reconcile', 'operationId' => identity,
                                       'startup' => {'candidatePackage' => selected, 'authorizedProfileToken' => token}})
        raise Error, 'startup recovery native readiness remains unproved' unless response.fetch('status') == 'startup-reconciled'
        # Restore only the selected package's ordinary timers. Failure preserves
        # the original pending intent and ready evidence for the same command.
        if selection.fetch('pending')
          configure_auto_archive_services(package: selected)
        else
          verify_startup_timers!(selected)
        end
        unless File.realpath(@profile) == selected && profile_link_token == token &&
               coordination_source_identity(source) == source_identity && pending_codex_update == pending
          raise Error, 'startup recovery completion was superseded'
        end
        clear_pending_codex_update if pending
        result = maintenance_control({'action' => 'startup-finish', 'operationId' => identity,
                                      'startup' => {'candidatePackage' => selected, 'authorizedProfileToken' => token}})
        @out.puts JSON.generate(result)
      end
      if options[:candidate]
        with_candidate_generation_lock { action.call }
      else
        with_current_generation_lock { action.call }
      end
    end

    def maintenance_startup_selection
      path = File.join(@state, 'maintenance', 'startup.json')
      return {'enabled' => false, 'pending' => false} unless File.exist?(path) || File.symlink?(path)

      maintenance_control({'action' => 'startup-status'}).fetch('startup')
    end

    # Startup-only selection also uses a pending grant, but ordinary switches
    # must restore normally. Only a real nonterminal operation holds writers.
    def maintenance_completion_held?
      return false if Dir.glob(File.join(@state, 'maintenance', '**', '*.json')).empty?

      response = maintenance_control({'action' => 'active'})
      response.fetch('status') != 'none' && !%w[complete cancelled].include?(response.fetch('operation').fetch('phase'))
    end

    def maintenance_registration_target
      return @maintenance_registration_target unless @maintenance_registration_target.nil?

      maintenance_startup_selection.fetch('enabled')
    end

    def with_maintenance_registration_target(target)
      previous = @maintenance_registration_target
      @maintenance_registration_target = target
      yield
    ensure
      @maintenance_registration_target = previous
    end

    def maintenance_package_support?(package)
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      return false unless File.file?(path)

      contract = parse_strict_json_object(File.binread(path, 64 * 1024 + 1), 'maintenance support contract', 64 * 1024)
      %w[maintenanceStateSchema maintenancePolicy].all? { |key| contract[key].is_a?(Integer) && contract[key] == 2 }
    rescue Error, JSON::ParserError, SystemCallError
      false
    end

    def require_maintenance_held_target!(package)
      path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      contract = parse_strict_json_object(File.binread(path, 64 * 1024 + 1), 'maintenance held-completion target contract', 64 * 1024)
      unless %w[maintenanceStateSchema maintenancePolicy].all? { |key| contract[key].is_a?(Integer) && contract[key] == 2 } && contract['maintenanceHeldCompletionPolicy'].is_a?(Integer) && contract['maintenanceHeldCompletionPolicy'] == 1
        raise Error, 'maintenance target cannot keep completion held; select a capable target'
      end
    rescue SystemCallError => error
      raise Error, "cannot inspect maintenance held-completion target: #{error.class}"
    end

    def maintenance_switch_required?(prepared)
      return false unless maintenance_package_support?(preselection_session_package)
      plans = with_maintenance_registration_target(prepared.fetch('maintenanceTarget')) { registration_plans(package: prepared.fetch('candidatePackage')) }
      return false unless codex_restart_required?(plans, prepared.fetch('candidateCodex'), package: prepared.fetch('candidatePackage'), predecessor: preselection_session_package)
      require_maintenance_held_target!(prepared.fetch('candidatePackage'))
      true
    end

    def maintenance_startup_request(action, prepared, **extra)
      startup = {'candidatePackage' => prepared.fetch('candidatePackage'),
                 'sourceProfileToken' => prepared.fetch('profileToken'),
                 'targetEnabled' => prepared.fetch('maintenanceTarget', maintenance_registration_target)}.merge(extra.transform_keys(&:to_s))
      maintenance_control({'action' => action, 'operationId' => prepared.fetch('switchOperationId'), 'startup' => startup})
    end

    def authorize_maintenance_startup(prepared)
      selection = maintenance_startup_selection
      return unless selection.fetch('pending')
      unless selection.fetch('operationId') == prepared.fetch('switchOperationId') &&
             File.realpath(@profile) == prepared.fetch('candidatePackage')
        raise Error, 'startup selection requires its exact owning activation'
      end
      plans = with_maintenance_registration_target(selection.fetch('targetEnabled')) { registration_plans(package: prepared.fetch('candidatePackage')) }
      grants = startup_grants(plans, prepared.fetch('candidateCodex'))
      maintenance_startup_request('startup-authorize', prepared,
                                 authorizedProfileToken: profile_link_token, launchGrants: grants)
      loop do
        response = maintenance_startup_request('startup-reconcile', prepared, authorizedProfileToken: profile_link_token)
        break if response.fetch('status') == 'startup-reconciled'
        raise Error, 'startup reconciliation returned an unsupported status' unless response.fetch('status') == 'retry-wait'
        sleep 0.1
      end
    end

    def maintenance_native_admission!(entry)
      return if Dir.glob(File.join(@state, 'maintenance', '**', '*.json')).empty?

      response = maintenance_control({'action' => 'active'})
      return if response.fetch('status') == 'none'
      operation = response.fetch('operation')
      return if %w[complete cancelled].include?(operation.fetch('phase'))
      return unless operation.fetch('schema') == 2 || operation.fetch('services').any? { |service| service.fetch('workspace') == entry.fetch('root') }

      startup = maintenance_startup_selection
      unless startup.fetch('pending') && startup.fetch('operationId') == operation.fetch('operationId') && operation.fetch('activationIntent', false)
        raise Error, 'native startup requires the owning maintenance activation grant'
      end
    end

    def maintenance_native_choice
      selection = maintenance_startup_selection
      selection.fetch(selection.fetch('pending') ? 'targetEnabled' : 'enabled')
    end

    def maintenance_native_startup(entry, native, plan)
      selection = maintenance_startup_selection
      return unless selection.fetch('pending')

      maintenance_control({'action' => 'startup-launch', 'operationId' => selection.fetch('operationId'),
                           'invocation' => @env.fetch('INVOCATION_ID', ''), 'pid' => Process.pid,
                           'grant' => {'unit' => "workspace-codex@#{entry.fetch('name')}.service",
                                       'workspace' => entry.fetch('root'), 'native' => native, 'argvDigest' => plan.digest},
                           'startup' => {'candidatePackage' => File.realpath(@profile),
                                         'authorizedProfileToken' => profile_link_token}})
    end

    def maintenance_activation_target!(held: false)
      selection = maintenance_startup_selection
      if held
        response = maintenance_control({'action' => 'active'})
        operation = response.fetch('operation', {})
        token = profile_link_token
        unless private_activation? && response.fetch('status') != 'none' && token &&
               operation['operationId'] == @env['DEV_WORKSPACE_MAINTENANCE_ACTIVATION'] &&
               operation['profile'] == @profile && operation['phase'] == 'activating' &&
               operation['cutoverIntent'] && operation['sourcesStopped'] && operation['activationIntent'] &&
               operation['activationPackage'] == package_root && File.realpath(@profile) == package_root &&
               selection['operationId'] == operation['operationId'] &&
               selection['candidatePackage'] == operation['activationPackage'] &&
               selection['sourceProfileToken'] == operation['sourceProfileToken'] &&
               selection['authorizedProfileToken'] == token
          raise Error, 'held startup selection requires its exact owning activation'
        end
      end
      return selection.fetch('enabled') unless selection.fetch('pending')

      unless @env['DEV_WORKSPACE_MAINTENANCE_ACTIVATION'] == selection.fetch('operationId') &&
             selection.fetch('authorizedProfileToken') == profile_link_token &&
             selection.fetch('candidatePackage') == File.realpath(@profile)
        raise Error, 'pending startup selection requires its owning activation or forward recovery'
      end
      selection.fetch('targetEnabled')
    end

    # Preparation is immutable and lives in the sole Go-owned operation record.
    # The systemd worker, not the portal or caller, owns all waits and recovery.
    def start_maintenance_switch(prepared)
      return false unless maintenance_switch_required?(prepared)
      require_maintenance_held_target!(prepared.fetch('candidatePackage'))

      services = registry.entries.filter_map do |entry|
        unit = "workspace-codex@#{entry.fetch('name')}.service"
        state = portal_service_state(unit)
        next if %w[inactive failed].include?(state.fetch('ActiveState')) && state.fetch('MainPID') == '0'
        raise Error, 'source native service is not settled' unless state.fetch('ActiveState') == 'active'

        service = {'workspace' => entry.fetch('root'), 'unit' => unit, 'pid' => Integer(state.fetch('MainPID')),
                   'socket' => instance_runtime(entry).fetch(:codex)}

        service
      end
      operation = {'schema' => 2, 'operationId' => prepared.fetch('switchOperationId'), 'revision' => 1,
                   'phase' => 'prepared', 'profile' => @profile, 'sourceProfileToken' => prepared.fetch('profileToken'),
                   'sourcePackage' => preselection_session_package, 'candidatePackage' => prepared.fetch('candidatePackage'),
                   'plansDigest' => prepared.fetch('plansDigest'), 'preparation' => prepared,
                   'services' => services, 'participants' => [], 'children' => [], 'blockers' => []}
      # Validate ownership before the operation can notify agents or launch its
      # worker. This short guard never spans readiness or service-drain waits.
      with_transition_lock do
        unless prepared.fetch('profileToken') == profile_link_token &&
               prepared.fetch('sourcePackage') == preselection_session_package &&
               prepared.fetch('sourceIdentity') == coordination_source_identity(prepared.fetch('source'))
          raise Error, 'maintenance source changed during preparation; prepare again'
        end
        require_coordination_profile_claim!(prepared.fetch('candidatePackage'))
        maintenance_control({'action' => 'prepare', 'operationId' => operation.fetch('operationId'), 'operation' => operation})
      end
      {'source' => operation.fetch('sourcePackage'), 'candidate' => operation.fetch('candidatePackage')}.each do |name, package|
        root_codex(File.join(package, 'bin/workspace-host'), File.join(@state, 'maintenance', operation.fetch('operationId'), "#{name}-root"))
      end
      unit = "workspace-maintenance@#{operation.fetch('operationId')}.service"
      @err.puts "workspace maintenance #{operation.fetch('operationId')}; waiting with Cancel available"
      system! 'systemctl', '--user', 'start', '--no-block', unit
      worker_stopped = false
      loop do
        current = maintenance_control({'action' => 'inspect', 'operationId' => operation.fetch('operationId')}).fetch('operation')
        if current.fetch('phase') == 'complete'
          @out.puts "workspace package: #{current.fetch('candidatePackage')}"
          return true
        end
        if current.fetch('phase') == 'cancelled'
          raise Error, current['outcome'] == 'source-recovered' ? 'workspace update was not applied; the selected source package was recovered' : 'workspace maintenance cancelled before native cutover'
        end
        if worker_stopped
          raise Error, "workspace maintenance #{operation.fetch('operationId')} requires maintenance recover; journal retained"
        end
        service = capture!('systemctl', '--user', 'show', unit, '--property=ActiveState', '--value').strip
        # Completion may race the service read. Inspect the same operation once
        # more before declaring recovery necessary for a stopped worker.
        worker_stopped = !%w[active activating].include?(service)
        sleep 1 unless worker_stopped
      end
    end

    def maintenance_worker_command(argv)
      identity = argv.shift
      raise Error, 'maintenance-worker requires one operation UUID' unless argv.empty? && identity&.match?(/\A[0-9a-f-]{36}\z/)
      # Restarts run through the current profile; the retained source remains
      # the protocol owner even after selection changed the profile.
      operation = maintenance_control({'action' => 'inspect', 'operationId' => identity}).fetch('operation')
      owner = operation.fetch('sourcePackage')
      unless File.realpath(owner) == owner && operation.fetch('operationId') == identity
        raise Error, 'maintenance protocol owner changed'
      end
      exec(@env.to_h, File.join(owner, 'bin/workspace-host'), 'maintenance-worker', identity) unless owner == File.realpath(package_root)
      lock = File.join(@state, 'maintenance', identity, 'worker.lock')
      File.open(lock, File::RDWR | File::CREAT, 0o600) do |file|
        raise Error, 'maintenance worker already owns this operation' unless file.flock(File::LOCK_EX | File::LOCK_NB)
        @maintenance_operation = identity
        maintenance_control({'action' => 'worker-claim', 'operationId' => identity,
                             'invocation' => @env.fetch('INVOCATION_ID', ''),
                             'operation' => {'sourcePackage' => File.realpath(@profile), 'sourceProfileToken' => profile_link_token}})
        loop do
          operation = maintenance_control({'action' => 'inspect', 'operationId' => identity}).fetch('operation')
          return if %w[complete cancelled].include?(operation.fetch('phase'))
          prepared = operation.fetch('preparation')
          if operation.fetch('sourceRecovered', false)
            prepared = prepared.merge('candidatePackage' => prepared.fetch('sourcePackage'),
                                      'candidateCodex' => prepared.fetch('sourceCodex'),
                                      'maintenanceTarget' => prepared.fetch('sourceMaintenanceTarget'),
                                      'plansDigest' => prepared.fetch('sourcePlansDigest'))
          end
          @coordination_attempt = prepared['coordinationAttempt']
          @env = @env.to_h.merge(prepared.fetch('coordinationEnvironment', {}))
          if operation['restoration']
            maintenance_finish_restoration(operation)
            return
          end
          unless operation.fetch('cutoverIntent', false)
            raise Error, 'maintenance source generation changed' unless prepared.fetch('profileToken') == profile_link_token
            response = maintenance_control({'action' => 'pause', 'operationId' => identity})
            if response.fetch('status') == 'cancelled'
              maintenance_cancel_restore(operation)
              return
            end
          end
          activation = nil
          cancelled = false
          with_transition_lock do
            operation = maintenance_control({'action' => 'inspect', 'operationId' => identity}).fetch('operation')
            if operation.fetch('cancelRequested', false) && !operation.fetch('cutoverIntent', false)
              cancelled = true
              next
            end
            unless operation.fetch('cutoverIntent', false) || prepared.fetch('profileToken') == profile_link_token
              raise Error, 'maintenance source profile changed; retain operation for recovery'
            end
            validate_candidate_preparation!(prepared.fetch('candidatePackage'), prepared:)
            unless operation.fetch('cutoverIntent', false)
              # Persist the restoration set before a stop can lose its reply.
              portals = operation.fetch('drainedPortals', []) + registry.entries.filter_map do |entry|
                unit = "workspace-portal@#{entry.fetch('name')}.service"
                unit if portal_service_state(unit).fetch('ActiveState') == 'active'
              end
              maintenance_control({'action' => 'writers-drained', 'operationId' => identity, 'operation' => {'drainedPortals' => portals.uniq}})
              drain_portal_services
              quiesce_sessions(package_transition: true, maintenance_operation: identity)
              stop = maintenance_control({'action' => 'stop-intent', 'operationId' => identity})
              if stop.fetch('status') == 'cancelled'
                cancelled = true
                next
              end
            end
            operation = maintenance_control({'action' => 'inspect', 'operationId' => identity}).fetch('operation')
            unless operation.fetch('sourcesStopped', false)
              maintenance_control({'action' => 'source-current', 'operationId' => identity})
              operation.fetch('services').each do |source|
                state = portal_service_state(source.fetch('unit'))
                unless [0, source.fetch('pid')].include?(state.fetch('MainPID').to_i)
                  raise Error, 'source unit was replaced; preserve the owning recovery'
                end
                system! 'systemctl', '--user', 'stop', source.fetch('unit')
              end
              maintenance_verify_source_exit!(operation)
              maintenance_control({'action' => 'sources-stopped', 'operationId' => identity})
            end
              @maintenance_drained = true
              maintenance_control({'action' => 'activation-intent', 'operationId' => identity,
                                   'operation' => {'activationPackage' => prepared.fetch('candidatePackage'),
                                                   'activationNative' => prepared.fetch('candidateCodex'),
                                                   'activationPlansDigest' => prepared.fetch('plansDigest')}})
              activation = activate_candidate(prepared.fetch('candidatePackage'), prepared: prepared)
          end
          if cancelled
            maintenance_cancel_restore(operation)
            return
          end
          complete_candidate_activation(activation)
          response = nil
          with_transition_lock do
            raise Error, 'maintenance activation was superseded' unless profile_link_token == activation.fetch('profileToken')
            response = maintenance_control({'action' => 'continue', 'operationId' => identity})
          end
          raise Error, 'maintenance continuation outcome remains unknown; recover the same operation' unless response.fetch('status') == 'continued'
          operation = maintenance_control({'action' => 'inspect', 'operationId' => identity}).fetch('operation')
          outcome = operation.fetch('sourceRecovered', false) ? 'source-recovered' : 'updated'
          maintenance_control({'action' => 'restoration-begin', 'operationId' => identity,
                               'restoration' => {'outcome' => outcome, 'package' => prepared.fetch('candidatePackage'),
                                                 'profileToken' => activation.fetch('profileToken')}})
          operation = maintenance_control({'action' => 'inspect', 'operationId' => identity}).fetch('operation')
          maintenance_finish_restoration(operation)
          return
        end
      end
    rescue StandardError => primary_error
      raise primary_error unless @maintenance_operation

      recovery_errors = []
      capture_recovery_error(recovery_errors, 'failed to record maintenance recovery status') do
        maintenance_control({'action' => 'recovery-required', 'operationId' => identity})
      end
      raise primary_error if recovery_errors.empty?
      raise Error, ([primary_error.message] + recovery_errors).join('; ')
    ensure
      @maintenance_operation = nil
      @maintenance_drained = nil
      @coordination_attempt = nil
    end

    def maintenance_verify_source_exit!(operation)
      operation.fetch('services').each do |source|
        state = capture!('systemctl', '--user', 'show', source.fetch('unit'), '--property=ActiveState,MainPID,ControlPID,Result').lines.to_h { |line| line.strip.split('=', 2) }
        unless state['ActiveState'] == 'inactive' && state['MainPID'] == '0' && state['ControlPID'] == '0' && state['Result'] == 'success'
          raise Error, 'owned source shutdown failed or processes remain; recover the same operation'
        end
      end
    end

    def maintenance_terminal_sessions(operation)
      operation = maintenance_control({'action' => 'terminal-restoration', 'operationId' => operation.fetch('operationId')}).fetch('operation')
      operation.fetch('terminalIntents', []).map do |intent|
        entry = registry.entries.find { |entry| entry.fetch('root') == intent.fetch('workspace') }
        raise Error, 'maintenance terminal workspace changed' unless entry
        [entry, intent.fetch('slug')]
      end
    end

    def maintenance_restore_writers(operation)
      operation.fetch('drainedPortals', []).each do |unit|
        system! 'systemctl', '--user', 'start', unit
        state = portal_service_state(unit)
        unless state['ActiveState'] == 'active' && state['MainPID'].to_i.positive? && state['ControlPID'] == '0'
          raise Error, 'owned portal writer restoration is not confirmed'
        end
      end
    end

    def maintenance_cancel_restore(operation)
      identity = operation.fetch('operationId')
      cleanup = maintenance_control({'action' => 'cancelled', 'operationId' => identity})
      raise Error, 'pause notice cleanup remains uncertain; recover the same operation' unless cleanup.fetch('status') == 'cancelling'
      with_transition_lock do
        current = maintenance_control({'action' => 'inspect', 'operationId' => identity}).fetch('operation')
        unless current.fetch('operationId') == identity && current.fetch('profile') == @profile &&
               current.fetch('cancelRequested', false) && !current.fetch('cutoverIntent', false) &&
               !%w[complete cancelled].include?(current.fetch('phase')) &&
               current.fetch('sourcePackage') == operation.fetch('sourcePackage') &&
               current.fetch('sourceProfileToken') == operation.fetch('sourceProfileToken') &&
               File.realpath(@profile) == current.fetch('sourcePackage') &&
               profile_link_token == current.fetch('sourceProfileToken')
          raise Error, 'cancellation source or ownership changed; recover the same operation'
        end
        response = maintenance_control({'action' => 'continue', 'operationId' => identity})
        raise Error, 'cancel resume remains uncertain; recover the same operation' unless response.fetch('status') == 'continued'
      end
      maintenance_control({'action' => 'restoration-begin', 'operationId' => identity,
                           'restoration' => {'outcome' => 'cancelled', 'package' => operation.fetch('sourcePackage'),
                                             'profileToken' => operation.fetch('sourceProfileToken')}})
      operation = maintenance_control({'action' => 'inspect', 'operationId' => identity}).fetch('operation')
      maintenance_finish_restoration(operation)
    end

    def maintenance_finish_restoration(operation)
      intent = operation.fetch('restoration')
      identity = operation.fetch('operationId')
      if intent.fetch('outcome') == 'cancelled'
        maintenance_control({'action' => 'cancel-notices', 'operationId' => identity})
      end
      with_transition_lock do
        unless profile_link_token == intent.fetch('profileToken') && File.realpath(@profile) == intent.fetch('package')
          raise Error, 'owned restoration package or profile changed'
        end
        # Native readiness/continuation is already settled. This grants only
        # the recorded sync/start effects; ordinary new-work admission stays shut.
        restore_quiesced_sessions(maintenance_terminal_sessions(operation), package: intent.fetch('package'), maintenance_restoration: identity)
        maintenance_restore_writers({'drainedPortals' => intent.fetch('portals')})
        maintenance_control({'action' => 'restoration-complete', 'operationId' => identity, 'restoration' => intent})
      end
    end

    def require_no_foreign_maintenance!
      return if Dir.glob(File.join(@state, 'maintenance', '**', '*.json')).empty?

      response = maintenance_control({'action' => 'active'})
      return if response.fetch('status') == 'none'
      operation = response.fetch('operation')
      return if %w[complete cancelled].include?(operation.fetch('phase'))
      owner = @maintenance_operation || (private_activation? && @env['DEV_WORKSPACE_MAINTENANCE_ACTIVATION'])
      raise Error, 'workspace maintenance must finish or recover before another transition' unless owner == operation.fetch('operationId')
    end

    # Only unrelated lifecycle-recovery equality ignores this additive host
    # behavior. Absence here never grants maintenance target admission.
    def maintenance_recovery_contract(contract)
      maintenance_predecessor_contract(contract)
    end

    # Only archive/failed-creation descriptor equality ignores these host fields.
    # This projection grants neither maintenance admission nor journal ownership.
    def maintenance_predecessor_contract(contract)
      pair = %w[maintenanceStateSchema maintenancePolicy]
      absent = pair.none? { |key| contract.key?(key) }
      if absent && Dir.glob(File.join(@state, 'maintenance', '**', '*.json')).any?
        raise Error, 'predecessor cannot recover retained maintenance state'
      end
      known = [1, 2].any? { |value| pair.all? { |key| contract[key].is_a?(Integer) && contract[key] == value } }
      raise Error, 'unsupported maintenance recovery contract' unless absent || known
      %w[maintenanceMcpPolicy maintenanceHeldCompletionPolicy].each do |key|
        if contract.key?(key) && !(contract[key].is_a?(Integer) && contract[key] == 1)
          raise Error, 'unsupported maintenance recovery capability'
        end
      end
      contract.reject { |key, _| (pair + %w[maintenanceMcpPolicy maintenanceHeldCompletionPolicy]).include?(key) }
    end

    def require_compatible_maintenance!(candidate)
      directory = File.join(@state, 'maintenance')
      return unless File.directory?(directory) && !Dir.glob(File.join(directory, '**', '*.json')).empty?

      path = File.join(candidate, 'share/workspace-portal/runtime-contract.json')
      contract = parse_strict_json_object(File.binread(path, 64 * 1024 + 1), 'maintenance reader contract', 64 * 1024)
      unless %w[maintenanceStateSchema maintenancePolicy].all? { |key| contract[key].is_a?(Integer) && contract[key] == 2 }
        raise Error, 'candidate cannot read retained maintenance state; preserve it and recover forward'
      end
    rescue SystemCallError => error
      raise Error, "cannot inspect maintenance compatibility: #{error.class}"
    end
  end
end
