# Store and Policy copied unchanged from c2182254d29c4175c3e9fe4267e5a92dd1da6b79,
# libexec/workspace-auto-archive.rb, before module Runner. Test-only predecessor.
# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'securerandom'
require 'time'

module WorkspaceAutoArchive
  class Error < StandardError; end
  class ObservationFailure < Error
    attr_reader :code, :category
    def initialize(code, message, category: 'activity_unknown')
      @code, @category = code, category
      super(message)
    end
  end

  # Private sidecars deliberately leave the session and lifecycle journal formats
  # unchanged. The workspace root is part of every record's identity.
  class Store
    attr_reader :root, :workspace

    def initialize(state_root:, workspace:)
      @workspace = File.realpath(workspace)
      @root = File.join(state_root, 'auto-archive', Digest::SHA256.hexdigest(@workspace))
    end

    def lock(name = 'state', nonblock: false)
      FileUtils.mkdir_p(root, mode: 0o700)
      File.open(File.join(root, "#{name}.lock"), File::RDWR | File::CREAT, 0o600) do |file|
        mode = File::LOCK_EX | (nonblock ? File::LOCK_NB : 0)
        raise Error, 'an automatic archive scan is already running' unless file.flock(mode)
        yield
      end
    end

    def read(name)
      path = File.join(root, "#{name}.json")
      begin
        info = File.lstat(path)
      rescue Errno::ENOENT
        return nil
      end
      raise Error, "automatic archive state is a symlink: #{path}" if File.symlink?(path)
      raise Error, 'automatic archive state is not a regular file' unless info.file?
      data = File.binread(path, 1024 * 1024 + 1)
      raise Error, 'automatic archive state exceeds 1 MiB' if data.bytesize > 1024 * 1024
      record = JSON.parse(data)
      unless record.is_a?(Hash) && record['schema'] == 1 && record['workspace'] == workspace
        raise Error, "invalid automatic archive state: #{path}"
      end
      record
    rescue JSON::ParserError => e
      raise Error, "invalid automatic archive state: #{e.message}"
    end

    def write(name, record)
      FileUtils.mkdir_p(root, mode: 0o700)
      path = File.join(root, "#{name}.json")
      temporary = "#{path}.#{SecureRandom.hex(8)}.tmp"
      payload = record.merge('schema' => 1, 'workspace' => workspace)
      File.open(temporary, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
        file.write(JSON.pretty_generate(payload) + "\n")
        file.flush
        file.fsync
      end
      File.rename(temporary, path)
      File.open(root) { |file| file.fsync }
    ensure
      File.unlink(temporary) if temporary && File.exist?(temporary)
    end

    def policy
      value = read('policy') || { 'enabled' => false, 'epoch' => nil }
      unless [true, false].include?(value['enabled']) &&
             (!value['enabled'] || valid_time?(value['epoch']))
        raise Error, 'invalid automatic archive policy'
      end
      value
    end

    def session(slug)
      raise Error, 'invalid automatic archive session' unless slug.match?(/\A[A-Za-z0-9][A-Za-z0-9_-]*\z/)
      value = read("session-#{slug}") || { 'slug' => slug, 'hold' => false }
      unless value['slug'] == slug && [true, false].include?(value['hold']) &&
             %w[idle_since checked_at reset_at eligible_at last_attempt_at].all? { |key| !value[key] || valid_time?(value[key]) } &&
             (!value.key?('fingerprint_version') || value['fingerprint_version'] == 1) &&
             (!value.key?('activity_known') || [true, false].include?(value['activity_known'])) &&
             (!value.key?('continuity_lost') || [true, false].include?(value['continuity_lost'])) &&
             (!value.key?('dimensions') || (value['dimensions'].is_a?(Hash) && value['dimensions'].keys.all? { |key| %w[activity obligations lifecycle artifacts heads worktrees content].include?(key) }))
        raise Error, 'invalid automatic archive session state'
      end
      operation = value['operation']
      if operation && !(operation.is_a?(Hash) && operation['id'].is_a?(String) && operation['id'].match?(/\A[0-9a-f]{64}\z/) &&
                        operation['identity'].is_a?(String) && !operation['identity'].empty? && Policy::TIERS.key?(operation['tier']) &&
                        operation['mode'] == (operation['tier'] == 'empty' ? 'abandoned' : 'complete') &&
                        (!operation.key?('identity_version') || operation['identity_version'] == 1))
        raise Error, 'invalid automatic archive operation state'
      end
      value
    end

    def valid_time?(value)
      value.is_a?(String) && Time.iso8601(value).utc.iso8601 == value
    rescue ArgumentError
      false
    end
  end

  # Pure retention decisions are shared by scan, final revalidation and tests.
  module Policy
    TIERS = { 'complete' => 86_400, 'merged' => 604_800, 'empty' => 1_209_600 }.freeze

    FINGERPRINT_VERSION = 1

    def self.diagnostic(code, category, message)
      { 'code' => code, 'category' => category, 'message' => message }
    end

    def self.bind(previous, identity, root_thread_id: nil, activity_known: false)
      return previous.dup if previous['identity'] == identity

      # Temporary old root-ID observation conversion. Generic lifecycle
      # maintainers own removal; docs/session-archive-recovery.md requires zero
      # dependent observations and zero supported reintroduction paths.
      legacy_same_root = !previous['fingerprint_version'] && activity_known &&
                         root_thread_id && previous['identity'] == root_thread_id
      { 'slug' => previous['slug'], 'identity' => identity,
        'hold' => legacy_same_root ? previous.fetch('hold', false) : false }
    end

    def self.observe(previous, snapshot, policy, now)
      stamp = now.utc.iso8601
      state = bind(previous, snapshot.fetch('identity'),
                   root_thread_id: snapshot['root_thread_id'], activity_known: snapshot['activity_known'])
      known = snapshot.fetch('activity_known')
      diagnostics = snapshot.fetch('diagnostics').dup
      unknown = snapshot.fetch('unknown_dimensions', [])
      current_dimensions = snapshot.fetch('dimensions')
      old_dimensions = state.fetch('dimensions', {})
      readable_changed = current_dimensions.any? { |key, value| old_dimensions[key] != value }
      dimensions = old_dimensions.merge(current_dimensions)
      baseline = state['fingerprint_version'] == FINGERPRINT_VERSION && state['fingerprint']
      changed = readable_changed || state['policy_epoch'] != policy['epoch'] || state['reset_at'] ||
                state['continuity_lost'] || (state['idle'] == false && snapshot['idle'] == true)
      if !known
        state['continuity_lost'] = true
        state['idle_since'] = nil
      elsif unknown.empty? && (!baseline || changed || !state['idle_since'])
        state['idle_since'] = stamp
        state['continuity_lost'] = false
      elsif baseline && readable_changed
        state['idle_since'] = stamp
      end
      if state['idle_since'] && Time.iso8601(state['idle_since']) > now
        state['idle_since'] = stamp
      end
      state.delete('reset_at') if known && unknown.empty?
      state.merge!(snapshot)
      state['dimensions'] = dimensions
      if known && unknown.empty?
        state['fingerprint'] = Digest::SHA256.hexdigest(JSON.generate(dimensions.sort.to_h))
        state['fingerprint_version'] = FINGERPRINT_VERSION
      end
      state['policy_epoch'] = policy['epoch']
      state['checked_at'] = stamp
      delay = TIERS[state['tier']]
      state['eligible_at'] = delay && state['idle_since'] ? (Time.iso8601(state['idle_since']) + delay).utc.iso8601 : nil
      diagnostics.unshift(diagnostic('policy_disabled', 'policy', 'Automatic archival is disabled.')) unless policy['enabled']
      diagnostics.unshift(diagnostic('keep_open', 'hold', 'Keep open is enabled.')) if state['hold']
      state['diagnostics'] = diagnostics
      state['blockers'] = diagnostics.map { |entry| entry.fetch('message') }
      state['eligible'] = !!(known && snapshot['idle'] && unknown.empty? && state['eligible_at'] &&
                            diagnostics.empty? && now >= Time.iso8601(state['eligible_at']))
      state
    end
  end

end
