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

  module Runner
    def auto_archive_store
      @auto_archive_store ||= Store.new(state_root: @workspace_state_root, workspace:)
    end

    def auto_archive_configure(enabled)
      auto_archive_store.lock do
        policy = auto_archive_store.policy
        if policy['enabled'] != enabled
          policy = { 'enabled' => enabled, 'epoch' => Time.now.utc.iso8601 }
          auto_archive_store.write('policy', policy)
        end
        policy
      end
    end

    def auto_archive_hold(slug, held, as_is:)
      slug = resolve_slug(slug, as_is:)
      with_slug_lock(slug) do
        validate_work_directory!(slug)
        reject_conflicting_lifecycle_journal!(slug)
        auto_archive_store.lock do
          state = auto_archive_current_state(slug)
          state['reset_at'] = Time.now.utc.iso8601 if state['hold'] && !held
          state['hold'] = held
          state['eligible'] = false
          state['eligible_at'] = nil
          state['blockers'] = held ? ['Keep open is enabled.'] : []
          auto_archive_store.write("session-#{slug}", state)
          state
        end
      end
    end

    def auto_archive_status(slug)
      state = begin
        auto_archive_current_state(slug)
      rescue StandardError
        { 'slug' => slug, 'lifecycle' => 'unknown', 'identity_kind' => 'unknown',
          'activity_known' => false, 'eligible' => false, 'repair_needed' => true, 'hold' => false,
          'diagnostics' => [Policy.diagnostic('legacy_record_invalid', 'legacy_format',
            'Tracking or cached archival state is invalid; repair unresolved metadata offline.')] }
      end
      policy = auto_archive_store.policy
      diagnostics = Array(state['diagnostics']).reject { |entry| %w[policy hold observation_stale].include?(entry['category']) }
      begin
        manifest = load_portal_manifest(File.join(auto_archive_tracking_directory(slug), 'portal.yml'), required: true, expected_slug: slug)
        require_ordinary_manifest_ready!(slug, manifest)
      rescue DevSession::Error, SystemCallError
        diagnostics << Policy.diagnostic('manifest_unverified', 'legacy_format', 'Explicit ordinary tracking is required; repair unresolved metadata offline before using this session.')
      end
      diagnostics << Policy.diagnostic('policy_disabled', 'policy', 'Automatic archival is disabled.') unless policy['enabled']
      diagnostics << Policy.diagnostic('keep_open', 'hold', 'Keep open is enabled.') if state['hold']
      if state['journal'] || state['operation']
        diagnostics << Policy.diagnostic('archive_operation_pending', 'lifecycle_pending', 'An accepted lifecycle operation needs its recorded retry.')
      end
      unless state['checked_at'] && state['fingerprint_version'] == Policy::FINGERPRINT_VERSION
        diagnostics << Policy.diagnostic('observation_missing', 'observation_stale', 'No current semantic activity observation is cached.')
      else
        if Time.now - Time.iso8601(state['checked_at']) > 7200
          diagnostics << Policy.diagnostic('observation_stale', 'observation_stale', 'The cached observation is older than two hours.')
        end
      end
      state['activity_known'] = false unless state['activity_known'] == true
      state['eligible'] = !!(state['eligible'] && state['activity_known'] && policy['enabled'] && !state['hold'] && diagnostics.empty?)
      state.delete('legacy_migration')
      state.delete('migration_needed')
      state.merge('schema' => 1, 'workspace' => workspace, 'slug' => slug,
                  'repair_needed' => diagnostics.any? { |entry| entry['category'] == 'legacy_format' },
                  'enabled' => policy['enabled'], 'diagnostics' => diagnostics,
                  'blockers' => diagnostics.map { |entry| entry['message'] })
    end

    def auto_archive_workspace_status
      policy = auto_archive_store.policy
      slugs = auto_archive_session_slugs
      if File.directory?(auto_archive_store.root)
        Dir.glob(File.join(auto_archive_store.root, 'session-*.json')).each do |path|
          slug = File.basename(path).delete_prefix('session-').delete_suffix('.json')
          slugs << slug if slug.match?(DevSession::SAFE_PART)
        end
      end
      rows = slugs.uniq.sort.filter_map do |slug|
        next unless path_exists?(work_dir(slug)) || auto_archive_store.session(slug)['operation']

        auto_archive_status(slug)
      rescue StandardError
        { 'slug' => slug, 'lifecycle' => 'unknown', 'identity_kind' => 'unknown',
          'activity_known' => false, 'eligible' => false, 'repair_needed' => true,
          'hold' => false, 'diagnostics' => [Policy.diagnostic('legacy_record_invalid', 'legacy_format',
            'Tracking or cached archival state is invalid; repair unresolved metadata offline.')],
          'blockers' => ['Tracking or cached archival state is invalid; repair unresolved metadata offline.'] }
      end
      counts = { 'total' => rows.length, 'eligible' => 0, 'held' => 0, 'unknown_activity' => 0, 'pending' => 0 }
      rows.each do |row|
        counts['eligible'] += 1 if row['eligible']
        counts['held'] += 1 if row['hold']
        counts['unknown_activity'] += 1 unless row['activity_known']
        counts['pending'] += 1 if row['operation'] || row['journal']
      end
      { 'schema' => 1, 'workspace' => workspace, 'policy' => policy.slice('enabled', 'epoch'),
        'last_scan' => auto_archive_store.read('scan'), 'counts' => counts, 'sessions' => rows }
    end

    # Undated --as-is sessions remain valid. Unrelated work folders have no
    # tracking or operation evidence and do not belong in session diagnostics.
    def auto_archive_session_slugs
      root = File.join(workspace, 'work')
      return [] unless File.directory?(root)

      Dir.children(root).select do |slug|
        next false unless slug.match?(DevSession::SAFE_PART)

        tracking = %w[portal.yml plan.md state.md].any? { |name| path_exists?(File.join(root, slug, name)) }
        private_names = %w[creation fork start] + DevSession::LIFECYCLE_JOURNAL_NAMES.values
        evidence = private_names.any? { |name| path_exists?(File.join(workspace, 'worktrees', '.locks', "#{slug}.#{name}.json")) }
        tracking || evidence || path_exists?(DevSession::ArchiveCleanup.new(self, slug).path)
      end
    end

    def auto_archive_reset(slug)
      return unless File.directory?(auto_archive_store.root)
      auto_archive_store.lock do
        state = auto_archive_store.session(slug)
        state['reset_at'] = Time.now.utc.iso8601
        state['eligible'] = false
        state['eligible_at'] = nil
        state.delete('operation')
        auto_archive_store.write("session-#{slug}", state)
      end
    end

    def auto_archive_scan(dry_run:, transition:, observation: transition)
      previous_out = @out
      @out = @err
      scan = lambda do
        slugs = if dry_run || auto_archive_store.policy['enabled']
                  auto_archive_session_slugs
                else
                  []
                end
        # A journal can already have moved tracking into archive/ before a crash.
        if File.directory?(auto_archive_store.root)
          Dir.glob(File.join(auto_archive_store.root, 'session-*.json')).each do |path|
            slug = File.basename(path).delete_prefix('session-').delete_suffix('.json')
            begin
              slugs << slug if auto_archive_store.session(slug)['operation']
            rescue Error
              slugs << slug if slug.match?(DevSession::SAFE_PART)
            end
          end
        end
        rows = slugs.uniq.sort.map do |slug|
          auto_archive_scan_session(slug, dry_run:, transition:, observation:)
        rescue DevSession::SupersededGenerationError
          raise
        rescue StandardError => e
          { 'slug' => slug, 'eligible' => false, 'blockers' => [e.message], 'result' => 'error' }
        end
        unless dry_run
          auto_archive_store.lock do
            auto_archive_store.write('scan', { 'checked_at' => Time.now.utc.iso8601,
              'result' => rows.any? { |row| %w[deferred error].include?(row['result']) } ? 'deferred' : 'complete',
              'counts' => { 'observed' => rows.length, 'archived' => rows.count { |row| row['result'] == 'archived' } } })
          end
        end
        rows
      end
      @command_runner.with_output(@err) do
        @command_runner.with_timeout(60) do
          dry_run ? scan.call : auto_archive_store.lock('scan', nonblock: true, &scan)
        end
      end
    ensure
      @out = previous_out
    end

    private

    def auto_archive_tracking_identity(slug, tracking, root_thread_id)
      stat = File.lstat(tracking)
      unless stat.directory? && File.realpath(tracking) == tracking
        raise ObservationFailure.new('tracking_identity_unverified', 'Tracking directory identity cannot be verified.')
      end
      Digest::SHA256.hexdigest(JSON.generate(['session-observation-v1', workspace, slug, stat.dev, stat.ino, root_thread_id]))
    end

    def auto_archive_tracking_directory(slug)
      File.directory?(work_dir(slug)) ? work_dir(slug) : archive_dir(slug)
    end

    def auto_archive_current_state(slug)
      tracking = auto_archive_tracking_directory(slug)
      manifest = load_portal_manifest(File.join(tracking, 'portal.yml'), required: true, expected_slug: slug)
      root = manifest.dig('codex', 'thread_id')
      identity = auto_archive_tracking_identity(slug, tracking, root)
      previous = auto_archive_store.session(slug)
      # Reads show predecessor holds but do not persist a conversion or baseline.
      state = if !previous['fingerprint_version'] && root && previous['identity'] == root
                previous.dup.merge('eligible' => false)
              else
                Policy.bind(previous, identity)
              end
      lifecycle = lifecycle_state(File.binread(File.join(tracking, 'state.md'), DevSession::TRACKING_MAX_SIZE + 1))
      journal = nil
      %w[archive revive delete].each do |kind|
        name = DevSession::LIFECYCLE_JOURNAL_NAMES.fetch(kind)
        path = File.join(workspace, 'worktrees', '.locks', "#{slug}.#{name}.json")
        next unless path_exists?(path)
        value = read_private_json_object(path, "#{kind} journal", max_size: 1024 * 1024, mode: 0o600)
        journal = { 'operation' => kind, 'operation_id' => value['operation_id'], 'phase' => value['phase'] }
      end
      if previous['operation']
        state['operation'] = previous['operation']
        state['eligible'] = false
      end
      state.merge('slug' => slug, 'identity' => identity, 'root_thread_id' => root,
                  'identity_kind' => root ? 'retained' : 'threadless', 'lifecycle' => lifecycle,
                  'repair_needed' => false, 'journal' => journal)
    end

    def auto_archive_scan_session(slug, dry_run:, transition:, observation:)
      result = nil
      operation = auto_archive_store.session(slug)['operation']
      if operation && !dry_run
        transition.call do
          auto_archive_resume(slug, operation)
          result = auto_archive_store.session(slug)
        end
        return result
      end
      observation.call do
        with_slug_observation_lock(slug) do
          select_tmux_for_slug!(slug)
          result = auto_archive_observe(slug, persist: !dry_run)
        end
      end
      if result['eligible'] && !dry_run
        transition.call do
          operation = {
            'id' => SecureRandom.hex(32), 'mode' => result['tier'] == 'empty' ? 'abandoned' : 'complete',
            'tier' => result['tier'], 'identity' => result['identity'], 'identity_version' => 1,
            'root_thread_id' => result['root_thread_id']
          }
          auto_archive_store.lock do
            state = auto_archive_store.session(slug)
            state['operation'] = operation
            auto_archive_store.write("session-#{slug}", state)
          end
          auto_archive_resume(slug, operation)
          result = auto_archive_store.session(slug)
        end
      end
      result
    rescue DevSession::SupersededGenerationError
      raise
    rescue StandardError => e
      unless dry_run
        observation.call do
          auto_archive_store.lock do
            state = auto_archive_store.session(slug)
            diagnostic = if e.is_a?(ObservationFailure)
                           if e.category == 'activity_unknown'
                             state['continuity_lost'] = true
                             state['idle_since'] = nil
                             state['eligible_at'] = nil
                             state['activity_known'] = false
                           end
                           Policy.diagnostic(e.code, e.category, e.message)
                         else
                           Policy.diagnostic('archive_proof_failed', 'tracking', 'Archival proof failed; retry after resolving the recorded failure.')
                         end
            state.merge!('eligible' => false, 'result' => 'deferred', 'blockers' => [diagnostic['message']],
                         'diagnostics' => [diagnostic], 'last_attempt_at' => Time.now.utc.iso8601,
                         'last_attempt_result' => 'deferred', 'last_attempt_error' => diagnostic['message'])
            auto_archive_store.write("session-#{slug}", state)
          end
        end
      end
      { 'slug' => slug, 'eligible' => false, 'blockers' => [e.message], 'result' => 'deferred' }
    end

    def auto_archive_resume(slug, operation)
      unless operation.is_a?(Hash) && operation['id'].to_s.match?(/\A[0-9a-f]{64}\z/) &&
             operation['identity'].is_a?(String) && !operation['identity'].empty? &&
             Policy::TIERS.key?(operation['tier']) &&
             operation['mode'] == (operation['tier'] == 'empty' ? 'abandoned' : 'complete')
        raise Error, 'invalid automatic archive operation receipt'
      end
      guard = lambda do |journal|
        if journal
          unless journal['operation_id'] == operation['id'] && journal['mode'] == operation['mode']
            raise Error, 'another lifecycle operation owns this session'
          end
          # Once prepared, finish the exact authorized operation using the
          # existing recovery checks, even if automatic archival was disabled.
          if operation['tier'] == 'empty' && File.directory?(work_dir(slug))
            manifest = load_portal_manifest(portal_file(slug), required: true)
            unless manifest.fetch('repositories', []).empty? && DevSession::ArchiveCleanup.new(self, slug).discover.fetch('worktrees').empty?
              raise Error, 'the abandoned archive now contains repositories or worktrees'
            end
          end
        else
          current = auto_archive_observe(slug, persist: true)
          unless current['eligible'] && current['tier'] == operation['tier'] &&
                 current['identity'] == operation['identity']
            raise Error, 'automatic archive eligibility changed; waiting for a new scan'
          end
        end
      end
      if !File.directory?(work_dir(slug)) && File.directory?(archive_dir(slug)) &&
         !path_exists?(lifecycle_journal_file(slug, 'archive')) &&
         !path_exists?(DevSession::ArchiveCleanup.new(self, slug).path)
        # The process can exit after finishing the archive but before recording
        # its result. A retained matching conversation proves the destination.
        manifest = load_portal_manifest(File.join(archive_dir(slug), 'portal.yml'), required: true)
        root = manifest.dig('codex', 'thread_id')
        bound = if operation['identity_version'] == 1
                  auto_archive_tracking_identity(slug, archive_dir(slug), root) == operation['identity']
                else
                  root && root == operation['identity']
                end
        unless bound && manifest['finalized_at'] &&
               lifecycle_state(File.binread(File.join(archive_dir(slug), 'state.md'), DevSession::TRACKING_MAX_SIZE + 1)) ==
                 (operation['mode'] == 'complete' ? 'complete' : 'abandoned')
          raise Error, 'archived session identity differs from automatic operation'
        end
      else
        archive(slug, as_is: true, abandoned: operation['mode'] == 'abandoned',
                operation_id: operation['id'], automatic: guard)
      end
      auto_archive_store.lock do
        state = auto_archive_store.session(slug)
        state.delete('operation')
        state.merge!('eligible' => false, 'result' => 'archived', 'archived_at' => Time.now.utc.iso8601,
                     'archive_mode' => operation['mode'], 'blockers' => [], 'diagnostics' => [],
                     'last_attempt_at' => Time.now.utc.iso8601, 'last_attempt_result' => 'archived', 'last_attempt_error' => nil)
        auto_archive_store.write("session-#{slug}", state)
      end
    rescue StandardError
      # No journal means no archive began: do not reserve a stale receipt across
      # a hold, policy change, manual operation, or new work.
      owned = begin
        load_archive_journal(slug)&.fetch('operation_id') == operation['id'] ||
          DevSession::ArchiveCleanup.new(self, slug).load&.fetch('operation_id') == operation['id']
      rescue StandardError
        false
      end
      unless owned
        auto_archive_store.lock do
          state = auto_archive_store.session(slug)
          state.delete('operation')
          auto_archive_store.write("session-#{slug}", state)
        end
      end
      raise
    end

    def auto_archive_observe(slug, persist:)
      snapshot = auto_archive_snapshot(slug)
      read = lambda do
        Policy.observe(auto_archive_store.session(slug), snapshot, auto_archive_store.policy, Time.now)
      end
      initial = persist ? auto_archive_store.lock(&read) : read.call
      proof_error = nil
      if initial['eligible']
        begin
          cleanup = DevSession::ArchiveCleanup.new(self, slug)
          proof_category = 'worktree'
          inventory = cleanup.discover
          proof_category = 'tracking'
          cleanup.plan(inventory)
          proof_category = 'merge_proof'
          cleanup.prove(inventory, 'complete') unless initial['tier'] == 'empty'
        rescue StandardError => e
          proof_error = e
        end
      end
      observe = lambda do
        state = read.call
        if state['eligible'] && !initial['eligible']
          state['eligible'] = false
          state['blockers'] << 'Automatic archive eligibility changed during observation.'
        elsif state['eligible'] && proof_error
          state['eligible'] = false
          state['blockers'] << proof_error.message
          state['diagnostics'] << Policy.diagnostic('cleanup_proof_failed', proof_category, 'Cleanup or exact merge proof is not satisfied.')
          state['last_attempt_error'] = state['diagnostics'].last.fetch('message')
          state['last_attempt_at'] = Time.now.utc.iso8601
          state['last_attempt_result'] = 'deferred'
        end
        auto_archive_store.write("session-#{slug}", state) if persist
        state
      end
      persist ? auto_archive_store.lock(&observe) : observe.call
    end

    def auto_archive_snapshot(slug)
      validate_work_directory!(slug)
      reject_conflicting_lifecycle_journal!(slug)
      lifecycle = validate_tracking_files!(slug)
      begin
        manifest = load_portal_manifest(portal_file(slug), required: true, expected_slug: slug)
      rescue StandardError
        raise ObservationFailure.new('manifest_unverified', 'A normalized manifest is required to verify conversation activity.')
      end
      root = manifest.dig('codex', 'thread_id')
      require_ordinary_manifest_ready!(slug, manifest)
      identity = auto_archive_tracking_identity(slug, work_dir(slug), root)
      activity = auto_archive_session_observation(slug, identity)
      if activity['activityKnown'] && root && !activity['subjects'].any? { |subject| subject['address'] == 'lead' && subject['threadId'] == root }
        raise ObservationFailure.new('observation_invalid', 'Exact retained root is absent from activity evidence.')
      end
      dimensions = {}
      diagnostics = activity.fetch('diagnostics').dup
      unknown_dimensions = []
      dimensions['activity'] = activity['activityToken'] if activity['activityKnown']
      repositories = manifest.fetch('repositories', [])
      artifacts = manifest.fetch('artifacts', [])
      # Obligations, feature tips and initial/final evidence are semantic;
      # fetched defaults and routine manifest/runtime metadata are not.
      dimensions['obligations'] = repositories.map { |repo| repo.slice('name', 'project', 'branch', 'default_branch', 'initial_base_sha', 'final_head_sha') }.sort_by { |repo| repo.fetch('name') }
      dimensions['lifecycle'] = lifecycle
      dimensions['artifacts'] = artifacts.sort_by { |artifact| artifact.fetch('path') }
      begin
        dimensions['heads'] = repositories.map do |repo|
          common = repository_common_dir(repo.fetch('project'))
          stat = File.lstat(common)
          raise Error, 'Repository identity is not a plain directory.' unless stat.directory?
          origin, = @command_runner.capture(['git', "--git-dir=#{common}", 'config', '--get', 'remote.origin.url'])
          [repo.fetch('name'), common, stat.dev, stat.ino, github_repository(origin.strip),
           resolve_commit(common, "refs/heads/#{repo.fetch('branch')}")]
        end.sort
      rescue StandardError
        unknown_dimensions << 'heads'
        diagnostics << Policy.diagnostic('feature_heads_unavailable', 'merge_proof', 'Exact feature heads cannot be read; cached inactivity is retained.')
      end
      cleanup = DevSession::ArchiveCleanup.new(self, slug)
      entries = nil
      additional_features = false
      begin
        inventory = cleanup.discover(clean: false)
        entries = inventory.fetch('worktrees')
        dimensions['worktrees'] = entries.map do |entry|
          entry.slice('path', 'project', 'common_dir', 'common_identity', 'admin_dir', 'admin_identity', 'identity', 'head', 'branch', 'detached', 'dirty')
        end.sort_by { |entry| entry.fetch('path') }
        additional_features = cleanup.additional_features?(inventory, repositories)
        if entries.any? { |entry| entry.fetch('dirty') }
          diagnostics << Policy.diagnostic('dirty_worktree', 'worktree', 'Session has uncommitted worktree changes.')
        end
      rescue StandardError
        unknown_dimensions << 'worktrees'
        diagnostics << Policy.diagnostic('inventory_unavailable', 'worktree', 'Worktree inventory cannot be verified; cached inactivity is retained.')
      end
      tier = if lifecycle == 'complete'
               'complete'
             elsif lifecycle == 'active' && (!repositories.empty? || additional_features)
               'merged'
             elsif lifecycle == 'active' && entries && entries.empty?
               'empty'
             end
      diagnostics << Policy.diagnostic('manual_abandoned', 'policy', 'Abandoned sessions require manual archival.') if lifecycle == 'abandoned'
      diagnostics << Policy.diagnostic('no_archive_rule', 'policy', 'Session has no automatic archive rule.') unless tier
      begin
        files = %w[plan.md state.md] + artifacts.map { |artifact| artifact.fetch('path') }
        dimensions['content'] = files.uniq.sort.map do |relative|
          path = File.expand_path(relative, work_dir(slug))
          unless path.start_with?(work_dir(slug) + '/') && File.realpath(path).start_with?(File.realpath(work_dir(slug)) + '/') && File.file?(path)
            raise Error, 'Declared activity artifact is not a confined regular file.'
          end
          [relative, Digest::SHA256.file(path).hexdigest]
        end
      rescue StandardError
        unknown_dimensions << 'content'
        diagnostics << Policy.diagnostic('content_unavailable', 'tracking', 'Declared tracking content cannot be read; cached inactivity is retained.')
      end
      { 'identity' => identity, 'root_thread_id' => root, 'identity_kind' => root ? 'retained' : 'threadless',
        'fingerprint_version' => Policy::FINGERPRINT_VERSION, 'dimensions' => dimensions,
        'unknown_dimensions' => unknown_dimensions, 'tier' => tier, 'lifecycle' => lifecycle,
        'activity_known' => activity['activityKnown'], 'activity_token' => activity['activityToken'],
        'last_activity_at' => activity['lastActivityAt'], 'idle' => activity['idle'], 'diagnostics' => diagnostics,
        'blockers' => diagnostics.map { |entry| entry.fetch('message') } }
    end

    def auto_archive_session_observation(slug, identity, operation_id: nil, archive_mode: nil, start_tmux_identity: nil, revive_operation_id: nil)
      unless (!operation_id && !archive_mode) ||
             (operation_id.is_a?(String) && operation_id.match?(/\A[0-9a-f]{64}\z/) && %w[complete abandoned].include?(archive_mode))
        raise ObservationFailure.new('operation_invalid', 'Archive observation needs its exact operation and mode together.')
      end
      unless [start_tmux_identity, revive_operation_id].all? { |value| value.nil? || value.is_a?(String) && value.match?(/\A[0-9a-f]{64}\z/) } &&
             (!operation_id || !start_tmux_identity && !revive_operation_id)
        raise ObservationFailure.new('operation_invalid', 'Retained observation context is invalid or conflicts with archive.')
      end
      unless @portal_command && @codex_socket && @authority_dir && @env['DEV_WORKSPACE_CODEX_HOME']
        raise ObservationFailure.new('observer_unavailable', 'Conversation activity reader is unavailable.')
      end
      command = [*@portal_command, 'session', 'observe', '--workspace', workspace, '--session-slug', slug,
                 '--socket', @codex_socket, '--user-state-root', @workspace_state_root,
                 '--authority-dir', @authority_dir, '--codex-home', @env['DEV_WORKSPACE_CODEX_HOME']]
      command += ['--expected-operation-id', operation_id, '--expected-archive-mode', archive_mode] if operation_id
      command += ['--expected-start-tmux-identity', start_tmux_identity] if start_tmux_identity
      command += ['--expected-revive-operation-id', revive_operation_id] if revive_operation_id
      output, = @command_runner.capture(command)
      activity = JSON.parse(output)
      valid = activity.is_a?(Hash) && activity['schema'] == 1 && activity['workspace'] == workspace &&
              activity['slug'] == slug && activity['identity'] == identity &&
              [true, false].include?(activity['activityKnown']) && [true, false].include?(activity['idle']) &&
              (!activity['idle'] || activity['activityKnown']) &&
              (activity['lastActivityAt'].nil? || (activity['lastActivityAt'].is_a?(Integer) &&
                activity['lastActivityAt'].positive? && activity['lastActivityAt'] <= Time.now.to_i)) &&
              activity['subjects'].is_a?(Array) && activity['subjects'].length <= 128 &&
              activity['subjects'].all? do |subject|
                subject.is_a?(Hash) && (subject.keys - %w[address threadId projectId activityToken lastActivityAt idle archiveState]).empty? &&
                  %w[address threadId].all? { |key| subject[key].is_a?(String) && subject[key].bytesize.between?(1, 256) && !subject[key].match?(/[[:cntrl:]]/) } &&
                  subject['activityToken'].is_a?(String) && subject['activityToken'].match?(/\A[0-9a-f]{64}\z/) &&
                  [true, false].include?(subject['idle']) &&
                  (!subject.key?('archiveState') || %w[active fresh archived].include?(subject['archiveState'])) &&
                  (subject['lastActivityAt'].nil? || (subject['lastActivityAt'].is_a?(Integer) && subject['lastActivityAt'].positive? && subject['lastActivityAt'] <= Time.now.to_i))
              end &&
              activity['diagnostics'].is_a?(Array) && activity['diagnostics'].length <= 512 &&
              activity['diagnostics'].all? do |entry|
                entry.is_a?(Hash) && entry.keys.sort == %w[category code message] &&
                  %w[activity_unknown busy].include?(entry['category']) &&
                  entry['code'].is_a?(String) && entry['code'].match?(/\A[a-z][a-z_]{0,63}\z/) &&
                  entry['message'].is_a?(String) && entry['message'].bytesize.between?(1, 240) && !entry['message'].match?(/[[:cntrl:]]/)
              end
      if activity['activityKnown']
        valid &&= activity['activityToken'].is_a?(String) && activity['activityToken'].match?(/\A[0-9a-f]{64}\z/) &&
                  activity['idle'] == activity['diagnostics'].empty?
      else
        valid &&= !activity['idle'] && activity['diagnostics'].any? { |entry| entry['category'] == 'activity_unknown' }
      end
      raise ObservationFailure.new('observation_invalid', 'Conversation activity is invalid or in the future.') unless valid

      activity
    rescue ObservationFailure
      raise
    rescue StandardError
      raise ObservationFailure.new('observation_unavailable', 'Conversation activity cannot be verified.')
    end
  end
end
