# frozen_string_literal: true

require_relative 'workspace-coordination-execution'
require_relative 'workspace-coordination-admission'
require_relative 'workspace-integration'

module DevWorkspaceHost
  # Host-owned command composition. The engine owns ledger writes; the existing
  # Host continues to own profile selection, runtime identity and activation.
  module WorkspaceCoordinationHost
    def coordination_source_identity(source)
      unless File.exist?(File.join(source, '.git'))
        files = Dir.glob(File.join(source, '**', '*'), File::FNM_DOTMATCH).reject { |path| File.directory?(path) }
        raise WorkspaceCoordination::Error, 'source snapshot exceeds its file limit' if files.length > 20_000
        return {'head' => nil, 'digest' => WorkspaceCoordination.digest(files.sort.map { |path| [path.delete_prefix(source + '/'), File.symlink?(path) ? File.readlink(path) : Digest::SHA256.file(path).hexdigest] })}
      end
      head = capture!('git', '-C', source, 'rev-parse', 'HEAD').strip
      diff, error, status, = WorkspaceCoordination::Command.run(['git', '-C', source, 'diff', '--binary', 'HEAD'], env: @env.to_h, digest: true)
      raise Error, "source fingerprint failed: #{error}" unless status.success?
      files = capture!('git', '-C', source, 'ls-files', '--others', '--exclude-standard', '-z').split("\0")
      untracked = files.sort.map do |relative|
        path = File.join(source, relative)
        [relative, File.symlink?(path) ? File.readlink(path) : Digest::SHA256.file(path).hexdigest]
      end
      {'head' => head, 'digest' => WorkspaceCoordination.digest([head, diff, untracked])}
    end
    def coordination_store
      @coordination_store ||= WorkspaceCoordination::Store.new(@state)
    end

    def coordination_build(source)
      diagnostics = WorkspaceCoordination::Command::Log.new
      @coordination_diagnostics = diagnostics
      capture!('nix', 'build', '--no-link', '--print-out-paths', source)
    ensure
      @coordination_diagnostics = nil
      if diagnostics
        directory = File.join(coordination_store.root, 'preparation', SecureRandom.uuid)
        FileUtils.mkdir_p(directory, mode: 0o700)
        File.open(File.join(directory, 'profile-build-log'), File::WRONLY | File::CREAT | File::TRUNC, 0o600) { |file| file.write(diagnostics.bytes) }
      end
    end

    def coordination_catalog
      @coordination_catalog ||= WorkspaceCoordination::Catalog.new(package_root)
    end

    def coordination_check_codex(candidate_host, codex)
      diagnostics = WorkspaceCoordination::Command::Log.new
      stdout, _stderr, status, overflow = WorkspaceCoordination::Command.run(
        [candidate_host, 'check-codex', '--codex', codex], log: diagnostics
      )
      # Preparation returns JSON. Compatibility output is bounded diagnostics.
      diagnostics.append('stdout', stdout)
      @err.write(diagnostics.bytes)
      raise Error, 'candidate Codex compatibility check failed' unless status.success? && !overflow
    end

    def coordination_engine(entry, package: package_root)
      runtime = instance_runtime(entry)
      WorkspaceCoordination::Engine.new(state: @state, package:, profile: @profile,
        workspace: entry.fetch('root'), entry: entry.merge('socket' => runtime.fetch(:codex),
          'authority' => runtime.fetch(:authority), 'portalUrl' => "https://#{entry.fetch('hostname')}"),
        env: @env.to_h.merge('DEV_WORKSPACE_NAME' => entry.fetch('name'), 'DEV_WORKSPACE_CODEX_HOME' => codex_home))
    end

    def coordination_input(path)
      payload = path == '-' ? $stdin.read(WorkspaceCoordination::MAX_REQUEST + 1) : File.binread(path, WorkspaceCoordination::MAX_REQUEST + 1)
      WorkspaceCoordination.parse(payload)
    end

    def coordination_actor_thread
      values = %w[CODEX_THREAD_ID CODEX_SESSION_ID].filter_map { |key| @env[key] unless @env[key].to_s.empty? }.uniq
      raise WorkspaceCoordination::Error.new('caller Codex conversation identities disagree', 4) if values.length > 1
      values.first
    end

    def verify_background_recipient!(owner, explicit: false)
      actor = coordination_actor_thread
      return if explicit || actor && actor == owner['thread']
      raise WorkspaceCoordination::Error.new('background recipient is not this conversation; use --foreground for supervised continuation or explicitly --to-session-lead', 4)
    end

    def coordination_session_command(entry, argv)
      action = argv.shift
      slug = argv.shift
      return coordination_claim(entry, action, slug, argv) if WorkspaceCoordinationClaimsHost::CLAIM_ACTIONS.include?(action)
      raise WorkspaceCoordination::Error, 'coordination requires an exact session slug and --as-is' unless slug && slug.match?(WorkspaceCoordination::PART)
      id = argv.shift if %w[refresh cancel finish reconcile integration-status integration-resume integration-pause integration-ack integration-cancel].include?(action)
      options = {}
      OptionParser.new do |opts|
        opts.on('--as-is') { options[:as_is] = true }
        opts.on('--json') { options[:json] = true }
        opts.on('--portal') { options[:portal] = true }
        opts.on('--request FILE') { |value| options[:request] = value }
        opts.on('--bootstrap-owner ID') { |value| options[:bootstrap_owner] = value }
        opts.on('--admission-timeout SECONDS', Float) { |value| options[:admission_timeout] = value }
        opts.on('--no-wait') { options[:no_wait] = true }
        opts.on('--background') { options[:background] = true }
        opts.on('--foreground') { options[:foreground] = true }
        opts.on('--to-session-lead') { options[:to_session_lead] = true }
      end.parse!(argv)
      raise WorkspaceCoordination::Error, 'use coordination ACTION SLUG [ID] --as-is --request FILE' unless argv.empty? && options[:as_is]
      raise WorkspaceCoordination::Error, '--background and --no-wait are mutually exclusive' if options[:background] && options[:no_wait]
      raise WorkspaceCoordination::Error, '--foreground requires an attached caller' if options[:foreground] && (options[:background] || options[:no_wait])
      raise WorkspaceCoordination::Error, '--to-session-lead requires --background' if options[:to_session_lead] && !options[:background]
      engine = coordination_engine(entry)
      @coordination_admission_timeout = options.fetch(:admission_timeout, 30)
      if options[:bootstrap_owner] && !%w[status reconcile cancel finish].include?(action)
        raise WorkspaceCoordination::Error.new('bootstrap maintenance cannot enqueue or approve new work', 4)
      end
      if action == 'status'
        with_coordination_session_admission(entry, slug) { engine.existing_bootstrap_owner(slug, options[:bootstrap_owner]) } if options[:bootstrap_owner]
        @out.puts JSON.generate(engine.status(slug, portal: options[:portal]))
        return
      end
      request = options[:request] && coordination_input(options[:request])
      if %w[integrate integration-status integration-resume integration-pause integration-ack integration-cancel].include?(action)
        owner = nil
        resumable_finalization = action == 'integration-resume' && id && id.match?(WorkspaceCoordination::UUID) &&
          File.file?(File.join(engine.store.root, 'integrations', "#{id}.json")) &&
          %w[finalizing complete].include?(WorkspaceCoordination.private_read(File.join(engine.store.root, 'integrations', "#{id}.json"))['phase'])
        if %w[integration-status integration-ack].include?(action) || resumable_finalization
          raise WorkspaceCoordination::Error, 'invalid integration run ID' unless id && id.match?(WorkspaceCoordination::UUID)
          journal = WorkspaceCoordination.private_read(File.join(engine.store.root, 'integrations', "#{id}.json"))
          owner = engine.store.read['owners'].fetch(journal['ownerId'])
          raise WorkspaceCoordination::Error.new('integration status belongs to another session', 4) unless owner['slug'] == slug && owner['workspace'] == entry['root']
        else
          with_coordination_session_admission(entry, slug) { owner = engine.owner(slug) }
        end
        admission = ->(&block) { with_coordination_session_admission(entry, slug, &block) }
        driver = WorkspaceCoordination::Integration.new(engine, owner, admission:,
          prepare: ->(fields) { coordination_prepare(engine, owner, fields) }, diagnostics: @err)
        verify_background_recipient!(owner, explicit: options[:to_session_lead]) if options[:background]
        result = case action
        when 'integrate'
          if claim_workflow_selected? && !(request && request['requestId'].is_a?(String) && request['requestId'].match?(WorkspaceCoordination::UUID) && File.file?(driver.path(request['requestId'])))
            raise WorkspaceCoordination::Error, 'new work uses coordination acquire; existing integrations remain resumable'
          end
          driver.start(request || raise(WorkspaceCoordination::Error, 'integration requires a request'), wait: !options[:no_wait], background: options[:background])
        when 'integration-status' then driver.status(id)
        when 'integration-pause' then driver.pause(id)
        when 'integration-ack'
          driver.acknowledge(id, request || raise(WorkspaceCoordination::Error, 'acknowledgement requires a request'), recipient: coordination_actor_thread)
        when 'integration-cancel' then driver.cancel_unsubmitted(id)
        when 'integration-resume'
          if options[:background]
            driver.arm_background(id)
          else
            if driver.read(id)['schema'] == 2
              raise WorkspaceCoordination::Error, 'use --background or --foreground to resume a background run' unless options[:foreground]
              driver.supervise(id)
            else
              driver.drive(id, wait: !options[:no_wait])
            end
          end
        end
        @out.puts JSON.generate(result)
        return
      end
      if action == 'reconcile'
        owner = nil
        with_coordination_session_admission(entry, slug) do
          owner = options[:bootstrap_owner] ? engine.existing_bootstrap_owner(slug, options[:bootstrap_owner]) : engine.owner(slug)
        end
        # The retained claim prevents lifecycle removal. Forward profile
        # recovery must acquire its own exclusive transition lock.
        @out.puts JSON.generate(engine.reconcile_job(owner, id))
        return
      end
      if claim_workflow_selected? && %w[prepare approve reserve submit submit-batch].include?(action)
        retained = request && engine.store.read['receipts'].key?(request['requestId'])
        if action == 'prepare'
          binding = request && request.delete('legacyJobId')
          previous = binding && engine.store.read['jobs'][binding]
          operation = previous && previous.dig('snapshot', 'operation')
          same_scope = previous && previous['kind'] == request['kind'] &&
            (request['kind'] == 'merge' ? operation['repository'] == request['repository'] : request['kind'] == 'profile_activate' || operation['target'] == request['target'])
          retained ||= same_scope && previous['ownerId'] == engine.owner(slug)['id']
        elsif action == 'approve' && !retained && request
          # A refreshed snapshot needs a fresh approval before it can replace
          # the retained job's snapshot. It cannot be submitted as a new job.
          owner = engine.owner(slug)
          saved = engine.prepared_snapshot!(owner, request['reviewedSnapshot'])
          operation = saved.fetch('snapshot').fetch('operation')
          retained = engine.store.read['jobs'].values.any? do |job|
            previous = job.fetch('snapshot').fetch('operation')
            job['ownerId'] == owner['id'] && job['kind'] == saved['kind'] &&
              !WorkspaceCoordination::TERMINAL.include?(job['state']) &&
              (saved['kind'] == 'merge' ? previous.slice('repository', 'target') == operation.slice('repository', 'target') :
                saved['kind'] == 'profile_activate' || previous['target'] == operation['target'])
          end
        end
        raise WorkspaceCoordination::Error, 'new work uses coordination acquire; retained operations keep their original recovery interface' unless retained
      end
      if action == 'prepare'
        raise WorkspaceCoordination::Error, 'prepare requires a request' unless request
        owner = nil
        with_coordination_session_admission(entry, slug) { owner = engine.owner(slug) }
        result = coordination_prepare(engine, owner, request)
        @out.puts JSON.generate(result)
        return
      end
      with_coordination_session_admission(entry, slug) do
        owner = options[:bootstrap_owner] ? engine.existing_bootstrap_owner(slug, options[:bootstrap_owner]) : engine.owner(slug)
        result = case action
        when 'finish' then engine.finish(owner, id, request || raise(WorkspaceCoordination::Error, 'finish requires a request'))
        when 'submit-batch' then engine.submit_batch(owner, request || raise(WorkspaceCoordination::Error, 'batch submission requires a request'))
        else
          raise WorkspaceCoordination::Error, 'mutation requires --request FILE' unless request
          engine.mutate(owner, action, request, job_id: id)
        end
        @out.puts JSON.generate(result)
      end
    end

    def coordination_prepare(engine, owner, request)
      with_current_generation_lock(mode: File::LOCK_SH) do
        engine.store.lock('preparation', mode: File::LOCK_SH) do
          engine.current_generation!
          repository = case request.fetch('kind')
          when 'merge' then request.fetch('repository')
          when 'machine_activate' then engine.catalog.machine(request.fetch('target')).fetch('configurationRepository')
          when 'profile_activate' then 'workspace'
          else raise WorkspaceCoordination::Error, 'unsupported preparation kind'
          end
          engine.with_repository_claim(owner, repository) { coordination_prepare_owned(engine, owner, request) }
        end
      end
    end

    def coordination_prepare_owned(engine, owner, request)
      case request.fetch('kind')
      when 'merge' then engine.prepare_merge(owner, request.fetch('repository'), request.fetch('evidence'))
      when 'machine_activate' then engine.prepare_machine(owner, request.fetch('target'), request.fetch('evidence'))
      when 'profile_activate'
        path, _common, _identity, registration = engine.repository(owner, 'workspace')
        head = engine.capture('git', '-C', path, 'rev-parse', "#{registration.fetch('branch')}^{commit}").strip
        source = engine.retain_commit(path, head)
        artifact = prepare_profile(['--source', source])
        artifact['coverage'] = engine.coverage(source, 'workspace', head)
        artifact = engine.save_artifact(artifact)
        engine.retain_root(artifact.fetch('id'), 'profile', artifact.fetch('candidatePackage'))
        operation = {'preparedArtifact' => artifact.fetch('id'), 'candidatePackage' => artifact.fetch('candidatePackage'),
          'expectedProfileToken' => artifact.fetch('profileToken'), 'coverage' => artifact.fetch('coverage')}
        engine.prepare_snapshot(owner, 'profile_activate', operation, request.fetch('evidence'))
      else raise WorkspaceCoordination::Error, 'unsupported preparation kind'
      end
    rescue DevWorkspaceHost::Error => error
      raise WorkspaceCoordination::Error, WorkspaceCoordination.diagnostic(error.message)
    end

    def with_coordination_session_admission(entry, slug, timeout: @coordination_admission_timeout || 30, candidate_token: nil)
      admission = lambda do |&block|
        if candidate_token
          with_candidate_generation_lock(mode: File::LOCK_SH) do
            raise WorkspaceCoordination::Error.new('candidate predecessor changed', 3) unless profile_link_token == candidate_token
            block.call
          end
        else
          with_current_generation_lock(mode: File::LOCK_SH, &block)
        end
      end
      admission.call do
        runtime = instance_runtime(entry)
        FileUtils.mkdir_p(runtime.fetch(:authority), mode: 0o700)
        path = File.join(runtime.fetch(:authority), "#{slug}.lock")
        raise WorkspaceCoordination::Error, 'session lock is a symlink' if File.symlink?(path)
        File.open(path, File::RDWR | File::CREAT, 0o600) do |file|
          WorkspaceCoordination::Admission.acquire(file, timeout:, diagnostics: @err)
          raise WorkspaceCoordination::Error.new('session lifecycle recovery must finish first', 5) if Dir[File.join(entry.fetch('root'), 'worktrees', '.locks', "#{slug}.{archive,removal,revive,fork,start}.json")].any?
          creation_path = File.join(entry.fetch('root'), 'worktrees', '.locks', "#{slug}.creation.json")
          if File.exist?(creation_path) || File.symlink?(creation_path)
            manifest = read_portal_manifest(File.join(entry.fetch('root'), 'work', slug, 'portal.yml'))
            unless manifest['slug'] == slug && manifest.dig('creation', 'state') == 'ready'
              raise WorkspaceCoordination::Error.new('session creation is unfinished or mismatched', 5)
            end
            require_ready_archive_recovery_creation!(entry, slug, manifest.fetch('creation'))
          end
          yield
        end
      end
    end

    def coordination_collect_command(argv)
      options = {apply: false, json: false}
      OptionParser.new do |opts|
        opts.on('--dry-run') { options[:apply] = false }
        opts.on('--apply') { options[:apply] = true }
        opts.on('--json') { options[:json] = true }
      end.parse!(argv)
      raise WorkspaceCoordination::Error, 'unexpected coordination gc arguments' unless argv.empty?
      entry = registry.entries.first || raise(WorkspaceCoordination::Error, 'no registered workspace')
      with_current_generation_lock(mode: File::LOCK_SH) do
        coordination_store.lock('preparation') do
          result = WorkspaceCoordination::Collector.new(coordination_engine(entry)).call(apply: options[:apply])
          if options[:json]
            @out.puts JSON.generate(result)
          else
            @out.puts "#{result['mode']}: #{result['actions'].length} collectible payloads/roots; #{result['removed'].length} removed"
            result['actions'].each { |item| @out.puts "#{item['kind']}: #{item['path']}" }
            @out.puts 'Ledger changed; the next sweep will revalidate remaining work.' if result['retry']
          end
        end
      end
    end

    def dispatch_coordination_collection
      revision = coordination_store.read['revision']
      now = Process.clock_gettime(Process::CLOCK_MONOTONIC)
      return if @collection_revision == revision && now < (@collection_next_sweep || 0)
      # Keep bulk deletion outside the scheduler loop. The selected executable
      # rechecks its generation after obtaining the transition exclusion.
      _out, _err, status, = WorkspaceCoordination::Command.run(
        ['systemd-run', '--user', '--quiet', '--collect', '--unit=workspace-coordination-gc',
         '--property=Type=oneshot', '--property=UMask=0077',
         File.join(package_root, 'bin/workspace-host'), 'coordination', 'gc', '--apply'], env: @env.to_h)
      if status.success?
        @collection_revision = revision
        @collection_next_sweep = now + 300
      else
        @collection_revision = revision
        @collection_next_sweep = now + 5
      end
    end

    # Managed first-upgrade entry for a predecessor that cannot decode a new
    # registration plan. Ledger authority stays with the selected generation;
    # only exact candidate preparation/recovery uses the newer host reader.
    def coordination_candidate_command(argv)
      action = argv.shift
      slug = argv.shift
      options = {}
      OptionParser.new do |opts|
        opts.on('--as-is') { options[:as_is] = true }
        opts.on('--json') { options[:json] = true }
        %w[source expected-profile-token owner reservation request job workspace].each do |name|
          opts.on("--#{name} VALUE") { |value| options[name.tr('-', '_').to_sym] = value }
        end
      end.parse!(argv)
      required = %i[source expected_profile_token owner reservation]
      unless argv.empty? && options[:as_is] && WorkspaceCoordination::PART.match?(slug.to_s) && required.all? { |key| options[key] }
        raise WorkspaceCoordination::Error, 'candidate operation requires exact session, source, predecessor token, owner and reservation'
      end
      unless %w[candidate-prepare candidate-reconcile].include?(action) &&
             (action == 'candidate-prepare' ? options[:request] && !options[:job] : options[:job] && !options[:request])
        raise WorkspaceCoordination::Error, 'candidate preparation requires request; recovery requires original job'
      end
      entry = registry.select(cwd: Dir.pwd, environment_name: options[:workspace] || @env['DEV_WORKSPACE_NAME'])
      token = options.fetch(:expected_profile_token)
      selected = nil
      with_candidate_generation_lock(mode: File::LOCK_SH) do
        raise WorkspaceCoordination::Error.new('candidate predecessor changed', 3) unless profile_link_token == token
        selected = File.realpath(@profile)
      end
      engine = coordination_engine(entry, package: selected)
      @coordination_catalog = engine.catalog
      owner = engine.store.read['owners'][options[:owner]]
      reservation = engine.store.read['reservations'][options[:reservation]]
      unless owner && !owner['bootstrap'] && owner['slug'] == slug && owner['workspace'] == entry['root'] &&
             coordination_actor_thread == owner['thread'] && reservation && reservation['ownerId'] == owner['id'] &&
             %w[held deployed_review blocked_recovery].include?(reservation['state'])
        raise WorkspaceCoordination::Error.new('candidate requires the actual managed owner and held reservation', 4)
      end
      with_coordination_session_admission(entry, slug, candidate_token: token) do
        raise WorkspaceCoordination::Error.new('candidate owner changed', 4) unless engine.owner(slug)['id'] == owner['id']
      end
      source = File.realpath(options.fetch(:source))
      path, _common, _identity, registration = engine.repository(owner, 'workspace')
      head = engine.capture('git', '-C', path, 'rev-parse', "#{registration.fetch('branch')}^{commit}").strip
      unless coordination_source_identity(source)['head'] == head && capture!('git', '-C', source, 'status', '--porcelain').empty?
        raise WorkspaceCoordination::Error.new('candidate source is not the exact clean registered workspace head', 3)
      end
      prepared = prepare_profile(['--source', source, '--from-candidate'], from_candidate: true)
      unless prepared['candidatePackage'] == File.realpath(package_root) && prepared['profileToken'] == token
        raise WorkspaceCoordination::Error.new('candidate source or predecessor identity changed', 3)
      end
      with_coordination_session_admission(entry, slug, candidate_token: token) do
        engine.current_generation!
        if action == 'candidate-prepare'
          request = coordination_input(options.fetch(:request))
          WorkspaceCoordination.keys!(request, %w[schema requestId kind evidence])
          raise WorkspaceCoordination::Error, 'candidate prepares only profile activation' unless request['kind'] == 'profile_activate'
          @out.puts JSON.generate(prepare_candidate_snapshot(engine, owner, path, head, prepared, request))
        else
          job = engine.store.read['jobs'][options[:job]]
          unless job && job['ownerId'] == owner['id'] && job['reservationId'] == reservation['id'] &&
                 job['kind'] == 'profile_activate' && job.dig('snapshot', 'operation', 'candidatePackage') == prepared['candidatePackage']
            raise WorkspaceCoordination::Error.new('candidate recovery differs from original owned profile job', 4)
          end
          artifact = engine.artifact!(job.dig('snapshot', 'operation', 'preparedArtifact'))
          unless artifact.slice(*WorkspaceCoordination::Engine::PROFILE_IDENTITY.reject { |key| key == 'coverage' }) == prepared.slice(*WorkspaceCoordination::Engine::PROFILE_IDENTITY.reject { |key| key == 'coverage' })
            raise WorkspaceCoordination::Error.new('candidate recovery differs from retained preparation', 3)
          end
          @candidate_recovery = [engine, owner, job['id']]
        end
      end
      if @candidate_recovery
        # The engine owns the original worker lock and proves service exit.
        # Activation takes its ordinary exclusive generation/claim locks.
        @out.puts JSON.generate(engine.reconcile_job(owner, options[:job], candidate_host: File.join(package_root, 'bin/workspace-host')))
      end
    ensure
      @candidate_recovery = nil
    end

    def prepare_candidate_snapshot(engine, owner, path, head, prepared, request)
      engine.store.lock('preparation', mode: File::LOCK_SH) do
        retained = engine.retain_commit(path, head)
        # Artifact provenance uses the retained exact source, not a mutable
        # caller checkout. Its complete identity must equal the built source.
        raise WorkspaceCoordination::Error.new('candidate retained source differs', 3) unless coordination_source_identity(retained) == prepared['sourceIdentity']
        prepared['source'] = retained
        prepared['coverage'] = engine.coverage(retained, 'workspace', head)
        artifact = engine.save_artifact(prepared)
        engine.retain_root(artifact.fetch('id'), 'profile', artifact.fetch('candidatePackage'))
        operation = {'preparedArtifact' => artifact['id'], 'candidatePackage' => artifact['candidatePackage'],
          'expectedProfileToken' => prepared.fetch('profileToken'), 'coverage' => artifact['coverage']}
        engine.prepare_snapshot(owner, 'profile_activate', operation, request['evidence'])
      end
    end

    def claim_workflow_selected?
      File.file?(File.join(package_root, 'share/dev-workspace/claim-contract.json'))
    end

    def coordination_host_command(argv)
      if %w[candidate-prepare candidate-reconcile].include?(argv.first)
        return coordination_candidate_command(argv)
      end
      return coordination_collect_command(argv.drop(1)) if argv.first == 'gc'
      if argv == ['guide']
        @out.puts "Selected package: #{File.realpath(package_root)}"
        @out.puts "Coordination catalog: #{coordination_catalog.digest} (#{File.join(package_root, 'share/dev-workspace/coordination.json')})"
        @out.puts File.read(File.join(package_root, 'share/dev-workspace/coordination.md'))
        @out.puts File.read(File.join(package_root, 'share/dev-workspace/coordination-operations.md'))
        return
      end
      raise WorkspaceCoordination::Error, 'workspace-host coordination supports status --json' unless argv.shift == 'status'
      options = {}
      OptionParser.new do |opts|
        opts.on('--json') { options[:json] = true }
        opts.on('--workspace NAME') { |value| options[:workspace] = value }
        opts.on('--portal') { options[:portal] = true }
      end.parse!(argv)
      raise WorkspaceCoordination::Error, 'unexpected status arguments' unless argv.empty?
      entries = options[:workspace] ? [registry.find(options[:workspace]) || raise(WorkspaceCoordination::Error, 'unknown workspace')] : registry.entries
      results = entries.map { |entry| coordination_engine(entry).status(nil, portal: options[:portal]) }
      @out.puts JSON.generate('schema' => 1, 'workspaces' => results)
    end

    def require_registration_reconciliation!(token: profile_link_token)
      unless token == profile_link_token && profile_selects_package?(File.realpath(package_root)) && pending_codex_update.nil?
        raise WorkspaceCoordination::Error.new('registration requires the settled selected package', 4)
      end
      data = coordination_store.read
      machine = coordination_catalog.data['profileMachine']
      machine = coordination_catalog.machine(machine)['id'] if machine
      if !coordination_store.held_claim_resources.empty? ||
         data['jobs'].values.any? { |job| job['kind'] == 'profile_activate' && WorkspaceCoordination::CLAIMED.include?(job['state']) } ||
         data['reservations'].values.any? { |item| item['machine'] == machine && !%w[requested released].include?(item['state']) }
        raise WorkspaceCoordination::Error.new('registration waits for the machine review reservation', 4)
      end
    end

    def prepare_profile_command(argv)
      coordination_store.lock('preparation', mode: File::LOCK_SH) { prepare_profile_artifact(argv) }
    end

    def prepare_profile_artifact(argv)
      argv = argv.reject { |argument| argument == '--json' }
      prepared = prepare_profile(argv)
      entry = registry.select(cwd: Dir.pwd, environment_name: @env['DEV_WORKSPACE_NAME'])
      engine = coordination_engine(entry)
      prepared = engine.save_artifact(prepared)
      engine.retain_root(prepared.fetch('id'), 'profile', prepared.fetch('candidatePackage'))
      @out.puts JSON.generate(prepared)
    end

    def require_coordination_profile_claim!(candidate)
      machine = coordination_catalog.data['profileMachine']
      if (claim = @env['DEV_WORKSPACE_CLAIM_ID'])
        canonical = machine && coordination_catalog.machine(machine)['id']
        raise WorkspaceCoordination::Error, 'profile activation requires a configured physical machine' unless canonical
        entry = registry.select(cwd: Dir.pwd, environment_name: @env['DEV_WORKSPACE_NAME'])
        coordination_engine(entry).profile_claim!(claim, canonical)
        return
      end
      return unless machine || File.exist?(File.join(coordination_store.root, 'ledger.json'))
      attempt = @coordination_attempt || @env['DEV_WORKSPACE_COORDINATION_ATTEMPT']
      unless attempt && attempt.match?(WorkspaceCoordination::UUID)
        raise WorkspaceCoordination::Error.new('profile activation requires a coordinated machine reservation', 4)
      end
      data = coordination_store.read
      job = data['jobs'].values.find { |value| value['attempt'] == attempt }
      reservation = job && data['reservations'][job['reservationId']]
      # An uncertain original attempt retains its claim while reconciliation
      # completes the exact prepared candidate. It must not need a new job or
      # a reservation-state rewrite to recover forward.
      recovering = job && job['state'] == 'reconciling' && reservation &&
        reservation['state'] == 'blocked_recovery' && reservation['history'].any? do |record|
          record['attempt'] == attempt && record['kind'] == 'profile_activate' &&
            record['artifact'] == job.dig('snapshot', 'operation', 'preparedArtifact') && record['outcome'] == 'unknown'
        end
      unless job && job['kind'] == 'profile_activate' && WorkspaceCoordination::CLAIMED.include?(job['state']) &&
             job.dig('snapshot', 'operation', 'candidatePackage') == candidate &&
             reservation&.fetch('ownerId') == job['ownerId'] &&
             (%w[held deployed_review].include?(reservation&.fetch('state')) || recovering)
        raise WorkspaceCoordination::Error.new('profile attempt has no exact admitted machine claim', 4)
      end
    end

    def activate_prepared_command(argv)
      options = {}
      OptionParser.new do |opts|
        opts.on('--artifact ID') { |value| options[:artifact] = value }
        opts.on('--attempt ID') { |value| options[:attempt] = value }
      end.parse!(argv)
      raise WorkspaceCoordination::Error, 'activate-prepared requires artifact and attempt' unless argv.empty? && options[:artifact] && options[:attempt]
      job = coordination_store.read['jobs'].values.find { |item| item['attempt'] == options[:attempt] }
      raise WorkspaceCoordination::Error, 'unknown profile attempt' unless job
      owner = coordination_store.read['owners'].fetch(job['ownerId'])
      entry = registry.entries.find { |item| item['root'] == owner['workspace'] && item['name'] == owner['workspaceName'] } || raise(WorkspaceCoordination::Error, 'attempt workspace is not registered')
      engine = coordination_engine(entry)
      prepared = engine.artifact!(options.fetch(:artifact))
      @coordination_attempt = options.fetch(:attempt)
      activation = nil
      with_transition_lock do
        require_coordination_profile_claim!(prepared.fetch('candidatePackage'))
        forward = File.symlink?(@profile) && File.realpath(@profile) == prepared.fetch('candidatePackage')
        phase_path = engine.attempt_path(options[:attempt], 'phase.json')
        phase = File.file?(phase_path) && WorkspaceCoordination.private_read(phase_path, limit: WorkspaceCoordination::MAX_REQUEST)
        forward &&= phase && phase['attemptId'] == options[:attempt] && %w[profile_intent verified].include?(phase['phase'])
        unless forward || prepared.fetch('profileToken') == profile_link_token
          raise WorkspaceCoordination::Error.new('selected package changed since preparation', 3)
        end
        activation = activate_candidate(prepared.fetch('candidatePackage'), prepared:)
      end
      complete_candidate_activation(activation)
    ensure
      @coordination_attempt = nil
    end

    def coordinated_profile_switch(prepared, from_candidate:)
      slug = @env['DEV_SESSION_SLUG']
      entry = registry.select(cwd: Dir.pwd, environment_name: @env['DEV_WORKSPACE_NAME'])
      unless slug && @env['DEV_SESSION_WORKSPACE'] == entry.fetch('root')
        raise WorkspaceCoordination::Error.new('use the owning session coordination prepare/submit workflow for this machine', 4)
      end
      engine = coordination_engine(entry)
      unless prepared.dig('sourceIdentity', 'head') && capture!('git', '-C', prepared.fetch('source'), 'status', '--porcelain').empty?
        raise WorkspaceCoordination::Error.new('coordinated profile selection requires a clean committed source', 4)
      end
      prepared['coverage'] = engine.coverage(prepared.fetch('source'), 'workspace', prepared.dig('sourceIdentity', 'head'))
      owner, existing = nil, nil
      with_coordination_session_admission(entry, slug) do
        owner = engine.owner(slug)
        existing = engine.find_or_submit_profile(owner, prepared)
      end
      @err.puts "profile operation #{existing['id']}; #{owner['portalUrl']}"
      if from_candidate && File.realpath(package_root) != prepared['candidatePackage']
        raise WorkspaceCoordination::Error.new('candidate recovery requires the exact prepared package', 4)
      end
      loop do
        current = engine.store.read['jobs'].fetch(existing['id'])
        if current['state'] == 'succeeded'
          observed = current.dig('result', 'observed')
          unless observed && observed['package'] == File.realpath(@profile) && observed['profileToken'] == profile_link_token
            raise WorkspaceCoordination::Error.new('successful profile receipt no longer matches live selection', 3)
          end
          @out.puts "workspace package: #{prepared['candidatePackage']}"
          return
        end
        unless %w[claimed running ready waiting_dependency waiting_resource waiting_approval].include?(current['state'])
          raise WorkspaceCoordination::Error.new("profile operation #{current['id']} is #{current['state']}; #{owner['portalUrl']}", 5)
        end
        sleep 2
      end
    end

    def coordination_worker_command(argv)
      raise WorkspaceCoordination::Error, 'coordination-worker requires --request PATH' unless argv.length == 2 && argv[0] == '--request'
      WorkspaceCoordination::Worker.new(argv[1]).run
    end

    def coordination_reconcile_command(argv)
      raise WorkspaceCoordination::Error, 'coordination-reconcile requires --request PATH' unless argv.length == 2 && argv[0] == '--request'
      WorkspaceCoordination::Worker.new(argv[1]).reconcile
    end

    def coordination_integration_command(argv)
      raise WorkspaceCoordination::Error, 'coordination-integration requires --request PATH' unless argv.length == 2 && argv[0] == '--request'
      request = WorkspaceCoordination.private_read(argv[1], limit: WorkspaceCoordination::MAX_REQUEST)
      WorkspaceCoordination.keys!(request, %w[schema runId ownerId identity workspace entry package profile])
      unless request['schema'] == 1 && request['package'] == File.realpath(package_root) && request['profile'] == @profile
        raise WorkspaceCoordination::Error.new('integration worker generation differs', 3)
      end
      entry = registry.entries.find { |value| value['root'] == request['workspace'] && value['name'] == request.dig('entry', 'name') }
      raise WorkspaceCoordination::Error.new('integration workspace changed', 4) unless entry
      engine = coordination_engine(entry)
      owner = nil
      with_coordination_session_admission(entry, engine.store.read['owners'].fetch(request['ownerId'])['slug']) do
        owner = engine.owner(engine.store.read['owners'].fetch(request['ownerId'])['slug'])
        unless owner['id'] == request['ownerId'] && owner['identity'] == request['identity']
          raise WorkspaceCoordination::Error.new('integration owner changed', 4)
        end
      end
      driver = WorkspaceCoordination::Integration.new(engine, owner,
        admission: ->(&block) { with_coordination_session_admission(entry, owner['slug'], &block) },
        prepare: ->(fields) { coordination_prepare(engine, owner, fields) }, diagnostics: @err)
      driver.run_background(request['runId'])
    end

    def coordination_notifications_command(argv)
      raise WorkspaceCoordination::Error, 'coordination-notifications requires --request PATH' unless argv.length == 2 && argv[0] == '--request'
      request = WorkspaceCoordination.private_read(argv[1], limit: WorkspaceCoordination::MAX_REQUEST)
      WorkspaceCoordination.keys!(request, %w[schema ownerId identity workspace entry package profile])
      unless request['schema'] == 1 && request['ownerId'].match?(WorkspaceCoordination::UUID) &&
             request['package'] == File.realpath(package_root) && request['profile'] == @profile
        raise WorkspaceCoordination::Error.new('notification worker generation differs', 3)
      end
      entry = registry.entries.find { |value| value['root'] == request['workspace'] && value['name'] == request.dig('entry', 'name') }
      raise WorkspaceCoordination::Error.new('notification workspace changed', 4) unless entry
      engine = coordination_engine(entry)
      engine.store.lock("notifications-#{request['ownerId']}", nonblock: true) do
        owner = engine.store.read['owners'].fetch(request['ownerId'])
        with_coordination_session_admission(entry, owner['slug']) do
          current = engine.notification_owner(owner)
          unless current['id'] == request['ownerId'] && current['identity'] == request['identity']
            raise WorkspaceCoordination::Error.new('notification owner changed', 4)
          end
          engine.deliver_notifications(owner_id: current['id'])
        end
      end
    end

    def run_coordinator(argv)
      raise WorkspaceCoordination::Error, 'run-coordinator takes no arguments' unless argv.empty?
      coordination_store.lock('scheduler', nonblock: true) do
        wake_path = File.join(coordination_store.root, 'wake.sock')
        File.unlink(wake_path) if File.exist?(wake_path)
        wake_socket = Socket.new(Socket::AF_UNIX, Socket::SOCK_DGRAM, 0)
        wake_socket.bind(Socket.sockaddr_un(wake_path))
        File.chmod(0o600, wake_path)
        generation_token = profile_link_token
        loop do
          break unless File.symlink?(@profile) && File.realpath(@profile) == File.realpath(package_root) && generation_token == profile_link_token
          entries = registry.entries
          entries.each do |entry|
            engine = coordination_engine(entry)
            engine.reconcile_attempts
            engine.store.read['owners'].values.select { |owner| owner['workspace'] == entry['root'] && !owner['fence'] }.each do |owner|
              coordinate_owner(entry, engine, owner)
            rescue WorkspaceCoordination::Error => e
              @err.puts "coordinator owner #{owner['slug']}: #{e.message}"
            end
          rescue WorkspaceCoordination::Error, Error => e
            @err.puts "coordinator: #{e.message}"
          end
          begin
            dispatch_coordination_collection
          rescue WorkspaceCoordination::Error, Error => error
            @err.puts "coordination collection: #{error.message}"
          end
          if IO.select([wake_socket], nil, nil, 5)
            wake_socket.recv_nonblock(64) while IO.select([wake_socket], nil, nil, 0)
          end
        end
      ensure
        wake_socket&.close
        File.unlink(wake_path) if wake_path && File.exist?(wake_path)
      end
    end

    def coordinate_owner(entry, engine, owner)
      claim_work = engine.store.claims['claims'].values.any? { |item| item['ownerId'] == owner['id'] && (item['state'] == 'waiting' || item['notificationPending']) }
      if claim_work
        with_coordination_session_admission(entry, owner['slug'], timeout: 0) do
          current = engine.claim_owner(owner['slug'])
          raise WorkspaceCoordination::Error.new('resource owner changed', 4) unless current['id'] == owner['id']
          engine.tick_claims(owner)
        end
      end
      return if owner['bootstrap']
      jobs = engine.store.read['jobs'].values.select { |job| job['ownerId'] == owner['id'] && !WorkspaceCoordination::TERMINAL.include?(job['state']) }
      background = engine.store.integration_journals.any? { |run| run['ownerId'] == owner['id'] && run['schema'] == 2 && run.dig('continuation', 'state') != 'complete' }
      unless jobs.empty? && !background
        with_coordination_session_admission(entry, owner['slug'], timeout: 0) do
          current = engine.owner(owner['slug'])
          raise WorkspaceCoordination::Error.new('retained coordination owner changed', 4) unless current['id'] == owner['id']
        end
        jobs.select { |job| WorkspaceCoordination::CLAIMED.include?(job['state']) }.each do |job|
          begin
            engine.dispatch_reconciliation(job)
          rescue WorkspaceCoordination::Error => error
            raise unless [5, 75].include?(error.code)
          end
        end
        claimed = []
        with_coordination_session_admission(entry, owner['slug'], timeout: 0) do
          current = engine.owner(owner['slug'])
          raise WorkspaceCoordination::Error.new('retained coordination owner changed', 4) unless current['id'] == owner['id']
          claimed = engine.claim(owner_id: owner['id'])
        end
        claimed.each { |job| engine.dispatch(job) }
        engine.dispatch_integrations(owner)
      end
      with_coordination_session_admission(entry, owner['slug'], timeout: 0) do
        engine.notification_owner(owner)
        engine.remind_integrations(owner)
      end
      engine.dispatch_notifications(owner)
    end

    def coordination_bootstrap(argv)
      options = {}
      OptionParser.new do |opts|
        opts.on('--source PATH') { |value| options[:source] = value }
        opts.on('--workspace NAME') { |value| options[:workspace] = value }
        opts.on('--session SLUG') { |value| options[:slug] = value }
        opts.on('--expected-profile-token TOKEN') { |value| options[:token] = value }
        opts.on('--request FILE') { |value| options[:request] = value }
        opts.on('--prepare') { options[:prepare] = true }
      end.parse!(argv)
      unless argv.empty? && (options.keys - [:prepare]).sort == %i[request slug source token workspace].sort
        raise WorkspaceCoordination::Error, 'bootstrap requires source, workspace, session, expected-profile-token and request'
      end
      entry = registry.find(options.fetch(:workspace)) || raise(WorkspaceCoordination::Error, 'bootstrap workspace is not registered')
      engine = coordination_engine(entry)
      request = coordination_input(options.fetch(:request))
      if options[:prepare]
        WorkspaceCoordination.keys!(request, %w[schema requestId approvalQuote evidence target])
        raise WorkspaceCoordination::Error.new('bootstrap preparation baseline changed', 3) unless options.fetch(:token) == profile_link_token
        raise WorkspaceCoordination::Error.new('bootstrap preparation requires recorded deployment direction', 4) unless request['approvalQuote'].is_a?(String) && !request['approvalQuote'].strip.empty?
        source_identity = coordination_source_identity(File.realpath(options.fetch(:source)))
        raise WorkspaceCoordination::Error, 'bootstrap source must be a clean committed checkout' unless source_identity['head'] && capture!('git', '-C', options[:source], 'status', '--porcelain').empty?
        source = engine.retain_commit(entry.fetch('root'), source_identity.fetch('head'))
        prepared = prepare_profile(['--source', source])
        raise WorkspaceCoordination::Error.new('bootstrap preparation must execute from its exact candidate package', 4) unless prepared.fetch('candidatePackage') == File.realpath(package_root)
        with_transition_lock do
          raise WorkspaceCoordination::Error.new('bootstrap preparation baseline changed', 3) unless options[:token] == profile_link_token
          old = engine.store.read
          raise WorkspaceCoordination::Error.new('bootstrap refuses unexplained coordination owners', 5) if old['owners'].values.any? { |owner| !owner['bootstrap'] || owner['workspace'] != entry['root'] || owner['slug'] != options[:slug] }
          @bootstrap_owner = engine.owner(options.fetch(:slug), bootstrap: true)
        end
        prepared['coverage'] = engine.coverage(source, 'workspace', source_identity.fetch('head'))
        prepared = engine.save_artifact(prepared)
        engine.retain_root(prepared.fetch('id'), 'profile', prepared.fetch('candidatePackage'))
        machine = engine.prepare_machine(@bootstrap_owner, request.fetch('target'), request.fetch('evidence'))
        @out.puts JSON.generate('schema' => 1, 'source' => source, 'request' => request.slice('schema', 'requestId', 'approvalQuote', 'evidence').merge(
          'machineSnapshot' => machine, 'profileArtifact' => prepared.fetch('id')))
        return
      end
      WorkspaceCoordination.keys!(request, %w[schema requestId approvalQuote evidence machineSnapshot profileArtifact])
      prepared = engine.artifact!(request.fetch('profileArtifact'))
      retained_bootstrap = coordination_store.read['receipts'].key?(request.fetch('requestId'))
      unless File.realpath(prepared.fetch('source')) == File.realpath(options.fetch(:source)) &&
             File.realpath(package_root) == prepared.fetch('candidatePackage') && options.fetch(:token) == prepared.fetch('profileToken') &&
             (options.fetch(:token) == profile_link_token || retained_bootstrap && File.realpath(@profile) == prepared.fetch('candidatePackage'))
        raise WorkspaceCoordination::Error.new('bootstrap is candidate-only and binds the exact prepared source and predecessor profile', 4)
      end
      with_transition_lock do
        raise WorkspaceCoordination::Error.new('bootstrap baseline changed', 3) unless options.fetch(:token) == profile_link_token || retained_bootstrap && File.realpath(@profile) == prepared.fetch('candidatePackage')
        old = coordination_store.read
        existing = old['owners'].values.find { |owner| owner['bootstrap'] && owner['workspace'] == entry['root'] && owner['slug'] == options[:slug] }
        raise WorkspaceCoordination::Error.new('bootstrap refuses unexplained existing coordination state', 5) if !existing && !old['owners'].empty?
        @bootstrap_owner = engine.owner(options.fetch(:slug), bootstrap: true)
      end
      # Candidate bootstrap may prepare a ledger while the predecessor remains
      # selected. Its authority is limited to this explicit synchronous pair.
      @out.puts JSON.generate(engine.bootstrap_submit(@bootstrap_owner, request, prepared))
    end
  end
end
