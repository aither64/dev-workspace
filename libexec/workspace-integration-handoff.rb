# frozen_string_literal: true

module WorkspaceCoordination
  # Transport receipts do not prove that an agent accepted responsibility.
  # Optional journal metadata preserves that distinction without holding jobs.
  module IntegrationHandoff
    ACK_LEASE = 300
    REMINDER_LIMIT = 900

    def retain_handoff(value, state)
      item = {'id' => value['id'], 'snapshot' => {'id' => value.fetch('continuation').fetch('generation').to_s}}
      event_id = WorkspaceCoordination.event_id(owner, item, state)
      return if value.dig('handoff', 'eventId') == event_id
      value['handoff'] = {'eventId' => event_id, 'generation' => value['continuation']['generation'],
        'recipient' => owner.fetch('thread'), 'state' => 'pending', 'reminders' => 0,
        'nextReminderAt' => Time.now.to_i + 60, 'leaseUntil' => 0}
      save(value)
    end

    def acknowledge(id, request, recipient:)
      WorkspaceCoordination.keys!(request, %w[schema requestId eventId generation])
      unless request['schema'] == 1 && request['requestId'].is_a?(String) && request['requestId'].match?(UUID) &&
             request['eventId'].is_a?(String) && request['eventId'].match?(UUID) &&
             request['generation'].is_a?(Integer) && request['generation'].between?(0, MAX_COUNTER)
        raise Error, 'acknowledgement requires a schema-1 request and exact event identity'
      end
      result = engine.store.lock("integration-#{id}", nonblock: true) do
        @admission.call do
          engine.notification_owner(owner)
          value = read(id)
          raise Error.new('acknowledgement is not from the retained recipient', 4) unless recipient == owner['thread']
          engine.store.transaction do |data|
            engine.store.request(data, owner['id'], ['integration-ack', id], request) do
              handoff = value['handoff']
              unless handoff && handoff['eventId'] == request['eventId'] && handoff['generation'] == request['generation'] &&
                     handoff['generation'] == value.dig('continuation', 'generation') && recipient == handoff['recipient'] &&
                     value['outcome'] != 'canceled' && (value['phase'] == 'complete' || value.dig('continuation', 'state') == 'needs_attention')
                raise Error.new('acknowledgement does not match this action and its recipient', 4)
              end
              handoff.merge!('state' => 'acknowledged', 'leaseUntil' => Time.now.to_i + ACK_LEASE)
              save(value)
              {'eventId' => handoff['eventId'], 'state' => handoff['state'], 'leaseUntil' => handoff['leaseUntil']}
            end
          end
        end
      end
      result || raise(Error.new('integration driver is already running', 75))
    end

    # Hold only the per-run driver lock while the external caller supervises.
    # Operation claims and session/package admission remain short and separate.
    def supervise(id)
      result = engine.store.lock("integration-#{id}", nonblock: true) do
        @supervised_id = id
        value = read(id)
        raise Error, 'supervised continuation requires a background run' unless value['schema'] == 2
        return finish_supervision(id) if value['phase'] == 'complete'
        @admission.call do
          engine.current_generation!
          current = engine.owner(owner['slug'])
          raise Error.new('integration owner changed', 4) unless current['id'] == owner['id']
          verify_tracking_binding
          paused = paused?(id)
          value['continuation']['state'] = 'runnable'
          retain_handoff(value, 'needs_attention') unless value['handoff']
          value['handoff']['state'] = 'supervised'
          save(value)
          File.unlink(pause_path(id)) if paused
        end
        begin
          loop do
            background_guard!
            result = resume_locked(id)
            if result['phase'] == 'complete'
              break finish_supervision(id)
            end
            sleep 2
          end
        rescue StandardError, SignalException
          if engine.current_generation?
            @admission.call do
              engine.current_generation!
              verify_tracking_binding
              value = read(id)
              value['continuation']['state'] = 'paused'
              save(value)
            end
          end
          raise
        end
      end
      result || raise(Error.new('integration driver is already running', 75))
    ensure
      @supervised_id = nil
    end

    def finish_supervision(id)
      @admission.call do
        engine.notification_owner(owner)
        verify_tracking_binding
        value = read(id)
        value['continuation']['state'] = 'complete'
        save(value)
      end
      status(id)
    end
  end

  class Engine
    def remind_integrations(owner)
      store.integration_journals.each do |run|
        next unless run['ownerId'] == owner['id'] && run['identity'] == owner['identity'] && run['handoff']
        store.lock("integration-#{run['id']}", nonblock: true) do
          path = File.join(store.root, 'integrations', "#{run['id']}.json")
          value = WorkspaceCoordination.private_read(path)
          handoff = value['handoff']
          next unless handoff && %w[pending acknowledged].include?(handoff['state']) && handoff['recipient'] == owner['thread']
          next unless handoff['generation'] == value.dig('continuation', 'generation')
          next if %w[paused held].include?(value.dig('continuation', 'state')) || File.exist?(path.sub(/\.json\z/, '.pause'))
          next unless value['phase'] == 'complete' || value.dig('continuation', 'state') == 'needs_attention'
          now = Time.now.to_i
          next if now < handoff['nextReminderAt'] || now < handoff['leaseUntil']
          store.transaction do |data|
            current_generation!
            current = data['owners'].fetch(owner['id'])
            next if data['suspended'] || current['fence'] || current['identity'] != owner['identity']
            event = data['outbox'][handoff['eventId']]
            next unless event && event['state'] == 'delivered'
            handoff['state'] = 'pending'
            handoff['reminders'] = [handoff['reminders'] + 1, 30].min
            handoff['nextReminderAt'] = now + [60 * (2**handoff['reminders']), IntegrationHandoff::REMINDER_LIMIT].min
            WorkspaceCoordination.atomic(path, value)
            event.merge!('state' => 'retry', 'attempts' => [event['attempts'] + 1, 1_000_000_000].min,
              'retryAt' => now, 'error' => nil)
          end
        end
      end
    end
  end
end
