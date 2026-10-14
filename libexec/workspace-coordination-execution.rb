# frozen_string_literal: true

require_relative 'workspace-coordination'

module WorkspaceCoordination
  class Engine
    PROFILE_IDENTITY = %w[kind candidatePackage sourceIdentity profileToken catalogDigest registryDigest plansDigest candidateCodex coverage].freeze

    def preparation_capacity!
      return if @fixtures
      FileUtils.mkdir_p(store.root, mode: 0o700)
      fields = capture('df', '-Pk', store.root).lines.last.to_s.split
      raise Error, 'coordination preparation requires at least 1 GiB free space' unless fields.length >= 6 && fields[3].match?(/\A[0-9]+\z/) && fields[3].to_i * 1024 >= 1024 * 1024 * 1024
    end

    def find_or_submit_profile(owner, prepared)
      current_generation!
      ledger = store.read
      machine = catalog.machine(catalog.data.fetch('profileMachine')).fetch('id')
      reservations = ledger['reservations'].values.select { |item| item['ownerId'] == owner['id'] && item['machine'] == machine && %w[requested held deployed_review].include?(item['state']) }
      raise Error.new('request this machine reservation before profile selection', 4) unless reservations.length == 1
      reservation = reservations.first
      jobs = ledger['jobs'].values.select { |job| job['ownerId'] == owner['id'] && job['kind'] == 'profile_activate' && job['reservationId'] == reservation['id'] }
      matching = jobs.select do |job|
        artifact = artifact!(job.dig('snapshot', 'operation', 'preparedArtifact'))
        exact = artifact.slice(*PROFILE_IDENTITY) == prepared.slice(*PROFILE_IDENTITY)
        if job['state'] == 'succeeded'
          identity = PROFILE_IDENTITY - ['profileToken']
          exact = artifact.slice(*identity) == prepared.slice(*identity) &&
            job.dig('result', 'observed', 'package') == File.realpath(profile) && job.dig('result', 'observed', 'profileToken') == DevWorkspaceProfileIdentity.token(profile)
        end
        exact && !%w[failed canceled needs_refresh].include?(job['state'])
      end
      raise Error.new('multiple retained profile operations match; use the explicit job workflow', 4) if matching.length > 1
      return matching.first if matching.length == 1
      pending = ledger['jobs'].values.any? { |job| job['ownerId'] == owner['id'] && job['reservationId'] == reservation['id'] && !TERMINAL.include?(job['state']) }
      raise Error.new('another deployment is pending; submit explicit dependencies', 4) if pending
      approvals = ledger['approvals'].values.select { |value| value['ownerId'] == owner['id'] && value['kind'] == 'deploy' }.sort_by { |item| item['id'] }
      candidates = approvals.filter_map do |approval|
        saved = prepared_snapshot!(owner, approval['reviewedSnapshot'])
        next unless saved['kind'] == 'profile_activate'
        snap = saved.fetch('snapshot')
        artifact = artifact!(snap.dig('operation', 'preparedArtifact'))
        next unless artifact.slice(*PROFILE_IDENTITY) == prepared.slice(*PROFILE_IDENTITY)
        synthetic = {'kind' => 'profile_activate', 'ownerId' => owner['id'], 'approvalId' => approval['id'], 'reservationId' => reservation['id'], 'snapshot' => snap}
        next unless approved?(ledger, synthetic)
        [approval, snap]
      end
      raise Error.new('prepare and approve this exact profile snapshot before switching', 4) if candidates.empty?
      unless candidates.map { |approval, snap| [snap['id'], approval['scope'], approval['iterationScope']] }.uniq.length == 1
        raise Error.new('conflicting profile approvals; use the explicit submit workflow', 4)
      end
      approval, snap = candidates.first
      hex = Digest::SHA256.hexdigest(JSON.generate(['profile-switch-v1', owner['id'], reservation['id'], snap['id'], approval['id']]))[0, 32]
      id = [hex[0, 8], hex[8, 4], hex[12, 4], hex[16, 4], hex[20, 12]].join('-')
      reply = mutate(owner, 'submit', {'schema' => 1, 'requestId' => id, 'kind' => 'profile_activate', 'dependsOn' => [],
        'approvalId' => approval['id'], 'reservationId' => reservation['id'], 'snapshot' => snap})
      store.read['jobs'].fetch(reply['id'])
    end

    def reconcile_job(owner, id)
      job = store.read['jobs'].fetch(id) { raise Error, 'unknown coordinated operation' }
      raise Error.new('operation belongs to another owner', 4) unless job['ownerId'] == owner['id']
      return reply(job, owner) unless CLAIMED.include?(job['state'])
      directory = File.join(store.root, 'attempts', job.fetch('attempt'))
      FileUtils.mkdir_p(directory, mode: 0o700)
      File.open(File.join(directory, 'worker.lock'), File::RDWR | File::CREAT, 0o600) do |lock|
        raise Error.new('operation worker is still running', 75) unless lock.flock(File::LOCK_EX | File::LOCK_NB)
        raise Error.new('operation service is still running', 75) if !@fixtures && attempt_service_running?(job['attempt'])
        request_path = File.join(directory, 'request.json')
        raise Error.new('dispatch intent has no durable request; retain its claim for operator recovery', 5) unless File.file?(request_path)
        request = WorkspaceCoordination.private_read(request_path, limit: MAX_REQUEST)
        raise Error.new('attempt request identity changed', 5) unless request['digest'] == WorkspaceCoordination.digest(request.reject { |key, _| key == 'digest' }) && request.dig('job', 'id') == id && request.dig('job', 'attempt') == job['attempt']
        phase_path = File.join(directory, 'phase.json')
        phase = File.file?(phase_path) ? WorkspaceCoordination.private_read(phase_path, limit: MAX_REQUEST) : nil
        raise Error.new('attempt phase identity changed', 5) if phase && (phase['attemptId'] != job['attempt'] || phase['requestDigest'] != request['digest'])
        operation = job.dig('snapshot', 'operation')
        outcome, observed, error = 'unknown', nil, nil
        if job['kind'] == 'merge'
          path, = repository(owner, operation.fetch('repository'))
          remote = remote_head(path, operation.fetch('target'))
          capture('git', '-C', path, 'fetch', 'origin', operation.fetch('target'))
          if ancestor?(path, operation.fetch('featureHead'), remote)
            outcome, observed = 'applied', {'head' => operation['featureHead'], 'remoteHead' => remote}
          elsif phase && phase['phase'] == 'admitted'
            outcome, error = 'not_applied', 'worker ended before local mutation intent'
          end
        elsif job['kind'] == 'machine_activate'
          response = adapter_call(operation.fetch('target'), 'reconcile', {'artifact' => artifact!(operation.fetch('preparedArtifact')),
            'expectedLive' => operation.fetch('expectedLive'), 'attemptId' => job['attempt'], 'requestDigest' => request['digest'], 'attemptDirectory' => directory})
          outcome, observed, error = response.values_at('outcome', 'observed', 'error')
          outcome, error = 'not_applied', 'worker ended before machine mutation intent' if phase && phase['phase'] == 'admitted'
        elsif File.realpath(profile) == operation.fetch('candidatePackage')
          # Selection alone is not successful activation. Re-enter the exact
          # admitted candidate to finish its forward activation and checks.
          capture(File.join(operation.fetch('candidatePackage'), 'bin/workspace-host'), 'activate-prepared',
            '--artifact', operation.fetch('preparedArtifact'), '--attempt', job['attempt'])
          outcome, observed = 'applied', {'package' => File.realpath(profile), 'profileToken' => DevWorkspaceProfileIdentity.token(profile)}
        elsif phase && phase['phase'] == 'admitted'
          outcome, error = 'not_applied', 'worker ended before profile mutation intent'
        end
        if outcome == 'unknown'
          raise Error.new(error || 'original effect cannot be proved; claim and reservation remain held', 5)
        end
        result = {'schema' => 1, 'attemptId' => job['attempt'], 'requestDigest' => request['digest'],
          'outcome' => outcome, 'phase' => 'reconciled', 'observed' => observed, 'error' => error && WorkspaceCoordination.diagnostic(error)}
        raise Error.new('reconciliation result exceeds its recovery budget', 5) if JSON.generate(result).bytesize + 1 > MAX_RESULT
        WorkspaceCoordination.atomic(File.join(directory, 'result.json'), result, limit: MAX_RESULT)
      end
      reconcile_attempts
      reply(store.read['jobs'].fetch(id), owner)
    end

    def bootstrap_submit(owner, request, prepared)
      raise Error.new('bootstrap requires a threadless owner', 4) unless owner['bootstrap'] && !owner['thread']
      @bootstrap_token = prepared.fetch('profileToken')
      if store.read['receipts'][request.fetch('requestId')]
        receipt = store.transaction { |data| store.request(data, owner['id'], 'bootstrap', request) { raise Error, 'bootstrap receipt disappeared' } }
        return wait_bootstrap(owner, receipt.fetch('jobs'), receipt.fetch('reservationId'))
      end
      machine_snapshot = snapshot(owner, request.fetch('machineSnapshot'), 'machine_activate').first
      profile_operation = {'preparedArtifact' => prepared.fetch('id'), 'candidatePackage' => prepared.fetch('candidatePackage'),
        'expectedProfileToken' => prepared.fetch('profileToken'), 'coverage' => prepared.fetch('coverage')}
      profile_snapshot = prepare_snapshot(owner, 'profile_activate', profile_operation, request.fetch('evidence'))
      deterministic = lambda do |label|
        hex = Digest::SHA256.hexdigest("#{request.fetch('requestId')}:#{label}")[0, 32]
        [hex[0, 8], hex[8, 4], hex[12, 4], hex[16, 4], hex[20, 12]].join('-')
      end
      machine = catalog.machine(machine_snapshot.dig('operation', 'target'))
      approvals = [machine_snapshot, profile_snapshot].each_with_index.map do |snap, index|
        mutate(owner, 'approve', {'schema' => 1, 'requestId' => deterministic.call("approval-#{index}"), 'kind' => 'deploy',
          'scope' => [{'machine' => machine['id'], 'actions' => %w[switch profile_activate], 'reboot' => false}],
          'source' => {'kind' => 'user_instruction', 'reference' => 'bootstrap initiating conversation', 'quote' => request.fetch('approvalQuote')}, 'reviewedSnapshot' => snap['id']}).fetch('id')
      end
      manifest = YAML.safe_load(File.binread(File.join(workspace, 'work', owner['slug'], 'portal.yml')), permitted_classes: [], aliases: false)
      obligations = manifest.fetch('repositories').map { |reg| {'repository' => reg['name'], 'target' => "refs/heads/#{reg['default_branch']}"} }
      reservation = mutate(owner, 'reserve', {'schema' => 1, 'requestId' => deterministic.call('reservation'), 'target' => machine_snapshot.dig('operation', 'target'),
        'mode' => 'deploy_first', 'approvalId' => approvals.first, 'integrationObligations' => obligations}).fetch('id')
      jobs = []
      [machine_snapshot, profile_snapshot].each_with_index do |snap, index|
        job = mutate(owner, 'submit', {'schema' => 1, 'requestId' => deterministic.call("job-#{index}"), 'kind' => index.zero? ? 'machine_activate' : 'profile_activate',
          'dependsOn' => jobs.dup, 'approvalId' => approvals[index], 'reservationId' => reservation, 'snapshot' => snap})
        jobs << job.fetch('id')
      end
      store.transaction do |data|
        store.request(data, owner['id'], 'bootstrap', request) { {'schema' => 1, 'id' => request['requestId'], 'jobs' => jobs, 'reservationId' => reservation} }
      end
      wait_bootstrap(owner, jobs, reservation)
    end

    def wait_bootstrap(owner, jobs, reservation)
      loop do
        reconcile_attempts
        values = jobs.map { |id| store.read['jobs'].fetch(id) }
        break if values.all? { |job| job['state'] == 'succeeded' }
        values.select { |job| job['state'] == 'reconciling' }.each do |job|
          begin
            reconcile_job(owner, job['id'])
          rescue Error => e
            raise unless e.code == 75
          end
        end
        failed = values.find { |job| %w[failed canceled needs_refresh].include?(job['state']) }
        raise Error.new("bootstrap operation #{failed['id']} is #{failed['state']}; retain reservation and reconcile", 5) if failed
        claim(owner_id: owner['id']).each { |job| dispatch(job) }
        sleep 2
        if File.realpath(profile) == package
          @profile_token = DevWorkspaceProfileIdentity.token(profile)
        end
      end
      {'schema' => 1, 'reservationId' => reservation, 'jobs' => jobs, 'state' => 'deployed_review', 'portalUrl' => owner['portalUrl']}
    end

    def remote_head(path, ref)
      rows = capture('git', '-C', path, 'ls-remote', '--exit-code', 'origin', ref).lines.map { |line| line.split }
      raise Error.new('remote target is missing or ambiguous', 3) unless rows.length == 1 && rows[0][1] == ref && rows[0][0].match?(SHA)
      rows[0][0]
    end

    def attempt_service_running?(attempt)
      raise Error, 'invalid attempt identity' unless attempt.match?(UUID)
      properties = capture('systemctl', '--user', 'show', "workspace-operation-#{attempt}.service",
        '--property=ActiveState,SubState,MainPID').lines.to_h { |line| line.strip.split('=', 2) }
      properties['MainPID'].to_i > 0 || %w[activating deactivating].include?(properties['ActiveState'])
    end

    def ancestor?(path, before, after)
      _out, _err, status = Open3.capture3(@env, 'git', '-C', path, 'merge-base', '--is-ancestor', before, after)
      raise Error.new('Git ancestry could not be checked', 75) unless [0, 1].include?(status.exitstatus)
      status.success?
    end

    def retain_commit(path, revision)
      preparation_capacity!
      raise Error, 'preparation requires an exact committed revision' unless revision.match?(SHA)
      destination = File.join(store.root, 'sources', revision)
      source = File.join(destination, 'checkout')
      FileUtils.mkdir_p(destination, mode: 0o700)
      fresh = !File.directory?(source)
      if fresh
        if block_given?
          # Materialize the exact tree before initializing dependencies, then
          # run the ordinary checkout hook in its prepared environment below.
          capture('git', '-C', path, 'worktree', 'add', '--detach', '--no-checkout', source, revision)
          capture('git', '-C', source, 'read-tree', '-mu', revision)
        else
          capture('git', '-C', path, 'worktree', 'add', '--detach', source, revision)
        end
      end
      actual = capture('git', '-C', source, 'rev-parse', 'HEAD').strip
      dirty = capture('git', '-C', source, 'status', '--porcelain').strip
      raise Error.new('retained source is dirty or changed', 5) unless actual == revision && dirty.empty?
      if block_given?
        hooks = yield source
        with_merge_hooks('hookArtifact' => hooks && hooks['id']) do
          capture('git', '-C', source, 'checkout', '--detach', revision)
        end
      end
      source
    end

    def retain_root(id, name, path)
      raise Error, 'retained closure must be a Nix store path' unless path.start_with?('/nix/store/') || @fixtures
      directory = File.join(store.root, 'roots')
      FileUtils.mkdir_p(directory, mode: 0o700)
      link = File.join(directory, "#{id.delete_prefix('sha256:')}-#{name}")
      if File.symlink?(link)
        raise Error.new('prepared GC root points to another closure', 5) unless File.realpath(link) == File.realpath(path)
      else
        capture('nix-store', '--add-root', link, '--indirect', '--realise', path) unless @fixtures
        File.symlink(path, link) if @fixtures
      end
    end

    def save_artifact(value)
      id = WorkspaceCoordination.digest(value)
      result = value.merge('id' => id)
      raise Error, 'prepared artifact exceeds its protocol bound' if JSON.generate(result).bytesize + 1 > MAX_REQUEST
      path = File.join(store.root, 'sources', id.delete_prefix('sha256:'), 'artifact.json')
      if File.exist?(path)
        raise Error.new('prepared artifact collision', 5) unless WorkspaceCoordination.private_read(path, limit: MAX_REQUEST) == result
      else
        WorkspaceCoordination.atomic(path, result, limit: MAX_REQUEST)
      end
      result
    end

    def coverage(source, source_repository, revision)
      lock = File.join(source, 'flake.lock')
      values = [{'repository' => source_repository, 'revision' => revision}]
      return values unless File.file?(lock)
      graph = WorkspaceCoordination.parse(File.binread(lock))
      catalog.data.fetch('repositories').each do |name, repository|
        mappings = repository.fetch('inputPaths', {})
        paths = mappings.is_a?(Hash) ? mappings.fetch(source_repository, []) : mappings
        paths.each do |input_path|
          node = graph.fetch('root')
          path = input_path.dup
          count = 0
          until path.empty?
            count += 1
            raise Error, 'cyclic flake input mapping' if count > 64
            selected = graph.fetch('nodes').fetch(node).fetch('inputs').fetch(path.shift)
            if selected.is_a?(Array)
              path = selected + path
              node = graph.fetch('root')
            else
              node = selected
            end
          end
          locked = graph.fetch('nodes').fetch(node).fetch('locked')
          next unless locked['rev']
          remote = locked['type'] == 'github' ? "github:#{locked.fetch('owner')}/#{locked.fetch('repo')}" : locked['url']
          next unless remote && input_remote_identity(remote) == WorkspaceCoordination.remote_identity(repository.fetch('remote'))
          values << {'repository' => name, 'revision' => locked.fetch('rev')}
        end
      end
      values.uniq
    end

    def prepare_merge(owner, name, evidence)
      @diagnostics = Command::Log.new
      preparation_capacity!
      capture('ssh', '-V')
      path, _common, _identity, reg = repository(owner, name)
      head = capture('git', '-C', path, 'rev-parse', "#{reg.fetch('branch')}^{commit}").strip
      target = "refs/heads/#{reg.fetch('default_branch')}"
      expected = remote_head(path, target)
      capture('git', '-C', path, 'fetch', 'origin', target)
      raise Error.new('feature must be rebased and verified before preparation', 3) unless ancestor?(path, expected, head)
      local_out, _err, local_status = Open3.capture3(@env, 'git', '-C', path, 'rev-parse', '--verify', target)
      local_head = local_status.success? ? local_out.strip : nil
      raise Error.new('local default branch has unpublished changes; reconcile it before preparation', 3) if local_head && !ancestor?(path, local_head, expected)
      ref = "refs/dev-workspace-coordination/#{SecureRandom.uuid}"
      capture('git', '-C', path, 'update-ref', ref, head)
      hooks = nil
      source = retain_commit(path, head) { |retained| hooks = prepare_merge_hooks(retained, name, head) }
      checkout = nil
      unless name == 'workspace'
        checkout = File.join(store.root, 'sources', SecureRandom.uuid, 'target')
        FileUtils.mkdir_p(File.dirname(checkout), mode: 0o700)
        with_merge_hooks('hookArtifact' => hooks && hooks['id']) do
          capture('git', '-C', path, 'worktree', 'add', '--detach', checkout, expected)
        end
      end
      operation = {'repository' => name, 'target' => target, 'featureBranch' => reg.fetch('branch'),
        'featureHead' => head, 'expectedRemoteHead' => expected, 'comparisonBase' => expected,
        'expectedLocalTargetHead' => local_head, 'targetCheckout' => checkout, 'hookArtifact' => hooks && hooks['id'],
        'patchDigest' => capture('git', '-C', path, 'diff', expected, head, digest: true), 'sourceRef' => ref}
      prepare_snapshot(owner, 'merge', operation, evidence)
    ensure
      persist_preparation_log
    end

    def persist_preparation_log
      return unless @diagnostics
      directory = File.join(store.root, 'preparation', SecureRandom.uuid)
      FileUtils.mkdir_p(directory, mode: 0o700)
      File.open(File.join(directory, 'command-log'), File::WRONLY | File::CREAT | File::TRUNC, 0o600) { |file| file.write(@diagnostics.bytes) }
      @diagnostics = nil
    end

    # Build the repository's hook dependencies before claiming Git resources.
    # Keep only declared hook variables, never the caller's environment dump.
    def prepare_merge_hooks(source, name, head)
      frameworks = %w[.overcommit.yml .pre-commit-config.yaml lefthook.yml .husky]
      return unless frameworks.any? { |file| File.exist?(File.join(source, file)) }
      raise Error, 'coordinated hooks require the repository flake development environment' unless File.file?(File.join(source, 'flake.nix'))
      allowed = %w[PATH GEM_HOME GEM_PATH BUNDLE_GEMFILE BUNDLE_PATH BUNDLE_APP_CONFIG BUNDLER_VERSION NIX_SSL_CERT_FILE SSL_CERT_FILE]
      # print-dev-env describes a shell before shellHook initializes local
      # dependencies. Enter the declared shell in the retained source instead.
      marker = 'WORKSPACE_MERGE_HOOK_ENV='
      program = <<~'RUBY'
        require 'json'
        if File.file?('.overcommit.yml') && File.file?('Gemfile')
          require 'bundler'
          Bundler.setup
          require 'overcommit'
        end
        puts "WORKSPACE_MERGE_HOOK_ENV=#{JSON.generate(ENV.to_h.slice(*ARGV))}"
      RUBY
      original = @env
      begin
        @env = @env.merge(allowed.reject { |key| %w[PATH NIX_SSL_CERT_FILE SSL_CERT_FILE].include?(key) }.to_h { |key| [key, nil] })
        output = capture('nix', 'develop', source, '--command', RbConfig.ruby, '-e', program, *allowed, cwd: source, limit: MAX_LEDGER)
      ensure
        @env = original
      end
      lines = output.lines.select { |value| value.start_with?(marker) }
      raise Error, 'repository shell did not publish one prepared hook environment' unless lines.length == 1
      environment = WorkspaceCoordination.parse(lines.first.delete_prefix(marker), limit: MAX_LEDGER)
      unless environment.is_a?(Hash) && (environment.keys - allowed).empty? && environment.values.all? { |value| value.is_a?(String) }
        raise Error, 'invalid prepared hook environment'
      end
      raise Error, 'prepared hook environment has no executable path' unless environment['PATH'].is_a?(String)
      artifact = save_artifact('kind' => 'merge_hooks', 'repository' => name, 'featureHead' => head, 'environment' => environment)
      paths = environment.values.flat_map { |value| value.scan(%r{/nix/store/[^/:\s]+}) }.uniq
      paths.each_with_index { |value, index| retain_root(artifact['id'], "hook-#{index}", value) }
      artifact
    end

    def with_merge_hooks(operation)
      original = @env
      artifact = artifact!(operation['hookArtifact']) if operation['hookArtifact']
      if artifact
        environment = artifact.fetch('environment')
        @env = @env.merge(environment).merge('PATH' => [environment.fetch('PATH'), @env.fetch('PATH')].join(File::PATH_SEPARATOR))
      end
      yield
    ensure
      @env = original
    end

    def prepare_snapshot(owner, kind, operation, evidence, change_scope: nil)
      value = {'catalogDigest' => catalog.digest, 'preparedGeneration' => {'package' => package,
        'profileToken' => DevWorkspaceProfileIdentity.token(profile)}, 'evidence' => evidence, 'operation' => operation}
      value['changeScope'] = change_scope if change_scope
      result = snapshot(owner, value, kind).first
      path = File.join(store.root, 'sources', result['id'].delete_prefix('sha256:'), 'snapshot.json')
      saved = {'schema' => 1, 'ownerId' => owner['id'], 'kind' => kind, 'snapshot' => result}
      if File.exist?(path)
        raise Error.new('prepared snapshot identity collision', 5) unless WorkspaceCoordination.private_read(path, limit: MAX_REQUEST) == saved
      else
        WorkspaceCoordination.atomic(path, saved, limit: MAX_REQUEST)
      end
      result
    end

    def adapter_call(target, operation, payload)
      machine = catalog.machine(target)
      command = catalog.adapter(machine.fetch('adapter')).fetch('command')
      request = payload.merge('schema' => 1, 'operation' => operation, 'target' => target, 'machine' => machine)
      WorkspaceCoordination.parse(capture(command, input: JSON.generate(request)))
    end

    def prepare_machine(owner, target, evidence)
      @diagnostics = Command::Log.new
      preparation_capacity!
      machine = catalog.machine(target)
      name = machine.fetch('configurationRepository')
      path, _common, _identity, reg = repository(owner, name)
      head = capture('git', '-C', path, 'rev-parse', "#{reg.fetch('branch')}^{commit}").strip
      baseline = remote_head(path, "refs/heads/#{reg.fetch('default_branch')}")
      capture('git', '-C', path, 'fetch', 'origin', "refs/heads/#{reg.fetch('default_branch')}")
      raise Error.new('configuration source lacks its current remote baseline', 3) unless ancestor?(path, baseline, head)
      source = retain_commit(path, head)
      result = adapter_call(target, 'prepare', {'requestId' => SecureRandom.uuid,
        'source' => {'path' => source, 'revision' => head}, 'stateRoot' => store.root})
      artifact = result.fetch('artifact').merge('kind' => 'machine', 'source' => source,
        'configurationRepository' => name, 'configurationBaseline' => baseline,
        'coverage' => effective_machine_coverage(result.fetch('artifact'), name, head), 'machine' => machine.fetch('id'), 'target' => target,
        'expectedLive' => result.fetch('observed'),
        'adapterCommand' => catalog.adapter(machine.fetch('adapter')).fetch('command'))
      artifact = save_artifact(artifact)
      %w[toplevel environmentPackage].each { |key| retain_root(artifact.fetch('id'), key, artifact.fetch(key)) }
      retain_root(artifact.fetch('id'), 'rollback', artifact['autoRollback']) if artifact['autoRollback']
      operation = {'target' => target, 'preparedArtifact' => artifact.fetch('id'), 'expectedLive' => result.fetch('observed'),
        'coverage' => artifact.fetch('coverage'), 'configurationHead' => head}
      prepare_snapshot(owner, 'machine_activate', operation, evidence)
    ensure
      persist_preparation_log
    end

    def effective_machine_coverage(artifact, name, head)
      result = [{'repository' => name, 'revision' => head}]
      artifact.fetch('inputsInfo').each_value do |info|
        next unless info['rev'] && info['url']
        catalog.data.fetch('repositories').each do |repository, config|
          identity = WorkspaceCoordination.remote_identity(config.fetch('remote'))
          normalized = input_remote_identity(info['url'])
          result << {'repository' => repository, 'revision' => info['rev']} if normalized == identity
        end
      end
      result.uniq
    end

    def input_remote_identity(value)
      url = value.delete_prefix('git+')
      if url.start_with?('github:')
        "github.com/#{url.delete_prefix('github:').split('/').first(2).join('/').downcase}"
      elsif url.start_with?('https://github.com/')
        "github.com/#{url.delete_prefix('https://github.com/').delete_suffix('.git').downcase}"
      else
        WorkspaceCoordination.remote_identity(url)
      end
    rescue Error
      nil
    end

    def frontier_valid!(owner, operation)
      return if operation['featureHead'] || (operation['target'].nil? && operation['candidatePackage'].nil?)
      machine_id = operation['target'] ? catalog.machine(operation['target']).fetch('id') : catalog.machine(catalog.data['profileMachine']).fetch('id')
      required = store.read.fetch('frontier').fetch(machine_id, [])
      kind = operation['target'] ? 'machine_activate' : 'profile_activate'
      required = required.select { |item| item['kind'] == kind }
      required.each do |obligation|
        selected = operation.fetch('coverage').select { |item| item['repository'] == obligation['repository'] }
        raise Error.new('prepared deployment omits integrated machine coverage; refresh inputs', 3) if selected.empty?
        name = obligation.fetch('repository')
        path = name == 'workspace' ? owner.fetch('workspace') : File.join(owner.fetch('workspace'), 'repos', "#{name}.git")
        remote = capture('git', '-C', path, 'remote', 'get-url', 'origin').strip
        raise Error.new('frontier repository remote differs from catalog', 4) unless WorkspaceCoordination.remote_identity(remote, fixtures: @fixtures) == WorkspaceCoordination.remote_identity(catalog.repository(name).fetch('remote'), fixtures: @fixtures)
        capture('git', '-C', path, 'fetch', 'origin', obligation.fetch('head'))
        selected.each { |item| capture('git', '-C', path, 'fetch', 'origin', item.fetch('revision')) }
        raise Error.new('prepared deployment would undo integrated machine coverage; rebuild', 3) unless selected.any? { |item| ancestor?(path, obligation.fetch('head'), item.fetch('revision')) }
      end
    end

    def coverage_contracts(owner, coverage)
      manifest = YAML.safe_load(File.binread(File.join(workspace, 'work', owner['slug'], 'portal.yml')), permitted_classes: [], aliases: false)
      coverage.map { |item| item.fetch('repository') }.uniq.map do |name|
        configured = catalog.repository(name)
        reg = manifest.fetch('repositories').find { |item| item['name'] == name }
        target = reg ? "refs/heads/#{reg.fetch('default_branch')}" : configured['defaultTarget']
        raise Error, "unregistered deployed input needs a catalog defaultTarget: #{name}" unless target
        {'repository' => name, 'target' => target, 'owned' => !reg.nil?,
          'remoteIdentity' => WorkspaceCoordination.remote_identity(configured.fetch('remote'), fixtures: @fixtures)}
      end
    end

    def canonical_input_repository(name, contract)
      configured = catalog.repository(name)
      path = name == 'workspace' ? workspace : File.join(workspace, 'repos', "#{name}.git")
      raise Error.new("canonical clone is missing for deployed input: #{name}", 4) unless File.directory?(path)
      identity = WorkspaceCoordination.remote_identity(capture('git', '-C', path, 'remote', 'get-url', 'origin').strip, fixtures: @fixtures)
      unless identity == contract['remoteIdentity'] && identity == WorkspaceCoordination.remote_identity(configured['remote'], fixtures: @fixtures)
        raise Error.new('deployed input canonical remote changed', 4)
      end
      if !contract['owned'] && contract['target'] != configured['defaultTarget']
        raise Error.new('catalog retargeted a deployed input; reviewed recovery is required', 5)
      end
      path
    end

    def fetched_target(path, target)
      ref = "refs/dev-workspace-coordination/proofs/#{SecureRandom.uuid}"
      capture('git', '-C', path, 'fetch', 'origin', "#{target}:#{ref}")
      capture('git', '-C', path, 'rev-parse', "#{ref}^{commit}").strip
    end

    def lineage_evidence(owner, proof)
      WorkspaceCoordination.keys!(proof, %w[kind path sha256])
      path = File.expand_path(proof.fetch('path'), workspace)
      unless path.start_with?(File.join(workspace, 'work', owner['slug']) + '/') && File.file?(path) && !File.symlink?(path) && Digest::SHA256.file(path).hexdigest == proof['sha256']
        raise Error.new('lineage evidence changed or belongs to another owner', 4)
      end
      path
    end

    def ordered_patch_ids(path, base, head)
      raise Error.new('patch-equivalent lineage does not support merge commits', 4) unless capture('git', '-C', path, 'rev-list', '--min-parents=2', "#{base}..#{head}").empty?
      capture('git', '-C', path, 'rev-list', '--reverse', "#{base}..#{head}").lines.map do |line|
        patch = capture('git', '-C', path, 'show', '--format=', '--binary', line.strip, limit: MAX_LEDGER)
        capture('git', 'patch-id', '--stable', input: patch).split.first || 'empty'
      end
    end

    def validate_replacements(owner, reservation, replacements, final)
      raise Error, 'replacement lineage must be a unique list' unless replacements.is_a?(Array) && replacements.uniq == replacements
      keys = replacements.map { |item| item.slice('repository', 'target', 'from') }
      raise Error, 'conflicting replacement mappings' unless keys.uniq.length == keys.length
      replacements.each do |item|
        WorkspaceCoordination.keys!(item, %w[repository target from fromBase to toBase relation approvalId evidence])
        raise Error, 'replacement lineage requires exact retained commits' unless %w[from fromBase to toBase].all? { |key| item[key].is_a?(String) && item[key].match?(SHA) }
        proof = final.find { |value| value.slice('repository', 'target') == item.slice('repository', 'target') }
        raise Error.new('replacement must name the exact final owned head', 4) unless proof && proof['head'] == item['to'] && item['from'] != item['to']
        records = reservation['history'].select do |record|
          record['outcome'] == 'applied' && record.fetch('coverageContracts').any? { |contract| contract['owned'] && contract.slice('repository', 'target') == item.slice('repository', 'target') } &&
            record.fetch('coverage').include?({'repository' => item['repository'], 'revision' => item['from']})
        end
        raise Error.new('replacement source was not deployed by this owner', 4) if records.empty?
        path, = repository(owner, item['repository'])
        %w[from fromBase to toBase].each { |key| capture('git', '-C', path, 'cat-file', '-e', "#{item[key]}^{commit}") }
        unless ancestor?(path, item['fromBase'], item['from']) && ancestor?(path, item['toBase'], item['to']) && !ancestor?(path, item['from'], item['to'])
          raise Error.new('replacement ranges are invalid or mapping is unused', 4)
        end
        raise Error, 'replacement must reference review evidence' unless item.dig('evidence', 'kind') == 'review'
        receipt = WorkspaceCoordination.parse(File.binread(lineage_evidence(owner, item['evidence']), MAX_REQUEST + 1))
        WorkspaceCoordination.keys!(receipt, %w[schema ownerId reservationId mapping decision reviewer artifacts])
        unless receipt['schema'] == 1 && receipt['ownerId'] == owner['id'] && receipt['reservationId'] == reservation['id'] &&
               receipt['mapping'] == item.reject { |key, _| key == 'evidence' } && receipt['decision'] == 'accepted' && receipt['reviewer'].is_a?(String) && !receipt['reviewer'].empty?
          raise Error.new('replacement review does not bind this exact owner and ranges', 4)
        end
        artifacts = receipt.fetch('artifacts')
        unless artifacts.is_a?(Array) && artifacts.map { |artifact| artifact['kind'] }.sort == %w[migration_review range_diff verification]
          raise Error.new('replacement review needs range, verification and migration evidence', 4)
        end
        artifacts.each { |artifact| lineage_evidence(owner, artifact) }
        range = artifacts.find { |artifact| artifact['kind'] == 'range_diff' }
        actual = capture('git', '-C', path, 'range-diff', '--no-color', '--creation-factor=80', "#{item['fromBase']}..#{item['from']}", "#{item['toBase']}..#{item['to']}", digest: true)
        raise Error.new('retained range-diff does not match replacement ranges', 4) unless actual == "sha256:#{range['sha256']}"
        approval = store.read['approvals'][item['approvalId']]
        unless approval && approval['ownerId'] == owner['id'] && approval['kind'] == 'merge' && approval['scope'].include?(item.slice('repository', 'target'))
          raise Error.new('replacement needs applicable user integration approval', 4)
        end
        saved = prepared_snapshot!(owner, approval['reviewedSnapshot'])
        approved_operation = saved['snapshot']['operation']
        unless saved['kind'] == 'merge' && approved_operation.slice('repository', 'target') == item.slice('repository', 'target')
          raise Error.new('replacement approval binds another operation', 4)
        end
        case item['relation']
        when 'patch_equivalent'
          expected_base = approved_operation['featureHead'] == item['from'] ? item['fromBase'] : item['toBase']
          unless [item['from'], item['to']].include?(approved_operation['featureHead']) && approved_operation['comparisonBase'] == expected_base && ordered_patch_ids(path, item['fromBase'], item['from']) == ordered_patch_ids(path, item['toBase'], item['to'])
            raise Error.new('replacement does not preserve the approved ordered patches', 4)
          end
        when 'reviewed_change'
          redeployed = records.all? do |old|
            reservation['history'].drop(reservation['history'].index(old) + 1).any? { |newer| newer['outcome'] == 'applied' && newer['kind'] == old['kind'] && newer['coverage'].include?({'repository' => item['repository'], 'revision' => item['to']}) }
          end
          unless approved_operation['featureHead'] == item['to'] && approved_operation['comparisonBase'] == item['toBase'] && redeployed
            raise Error.new('material replacement needs new approval and later deployment', 4)
          end
        else raise Error, 'unsupported replacement relation'
        end
      end
      replacements
    end

    def finish(owner, id, request)
      WorkspaceCoordination.keys!(request, %w[schema requestId expectedRevision deploymentWorkFinished evidence finalHeads], %w[replacements])
      data = store.read
      if data['receipts'][request['requestId']]
        return store.transaction { |ledger| store.request(ledger, owner['id'], ['finish', id], request) { raise Error, 'missing completion receipt' } }
      end
      reservation = data['reservations'].fetch(id) { raise Error, 'unknown machine reservation' }
      raise Error.new('reservation belongs to another owner', 4) unless reservation['ownerId'] == owner['id']
      raise Error.new('reservation is already released; reuse its completion requestId', 3) if reservation['state'] == 'released'
      raise Error.new('reservation revision changed', 3) unless reservation['revision'] == request['expectedRevision']
      raise Error.new('deployment and human review completion must be declared', 4) unless request['deploymentWorkFinished'] == true && request['evidence'].is_a?(String) && !request['evidence'].empty?
      associated = data['jobs'].values.select { |job| job['ownerId'] == owner['id'] && job['kind'] != 'merge' }
      if associated.any? { |job| !TERMINAL.include?(job['state']) } || reservation['state'] == 'blocked_recovery'
        raise Error.new('deployment operations must settle before machine release', 5)
      end
      manifest = YAML.safe_load(File.binread(File.join(workspace, 'work', owner.fetch('slug'), 'portal.yml')), permitted_classes: [], aliases: false)
      obligations = manifest.fetch('repositories').map { |reg| {'repository' => reg['name'], 'target' => "refs/heads/#{reg['default_branch']}"} }
      obligations += reservation.fetch('integrationObligations')
      final = request.fetch('finalHeads')
      raise Error, 'finalHeads must be a unique list' unless final.is_a?(Array) && final.uniq == final
      final.each { |item| WorkspaceCoordination.keys!(item, %w[repository target head]) }
      replacements = request.fetch('replacements', [])
      validate_replacements(owner, reservation, replacements, final)
      deployed = reservation['history'].flat_map { |item| item['coverage'] || [] }
      contracts = reservation['history'].flat_map { |item| item.fetch('coverageContracts') }.uniq
      contracts.select { |item| item['owned'] }.each do |item|
        obligations << item.slice('repository', 'target')
      end
      raise Error, 'finalHeads has conflicting repository targets' unless final.map { |item| item.slice('repository', 'target') }.uniq.length == final.length
      raise Error, 'finalHeads differs from owned integration obligations' unless final.map { |item| item.slice('repository', 'target') }.sort_by(&:to_s) == obligations.uniq.sort_by(&:to_s)
      integration_proofs = []
      obligations.uniq.each do |obligation|
        proof = final.find { |item| item.slice('repository', 'target') == obligation }
        raise Error.new('machine release requires every exact final feature head', 4) unless proof && proof['head'].match?(SHA)
        path, _common, _identity, reg = repository(owner, obligation.fetch('repository'))
        feature = capture('git', '-C', path, 'rev-parse', "#{reg.fetch('branch')}^{commit}").strip
        raise Error.new('final integration proof is not the current exact feature head', 3) unless feature == proof['head']
        head = fetched_target(path, obligation.fetch('target'))
        raise Error.new('machine release requires integration into the configured remote target', 4) unless ancestor?(path, proof['head'], head)
        deployed.select { |item| item['repository'] == obligation['repository'] }.each do |item|
          unless ancestor?(path, item['revision'], proof['head']) || replacements.any? { |mapping| mapping['repository'] == item['repository'] && mapping['from'] == item['revision'] && mapping['to'] == proof['head'] }
            raise Error.new('deployed source was rewritten; retain ownership until reviewed replacement lineage is recorded', 5)
          end
        end
        integration_proofs << proof.merge('remoteHead' => head, 'owned' => true)
      end
      contracts.reject { |item| item['owned'] }.each do |contract|
        path = canonical_input_repository(contract['repository'], contract)
        remote = fetched_target(path, contract['target'])
        deployed.select { |item| item['repository'] == contract['repository'] }.each do |item|
          capture('git', '-C', path, 'fetch', 'origin', item['revision'])
          raise Error.new('unregistered deployed input is not integrated; register its actual owned feature or recover', 4) unless ancestor?(path, item['revision'], remote)
          integration_proofs << contract.slice('repository', 'target', 'owned').merge('head' => item['revision'], 'remoteHead' => remote)
        end
      end
      if reservation['history'].any?
        reservation['history'].group_by { |item| item['kind'] }.each_value do |history|
        last = history.last
        if last['kind'] == 'machine_activate'
          artifact = artifact!(last.fetch('artifact'))
          observed = adapter_call(reservation.fetch('target'), 'inspect', {'artifact' => artifact}).fetch('observed')
          raise Error.new('machine live state changed before release', 5) unless observed == last.fetch('observed')
        elsif File.realpath(profile) != last.fetch('observed').fetch('package')
          raise Error.new('workspace profile changed before machine release', 5)
        end
        end
      end
      store.transaction do |ledger|
        current = ledger['reservations'].fetch(id)
        raise Error.new('reservation changed during final proof', 3) unless current == reservation
        pending = ledger['jobs'].values.any? { |job| job['ownerId'] == owner['id'] && !TERMINAL.include?(job['state']) }
        raise Error.new('owner still has pending coordinated operations', 5) if pending
        raise Error.new('owner still has unresolved notifications or lifecycle recovery', 5) if ledger['owners'][owner['id']]['fence'] || ledger['outbox'].values.any? { |item| item['ownerId'] == owner['id'] && item['state'] != 'delivered' }
        store.request(ledger, owner['id'], ['finish', id], request) do
          current['state'] = 'released'
          current['revision'] += 1
          current['completion'] = request.merge('integrationProofs' => integration_proofs)
          additions = reservation['history'].group_by { |item| item['kind'] }.flat_map do |kind, history|
            names = history.last.fetch('coverage').map { |item| item['repository'] }
            integration_proofs.select { |item| names.include?(item['repository']) }.map { |item| item.slice('repository', 'target', 'head').merge('kind' => kind) }
          end
          ledger['frontier'][current['machine']] = (ledger['frontier'].fetch(current['machine'], []) + additions).uniq
          event(ledger, owner, current, 'released')
          reply(current, owner).merge('requestId' => request.fetch('requestId'))
        end
      end
    end

    def attempt_path(id, file)
      raise Error, 'invalid coordination attempt ID' unless id.match?(UUID)
      File.join(store.root, 'attempts', id, file)
    end

    # Network/forward recovery belongs to an independent process, never the
    # scheduler. The original claim remains held while a probe stalls.
    def dispatch_reconciliation(job)
      request = attempt_path(job.fetch('attempt'), 'request.json')
      return unless File.file?(request)
      retry_path = attempt_path(job['attempt'], 'reconcile-retry.json')
      return if File.file?(retry_path) && WorkspaceCoordination.private_read(retry_path)['retryAt'] > Time.now.to_i
      unit = "workspace-reconcile-#{job.fetch('attempt')}"
      output = capture('systemctl', '--user', 'show', "#{unit}.service", '--property=ActiveState', '--value')
      return if %w[active activating deactivating].include?(output.strip)
      capture('systemd-run', '--user', '--quiet', '--collect', "--unit=#{unit}",
        '--property=Type=exec', '--property=UMask=0077', '--property=Restart=no',
        File.join(package, 'bin/workspace-host'), 'coordination-reconcile', '--request', request)
    end

    def worker_request(job, owner)
      # Audit history and prior outcomes stay in the ledger. The worker seals
      # only its current operation, so refreshes do not enlarge its envelope.
      request = {'schema' => 1, 'job' => job.merge('history' => [], 'result' => nil), 'owner' => owner, 'package' => package,
        'profile' => profile, 'stateRoot' => File.dirname(store.root), 'entry' => entry,
        'workspace' => owner.fetch('workspace'), 'catalogDigest' => catalog.digest}
      request['digest'] = WorkspaceCoordination.digest(request)
      request
    end

    def dispatch(job)
      owner = store.read.fetch('owners').fetch(job.fetch('ownerId'))
      request = worker_request(job, owner)
      path = attempt_path(job.fetch('attempt'), 'request.json')
      WorkspaceCoordination.atomic(path, request, limit: MAX_REQUEST)
      retain_root(job.fetch('attempt'), 'worker', package)
      command = File.join(package, 'bin/workspace-host')
      capture('systemd-run', '--user', '--quiet', "--unit=workspace-operation-#{job.fetch('attempt')}",
        '--property=Type=exec', '--property=UMask=0077', '--property=Restart=no',
        '--property=LogRateLimitIntervalSec=30s', '--property=LogRateLimitBurst=200',
        '--property=RemainAfterExit=yes', command, 'coordination-worker', '--request', path)
    rescue Error => e
      # The unit may have started even when systemd-run lost its reply. Keep the
      # intent; reconciliation inspects the exact unit and worker receipt.
      store.transaction do |data|
        current = data['jobs'].fetch(job.fetch('id'))
        transition(data, current, 'reconciling', {'code' => 'dispatch_uncertain', 'message' => e.message})
      end
    end

    def reconcile_attempts
      store.read.fetch('jobs').values.select { |job| CLAIMED.include?(job['state']) }.each do |job|
        result_path = attempt_path(job.fetch('attempt'), 'result.json')
        phase_path = attempt_path(job.fetch('attempt'), 'phase.json')
        if File.file?(result_path)
          result = WorkspaceCoordination.private_read(result_path, limit: MAX_RESULT)
          WorkspaceCoordination.keys!(result, %w[schema attemptId requestDigest outcome phase observed error])
          request = WorkspaceCoordination.private_read(attempt_path(job.fetch('attempt'), 'request.json'), limit: MAX_REQUEST)
          unless result['schema'] == 1 && result['attemptId'] == job['attempt'] && result['requestDigest'] == request['digest'] &&
                 %w[applied not_applied baseline_changed unknown].include?(result['outcome'])
            raise Error.new('attempt result does not match its admitted request', 5)
          end
          store.transaction do |data|
            current = data['jobs'].fetch(job.fetch('id'))
            next unless current['attempt'] == job['attempt'] && CLAIMED.include?(current['state'])
            current['result'] = result
            state = {'applied' => 'succeeded', 'baseline_changed' => 'needs_refresh', 'not_applied' => 'failed', 'unknown' => 'reconciling'}.fetch(result['outcome'])
            transition(data, current, state, result['error'] && {'code' => result['outcome'], 'message' => result['error']})
            if current['reservationId'] && %w[applied unknown].include?(result['outcome'])
              reservation = data['reservations'].fetch(current.fetch('reservationId'))
              record = {'attempt' => job['attempt'], 'kind' => job['kind'], 'artifact' => job.dig('snapshot', 'operation', 'preparedArtifact'),
                'coverage' => job.dig('snapshot', 'operation', 'coverage'), 'coverageContracts' => job['coverageContracts'], 'observed' => result['observed'], 'outcome' => result['outcome']}
              existing = reservation['history'].find { |item| item['attempt'] == job['attempt'] }
              if existing && existing['outcome'] != record['outcome']
                unless existing['outcome'] == 'unknown' && record['outcome'] == 'applied' && !existing.key?('dispositions')
                  raise Error.new('deployment history has an unsupported repeated disposition', 5)
                end
                existing['dispositions'] ||= []
                existing['dispositions'] << existing.slice('outcome', 'observed')
                existing['outcome'], existing['observed'] = record['outcome'], record['observed']
                reservation['state'] = reservation['history'].any? { |item| item['outcome'] == 'unknown' } ? 'blocked_recovery' : 'deployed_review'
                reservation['revision'] += 1
              elsif !existing
                reservation['history'] << record
                reservation['state'] = result['outcome'] == 'applied' ? 'deployed_review' : 'blocked_recovery'
                reservation['revision'] += 1
              end
            end
          end
        elsif File.file?(phase_path)
          phase = WorkspaceCoordination.private_read(phase_path, limit: MAX_REQUEST)
          store.transaction { |data| transition(data, data['jobs'].fetch(job['id']), 'running') if phase['attemptId'] == job['attempt'] && job['state'] == 'claimed' }
          active = capture('systemctl', '--user', 'show', "workspace-operation-#{job['attempt']}.service", '--property=ActiveState', '--value').strip
          unless %w[active activating deactivating].include?(active)
            store.transaction { |data| transition(data, data['jobs'].fetch(job['id']), 'reconciling', {'code' => 'worker_interrupted'}) }
          end
        else
          active = capture('systemctl', '--user', 'show', "workspace-operation-#{job['attempt']}.service", '--property=ActiveState', '--value').strip
          # Missing start acknowledgement does not prove that a worker cannot
          # still start. Operator reconciliation retains the resource claim.
          unless %w[active activating].include?(active)
            store.transaction { |data| transition(data, data['jobs'].fetch(job['id']), 'reconciling', {'code' => 'dispatch_requires_reconciliation'}) }
          end
        end
      rescue Error, SystemCallError => e
        warn "coordination reconciliation #{job['id']}: #{e.message}"
      end
    end
  end

  class Worker
    def initialize(path, fixtures: false)
      @request = WorkspaceCoordination.private_read(path, limit: MAX_REQUEST)
      WorkspaceCoordination.keys!(@request, %w[schema job owner package profile stateRoot entry workspace catalogDigest digest])
      raise Error.new('unsupported attempt request schema', 5) unless @request['schema'] == 1
      raise Error.new('attempt request digest changed', 5) unless @request['digest'] == WorkspaceCoordination.digest(@request.reject { |key, _| key == 'digest' })
      @job, @owner = @request.fetch('job'), @request.fetch('owner')
      @engine = Engine.new(state: @request.fetch('stateRoot'), package: @request.fetch('package'),
        profile: @request.fetch('profile'), workspace: @request.fetch('workspace'), entry: @request.fetch('entry'), fixtures:)
      @path = File.dirname(path)
      raise Error.new('attempt package catalog differs', 5) unless @request['catalogDigest'] == @engine.catalog.digest
      raise Error, 'attempt path does not match request' unless @path == File.join(@engine.store.root, 'attempts', @job.fetch('attempt'))
      @fixtures = fixtures
      @diagnostics = Command::Log.new
      @engine.diagnostics = @diagnostics
    end

    def phase(name, extra = {})
      value = {'schema' => 1, 'attemptId' => @job.fetch('attempt'), 'requestDigest' => @request.fetch('digest'),
        'phase' => name, 'updatedAt' => Time.now.utc.iso8601}.merge(extra)
      WorkspaceCoordination.atomic(File.join(@path, 'phase.json'), value)
      @phase = name
    end

    def reconcile
      @engine.reconcile_job(@owner, @job.fetch('id'))
    rescue Error => e
      WorkspaceCoordination.atomic(File.join(@path, 'reconcile-retry.json'),
        {'retryAt' => Time.now.to_i + 30, 'error' => WorkspaceCoordination.diagnostic(e.message)})
      raise
    end

    def result(outcome, observed = nil, error = nil)
      value = {'schema' => 1,
        'attemptId' => @job.fetch('attempt'), 'requestDigest' => @request.fetch('digest'),
        'outcome' => outcome, 'phase' => @phase, 'observed' => observed, 'error' => error && WorkspaceCoordination.diagnostic(error)}
      raise Error.new('attempt result exceeds its recovery budget', 5) if JSON.generate(value).bytesize + 1 > MAX_RESULT
      WorkspaceCoordination.atomic(File.join(@path, 'result.json'), value, limit: MAX_RESULT)
    end

    def run
      File.open(File.join(@path, 'worker.lock'), File::RDWR | File::CREAT, 0o600) do |lock|
        raise Error.new('attempt worker is already running', 5) unless lock.flock(File::LOCK_EX | File::LOCK_NB)
        raise Error.new('attempt already has a result; reconcile it', 5) if File.exist?(File.join(@path, 'result.json'))
        unless @fixtures || ENV['SYSTEMD_EXEC_PID'] == Process.pid.to_s
          raise Error.new('coordination worker must execute as its independent systemd service', 4)
        end
        begin
          @engine.snapshot(@owner, @job.fetch('snapshot'), @job.fetch('kind'))
          admit!
          @engine.frontier_valid!(@owner, @job.fetch('snapshot').fetch('operation'))
          case @job.fetch('kind')
          when 'merge' then merge
          when 'machine_activate' then machine
          when 'profile_activate' then profile
          end
        rescue Error => e
          mutated = %w[local_merge_intent local_merged push_intent machine_intent profile_intent].include?(@phase)
          result(mutated ? 'unknown' : (e.code == 3 ? 'baseline_changed' : 'not_applied'), nil, e.message)
        rescue StandardError => e
          result('unknown', nil, e.message)
        end
      end
    ensure
      if @diagnostics && @path
        log = File.join(@path, 'log')
        File.open(log, File::WRONLY | File::CREAT | File::TRUNC, 0o600) { |file| file.write(@diagnostics.bytes) }
      end
    end

    def admit!
      state = @request.fetch('stateRoot')
      File.open(File.join(state, 'transition.lock'), File::RDWR | File::CREAT, 0o600) do |transition|
        # Contention is a wait, not stale evidence. Revalidate the retained
        # attempt after acquisition, including a generation changed while waiting.
        transition.flock(File::LOCK_SH)
        authority = @request.fetch('entry').fetch('authority')
        FileUtils.mkdir_p(authority, mode: 0o700)
        File.open(File.join(authority, "#{@owner.fetch('slug')}.lock"), File::RDWR | File::CREAT, 0o600) do |session_lock|
          session_lock.flock(File::LOCK_EX)
          live = @engine.owner(@owner['slug'], bootstrap: @owner['bootstrap'])
          raise Error.new('attempt owner identity changed', 4) unless live['id'] == @owner['id']
          ledger = @engine.store.read
          current = ledger.fetch('jobs').fetch(@job.fetch('id'))
          unless current['attempt'] == @job['attempt'] && CLAIMED.include?(current['state']) && current['snapshot'] == @job['snapshot'] &&
                 !ledger['suspended'] && !ledger['owners'][@owner['id']]['fence'] && @engine.approved?(ledger, current)
            raise Error.new('attempt is no longer admitted under its approval', 4)
          end
          if current['reservationId']
            reservation = ledger['reservations'].fetch(current['reservationId'])
            raise Error.new('attempt reservation is not held', 4) unless reservation['ownerId'] == @owner['id'] && %w[held deployed_review].include?(reservation['state'])
          end
          @engine.snapshot(@owner, @job.fetch('snapshot'), @job.fetch('kind'))
          phase('admitted')
        end
      end
    end

    def merge
      operation = @job.dig('snapshot', 'operation')
      path, common, _identity, reg = @engine.repository(@owner, operation.fetch('repository'))
      observed = @engine.remote_head(path, operation.fetch('target'))
      raise Error.new('remote target advanced; rebase and verify outside the queue', 3) unless observed == operation['expectedRemoteHead']
      raise Error.new('prepared feature is not a fast-forward', 3) unless @engine.ancestor?(path, observed, operation.fetch('featureHead'))
      checkout = operation['repository'] == 'workspace' ? path : operation.fetch('targetCheckout')
      File.open(File.join(common, 'dev-session-tracking.lock'), File::RDWR | File::CREAT, 0o600) do |lock|
        lock.flock(File::LOCK_EX)
        raise Error.new('remote target changed before local integration', 3) unless @engine.remote_head(path, operation.fetch('target')) == observed
        if operation['repository'] == 'workspace'
          branch = @engine.capture('git', '-C', checkout, 'symbolic-ref', 'HEAD').strip
          local = @engine.capture('git', '-C', checkout, 'rev-parse', 'HEAD').strip
          raise Error.new('shared workspace target changed; prepare again', 3) unless branch == operation['target'] && local == observed
        end
        local_out, _err, status = Open3.capture3('git', '-C', path, 'rev-parse', '--verify', operation.fetch('target'))
        local_head = status.success? ? local_out.strip : nil
        raise Error.new('local default branch advanced; prepare again', 3) unless local_head == operation['expectedLocalTargetHead']
        phase('local_merge_intent')
        @engine.with_merge_hooks(operation) do
          @engine.capture('git', '-C', checkout, 'merge', '--ff-only', operation.fetch('featureHead'))
        end
        phase('local_merged')
        if operation['repository'] != 'workspace'
          @engine.capture('git', '-C', path, 'update-ref', operation.fetch('target'), operation.fetch('featureHead'), local_head || ('0' * 40))
        end
        phase('push_intent')
        begin
          @engine.with_merge_hooks(operation) do
            @engine.capture('git', '-C', checkout, 'push', 'origin', "#{operation.fetch('featureHead')}:#{operation.fetch('target')}")
          end
        rescue Error
          # A lost push response is settled by exact remote ancestry below.
        end
        remote = @engine.remote_head(path, operation.fetch('target'))
        @engine.capture('git', '-C', path, 'fetch', 'origin', operation.fetch('target'))
        raise Error.new('local fast-forward is retained; remote push needs reconciliation', 5) unless @engine.ancestor?(path, operation.fetch('featureHead'), remote)
        phase('verified')
        result('applied', {'head' => operation.fetch('featureHead'), 'remoteHead' => remote})
      end
    end

    def machine
      operation = @job.dig('snapshot', 'operation')
      artifact = @engine.artifact!(operation.fetch('preparedArtifact'))
      path, = @engine.repository(@owner, artifact.fetch('configurationRepository'))
      contract = @job.fetch('coverageContracts').find { |item| item['repository'] == artifact['configurationRepository'] }
      baseline = @engine.fetched_target(path, contract.fetch('target'))
      unless @engine.ancestor?(path, baseline, operation.fetch('configurationHead'))
        raise Error.new('configuration target advanced beyond the prepared source; prepare again', 3)
      end
      phase('machine_intent')
      response = @engine.adapter_call(operation.fetch('target'), 'activate', {'artifact' => artifact,
        'expectedLive' => operation.fetch('expectedLive'), 'attemptId' => @job.fetch('attempt'),
        'requestDigest' => @request.fetch('digest'), 'attemptDirectory' => @path})
      result(response.fetch('outcome'), response['observed'], response['error'])
    end

    def profile
      operation = @job.dig('snapshot', 'operation')
      phase('profile_intent')
      command = File.join(@request.fetch('package'), 'bin/workspace-host')
      @engine.capture(command, 'activate-prepared', '--artifact', operation.fetch('preparedArtifact'),
        '--attempt', @job.fetch('attempt'))
      selected = File.realpath(@request.fetch('profile'))
      raise Error.new('selected profile differs from the prepared target', 5) unless selected == operation.fetch('candidatePackage')
      phase('verified')
      result('applied', {'package' => selected, 'profileToken' => DevWorkspaceProfileIdentity.token(@request.fetch('profile'))})
    end
  end
end
