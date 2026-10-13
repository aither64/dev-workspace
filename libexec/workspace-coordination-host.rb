# frozen_string_literal: true

require_relative 'workspace-coordination-execution'

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

    def coordination_engine(entry)
      runtime = instance_runtime(entry)
      WorkspaceCoordination::Engine.new(state: @state, package: package_root, profile: @profile,
        workspace: entry.fetch('root'), entry: entry.merge('socket' => runtime.fetch(:codex),
          'authority' => runtime.fetch(:authority), 'portalUrl' => "https://#{entry.fetch('hostname')}"),
        env: @env.to_h.merge('DEV_WORKSPACE_NAME' => entry.fetch('name'), 'DEV_WORKSPACE_CODEX_HOME' => codex_home))
    end

    def coordination_input(path)
      payload = path == '-' ? $stdin.read(WorkspaceCoordination::MAX_REQUEST + 1) : File.binread(path, WorkspaceCoordination::MAX_REQUEST + 1)
      WorkspaceCoordination.parse(payload)
    end

    def coordination_session_command(entry, argv)
      action = argv.shift
      slug = argv.shift
      raise WorkspaceCoordination::Error, 'coordination requires an exact session slug and --as-is' unless slug && slug.match?(WorkspaceCoordination::PART)
      id = argv.shift if %w[refresh cancel finish reconcile].include?(action)
      options = {}
      OptionParser.new do |opts|
        opts.on('--as-is') { options[:as_is] = true }
        opts.on('--json') { options[:json] = true }
        opts.on('--portal') { options[:portal] = true }
        opts.on('--request FILE') { |value| options[:request] = value }
        opts.on('--bootstrap-owner ID') { |value| options[:bootstrap_owner] = value }
      end.parse!(argv)
      raise WorkspaceCoordination::Error, 'use coordination ACTION SLUG [ID] --as-is --request FILE' unless argv.empty? && options[:as_is]
      engine = coordination_engine(entry)
      if options[:bootstrap_owner] && !%w[status reconcile cancel finish].include?(action)
        raise WorkspaceCoordination::Error.new('bootstrap maintenance cannot enqueue or approve new work', 4)
      end
      if action == 'status'
        with_coordination_session_admission(entry, slug) { engine.existing_bootstrap_owner(slug, options[:bootstrap_owner]) } if options[:bootstrap_owner]
        @out.puts JSON.generate(engine.status(slug, portal: options[:portal]))
        return
      end
      request = options[:request] && coordination_input(options[:request])
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
      if action == 'prepare'
        raise WorkspaceCoordination::Error, 'prepare requires a request' unless request
        owner = nil
        with_coordination_session_admission(entry, slug) { owner = engine.owner(slug) }
        result = case request.fetch('kind')
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
        @out.puts JSON.generate(result)
        return
      end
      with_coordination_session_admission(entry, slug) do
        owner = options[:bootstrap_owner] ? engine.existing_bootstrap_owner(slug, options[:bootstrap_owner]) : engine.owner(slug)
        result = case action
        when 'finish' then engine.finish(owner, id, request || raise(WorkspaceCoordination::Error, 'finish requires a request'))
        else
          raise WorkspaceCoordination::Error, 'mutation requires --request FILE' unless request
          engine.mutate(owner, action, request, job_id: id)
        end
        @out.puts JSON.generate(result)
      end
    end

    def with_coordination_session_admission(entry, slug)
      with_current_generation_lock(mode: File::LOCK_SH) do
        runtime = instance_runtime(entry)
        FileUtils.mkdir_p(runtime.fetch(:authority), mode: 0o700)
        path = File.join(runtime.fetch(:authority), "#{slug}.lock")
        raise WorkspaceCoordination::Error, 'session lock is a symlink' if File.symlink?(path)
        File.open(path, File::RDWR | File::CREAT, 0o600) do |file|
          raise WorkspaceCoordination::Error.new('session lifecycle admission is busy', 75) unless file.flock(File::LOCK_EX | File::LOCK_NB)
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

    def coordination_host_command(argv)
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
      if data['jobs'].values.any? { |job| job['kind'] == 'profile_activate' && WorkspaceCoordination::CLAIMED.include?(job['state']) } ||
         data['reservations'].values.any? { |item| item['machine'] == machine && !%w[requested released].include?(item['state']) }
        raise WorkspaceCoordination::Error.new('registration waits for the machine review reservation', 4)
      end
    end

    def prepare_profile_command(argv)
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
      return unless machine || File.exist?(File.join(coordination_store.root, 'ledger.json'))
      attempt = @coordination_attempt || @env['DEV_WORKSPACE_COORDINATION_ATTEMPT']
      unless attempt && attempt.match?(WorkspaceCoordination::UUID)
        raise WorkspaceCoordination::Error.new('profile activation requires a coordinated machine reservation', 4)
      end
      data = coordination_store.read
      job = data['jobs'].values.find { |value| value['attempt'] == attempt }
      unless job && job['kind'] == 'profile_activate' && WorkspaceCoordination::CLAIMED.include?(job['state']) &&
             job.dig('snapshot', 'operation', 'candidatePackage') == candidate &&
             data['reservations'][job['reservationId']]&.fetch('ownerId') == job['ownerId'] &&
             %w[held deployed_review].include?(data['reservations'][job['reservationId']]&.fetch('state'))
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
              next if owner['bootstrap']
              claimed = []
              with_coordination_session_admission(entry, owner['slug']) do
                current = engine.owner(owner['slug'])
                raise WorkspaceCoordination::Error.new('retained coordination owner changed', 4) unless current['id'] == owner['id']
              end
              engine.store.read['jobs'].values.select { |job| job['ownerId'] == owner['id'] && WorkspaceCoordination::CLAIMED.include?(job['state']) }.each do |job|
                begin
                  engine.dispatch_reconciliation(job)
                rescue WorkspaceCoordination::Error => e
                  raise unless [5, 75].include?(e.code)
                end
              end
              with_coordination_session_admission(entry, owner['slug']) do
                current = engine.owner(owner['slug'])
                raise WorkspaceCoordination::Error.new('retained coordination owner changed', 4) unless current['id'] == owner['id']
                claimed = engine.claim(owner_id: owner['id'])
                engine.deliver_notifications(owner_id: owner['id'])
              end
              claimed.each { |job| engine.dispatch(job) }
            rescue WorkspaceCoordination::Error => e
              @err.puts "coordinator owner #{owner['slug']}: #{e.message}"
            end
          rescue WorkspaceCoordination::Error, Error => e
            @err.puts "coordinator: #{e.message}"
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
