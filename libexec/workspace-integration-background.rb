# frozen_string_literal: true

module WorkspaceCoordination
  # Owns continuation only; operation workers retain approval, claims and effects.
  module BackgroundIntegration
    def admit(&block)
      @admission.call do
        background_guard! if @background_id || @supervised_id
        block.call
      end
    end

    def pause_path(id)
      path(id).sub(/\.json\z/, '.pause')
    end

    def paused?(id)
      file = pause_path(id)
      return false unless File.exist?(file)
      marker = WorkspaceCoordination.private_read(file)
      unless marker == owner.slice('id', 'identity')
        raise Error.new('background pause belongs to another owner', 4)
      end
      true
    end

    def background_guard!
      engine.current_generation!
      current = engine.store.read
      retained = current['owners'].fetch(owner['id'])
      raise Error.new('background owner is fenced by lifecycle recovery', 75) if retained['fence']
      raise Error.new('background scheduling is suspended', 75) if current['suspended']
      id = @background_id || @supervised_id
      raise Error.new('background integration is paused', 75) if id && paused?(id)
      verify_tracking_binding
    end

    def pause(id)
      value = read(id)
      raise Error, 'only background integration runs can be paused' unless value['schema'] == 2
      return status(id) if value['phase'] == 'complete'
      @admission.call do
        engine.current_generation!
        verify_tracking_binding
        engine.store.transaction do |_data|
          WorkspaceCoordination.atomic(pause_path(id), owner.slice('id', 'identity'))
        end
      end
      engine.store.wake
      status(id)
    end

    def arm_background(id)
      acquired = engine.store.lock("integration-#{id}", nonblock: true) do
        value = read(id)
        raise Error, 'resume a manual integration without --background' unless value['schema'] == 2
        return status(id) if value['phase'] == 'complete'
        @admission.call do
          engine.current_generation!
          verify_tracking_binding
          if paused?(id)
            File.unlink(pause_path(id))
          end
          generation = value['continuation']['generation']
          raise Error.new('background continuation counter exhausted', 5) if generation >= MAX_COUNTER
          value['continuation'] = {'state' => 'runnable', 'generation' => generation + 1, 'observedRevision' => nil}
          value['handoff']['state'] = 'pending' if value['handoff']
          value.delete('reason')
          save(value)
        end
        status(id)
      end
      acquired || raise(Error.new('integration driver is already running; pause remains in effect', 75))
    end

    def background_notice(value, state)
      return if @supervised_id
      @admission.call do
        background_guard!
        retain_handoff(value, state)
        engine.store.transaction do |data|
          item = {'id' => value['id'], 'kind' => 'integration', 'snapshot' => {'id' => value['continuation']['generation'].to_s}}
          engine.event(data, owner, item, state, value['reason'])
        end
      end
    end

    def cancel_unsubmitted(id)
      result = engine.store.lock("integration-#{id}", nonblock: true) do
        value = read(id)
        return status(id) if value['outcome'] == 'canceled'
        unless value['schema'] == 2 && value['phase'] == 'prepare' && value.fetch('jobs').empty? &&
               !value.fetch('steps').key?('submit-batch') && !value['deploymentComplete']
          raise Error.new('only preparation with no submitted operations can be canceled', 5)
        end
        @admission.call do
          engine.current_generation!
          current = engine.owner(owner['slug'])
          raise Error.new('integration owner changed', 4) unless current['id'] == owner['id']
          verify_tracking_binding
          value['outcome'], value['phase'] = 'canceled', 'complete'
          value['finishedAt'] = Time.now.utc.iso8601
          value['continuation']['state'] = 'complete'
          value['handoff']['state'] = 'canceled' if value['handoff']
          save(value)
        end
        status(id)
      end
      result || raise(Error.new('integration driver is already running', 75))
    end

    def run_background(id)
      result = engine.store.lock("integration-#{id}", nonblock: true) { run_background_locked(id) }
      result || raise(Error.new('integration driver is already running', 75))
    end

    def run_background_locked(id)
      @background_id = id
      value = read(id)
      return status(id) unless value['schema'] == 2
      # A dispatched worker can wait across foreground takeover. Repeat the
      # scheduler's exclusion under the run lock, including after abrupt exit.
      return status(id) if value.dig('handoff', 'state') == 'supervised'
      if value.dig('continuation', 'state') == 'needs_attention'
        background_notice(value, 'needs_attention')
        return status(id)
      end
      return status(id) unless %w[runnable waiting].include?(value.dig('continuation', 'state'))
      observed = engine.store.read['revision']
      phase = value['phase']
      refreshing = value.key?('pendingRefresh')
      begin
        background_guard!
        resume_locked(id)
        value = read(id)
        # Replaying a committed receipt can advance the journal without a new
        # ledger revision. Observe the resulting phase before parking the run.
        value['continuation']['state'] = if value['phase'] == 'complete'
          'complete'
        elsif value['phase'] != phase || (refreshing && !value.key?('pendingRefresh'))
          'runnable'
        else
          'waiting'
        end
      rescue Error => error
        value = read(id)
        ledger = engine.store.read
        if !engine.current_generation?
          value['continuation']['state'] = 'runnable'
          observed = nil
        elsif ledger['owners'].fetch(owner['id'])['fence']
          value['continuation']['state'] = 'held'
        elsif paused?(id)
          value['continuation']['state'] = 'paused'
        elsif error.is_a?(AdmissionBusy) || ledger['suspended']
          value['continuation']['state'] = 'waiting'
          observed = nil # Contention can end without changing the ledger.
        else
          value['reason'] = {'code' => error.code, 'message' => WorkspaceCoordination.diagnostic(error.message)}
          value['continuation']['state'] = 'needs_attention'
          # Publish the actionable reason before its event. A crash here is
          # rediscovered below until the deterministic notice exists.
          save(value)
          background_notice(value, 'needs_attention')
        end
      end
      value['continuation']['observedRevision'] = observed
      save(value)
      status(id)
    ensure
      @background_id = nil
    end
  end

  class Engine
    def dispatch_integrations(owner)
      ledger = store.read
      return if ledger['suspended'] || ledger['owners'].fetch(owner['id'])['fence']
      store.integration_journals.each do |run|
        next unless run['schema'] == 2 && run['ownerId'] == owner['id'] && run['identity'] == owner['identity']
        next if File.exist?(File.join(store.root, 'integrations', "#{run['id']}.pause")) && store.background_stopped?(run)
        next if run.dig('handoff', 'state') == 'supervised'
        next unless store.lock("integration-#{run['id']}", nonblock: true) { true }
        state = run.dig('continuation', 'state')
        next if %w[paused held complete].include?(state)
        if state == 'needs_attention'
          item = {'id' => run['id'], 'snapshot' => {'id' => run['continuation']['generation'].to_s}}
          next if ledger['outbox'].key?(WorkspaceCoordination.event_id(owner, item, 'needs_attention'))
          # The worker republishes only this event after an interrupted send;
          # it must not restart the failed operation.
        elsif state == 'waiting' && run.dig('continuation', 'observedRevision') == ledger['revision']
          next
        end
        unit = "workspace-integration-#{run['id']}"
        next if %w[active activating deactivating].include?(capture('systemctl', '--user', 'show', "#{unit}.service", '--property=ActiveState', '--value').strip)
        request = {'schema' => 1, 'runId' => run['id'], 'ownerId' => owner['id'], 'identity' => owner['identity'],
          'workspace' => workspace, 'entry' => entry, 'package' => package, 'profile' => profile}
        request_path = File.join(store.root, 'integration-workers', "#{run['id']}.json")
        WorkspaceCoordination.atomic(request_path, request, limit: MAX_REQUEST)
        retain_root("#{run['id']}-#{Digest::SHA256.hexdigest(package)[0, 12]}", 'integration', package)
        capture('systemd-run', '--user', '--quiet', '--collect', "--unit=#{unit}",
          '--property=Type=exec', '--property=UMask=0077', '--property=Restart=no',
          File.join(package, 'bin/workspace-host'), 'coordination-integration', '--request', request_path)
      end
    end
  end
end
