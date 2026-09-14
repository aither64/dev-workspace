# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'securerandom'
require 'time'

module WorkspaceAutoArchive
  class Error < StandardError; end

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
      return nil unless File.exist?(path)
      raise Error, "automatic archive state is a symlink: #{path}" if File.symlink?(path)
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
             %w[idle_since checked_at reset_at].all? { |key| !value[key] || valid_time?(value[key]) }
        raise Error, 'invalid automatic archive session state'
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

    def self.bind(previous, identity)
      return previous.dup if previous['identity'] == identity

      { 'slug' => previous['slug'], 'identity' => identity, 'hold' => false }
    end

    def self.observe(previous, snapshot, policy, now)
      stamp = now.utc.iso8601
      state = bind(previous, snapshot.fetch('identity'))
      changed = state['fingerprint'] != snapshot.fetch('fingerprint') ||
                state['identity'] != snapshot.fetch('identity') ||
                state['policy_epoch'] != policy['epoch'] || state['reset_at'] ||
                !state['idle_since']
      state['idle_since'] = stamp if changed
      # A backwards clock must never shorten the grace period.
      state['idle_since'] = stamp if Time.iso8601(state.fetch('idle_since')) > now
      state.delete('reset_at')
      state.merge!(snapshot)
      state['policy_epoch'] = policy['epoch']
      state['checked_at'] = stamp
      delay = TIERS[state['tier']]
      state['eligible_at'] = delay ? (Time.iso8601(state['idle_since']) + delay).utc.iso8601 : nil
      blockers = snapshot.fetch('blockers').dup
      blockers.unshift('Automatic archival is disabled.') unless policy['enabled']
      blockers.unshift('Keep open is enabled.') if state['hold']
      state['blockers'] = blockers
      state['eligible'] = !!(delay && blockers.empty? && now >= Time.iso8601(state['eligible_at']))
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
      state = auto_archive_current_state(slug)
      enabled = auto_archive_store.policy['enabled']
      state['eligible'] = false unless enabled && !state['hold']
      state.merge('enabled' => enabled)
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
                  Dir.children(File.join(workspace, 'work')).select { |slug| slug.match?(DevSession::SAFE_PART) }
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
        slugs.uniq.sort.map do |slug|
          auto_archive_scan_session(slug, dry_run:, transition:, observation:)
        rescue DevSession::SupersededGenerationError
          raise
        rescue StandardError => e
          { 'slug' => slug, 'eligible' => false, 'blockers' => [e.message], 'result' => 'error' }
        end
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

    def auto_archive_current_state(slug)
      tracking = File.directory?(work_dir(slug)) ? work_dir(slug) : archive_dir(slug)
      manifest = load_portal_manifest(File.join(tracking, 'portal.yml'), required: true)
      identity = manifest.dig('codex', 'thread_id')
      raise Error, 'Session has no recorded conversation identity.' if identity.to_s.empty?

      Policy.bind(auto_archive_store.session(slug), identity)
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
        with_slug_lock(slug) do
          select_tmux_for_slug!(slug)
          result = auto_archive_observe(slug, persist: !dry_run)
        end
      end
      if result['eligible'] && !dry_run
        transition.call do
          operation = {
            'id' => SecureRandom.hex(32), 'mode' => result['tier'] == 'empty' ? 'abandoned' : 'complete',
            'tier' => result['tier'], 'identity' => result['identity']
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
            state.merge!('eligible' => false, 'result' => 'deferred', 'blockers' => [e.message],
                         'checked_at' => Time.now.utc.iso8601, 'reset_at' => Time.now.utc.iso8601)
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
            unless manifest.fetch('repositories', []).empty? && worktree_entries(slug).empty?
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
         !path_exists?(lifecycle_journal_file(slug, 'archive'))
        # The process can exit after finishing the archive but before recording
        # its result. A retained matching conversation proves the destination.
        manifest = load_portal_manifest(File.join(archive_dir(slug), 'portal.yml'), required: true)
        unless manifest.dig('codex', 'thread_id') == operation['identity']
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
                     'archive_mode' => operation['mode'], 'blockers' => [])
        auto_archive_store.write("session-#{slug}", state)
      end
    rescue StandardError
      # No journal means no archive began: do not reserve a stale receipt across
      # a hold, policy change, manual operation, or new work.
      owned = begin
        load_archive_journal(slug)&.fetch('operation_id') == operation['id']
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
      observe = lambda do
        state = Policy.observe(auto_archive_store.session(slug), snapshot, auto_archive_store.policy, Time.now)
        if state['eligible']
          begin
            plan = prepare_cleanup(slug, force: false)
            prove_registered_branches_merged!(slug, plan) unless state['tier'] == 'empty'
          rescue StandardError => e
            state['eligible'] = false
            state['blockers'] << e.message
          end
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
      manifest = load_portal_manifest(portal_file(slug), required: true)
      thread = manifest.dig('codex', 'thread_id')
      raise Error, 'Session has no recorded conversation activity.' if thread.to_s.empty?
      unless manifest.dig('creation', 'state') == 'ready'
        raise Error, 'Session creation is unfinished.'
      end
      raise Error, 'Conversation activity reader is unavailable.' unless @portal_command && @codex_socket
      output, = @command_runner.capture([
        *@portal_command, 'thread', 'activity', '--thread-id', thread,
        '--cwd', work_dir(slug), '--socket', @codex_socket
      ])
      activity = JSON.parse(output)
      unless activity['threadId'] == thread && activity['cwd'] == work_dir(slug) &&
             activity['updatedAt'].is_a?(Integer) && activity['updatedAt'].positive? &&
             activity['updatedAt'] <= Time.now.to_i + 60
        raise Error, 'Conversation activity is invalid or in the future.'
      end
      repositories = manifest.fetch('repositories', [])
      entries = worktree_entries(slug)
      unexpected = unexpected_worktree_entries(slug, entries)
      raise Error, 'Session contains unmanaged worktree entries.' unless unexpected.empty?
      heads = repositories.map do |repository|
        common = repository_common_dir(repository.fetch('project'))
        [repository.fetch('name'), resolve_commit(common, "refs/heads/#{repository.fetch('branch')}")]
      end
      dirty = entries.any? do |entry|
        validate_worktree_path!(slug, entry)
        worktree_dirty?(entry.fetch(:path))
      end
      tier = if lifecycle == 'complete'
               'complete'
             elsif lifecycle == 'active' && !repositories.empty?
               'merged'
             elsif lifecycle == 'active' && entries.empty?
               'empty'
             end
      blockers = []
      blockers << 'Abandoned sessions require manual archival.' if lifecycle == 'abandoned'
      blockers << 'Session has uncommitted worktree changes.' if dirty
      blockers << 'Session has no automatic archive rule.' unless tier
      # Idle checks include active turns, pending requests, queued messages and
      # unresolved submission attempts. A failed check never ages a candidate.
      begin
        @command_runner.capture([
          *@portal_command, 'thread', 'require-idle', '--thread-id', thread,
          '--cwd', work_dir(slug), '--socket', @codex_socket
        ])
      rescue StandardError => e
        blockers << e.message
      end
      files = %w[plan.md state.md] + manifest.fetch('artifacts', []).map { |artifact| artifact.fetch('path') }
      contents = files.uniq.sort.map do |relative|
        path = File.expand_path(relative, work_dir(slug))
        unless path.start_with?(work_dir(slug) + '/') && File.realpath(path).start_with?(File.realpath(work_dir(slug)) + '/')
          raise Error, 'Declared activity artifact is outside the session.'
        end
        # Stream curated files: screenshots and reports need not fit in memory.
        raise Error, 'Declared activity artifact is not a regular file.' unless File.file?(path)
        [relative, Digest::SHA256.file(path).hexdigest]
      end
      fingerprint = Digest::SHA256.hexdigest(JSON.generate([
        thread, activity.fetch('updatedAt'), lifecycle, repositories, heads,
        entries.map { |entry| [entry[:name], entry[:path]] }.sort, dirty, contents,
        manifest.fetch('artifacts', []), blockers,
        manifest.fetch('creation', {}).slice('tracking_origin', 'tracking_plan_sha256', 'tracking_state_sha256'),
        @selected_authority&.fetch('tmux_identity', nil)
      ]))
      {
        'identity' => thread, 'fingerprint' => fingerprint, 'tier' => tier,
        'blockers' => blockers, 'conversation_updated_at' => activity.fetch('updatedAt')
      }
    end
  end
end
