# frozen_string_literal: true

module WorkspaceCoordination
  # Optional display metadata. Ledger commits remain successful when dates
  # cannot be written, and predecessor packages can ignore this file entirely.
  class Timing
    FIELDS = %w[createdAt stateChangedAt finishedAt heldAt].freeze

    def initialize(root)
      @path = File.join(root, 'timing.json')
    end

    def self.states(data)
      %w[jobs reservations].to_h do |kind|
        [kind, data.fetch(kind).transform_values { |item| item.slice('revision', 'state') }]
      end
    end

    def empty
      {'schema' => 1, 'jobs' => {}, 'reservations' => {}}
    end

    def read
      return empty unless File.exist?(@path) || File.symlink?(@path)
      data = WorkspaceCoordination.private_read(@path)
      WorkspaceCoordination.keys!(data, %w[schema jobs reservations])
      raise Error, 'unsupported timing metadata' unless data['schema'] == 1
      %w[jobs reservations].each do |kind|
        raise Error, 'invalid timing collection' unless data[kind].is_a?(Hash)
        states = kind == 'jobs' ? STATES : RESERVATIONS
        data[kind].each do |id, item|
          WorkspaceCoordination.keys!(item, %w[revision state], FIELDS)
          unless id.match?(UUID) && item['revision'].is_a?(Integer) && item['revision'].between?(1, MAX_COUNTER) && states.include?(item['state'])
            raise Error, 'invalid timing identity'
          end
          FIELDS.each do |field|
            next unless item.key?(field)
            value = item[field]
            unless value.is_a?(String) && value.match?(/\A\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\z/) && Time.iso8601(value).utc.iso8601 == value
              raise Error, 'invalid event time'
            end
          end
        end
      end
      data
    rescue StandardError
      empty
    end

    def matching(data, kind, item)
      value = data.fetch(kind)[item.fetch('id')]
      value && value.slice('revision', 'state') == item.slice('revision', 'state') ? value.slice(*FIELDS) : {}
    end

    # Called only after authoritative publication, still under the ledger mutex.
    # A crash between the files leaves unavailable dates, never a false event.
    def publish(before, after)
      return if before == self.class.states(after)
      metadata = read
      now = Time.now.utc.iso8601
      %w[jobs reservations].each do |kind|
        metadata[kind].select! { |id, _| after.fetch(kind).key?(id) }
        after.fetch(kind).each do |id, item|
          previous = before.fetch(kind)[id]
          identity = item.slice('revision', 'state')
          next if previous == identity
          retained = metadata[kind][id]
          dates = retained && retained.slice('revision', 'state') == previous ? retained.slice(*FIELDS) : {}
          dates['createdAt'] = now unless previous
          if !previous || previous['state'] != item['state']
            dates['stateChangedAt'] = now
            terminal = kind == 'jobs' ? TERMINAL.include?(item['state']) : item['state'] == 'released'
            terminal ? dates['finishedAt'] = now : dates.delete('finishedAt')
            dates['heldAt'] ||= now if kind == 'reservations' && item['state'] == 'held'
          end
          metadata[kind][id] = identity.merge(dates)
        end
      end
      WorkspaceCoordination.atomic(@path, metadata)
    rescue StandardError => e
      warn "coordination timing is unavailable (#{e.class})"
    end
  end
end
