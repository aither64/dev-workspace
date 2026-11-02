# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'securerandom'
require 'tempfile'
require 'time'
require 'yaml'

# Only explicit registrations confer ownership. In particular, none of these
# readers enumerates systemd units or attempts to adopt an existing service.
module WorkspaceSessionServices
  class Error < StandardError; end
  class StrictHash < Hash
    def []=(key, value)
      raise JSON::ParserError, 'duplicate service state key' if key?(key)
      super
    end
  end

  MAX_BYTES = 1024 * 1024
  MAX_RESOURCES = 256
  UUID = /\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/
  HEX = /\A[0-9a-f]{64}\z/
  OWNER_KEYS = %w[schema workspace slug registry_id retained_thread_id tracking_identity].freeze
  MARKER_KEYS = (OWNER_KEYS + %w[state created_at]).freeze
  REGISTRY_KEYS = (OWNER_KEYS + %w[revision activity_token resources]).freeze
  PRELUDE_KEYS = (OWNER_KEYS + %w[operation operation_id mode force automatic resource_ids phase created_at updated_at]).freeze
  RESOURCE_KEYS = %w[id label unit description argv cwd request_sha256 phase created_at updated_at boot_id manager_uid manager_control_group expected_control_group invocation_id control_group stopped_at retired_at error].freeze
  SAVED_ERRORS = {
    'launch_uncertain' => 'Service launch could not be verified; retry this resource ID.',
    'stop_unverified' => 'Registered service shutdown could not be verified; lifecycle cleanup remains pending.'
  }.freeze

  def self.paths(workspace:, state_root:, slug:)
    raise Error, 'invalid service session slug' unless slug.is_a?(String) && slug.match?(/\A[A-Za-z0-9][A-Za-z0-9_-]*\z/)
    workspace = File.realpath(workspace)
    id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
    locks = File.join(workspace, 'worktrees', '.locks')
    { marker: File.join(locks, "#{slug}.services-registration.json"),
      registry: File.join(File.expand_path(state_root), 'session-services', id, "#{slug}.json"),
      prelude: File.join(locks, "#{slug}.services-cleanup.json") }
  end

  def self.exists?(path)
    File.lstat(path)
    true
  rescue Errno::ENOENT
    false
  rescue SystemCallError => error
    raise Error, "service state cannot be read: #{path} (#{error.class})"
  end

  def self.read(path, limit = MAX_BYTES)
    return nil unless exists?(path)
    stat = File.lstat(path)
    unless stat.file? && stat.uid == Process.euid && (stat.mode & 0o777) == 0o600 && File.realpath(path) == path
      raise Error, "service state must be an owned mode-0600 file: #{path}"
    end
    bytes = File.binread(path, limit + 1)
    raise Error, "service state exceeds its size limit: #{path}" if bytes.bytesize > limit
    data = JSON.parse(bytes, object_class: StrictHash)
    raise Error, "invalid service state object: #{path}" unless data.is_a?(Hash)
    # StrictHash rejects duplicate input keys; writers need ordinary mutable
    # hashes after that validation, including nested resource records.
    JSON.parse(bytes)
  rescue JSON::ParserError
    raise Error, "invalid service state JSON: #{path}"
  end

  def self.timestamp?(value)
    value.is_a?(String) && Time.iso8601(value).utc_offset.zero?
  rescue ArgumentError
    false
  end

  def self.owner?(data, workspace, slug)
    data['schema'] == 1 && data['workspace'] == workspace && data['slug'] == slug &&
      data['registry_id'].is_a?(String) && data['registry_id'].match?(UUID) &&
      (data['retained_thread_id'].nil? || data['retained_thread_id'].is_a?(String) && !data['retained_thread_id'].empty?) &&
      data['tracking_identity'].is_a?(Hash) && data['tracking_identity'].keys.sort == %w[dev ino] &&
      data['tracking_identity'].values.all? { |part| part.is_a?(Integer) && part >= 0 }
  end

  def self.validate_marker!(data, workspace, slug)
    unless data.keys.sort == MARKER_KEYS.sort && owner?(data, workspace, slug) &&
           %w[initializing ready].include?(data['state']) && timestamp?(data['created_at'])
      raise Error, "invalid service registration marker: #{slug}"
    end
  end

  def self.validate_registry!(data, marker)
    resources = data['resources']
    unless data.keys.sort == REGISTRY_KEYS.sort && data.slice(*OWNER_KEYS) == marker.slice(*OWNER_KEYS) &&
           data['revision'].is_a?(Integer) && data['revision'] >= 0 && data['activity_token'].is_a?(String) && data['activity_token'].match?(UUID) &&
           resources.is_a?(Array) && resources.length <= MAX_RESOURCES && resources.all? { |row| resource?(row, marker) } &&
           resources.map { |row| row['id'] }.uniq.length == resources.length
      raise Error, "invalid service registry: #{marker['slug']}"
    end
  end

  def self.resource?(row, marker)
    row.is_a?(Hash) && row.keys.sort == RESOURCE_KEYS.sort && row['id'].is_a?(String) && row['id'].match?(UUID) &&
      row['unit'] == "workspace-session-#{row['id']}.service" &&
      row['description'] == "Workspace session service #{marker['registry_id']} #{row['id']}" &&
      row['label'].is_a?(String) && row['label'].bytesize.between?(1, 256) && !row['label'].match?(/[[:cntrl:]]/) &&
      row['cwd'].is_a?(String) && row['cwd'].start_with?('/') && !row['cwd'].include?("\0") &&
      row['argv'].is_a?(Array) && !row['argv'].empty? && row['argv'].all? { |arg| arg.is_a?(String) && !arg.include?("\0") } &&
      row['request_sha256'] == Digest::SHA256.hexdigest(JSON.generate([row['label'], row['cwd'], row['argv']])) &&
      %w[starting running stopping retired].include?(row['phase']) && timestamp?(row['created_at']) && timestamp?(row['updated_at']) &&
      row['boot_id'].is_a?(String) && row['boot_id'].match?(UUID) && row['manager_uid'] == Process.euid &&
      cgroup_path?(row['manager_control_group']) &&
      row['expected_control_group'] == File.join(row['manager_control_group'], 'app.slice', row['unit']) &&
      (row['invocation_id'].nil? || row['invocation_id'].is_a?(String) && row['invocation_id'].match?(/\A[0-9a-f]{32}\z/)) &&
      (row['control_group'].nil? || row['control_group'] == row['expected_control_group']) &&
      %w[stopped_at retired_at].all? { |key| row[key].nil? || timestamp?(row[key]) } &&
      (row['phase'] != 'retired' || row['stopped_at'] && row['retired_at']) &&
      (row['error'].nil? || row['error'].is_a?(Hash) && row['error'].keys.sort == %w[code message] &&
        SAVED_ERRORS[row['error']['code']] == row['error']['message'] && row['error']['message'].is_a?(String))
  end

  def self.cgroup_path?(value)
    value.is_a?(String) && value.bytesize.between?(1, 4096) && value.start_with?('/') &&
      !value.match?(/[[:cntrl:]]/) && (value == '/' || !value.end_with?('/') && !value.include?('//')) &&
      value.split('/').none? { |part| %w[. ..].include?(part) }
  end

  def self.validate_prelude!(data, marker, registry)
    unless data.keys.sort == PRELUDE_KEYS.sort && data.slice(*OWNER_KEYS) == marker.slice(*OWNER_KEYS) &&
           %w[archive delete].include?(data['operation']) && data['operation_id'].is_a?(String) && data['operation_id'].match?(HEX) &&
           (data['operation'] == 'archive' ? %w[archived complete abandoned].include?(data['mode']) && data['force'].nil? :
             data['mode'].nil? && [true, false].include?(data['force'])) &&
           [true, false].include?(data['automatic']) && %w[services_pending services_stopped].include?(data['phase']) &&
           data['resource_ids'].is_a?(Array) && data['resource_ids'] == data['resource_ids'].sort.uniq &&
           data['resource_ids'].all? { |id| registry.fetch('resources').any? { |row| row['id'] == id } } &&
           registry.fetch('resources').reject { |row| row['phase'] == 'retired' }.all? { |row| data['resource_ids'].include?(row['id']) } &&
           timestamp?(data['created_at']) && timestamp?(data['updated_at'])
      raise Error, "invalid service cleanup prelude: #{marker['slug']}"
    end
  end

  # A package check has no Runner. Validate the retained identity from the exact
  # ordinary tracking location without creating locks, records or directories.
  def self.tracking_owner(workspace, slug)
    roots = %w[work archive].map { |location| File.join(workspace, location, slug) }.select { |path| exists?(path) }
    raise Error, "ambiguous service tracking identity: #{slug}" unless roots.length == 1
    root = roots.first
    stat = File.lstat(root)
    raise Error, "invalid service tracking directory: #{slug}" unless stat.directory? && File.realpath(root) == root
    path = File.join(root, 'portal.yml')
    raise Error, "missing service session manifest: #{slug}" unless File.file?(path) && !File.symlink?(path) && File.size(path) <= 1024 * 1024
    manifest = YAML.safe_load(File.read(path), permitted_classes: [Time], aliases: false)
    unless manifest.is_a?(Hash) && manifest['slug'] == slug && (manifest['codex'].nil? || manifest['codex'].is_a?(Hash))
      raise Error, "invalid service session manifest: #{slug}"
    end
    { 'tracking_identity' => { 'dev' => stat.dev, 'ino' => stat.ino }, 'retained_thread_id' => manifest.dig('codex', 'thread_id') }
  rescue Psych::Exception
    raise Error, "invalid service session manifest: #{slug}"
  end

  def self.load_state(workspace:, state_root:, slug:, identity: nil)
    files = paths(workspace:, state_root:, slug:)
    marker = read(files[:marker], 16 * 1024)
    registry = read(files[:registry])
    prelude = read(files[:prelude], 64 * 1024)
    return [nil, nil, nil] unless marker || registry || prelude
    raise Error, "service registration marker is missing: #{slug}" unless marker
    validate_marker!(marker, workspace, slug)
    identity ||= tracking_owner(workspace, slug)
    unless marker.slice('tracking_identity', 'retained_thread_id') == identity
      raise Error, "service session identity changed: #{slug}"
    end
    if registry
      validate_registry!(registry, marker)
    elsif marker['state'] != 'initializing' || prelude
      raise Error, "service registry is missing: #{slug}"
    end
    validate_prelude!(prelude, marker, registry) if prelude
    [marker, registry, prelude]
  end

  def self.requirements(workspace:, state_root:)
    workspace = File.realpath(workspace)
    sample = paths(workspace:, state_root:, slug: 'placeholder')
    slugs = inventory_children(File.dirname(sample[:registry])).select { |name| name.end_with?('.json') }.map { |name| name.delete_suffix('.json') }
    lock_names = inventory_children(File.join(workspace, 'worktrees', '.locks'))
    %w[services-registration services-cleanup].each do |suffix|
      slugs.concat(lock_names.select { |name| name.end_with?(".#{suffix}.json") }.map { |name| name.delete_suffix(".#{suffix}.json") })
    end
    required = false
    pending = []
    slugs.uniq.sort.each do |slug|
      marker, _registry, prelude = load_state(workspace:, state_root:, slug:)
      # Retired processes still have lifecycle metadata that an incapable
      # predecessor would strand when it deletes their tracking directory.
      required ||= !marker.nil?
      pending << prelude.slice('slug', 'operation', 'operation_id') if prelude
    end
    { required:, pending: }
  end

  def self.inventory_children(root)
    stat = begin
      File.lstat(root)
    rescue Errno::ENOENT
      return []
    end
    unless stat.directory? && stat.uid == Process.euid && File.realpath(root) == root &&
           File.readable?(root) && File.executable?(root)
      raise Error, "service inventory directory cannot be read: #{root}"
    end
    # Dir.glob suppresses ordinary inventory I/O errors. They must remain
    # unknown state at a package boundary, never evidence of zero registrations.
    Dir.children(root)
  rescue SystemCallError => error
    raise Error, "service inventory directory cannot be read: #{root} (#{error.class})"
  end

  module RunnerMethods
    def session_services(slug)
      Store.new(workspace:, state_root: @workspace_state_root, slug:, runner: self)
    end

    def require_service_root_creation_allowed!(slug)
      marker, = session_services(slug).load!
      return unless marker && marker['retained_thread_id'].nil?

      raise DevSession::Error,
        'this session has registered temporary services and cannot add a Codex conversation; keep using it without Codex or create a separate session with Codex first'
    rescue WorkspaceSessionServices::Error => error
      raise DevSession::Error, error.message
    end

    def service_session_identity(slug)
      directory = [work_dir(slug), archive_dir(slug)].find { |path| path_exists?(path) }
      if !directory && @service_removal && @service_removal['slug'] == slug
        directory = File.join(@service_removal.fetch('recovery'), @service_removal.fetch('tracking'))
      end
      raise Error, 'service session tracking is missing' unless directory
      stat = File.lstat(directory)
      raise Error, 'service session tracking is not a plain directory' unless stat.directory? && File.realpath(directory) == directory
      manifest = load_portal_manifest(File.join(directory, 'portal.yml'), required: true, expected_slug: slug)
      { 'tracking_identity' => { 'dev' => stat.dev, 'ino' => stat.ino }, 'retained_thread_id' => manifest.dig('codex', 'thread_id') }
    end

    def service_command_runner
      @command_runner
    end

    def service_environment
      @env
    end

    def service_lifecycle_projection(slug)
      %w[archive delete].each do |operation|
        suffix = operation == 'archive' ? 'archive' : 'removal'
        path = File.join(workspace, 'worktrees', '.locks', "#{slug}.#{suffix}.json")
        record = WorkspaceSessionServices.read(path)
        next unless record
        unless record['workspace'] == workspace && record['slug'] == slug &&
               record['operation_id'].is_a?(String) && record['operation_id'].match?(HEX) &&
               record['phase'].is_a?(String) && record['phase'].match?(/\A[a-z_]{1,64}\z/)
          raise Error, 'invalid registered-service lifecycle identity'
        end
        return { 'operation' => operation, 'operationId' => record['operation_id'], 'phase' => record['phase'] }
      end
      nil
    end

    def service_bind_lifecycle(slug, operation, operation_id, &block)
      coordination_store.bind_lifecycle(workspace, slug, operation, operation_id, &block)
    end

    def service_action(input, as_is:, action:, **options)
      slug = lookup_slug(input, as_is:)
      if action == 'list'
        # Unlike session_lock_file/with_host_lock this never initializes a lock
        # directory. An untouched session needs no lock or native observation.
        path = File.join(@authority_dir || File.join(workspace, 'worktrees', '.locks'), "#{slug}.lock")
        return session_services(slug).public_status unless path_exists?(path)
        raise Error, 'service observation lock is a symlink' if File.symlink?(path)
        File.open(path, File::RDONLY | File::NOFOLLOW) do |file|
          stat = file.stat
          raise Error, 'invalid service observation lock' unless stat.file? && stat.uid == Process.euid && (stat.mode & 0o777) == 0o600
          raise DevSession::SessionBusyError, 'session services are busy' unless file.flock(File::LOCK_SH | File::LOCK_NB)
          return session_services(slug).public_status
        end
      end
      with_creation_lock(slug) do
        with_slug_lock(slug) do
          reject_conflicting_lifecycle_journal!(slug)
          directory = validate_work_directory!(slug)
          manifest = load_portal_manifest(File.join(directory, 'portal.yml'), required: true)
          require_ordinary_manifest_ready!(slug, manifest)
          store = session_services(slug)
          action == 'run' ? store.run!(**options) : store.stop!(**options)
        end
      end
    rescue WorkspaceSessionServices::Error => error
      raise DevSession::Error, error.message
    end

    def service_cleanup_guard!(slug, journal:, operation:, automatic: false)
      store = session_services(slug)
      return unless store.registered?
      return store.verify_clean! unless store.needs_cleanup?
      store.prepare_cleanup!(operation:, operation_id: journal.fetch('operation_id'),
        mode: operation == 'archive' ? journal.fetch('mode') : nil,
        force: operation == 'delete' ? journal.fetch('force') : nil, automatic:)
      store.cleanup!
      store.handoff_cleanup!(journal:)
      store.verify_clean!
    end

    # Transfer the complete retired evidence into the deletion journal's existing
    # recovery directory. Both copies are durable before either source disappears;
    # retry can reconcile a crash between those unlinks without recreating state.
    def preserve_removed_services!(slug, removal)
      files = WorkspaceSessionServices.paths(workspace:, state_root: @workspace_state_root, slug:)
      saved = %i[marker registry].to_h { |key| [key, File.join(removal.fetch('recovery'), "services-#{key}.json")] }
      return unless (files.values_at(:marker, :registry) + saved.values).any? { |path| path_exists?(path) }
      data = {}
      %i[marker registry].each do |key|
        source, target = files.fetch(key), saved.fetch(key)
        original = path_exists?(source) ? File.binread(source, MAX_BYTES + 1) : nil
        retained = path_exists?(target) ? File.binread(target, MAX_BYTES + 1) : nil
        raise Error, 'deleted service recovery evidence changed' if original && retained && original != retained
        raise Error, 'deleted service recovery evidence is missing' unless original || retained
        data[key] = WorkspaceSessionServices.read(original ? source : target)
      end
      WorkspaceSessionServices.validate_marker!(data[:marker], workspace, slug)
      WorkspaceSessionServices.validate_registry!(data[:registry], data[:marker])
      unless data[:registry].fetch('resources').all? { |row| row['phase'] == 'retired' }
        raise Error, 'deleted service evidence is not fully retired'
      end
      %i[marker registry].each do |key|
        target = saved.fetch(key)
        next if path_exists?(target)
        File.open(target, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
          file.write(JSON.generate(data.fetch(key)) + "\n")
          file.flush
          file.fsync
        end
      end
      fsync_directory(removal.fetch('recovery'))
      %i[marker registry].each do |key|
        source = files.fetch(key)
        next unless path_exists?(source)
        File.unlink(source)
        fsync_directory(File.dirname(source))
      end
    end
  end

  class Store
    attr_reader :files

    def initialize(workspace:, state_root:, slug:, runner:)
      @workspace, @state_root, @slug, @runner = File.realpath(workspace), state_root, slug, runner
      @files = WorkspaceSessionServices.paths(workspace: @workspace, state_root:, slug:)
    end

    def load!
      unless @files.values.any? { |path| WorkspaceSessionServices.exists?(path) }
        @marker = @registry = @prelude = nil
        return [nil, nil, nil]
      end
      @marker, @registry, @prelude = WorkspaceSessionServices.load_state(
        workspace: @workspace, state_root: @state_root, slug: @slug,
        identity: @runner.service_session_identity(@slug))
    end

    def registered?
      load!
      !@marker.nil?
    end

    def activity_token
      load!
      @registry&.fetch('activity_token')
    end

    def needs_cleanup?
      load!
      @marker && (@prelude || !@registry || @marker['state'] == 'initializing' || @registry.fetch('resources').any? { |row| row['phase'] != 'retired' })
    end

    def prelude
      load!
      @prelude
    end

    def run!(id:, label:, cwd:, argv:)
      id ||= SecureRandom.uuid
      raise Error, 'invalid service resource ID' unless id.is_a?(String) && id.match?(UUID)
      raise Error, 'service name must contain 1–256 bytes without control characters' unless label.is_a?(String) && label.bytesize.between?(1, 256) && !label.match?(/[[:cntrl:]]/)
      raise Error, 'service command is required' unless argv.is_a?(Array) && !argv.empty? && argv.all? { |arg| arg.is_a?(String) && !arg.include?("\0") }
      cwd = File.realpath(cwd)
      raise Error, 'service working directory must be a directory' unless File.directory?(cwd)
      load!
      raise Error, 'service cleanup is pending; retry its lifecycle command' if @prelude
      initializing = @marker && @marker['state'] == 'initializing'
      initialize_registry! unless @marker
      initialize_empty_registry! unless @registry
      request = Digest::SHA256.hexdigest(JSON.generate([label, cwd, argv]))
      row = @registry.fetch('resources').find { |item| item['id'] == id }
      fresh = row.nil?
      if initializing && fresh && !@registry.fetch('resources').empty?
        raise Error, 'retry the original launch with the same resource ID to finish registration'
      end
      if row
        raise Error, 'service resource ID belongs to a different request' unless row['request_sha256'] == request
        raise Error, 'retired service IDs cannot be reused' if row['phase'] == 'retired'
      else
        raise Error, 'service registry resource limit reached' if @registry.fetch('resources').length >= MAX_RESOURCES
        manager_group = bounded { manager_control_group }
        now = timestamp
        row = { 'id' => id, 'label' => label, 'unit' => "workspace-session-#{id}.service",
          'description' => "Workspace session service #{@marker.fetch('registry_id')} #{id}",
          'argv' => argv, 'cwd' => cwd, 'request_sha256' => request, 'phase' => 'starting',
          'created_at' => now, 'updated_at' => now, 'boot_id' => boot_id, 'manager_uid' => Process.euid,
          'manager_control_group' => manager_group,
          'expected_control_group' => File.join(manager_group, 'app.slice', "workspace-session-#{id}.service"),
          'invocation_id' => nil, 'control_group' => nil, 'stopped_at' => nil, 'retired_at' => nil, 'error' => nil }
        @registry.fetch('resources') << row
        @registry['activity_token'] = SecureRandom.uuid
        persist_registry!
      end
      if @marker['state'] == 'initializing'
        @marker['state'] = 'ready'
        write(@files[:marker], @marker)
      end
      if fresh || initializing
        attempted = true
        bounded do
          # The ready marker and complete intent are durable before this call.
          command(['systemd-run', '--user', '--quiet', "--unit=#{row.fetch('unit')}",
            "--description=#{row.fetch('description')}", '--property=Type=exec', '--property=Restart=no',
            '--property=RemainAfterExit=yes', '--property=Slice=app.slice', '--property=KillMode=control-group', '--property=SendSIGKILL=yes',
            '--property=TimeoutStopSec=15s', "--working-directory=#{cwd}", '--', *argv])
          observe!(row)
          row['phase'] = 'running'
          row['error'] = nil
          persist_registry!
        end
      else
        attempted = true
        bounded { observe!(row); row['phase'] = 'running'; row['error'] = nil; persist_registry! }
      end
      public_status
    rescue StandardError => error
      record_error(row, 'launch_uncertain', 'Service launch could not be verified; retry this resource ID.') if attempted && row && @registry
      raise error if error.is_a?(Error)
      raise Error, 'service launch could not be verified; registration retained for recovery'
    end

    def stop!(id:)
      load!
      raise Error, 'service cleanup is pending; retry its lifecycle command' if @prelude
      row = @registry&.fetch('resources')&.find { |item| item['id'] == id }
      raise Error, 'registered service resource was not found' unless row
      stop_row!(row)
      public_status
    end

    def public_status
      load!
      now = timestamp
      envelope = { 'schema' => 1, 'workspace' => @workspace, 'slug' => @slug, 'registryId' => @marker&.fetch('registry_id'),
        'registered' => !@marker.nil?, 'count' => 0, 'countKnown' => true, 'observedAt' => now,
        'services' => [], 'history' => [], 'cleanup' => @prelude && { 'operation' => @prelude['operation'],
          'operationId' => @prelude['operation_id'], 'phase' => @prelude['phase'] }, 'notice' => nil }
      return envelope unless @marker
      envelope['cleanup'] ||= @runner.service_lifecycle_projection(@slug)
      unless @registry
        envelope['countKnown'] = false
        envelope['notice'] = safe_error('registration_initializing', 'Service registration is incomplete. Retry the original launch command.')
        return envelope
      end
      bounded do
        @registry.fetch('resources').each do |row|
          state, error = 'stopped', row['error']
          if row['phase'] != 'retired'
            begin
              state = @marker['state'] == 'initializing' ? 'starting' : observed_state(snapshot(row), row)
            rescue StandardError
              state = 'unknown'
              error = safe_error('observation_unavailable', 'Service state could not be verified.')
            end
          end
          projected = { 'id' => row['id'], 'label' => row['label'], 'phase' => row['phase'], 'state' => state,
            'observedAt' => row['phase'] == 'retired' ? row['stopped_at'] : now, 'retiredAt' => row['retired_at'], 'error' => error }
          envelope[row['phase'] == 'retired' ? 'history' : 'services'] << projected
        end
      end
      envelope['count'] = envelope['services'].length
      raise Error, 'service status exceeds its size limit' if JSON.generate(envelope).bytesize > MAX_BYTES
      envelope
    end

    def prepare_cleanup!(operation:, operation_id:, mode:, force:, automatic:)
      load!
      return unless @marker
      finish_initialization_without_launch! if @marker['state'] == 'initializing'
      expected = { 'operation' => operation, 'operation_id' => operation_id, 'mode' => mode, 'force' => force, 'automatic' => automatic }
      if @prelude
        upgrade = operation == 'delete' && @prelude['force'] == false && force == true
        unless @prelude.slice(*expected.keys).merge(upgrade ? { 'force' => true } : {}) == expected
          raise Error, 'service cleanup belongs to another lifecycle request; retry its original command'
        end
        if upgrade
          @prelude['force'] = true
          @prelude['updated_at'] = timestamp
          write(@files[:prelude], @prelude)
        end
        return @prelude
      end
      now = timestamp
      @prelude = @marker.slice(*OWNER_KEYS).merge(expected).merge('resource_ids' => @registry.fetch('resources').map { |row| row['id'] }.sort,
        'phase' => 'services_pending', 'created_at' => now, 'updated_at' => now)
      WorkspaceSessionServices.validate_prelude!(@prelude, @marker, @registry)
      @runner.service_bind_lifecycle(@slug, operation, operation_id) { write(@files[:prelude], @prelude) }
      @prelude
    end

    def cleanup!
      load!
      return unless @marker
      raise Error, 'service cleanup has no lifecycle intent' unless @prelude
      @prelude.fetch('resource_ids').each do |id|
        stop_row!(@registry.fetch('resources').find { |row| row['id'] == id })
      end
      verify_clean!
      @prelude['phase'] = 'services_stopped'
      @prelude['updated_at'] = timestamp
      write(@files[:prelude], @prelude)
    end

    def verify_clean!
      load!
      return unless @marker
      raise Error, 'registered services have not been verified retired' unless @registry && @marker['state'] == 'ready' && @registry.fetch('resources').all? { |row| row['phase'] == 'retired' }
      # Retirement includes a durable final proof and unit unload. Never probe a
      # retired name: the globally unique name is not ongoing ownership.
      true
    end

    def handoff_cleanup!(journal:)
      load!
      return unless @prelude
      operation = @prelude.fetch('operation')
      unless journal['operation_id'] == @prelude['operation_id'] && journal['workspace'] == @workspace && journal['slug'] == @slug &&
             (operation == 'archive' ? journal['mode'] == @prelude['mode'] : journal['force'] == @prelude['force'])
        raise Error, 'service cleanup and lifecycle journal identities differ'
      end
      verify_clean!
      raise Error, 'service cleanup is not complete' unless @prelude['phase'] == 'services_stopped'
      File.unlink(@files[:prelude])
      fsync_directory(File.dirname(@files[:prelude]))
      @prelude = nil
    end

    private

    def timestamp
      Time.now.utc.iso8601(6)
    end

    def boot_id
      File.read('/proc/sys/kernel/random/boot_id').strip
    end

    def safe_error(code, message)
      { 'code' => code, 'message' => message }
    end

    def record_error(row, code, message)
      row['error'] = safe_error(code, message)
      row['updated_at'] = timestamp
      persist_registry!
    end

    def initialize_registry!
      identity = @runner.service_session_identity(@slug)
      @marker = identity.merge('schema' => 1, 'workspace' => @workspace, 'slug' => @slug,
        'registry_id' => SecureRandom.uuid, 'state' => 'initializing', 'created_at' => timestamp)
      write(@files[:marker], @marker)
    end

    def initialize_empty_registry!
      raise Error, 'cannot replace a missing registered service registry' unless @marker['state'] == 'initializing'
      @registry = @marker.slice(*OWNER_KEYS).merge('revision' => 0, 'activity_token' => SecureRandom.uuid, 'resources' => [])
      persist_registry!
    end

    def finish_initialization_without_launch!
      initialize_empty_registry! unless @registry
      @registry.fetch('resources').each do |row|
        row.merge!('phase' => 'retired', 'stopped_at' => timestamp, 'retired_at' => timestamp, 'error' => nil)
      end
      persist_registry!
      @marker['state'] = 'ready'
      write(@files[:marker], @marker)
    end

    def persist_registry!
      @registry['revision'] += 1
      WorkspaceSessionServices.validate_registry!(@registry, @marker)
      write(@files[:registry], @registry)
    end

    def write(path, data)
      bytes = JSON.generate(data) + "\n"
      limit = path == @files[:prelude] ? 64 * 1024 : MAX_BYTES
      raise Error, 'service state exceeds its size limit' if bytes.bytesize > limit
      parent = File.dirname(path)
      missing = []
      cursor = parent
      until File.directory?(cursor)
        missing << cursor
        cursor = File.dirname(cursor)
      end
      FileUtils.mkdir_p(parent, mode: 0o700)
      raise Error, 'service state directory is a symlink' unless File.realpath(parent) == parent
      raise Error, 'service state directory has another owner' unless File.stat(parent).uid == Process.euid
      File.chmod(0o700, parent)
      missing.reverse_each { |directory| fsync_directory(File.dirname(directory)) }
      file = Tempfile.new(['.session-services-', '.tmp'], parent)
      begin
        file.chmod(0o600)
        file.write(bytes)
        file.flush
        file.fsync
        File.rename(file.path, path)
        fsync_directory(parent)
      ensure
        file.close!
      end
    end

    def fsync_directory(path)
      File.open(path) { |file| file.fsync }
    end

    def monotonic
      Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end

    def bounded
      previous = @deadline
      @deadline = previous || monotonic + 20
      yield
    ensure
      @deadline = previous
    end

    def command(argv, allow_failure: false)
      seconds = @deadline - monotonic
      raise Error, 'service observation deadline exceeded' unless seconds.positive?
      # An uncertain native client must not remain as a late launch producer.
      # This inner limit kills its entire process group at the actual deadline;
      # the ordinary CommandRunner TERM grace must not extend the service bound.
      @runner.service_command_runner.with_timeout(nil) do
        @runner.service_command_runner.capture(['timeout', '--signal=KILL', "#{seconds}s", *argv], allow_failure:, env: @runner.service_environment)
      end
    end

    def manager_control_group
      raise Error, 'managed services require the unified cgroup-v2 hierarchy' unless File.file?(File.join(cgroup_root, 'cgroup.controllers'))
      stdout, = command(['systemctl', '--user', 'show', '--property=ControlGroup', '--value'])
      group = stdout.strip
      raise Error, 'user manager cgroup placement is unavailable' unless WorkspaceSessionServices.cgroup_path?(group)
      group
    end

    def snapshot(row)
      raise Error, 'service manager owner changed' unless row['manager_uid'] == Process.euid
      raise Error, 'user manager cgroup placement changed' unless manager_control_group == row['manager_control_group']
      properties = %w[Id LoadState ActiveState SubState Description InvocationID ControlGroup Job Transient Type Restart RemainAfterExit Slice KillMode SendSIGKILL TimeoutStopUSec Triggers TriggeredBy]
      stdout, _stderr, status = command(['systemctl', '--user', 'show', row.fetch('unit'), *properties.map { |key| "--property=#{key}" }], allow_failure: true)
      raise Error, 'service manager observation unavailable' if stdout.bytesize > 64 * 1024
      values = stdout.lines.to_h { |line| line.chomp.split('=', 2) }
      # Some systemd versions return nonzero for a positively reported not-found.
      raise Error, 'service manager observation unavailable' unless status.success? || values['LoadState'] == 'not-found'
      # systemctl prints the native zero-job tuple as Job= with explicit properties.
      # A missing property still provides no job-state observation.
      values['Job'] = '0' if values.key?('Job') && values['Job'] == ''
      raise Error, 'service manager job state unavailable' unless values['Job'] == '0' || values['Job']&.match?(/\A[1-9][0-9]*\z/)
      if values['LoadState'] != 'not-found'
        unless values['Id'] == row['unit'] && values['Description'] == row['description'] && values['Transient'] == 'yes' &&
               values['Type'] == 'exec' && values['Restart'] == 'no' && values['RemainAfterExit'] == 'yes' &&
               values['Slice'] == 'app.slice' &&
               values['KillMode'] == 'control-group' && values['SendSIGKILL'] == 'yes' &&
               values['TimeoutStopUSec'] == '15s' && values['Triggers'] == '' && values['TriggeredBy'] == ''
          raise Error, 'registered service unit identity or stop policy changed'
        end
        invocation = values['InvocationID']
        group = values['ControlGroup']
        raise Error, 'registered service invocation changed' if row['invocation_id'] && invocation != row['invocation_id']
        raise Error, 'registered service cgroup changed' if !group.to_s.empty? && group != row['expected_control_group']
        raise Error, 'registered service boot changed' if row['boot_id'] != boot_id
      end
      values
    rescue ArgumentError
      raise Error, 'invalid service manager observation'
    end

    def observe!(row)
      values = snapshot(row)
      raise Error, 'registered service launch is uncertain' if values['LoadState'] == 'not-found'
      invocation, group = values.values_at('InvocationID', 'ControlGroup')
      raise Error, 'registered service has no observed invocation' unless invocation&.match?(/\A[0-9a-f]{32}\z/) && invocation != '0' * 32
      row['invocation_id'] ||= invocation
      unless group.to_s.empty?
        raise Error, 'invalid registered service cgroup' unless group.start_with?('/') && group.split('/').none? { |part| %w[. ..].include?(part) }
        row['control_group'] ||= group
      end
      row['updated_at'] = timestamp
      values
    end

    def observed_state(values, row)
      return 'unknown' if values['LoadState'] == 'not-found' && row['phase'] != 'retired'
      case values['ActiveState']
      when 'activating' then 'starting'
      when 'deactivating' then 'stopping'
      when 'failed' then 'failed'
      when 'inactive' then 'stopped'
      when 'active' then values['SubState'] == 'exited' ? 'exited' : 'running'
      else 'unknown'
      end
    end

    def cgroup_empty?(row)
      group = row.fetch('expected_control_group')
      path = File.join(cgroup_root, group.delete_prefix('/'))
      return true unless WorkspaceSessionServices.exists?(path)
      raise Error, 'registered cgroup is not a plain directory' unless File.directory?(path) && File.realpath(path) == path
      # cgroup.events populated includes all descendants, unlike MainPID or a
      # read of only this group's cgroup.procs.
      events = File.read(File.join(path, 'cgroup.events'), 4096)
      events.lines.any? { |line| line.strip == 'populated 0' }
    end

    def cgroup_root
      '/sys/fs/cgroup'
    end

    def stopped_proof?(row, values)
      return false unless values['Job'] == '0'
      if row['boot_id'] != boot_id
        return values['LoadState'] == 'not-found'
      end
      terminal = values['LoadState'] == 'not-found' || %w[inactive failed].include?(values['ActiveState'])
      terminal && !row['invocation_id'].nil? && cgroup_empty?(row)
    end

    def stop_row!(row)
      return if row['phase'] == 'retired'
      bounded do
        values = snapshot(row)
        if values['LoadState'] != 'not-found'
          observe!(row)
          row['phase'] = 'stopping'
          persist_registry!
          command(['systemctl', '--user', 'stop', '--no-block', row.fetch('unit')])
        end
        loop do
          values = snapshot(row)
          break if stopped_proof?(row, values)
          raise Error, 'registered service stop could not be proved before its deadline' if monotonic >= @deadline
          sleep 0.1
        end
        row['stopped_at'] = timestamp
        persist_registry!
        # Reset only this exact failed transient unit. Successful stopped units
        # unload on their own; reset-failed of an already unloaded unit is harmless.
        if values['LoadState'] != 'not-found'
          command(['systemctl', '--user', 'reset-failed', row.fetch('unit')], allow_failure: true)
        end
        loop do
          values = snapshot(row)
          break if values['LoadState'] == 'not-found' && stopped_proof?(row, values)
          raise Error, 'registered transient service did not unload before its deadline' if monotonic >= @deadline
          sleep 0.1
        end
        row.merge!('phase' => 'retired', 'retired_at' => timestamp, 'updated_at' => timestamp, 'error' => nil)
        persist_registry!
      end
    rescue StandardError => error
      record_error(row, 'stop_unverified', 'Registered service shutdown could not be verified; lifecycle cleanup remains pending.')
      raise Error, "shutdown of registered service #{row.fetch('id')} could not be verified; retry the original command"
    end
  end
end
