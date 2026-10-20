# frozen_string_literal: true

require_relative 'workspace-integration-certification'
require_relative 'workspace-integration-background'
require_relative 'workspace-integration-handoff'
require 'tempfile'

module WorkspaceCoordination
  class IntegrationCatalog
    attr_reader :data
    def initialize(package)
      path = File.join(package, 'share/dev-workspace/integration.json')
      @data = File.file?(path) ? WorkspaceCoordination.parse(File.binread(path)) : {'schema' => 1, 'recipes' => {}}
      WorkspaceCoordination.keys!(@data, %w[schema recipes], %w[refreshes])
      raise Error, 'unsupported integration catalog' unless @data['schema'] == 1 && @data['recipes'].is_a?(Hash)
      @data['recipes'].each do |name, value|
        WorkspaceCoordination.keys!(value, %w[protocol command])
        unless name.match?(PART) && value['protocol'] == 1 && value['command'].start_with?('/nix/store/') && File.executable?(value['command'])
          raise Error, 'integration recipes must be immutable protocol-1 executables'
        end
      end
    end

    def recipe(name)
      data['recipes'].fetch(name) { raise Error, 'integration recipe is not installed' }
    end
  end

  # A resumable composition of existing operations. It owns no Git integration,
  # reservation, activation or uncertain-effect recovery implementation.
  class Integration
    include BackgroundIntegration
    include IntegrationHandoff
    TERMINAL_PHASES = %w[complete].freeze
    attr_reader :engine, :owner

    def initialize(engine, owner, admission:, prepare:, diagnostics: $stderr)
      @engine, @owner, @admission, @prepare, @diagnostics = engine, owner, admission, prepare, diagnostics
      @catalog = IntegrationCatalog.new(engine.package)
    end

    def path(id)
      raise Error, 'integration run requires an exact UUID' unless id.is_a?(String) && id.match?(UUID)
      File.join(engine.store.root, 'integrations', "#{id}.json")
    end

    def read(id)
      value = WorkspaceCoordination.private_read(path(id))
      unless [1, 2].include?(value['schema']) && value['ownerId'] == owner['id'] && value['identity'] == owner['identity']
        raise Error.new('integration run belongs to another session incarnation', 4)
      end
      value
    end

    def save(value)
      WorkspaceCoordination.atomic(path(value['id']), value)
      engine.store.wake if value['schema'] == 2
    end

    def start(request, wait: true, background: false)
      request = WorkspaceCoordination.canonical(request)
      Array(request['repositories']).each do |item|
        item['recipe'] ||= data_refresh(item['repository'])
        item.delete('recipe') unless item['recipe']
      end
      WorkspaceCoordination.keys!(request, %w[schema requestId mode repositories evidence approval], %w[checks deployment completion])
      raise Error, 'unsupported integration mode' unless request['schema'] == 1 && %w[merge_only integrate_first deploy_first].include?(request['mode'])
      id = request.fetch('requestId')
      path(id)
      engine.store.lock("integration-#{id}", nonblock: true) do
        if File.exist?(path(id))
          value = read(id)
          raise Error.new('integration execution mode differs from the retained run', 3) unless (value['schema'] == 2) == background
          raise Error.new('integration requestId reused with changed input', 3) unless value['request'] == WorkspaceCoordination.canonical(request)
        else
          validate(request)
          preflight(request)
          value = {'schema' => background ? 2 : 1, 'id' => id, 'ownerId' => owner['id'], 'identity' => owner['identity'],
            'phase' => 'prepare', 'request' => WorkspaceCoordination.canonical(request), 'steps' => {},
            'operations' => {}, 'jobs' => {}, 'replacements' => [], 'startedAt' => Time.now.utc.iso8601,
            'phaseSeconds' => {}, 'verificationSeconds' => 0, 'preparationSeconds' => 0,
            'queueSeconds' => 0, 'lastObservedAt' => Time.now.to_f}
          value['continuation'] = {'state' => 'runnable', 'generation' => 0, 'observedRevision' => nil} if background
          admit do
            engine.current_generation!
            verify_tracking_binding
            engine.store.transaction do |data|
              save(value)
              begin
                engine.store.admission_capacity!(data)
              rescue Error
                File.unlink(path(id))
                raise
              end
            end
          end
        end
        true
      end || raise(Error.new('integration driver is already running', 75))
      background ? status(id) : drive(id, wait:)
    end

    def data_refresh(repository)
      @catalog.data.fetch('refreshes', {})[repository]
    end

    def preflight(request)
      request.fetch('repositories').each do |item|
        name = item.fetch('repository')
        repository, _, _, = engine.repository(owner, name)
        engine.capture('git', '-C', repository, 'fetch', 'origin', item.fetch('target'))
        remote = engine.remote_head(repository, item.fetch('target'))
        local, _, status, = Command.run(['git', '-C', repository, 'rev-parse', '--verify', item.fetch('target')])
        if status.success? && !engine.ancestor?(repository, local.strip, remote)
          raise Error.new("#{name}: local #{item['target']} has unpublished commits; reconcile tracking publication before integration", 3)
        end
        worktree = File.join(engine.workspace, 'worktrees', owner['slug'], name)
        if File.directory?(worktree) && !engine.capture('git', '-C', worktree, 'status', '--porcelain').empty?
          raise Error.new("#{name}: owned feature worktree has uncommitted changes", 3)
        end
      end
    end

    def validate(request)
      repositories = request.fetch('repositories')
      unless repositories.is_a?(Array) && !repositories.empty? && repositories.map { |item| item['repository'] }.uniq.length == repositories.length
        raise Error, 'integration repositories must be a nonempty unique list'
      end
      prior = []
      repositories.each do |item|
        WorkspaceCoordination.keys!(item, %w[repository target base head review dependsOn], %w[recipe])
        engine.validate_obligations!(owner, [item.slice('repository', 'target')])
        unless item['dependsOn'].is_a?(Array) && (item['dependsOn'] - prior).empty?
          raise Error, 'repository dependencies must name earlier repositories'
        end
        review = WorkspaceCoordination.parse(File.binread(engine.lineage_evidence(owner, item['review'])))
        unless review['schema'] == 1 && review['decision'] == 'accepted' && review['reviewer'].is_a?(String) && !review['reviewer'].empty? &&
               review.slice('repository', 'target', 'base', 'head') == item.slice('repository', 'target', 'base', 'head') && review['migrations'].is_a?(Array)
          raise Error.new('integration requires a binding independent review and migration inventory', 4)
        end
        prior << item['repository']
      end
      request.fetch('evidence').each { |ref| engine.lineage_evidence(owner, ref) }
      source = request.fetch('approval')
      WorkspaceCoordination.keys!(source, %w[kind reference quote])
      raise Error.new('integration requires the actual user instruction', 4) unless source['kind'] == 'user_instruction' && %w[quote reference].all? { |key| source[key].is_a?(String) && !source[key].strip.empty? }
      if request['mode'] != 'merge_only'
        raise Error, 'deployment modes require a deployment section' unless request['deployment']
      elsif request['deployment']
        raise Error, 'merge-only mode does not deploy'
      end
    end

    def status(id)
      value = read(id)
      value['continuation'] = value['continuation'].merge('state' => 'paused') if value['schema'] == 2 && value.dig('continuation', 'state') != 'held' && paused?(id)
      current_jobs = engine.store.read['jobs']
      value.slice('schema', 'id', 'phase', 'startedAt', 'finishedAt', 'phaseSeconds', 'queueSeconds', 'verificationSeconds', 'preparationSeconds', 'reason', 'continuation').merge(
        'administrationSeconds' => [value['phaseSeconds'].values.sum - value.fetch('verificationSeconds', 0) - value.fetch('preparationSeconds', 0), 0].max,
        'outcome' => value['outcome'], 'handoff' => value['handoff'],
        'heads' => value['operations'].transform_values { |operation| operation.dig('snapshot', 'operation', 'featureHead') },
        'jobs' => value['jobs'].transform_values { |job_id| current_jobs[job_id]&.slice('id', 'kind', 'state', 'reason', 'revision') })
    end

    def drive(id, wait: true)
      loop do
        result = resume(id)
        return result if !wait || result['phase'] == 'complete'
        sleep 2
      end
    end

    def resume(id)
      acquired = engine.store.lock("integration-#{id}", nonblock: true) { resume_locked(id) }
      acquired || raise(Error.new('integration driver is already running', 75))
    end

    def resume_locked(id)
      value = read(id)
      return status(id) if TERMINAL_PHASES.include?(value['phase'])
      engine.current_generation!
      phase = value['phase']
      started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
      begin
        value.delete('reason')
        advance(value)
      rescue Error => error
        value['reason'] = {'code' => error.code, 'message' => error.message}
        raise
      ensure
        value['phaseSeconds'][phase] = value['phaseSeconds'].fetch(phase, 0) + Process.clock_gettime(Process::CLOCK_MONOTONIC) - started
        save(value)
      end
      status(id)
    end

    def step(value, name)
      background_guard! if @background_id
      entry = value['steps'][name] ||= {'requestId' => SecureRandom.uuid}
      save(value) # Persist identity before the caller can mutate anything.
      unless entry.key?('result')
        entry['result'] = yield entry['requestId']
        save(value)
      end
      entry['result']
    end

    def mutation(value, name, action, fields = nil, job_id: nil, **keywords)
      fields ||= keywords
      step(value, name) do |request_id|
        entry = value['steps'].fetch(name)
        entry['payload'] ||= fields.merge('schema' => 1, 'requestId' => request_id)
        entry['jobId'] ||= job_id
        save(value)
        admit do
          engine.mutate(owner, action, entry['payload'], job_id: entry['jobId'])
        end
      end
    end

    def advance(value)
      request = value['request']
      case value['phase']
      when 'prepare'
        request['repositories'].each do |item|
          name = item['repository']
          operation = value['operations'][name] ||= {}
          unless operation['snapshot']
            operation['expectedHead'] = refresh(value, item)
            save(value)
            verify_head(value, name)
            verify_checks(value, name)
            snapshot = step(value, "prepare-#{name}") { @prepare.call('kind' => 'merge', 'repository' => name, 'evidence' => request['evidence']) }
            verify_merge_snapshot(value, name, snapshot)
            operation['snapshot'] = snapshot
            save(value)
          end
          unless operation['approvalId']
            verify_merge_snapshot(value, name, operation['snapshot'])
            operation['approvalId'] = approve(value, "approve-#{name}", operation['snapshot'], 'merge', [item.slice('repository', 'target')], request['approval'])
            save(value)
          end
        end
        value['phase'] = request['mode'] == 'deploy_first' && !value['deploymentComplete'] ? 'deploy' : 'submit'
        advance(value)
      when 'submit'
        jobs = request['repositories'].map do |item|
          operation = value['operations'].fetch(item['repository'])
          {'key' => item['repository'], 'kind' => 'merge', 'dependsOn' => item['dependsOn'],
            'approvalId' => operation['approvalId'], 'reservationId' => nil, 'snapshot' => operation['snapshot']}
        end
        submitted = step(value, 'submit-batch') do |request_id|
          admit { engine.submit_batch(owner, 'schema' => 1, 'requestId' => request_id, 'jobs' => jobs) }
        end
        submitted['jobs'].each { |job| value['jobs']["merge:#{job['key']}"] = job['id'] }
        value['phase'] = 'merging'
      when 'merging'
        return unless settled(value, request['repositories'].map { |item| "merge:#{item['repository']}" })
        value['phase'] = request['mode'] == 'integrate_first' ? 'deploy' : 'complete-records'
        advance(value)
      when 'deploy'
        deploy(value)
      when 'deploying'
        return unless settled(value, %w[system profile])
        value['deploymentComplete'] = true
        if request['mode'] == 'deploy_first'
          request['repositories'].each do |item|
            name = item['repository']
            value['operations'][name].delete('snapshot')
            value['operations'][name].delete('approvalId')
            value['steps'].delete("prepare-#{name}")
            value['steps'].delete("approve-#{name}")
          end
          value['phase'] = 'prepare'
        else
          value['phase'] = 'complete-records'
        end
        save(value)
        engine.current_generation!
        advance(value)
      when 'complete-records', 'finalizing'
        complete(value)
      else raise Error.new('unknown integration phase; retain journal for recovery', 5)
      end
      @diagnostics.puts "integration #{value['id']}: #{value['phase']}"
    end

    def settled(value, names)
      # A refresh receipt may have committed while its response was lost. Recover
      # the original request before inspecting the job's now-current state.
      return resume_refresh(value) if value['pendingRefresh']
      jobs = engine.store.read['jobs']
      selected = names.map { |name| jobs.fetch(value['jobs'].fetch(name)) }
      stale = selected.find { |job| job['state'] == 'needs_refresh' }
      if stale
        name = value['jobs'].key(stale['id'])
        raise Error.new('deployment baseline changed; prepare exact final artifact', 3) if %w[system profile].include?(name)
        name = name.delete_prefix('merge:')
        value['pendingRefresh'] = {'repository' => name, 'jobId' => stale['id'], 'revision' => stale['revision']}
        save(value)
        return resume_refresh(value)
      end
      failure = selected.find { |job| %w[failed canceled reconciling].include?(job['state']) }
      raise Error.new("operation #{failure['id']} requires reconciliation: #{failure['state']}", 5) if failure
      now = Time.now.to_f
      value['queueSeconds'] += [now - value['lastObservedAt'], 0].max if selected.any? { |job| %w[waiting_resource waiting_dependency].include?(job['state']) }
      value['lastObservedAt'] = now
      selected.all? { |job| job['state'] == 'succeeded' }
    end

    def resume_refresh(value)
      pending = value.fetch('pendingRefresh')
      name = pending.fetch('repository')
      item = value['request']['repositories'].find { |entry| entry['repository'] == name }
      key = "refresh-#{name}-#{pending.fetch('revision')}"
      unless pending['snapshot']
        pending['expectedHead'] ||= refresh(value, item)
        value['operations'][name]['expectedHead'] = pending['expectedHead']
        save(value)
        verify_head(value, name)
        verify_checks(value, name)
        snapshot = step(value, "prepare-#{key}") do
          @prepare.call('kind' => 'merge', 'repository' => name, 'evidence' => value['request']['evidence'])
        end
        verify_merge_snapshot(value, name, snapshot)
        pending['snapshot'] = snapshot
        save(value)
      end
      verify_merge_snapshot(value, name, pending['snapshot'])
      pending['approvalId'] ||= approve(value, "approve-#{key}", pending['snapshot'], 'merge',
        [item.slice('repository', 'target')], value['request']['approval'])
      mutation(value, key, 'refresh', {'expectedRevision' => pending['revision'],
        'snapshot' => pending['snapshot'], 'approvalId' => pending['approvalId']}, job_id: pending['jobId'])
      value['operations'][name].merge!(pending.slice('snapshot', 'approvalId'))
      value.delete('pendingRefresh')
      save(value)
      false
    end

    def approve(value, name, snapshot, kind, scope, source)
      mutation(value, name, 'approve', 'kind' => kind, 'scope' => scope, 'source' => source, 'reviewedSnapshot' => snapshot['id']).fetch('id')
    end

    def verify_head(value, name)
      path, _, _, registration = engine.repository(owner, name)
      actual = engine.capture('git', '-C', path, 'rev-parse', registration['branch']).strip
      raise Error.new('feature changed after review/certification; review before resuming', 4) unless actual == value['operations'].fetch(name).fetch('expectedHead')
    end

    def verify_merge_snapshot(value, name, snapshot)
      verify_head(value, name)
      unless snapshot.dig('operation', 'featureHead') == value['operations'].fetch(name).fetch('expectedHead')
        raise Error.new('prepared merge differs from the reviewed/certified candidate', 4)
      end
    end

    def refresh(value, item)
      return refresh_owned(value, item) unless File.file?(File.join(engine.package, 'share/dev-workspace/claim-contract.json'))
      engine.with_repository_claim(owner, item['repository'], admission: @admission) { refresh_owned(value, item) }
    end

    def refresh_owned(value, item)
      path, _, _, registration = engine.repository(owner, item['repository'])
      engine.capture('git', '-C', path, 'fetch', 'origin', item['target'])
      target = engine.remote_head(path, item['target'])
      head = engine.capture('git', '-C', path, 'rev-parse', registration['branch']).strip
      if (intent = value.fetch('publications', {})[item['repository']])
        publish_refresh(value, item, path, registration, intent['oldHead'], intent['head'], intent['proof'])
        return intent['head'] if engine.ancestor?(path, target, intent['head'])
      end
      matching_pin = true
      if item['recipe']
        parameters = item['recipe'].fetch('parameters')
        desired = recipe_heads(value, item, path)[parameters.fetch('repository')]
        matching_pin = managed_revision(path, head, parameters.fetch('inputPath')) == desired
      end
      return head if head == item['head'] && engine.ancestor?(path, target, head) && matching_pin
      if item['recipe']
        refresh_recipe(value, item, path, registration, target, head)
      else
        certificate = Certification.new(engine)
        retained = engine.retain_commit(path, head) { |source| engine.prepare_merge_hooks(source, item['repository'], head) }
        hooks = engine.prepare_merge_hooks(retained, item['repository'], head)
        source = scratch(value, item['repository'], path, head, target, hooks)
        candidate = nil
        engine.with_merge_hooks('hookArtifact' => hooks && hooks['id']) do
          if engine.capture('git', '-C', source, 'rev-parse', 'HEAD').strip == head
            engine.capture('git', '-C', source, 'rebase', '--onto', target, item['base'])
          end
          candidate = engine.capture('git', '-C', source, 'rev-parse', 'HEAD').strip
        end
        proof = certificate.prove(path, from_base: item['base'], from: item['head'], to_base: target, to: candidate)
        publish_refresh(value, item, path, registration, head, candidate, proof)
      end
    end

    def publish_refresh(value, item, path, registration, old_head, head, proof)
      retained = engine.retain_commit(path, old_head) { |source| engine.prepare_merge_hooks(source, item['repository'], old_head) }
      hooks = engine.prepare_merge_hooks(retained, item['repository'], old_head)
      admit do
        current = engine.owner(owner['slug'])
        raise Error.new('refresh owner changed', 4) unless current['id'] == owner['id'] && current['identity'] == owner['identity']
        engine.with_merge_hooks('hookArtifact' => hooks && hooks['id']) do
          publish_refresh_with_hooks(value, item, path, registration, old_head, head, proof)
        end
      end
    end

    def publish_refresh_with_hooks(value, item, path, registration, old_head, head, proof)
      # A detached prepared candidate updates only this session's clean branch.
      worktree = File.join(engine.workspace, 'worktrees', owner['slug'], item['repository'])
      actual = engine.capture('git', '-C', worktree, 'rev-parse', 'HEAD').strip if File.directory?(worktree)
      symbolic, _, symbolic_status, = Command.run(['git', '-C', worktree, 'symbolic-ref', '--short', 'HEAD'])
      recovering = value.fetch('publications', {}).key?(item['repository'])
      unless File.directory?(worktree) && engine.capture('git', '-C', worktree, 'status', '--porcelain').empty? &&
             [old_head, head].include?(actual) &&
             (symbolic.strip == registration['branch'] || (recovering && !symbolic_status.success?))
        raise Error.new('mechanical refresh requires the clean exact owned feature worktree', 4)
      end
      value['publications'] ||= {}
      value['publications'][item['repository']] = {'oldHead' => old_head, 'head' => head, 'proof' => proof}
      save(value)
      engine.capture('git', '-C', path, 'update-ref', "refs/dev-workspace-integration/#{value['id']}/#{item['repository']}/#{old_head}", old_head)
      if actual != head || !symbolic_status.success?
        engine.capture('git', '-C', worktree, 'checkout', '--detach', old_head) if symbolic_status.success?
        branch_head = engine.capture('git', '-C', path, 'rev-parse', registration['branch']).strip
        raise Error.new('owned feature branch changed during refresh', 3) unless [old_head, head].include?(branch_head)
        engine.capture('git', '-C', path, 'update-ref', "refs/heads/#{registration['branch']}", head, old_head) if branch_head == old_head
        engine.capture('git', '-C', worktree, 'checkout', registration['branch'])
      end
      remote_out, _, remote_status, = Command.run(['git', '-C', path, 'ls-remote', '--refs', 'origin', "refs/heads/#{registration['branch']}"], env: engine.instance_variable_get(:@env))
      raise Error.new('cannot inspect published feature before refresh', 75) unless remote_status.success?
      remote_head = remote_out.split.first || ''
      unless remote_head.empty? || remote_head == old_head || remote_head == head
        raise Error.new('published feature was changed by another writer', 3)
      end
      engine.capture('git', '-C', worktree, 'push', "--force-with-lease=refs/heads/#{registration['branch']}:#{remote_head}", 'origin', "HEAD:refs/heads/#{registration['branch']}")
      value['operations'][item['repository']] ||= {}
      value['operations'][item['repository']]['proof'] = proof
      save(value)
      head
    end

    def refresh_recipe(value, item, path, registration, target, old_head)
      recipe = @catalog.recipe(item['recipe'].fetch('name'))
      retained = engine.retain_commit(path, target) { |source| engine.prepare_merge_hooks(source, item['repository'], target) }
      hooks = engine.prepare_merge_hooks(retained, item['repository'], target)
      source = scratch(value, item['repository'], path, target, old_head, hooks)
      inputs = recipe_heads(value, item, path)
      key = "recipe-#{item['repository']}-#{target}-#{old_head}"
      result = step(value, key) do |_request_id|
        entry = value['steps'].fetch(key)
        entry['payload'] ||= {'schema' => 1, 'action' => 'refresh', 'source' => source, 'fromBase' => item['base'], 'from' => item['head'],
          'toBase' => target, 'parameters' => item['recipe'].fetch('parameters'), 'heads' => inputs}
        save(value)
        candidate = engine.capture('git', '-C', source, 'rev-parse', 'HEAD').strip
        payload = if candidate == target
          entry['payload']
        else
          entry['payload'].merge('action' => 'verify', 'to' => candidate)
        end
        WorkspaceCoordination.parse(engine.capture(recipe['command'], input: JSON.generate(payload)))
      end
      unless result['schema'] == 1 && result['verified'] == true && result['head'].is_a?(String) && result['head'].match?(SHA)
        raise Error.new('pin recipe did not certify its complete generated change', 4)
      end
      publish_refresh(value, item, path, registration, old_head, result['head'], result.fetch('proof').merge('recipe' => item['recipe']['name']))
    end

    def recipe_heads(value, item, path)
      inputs = value['operations'].transform_values { |operation| operation.dig('snapshot', 'operation', 'featureHead') }.compact
      parameters = item['recipe'].fetch('parameters')
      inputs[parameters.fetch('repository')] ||= managed_revision(path, item['head'], parameters.fetch('inputPath'))
      inputs
    end

    def managed_revision(path, head, input_path)
      graph = WorkspaceCoordination.parse(engine.capture('git', '-C', path, 'show', "#{head}:flake.lock"))
      pending = input_path.dup
      node = graph.fetch('root')
      64.times do
        return graph.fetch('nodes').fetch(node).fetch('locked').fetch('rev') if pending.empty?
        reference = graph.fetch('nodes').fetch(node).fetch('inputs').fetch(pending.shift)
        if reference.is_a?(Array)
          pending = reference + pending
          node = graph.fetch('root')
        else
          node = reference
        end
      end
      raise Error, 'managed input path has cyclic follows'
    end

    def scratch(value, name, path, head, discriminator, hooks)
      directory = File.join(engine.store.root, 'integrations', value['id'], "#{name}-#{head[0, 12]}-#{discriminator[0, 12]}")
      unless File.directory?(directory)
        FileUtils.mkdir_p(File.dirname(directory), mode: 0o700)
        engine.with_merge_hooks('hookArtifact' => hooks && hooks['id']) do
          engine.capture('git', '-C', path, 'worktree', 'add', '--detach', directory, head)
        end
      end
      raise Error.new('prepared refresh has conflicts or dirty files; review before resuming', 4) unless engine.capture('git', '-C', directory, 'status', '--porcelain').empty?
      directory
    end

    def verify_checks(value, repository)
      Array(value['request']['checks']).select { |check| check['repository'] == repository }.each do |check|
        source = File.join(engine.workspace, 'worktrees', owner['slug'], repository)
        verify_head(value, repository)
        unless engine.capture('git', '-C', source, 'status', '--porcelain').empty?
          raise Error.new('verification requires the clean certified feature worktree', 4)
        end
        installable = "#{source}##{check.fetch('output')}"
        derivation = engine.capture('nix', 'eval', '--raw', "#{installable}.drvPath").strip
        original = WorkspaceCoordination.parse(File.binread(engine.lineage_evidence(owner, check.fetch('receipt'))))
        inputs = {'derivation' => derivation}
        if Certification.reusable_check?(original, derivation:, inputs:)
          verify_head(value, repository)
          next
        end
        started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
        begin
          engine.capture('nix', 'build', '--no-link', '--print-out-paths', installable)
        ensure
          value['verificationSeconds'] += Process.clock_gettime(Process::CLOCK_MONOTONIC) - started
        end
        verify_head(value, repository)
        unless engine.capture('git', '-C', source, 'status', '--porcelain').empty?
          raise Error.new('feature worktree changed during verification', 4)
        end
        receipt = {'schema' => 1, 'exitStatus' => 0, 'derivation' => derivation, 'inputs' => inputs}
        value['operations'][repository]['checks'] ||= []
        value['operations'][repository]['checks'] << receipt
        save(value)
      end
    end

    def deploy(value)
      section = value['request'].fetch('deployment')
      scopes = [{'machine' => engine.catalog.machine(section.fetch('target'))['id'], 'actions' => %w[switch profile_activate], 'reboot' => false}]
      snapshots = {}
      %w[system profile].each do |name|
        value['request']['repositories'].each { |item| verify_head(value, item['repository']) }
        fields = {'kind' => name == 'system' ? 'machine_activate' : 'profile_activate', 'evidence' => section.fetch('evidence')}
        fields['target'] = section['target'] if name == 'system'
        snapshots[name] = step(value, "prepare-#{name}") do
          started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
          begin
            @prepare.call(fields)
          ensure
            value['preparationSeconds'] += Process.clock_gettime(Process::CLOCK_MONOTONIC) - started
          end
        end
        value['request']['repositories'].each { |item| verify_head(value, item['repository']) }
        Array(snapshots[name].dig('operation', 'coverage')).each do |coverage|
          operation = value['operations'][coverage['repository']]
          if operation && coverage['revision'] != operation.fetch('expectedHead')
            raise Error.new('deployment coverage differs from the reviewed/certified candidate', 4)
          end
        end
      end
      approvals = snapshots.to_h { |name, snapshot| [name, approve(value, "approve-#{name}", snapshot, 'deploy', scopes, section.fetch('approval'))] }
      reservation = section['reservationId'] || mutation(value, 'reserve', 'reserve',
        'target' => section['target'], 'mode' => value['request']['mode'] == 'deploy_first' ? 'deploy_first' : 'integrated',
        'approvalId' => approvals['system'], 'integrationObligations' => value['request']['repositories'].map { |item| item.slice('repository', 'target') }).fetch('id')
      value['reservationId'] = reservation
      %w[system profile].each do |name|
        value['jobs'][name] = mutation(value, "submit-#{name}", 'submit', 'kind' => name == 'system' ? 'machine_activate' : 'profile_activate',
          'dependsOn' => name == 'profile' ? [value['jobs']['system']] : [], 'approvalId' => approvals[name], 'reservationId' => reservation, 'snapshot' => snapshots[name]).fetch('id')
      end
      value['phase'] = 'deploying'
    end

    def complete(value)
      completion = value['request']['completion']
      if completion
        final = value['request']['repositories'].map do |item|
          item.slice('repository', 'target').merge('head' => value['operations'][item['repository']].dig('snapshot', 'operation', 'featureHead'))
        end
        step(value, 'finish') do |request_id|
          entry = value['steps'].fetch('finish')
          unless entry['payload']
            reservation = engine.store.read['reservations'].fetch(value.fetch('reservationId'))
            entry['reservationId'] = reservation['id']
            entry['payload'] = {'schema' => 1, 'requestId' => request_id, 'expectedRevision' => reservation['revision'],
              'deploymentWorkFinished' => completion.fetch('deploymentWorkFinished'), 'evidence' => completion.fetch('evidence'),
              'finalHeads' => final, 'replacements' => replacements(value, reservation, final) + completion.fetch('replacements', [])}
            save(value)
          end
          admit do
            engine.finish(owner, entry['reservationId'], entry['payload'])
          end
        end
      end
      value['phase'] = 'finalizing'
      save(value)
      export(value)
      if value['schema'] == 2
        background_notice(value, 'complete')
      end
      record_completion(value)
      value['phase'] = 'complete'
      value['finishedAt'] = Time.now.utc.iso8601
    end

    def replacements(value, reservation, final)
      reservation['history'].flat_map do |record|
        next [] unless record['outcome'] == 'applied'
        record['coverage'].filter_map do |coverage|
          item = value['request']['repositories'].find { |entry| entry['repository'] == coverage['repository'] }
          next unless item && record['coverageContracts'].any? { |contract| contract['owned'] && contract['repository'] == item['repository'] }
          selected = final.find { |entry| entry['repository'] == item['repository'] }
          path, = engine.repository(owner, item['repository'])
          next if engine.ancestor?(path, coverage['revision'], selected['head'])
          existing = Array(value['request'].dig('completion', 'replacements')).any? { |mapping| mapping['repository'] == item['repository'] && mapping['from'] == coverage['revision'] && mapping['to'] == selected['head'] }
          next if existing
          operation = value['operations'].fetch(item['repository'])
          from_base = item['base']
          if coverage['revision'] != item['head']
            snapshots = engine.store.read['jobs'].values.select { |job| job['ownerId'] == owner['id'] }.flat_map do |job|
              [job['snapshot']] + job['history'].map { |history| history['snapshot'] }
            end
            previous = snapshots.find { |snapshot| snapshot.dig('operation', 'repository') == item['repository'] && snapshot.dig('operation', 'featureHead') == coverage['revision'] }
            raise Error.new('intermediate deployed revision has no retained exact comparison base', 4) unless previous
            from_base = previous.dig('operation', 'comparisonBase')
          end
          mapping = item.slice('repository', 'target').merge('from' => coverage['revision'], 'fromBase' => from_base,
            'to' => selected['head'], 'toBase' => operation['snapshot'].dig('operation', 'comparisonBase'),
            'relation' => 'certified_equivalent', 'approvalId' => operation['approvalId'])
          proof = if item['recipe']
            old_proof = operation.fetch('proof')
            Certification.new(engine).verify_proof(path, old_proof, mapping)
          else
            Certification.new(engine).prove(path, from_base: mapping['fromBase'], from: mapping['from'], to_base: mapping['toBase'], to: mapping['to'])
          end
          verification = value['request']['evidence'].find { |ref| ref['kind'] == 'verification' } || raise(Error.new('completion requires retained verification evidence', 4))
          receipt = {'schema' => 1, 'ownerId' => owner['id'], 'reservationId' => reservation['id'], 'mapping' => mapping,
            'originalReview' => item['review'], 'proof' => proof, 'verification' => verification}
          if coverage['revision'] != item['head']
            bridge = {'fromBase' => item['base'], 'from' => item['head'], 'toBase' => from_base, 'to' => coverage['revision']}
            if item['recipe']
              template = operation.fetch('proof').merge('heads' => operation.fetch('proof')['heads'].merge(item['recipe']['parameters']['repository'] => managed_revision(path, coverage['revision'], item['recipe']['parameters']['inputPath'])))
              receipt['priorProof'] = Certification.new(engine).verify_proof(path, template, bridge)
            else
              receipt['priorProof'] = Certification.new(engine).prove(path, from_base: item['base'], from: item['head'], to_base: from_base, to: coverage['revision'])
            end
          end
          receipt_path = File.join(record_directory(value), "equivalence-#{item['repository']}-#{coverage['revision'][0, 12]}.json")
          admit do
            verify_tracking_binding
            WorkspaceCoordination.atomic(receipt_path, receipt, limit: MAX_REQUEST)
          end
          mapping.merge('evidence' => {'kind' => 'equivalence', 'path' => receipt_path, 'sha256' => Digest::SHA256.file(receipt_path).hexdigest})
        end
      end.uniq
    end

    def export(value)
      admit do
        verify_tracking_binding
        directory = record_directory(value)
        FileUtils.mkdir_p(directory)
        output = status(value['id']).merge('phase' => 'operations_complete')
        WorkspaceCoordination.atomic(File.join(directory, 'result.json'), output)
        summary = "# Integration #{value['id']}\n\nAll requested operations succeeded.\n\n" + output['heads'].map { |name, head| "- #{name}: #{head}\n" }.join
        File.write(File.join(directory, 'summary.md'), summary)
      end
    end

    def record_directory(value)
      File.join(engine.workspace, 'work', owner['slug'], 'integration', value.fetch('id'))
    end

    def record_completion(value)
      return record_completion_owned(value) unless File.file?(File.join(engine.package, 'share/dev-workspace/claim-contract.json'))
      engine.with_repository_claim(owner, 'workspace', admission: @admission) { record_completion_owned(value) }
    end

    def record_completion_owned(value)
      return if value['steps'].dig('tracking-commit', 'result')
      admit do
        verify_tracking_binding
        root = File.join(engine.workspace, 'work', owner['slug'])
        state = File.read(File.join(root, 'state.md'))
        marker = "## Integration #{value['id']}"
        state += "\n#{marker}\n\nRequested exact feature heads are integrated. See integration/#{value['id']}/summary.md\nand integration/#{value['id']}/result.json for operation receipts. Feature refs and session retained.\n" unless state.include?(marker)
        File.write(File.join(root, 'state.md'), state)
      end
      # The standard artifact writer owns manifest locking and incarnation checks.
      %w[summary.md result.json].each do |file|
        engine.capture(File.join(engine.package, 'bin/dev-session'), 'artifact', 'add', owner['slug'],
          File.join('integration', value['id'], file), '--label', "Integration #{value['id']} #{file}",
          '--expected-owner', owner['id'], '--as-is')
      end
      local_completion = admit do
        verify_tracking_binding
        manifest = YAML.safe_load(File.binread(File.join(engine.workspace, 'work', owner['slug'], 'portal.yml')), permitted_classes: [], aliases: false)
        if manifest['sensitive'] == true
          update_tracking_lifecycle(value, manifest)
          value['steps']['tracking-commit'] = {'result' => {'local' => true}}
          save(value)
          true
        end
      end
      return if local_completion
      step(value, 'tracking-commit') do
        engine.capture('git', '-C', engine.workspace, 'fetch', 'origin')
        source_head = engine.capture('git', '-C', engine.workspace, 'rev-parse', 'HEAD').strip
        source = engine.retain_commit(engine.workspace, source_head)
        hooks = engine.prepare_merge_hooks(source, 'workspace', source_head)
        common = engine.capture('git', '-C', engine.workspace, 'rev-parse', '--path-format=absolute', '--git-common-dir').strip
        admit do
          verify_tracking_binding
          File.open(File.join(common, 'dev-session-tracking.lock'), File::RDWR | File::CREAT, 0o600) do |lock|
            lock.flock(File::LOCK_EX)
            engine.capture('git', '-C', engine.workspace, 'merge', '--ff-only', 'origin/master')
            prefix = File.join('work', owner['slug'])
            owned = %w[state.md portal.yml].map { |file| File.join(prefix, file) }
            owned.concat(%w[summary.md result.json].map { |file| File.join(prefix, 'integration', value['id'], file) })
            owned.concat(Dir[File.join(record_directory(value), 'equivalence-*.json')].map { |file| file.delete_prefix(engine.workspace + '/') })
            root = File.join(engine.workspace, prefix)
            manifest = YAML.safe_load(File.binread(File.join(root, 'portal.yml')), permitted_classes: [], aliases: false)
            update_tracking_lifecycle(value, manifest)
            engine.capture('git', '-C', engine.workspace, 'add', '--', *owned)
            unless engine.capture('git', '-C', engine.workspace, 'diff', '--cached', '--name-only', '--', *owned).empty?
              Tempfile.create('integration-tracking-message') do |message|
                message.write("workspace: record coordinated integration completion\n\nRecord exact integrated revisions and their operation receipts while\nretaining session ownership and feature references.\n\nIntegration-Run: #{value['id']}\n")
                message.flush
                engine.with_merge_hooks('hookArtifact' => hooks && hooks['id']) do
                  engine.capture('git', '-C', engine.workspace, 'commit', '-F', message.path, '--only', '--', *owned)
                end
              end
            end
            engine.capture('git', '-C', engine.workspace, 'push', 'origin', 'master')
            {'head' => engine.capture('git', '-C', engine.workspace, 'rev-parse', 'HEAD').strip}
          end
        end
      end
    rescue StandardError
      begin
        admit do
          verify_tracking_binding
          state_path = File.join(engine.workspace, 'work', owner['slug'], 'state.md')
          if File.file?(state_path)
            text = File.read(state_path)
            File.write(state_path, text.sub(/\A---\nlifecycle: complete\n---/, "---\nlifecycle: active\n---"))
          end
        end
      rescue StandardError
        # The original error owns recovery; never write into a new incarnation.
      end
      raise
    end


    def update_tracking_lifecycle(value, manifest)
      exact = manifest.fetch('repositories').map { |registration| registration['name'] }.sort == value['request']['repositories'].map { |item| item['repository'] }.sort &&
        value['request']['repositories'].all? do |item|
          path, _, _, registration = engine.repository(owner, item['repository'])
          engine.capture('git', '-C', path, 'rev-parse', registration['branch']).strip == value['operations'][item['repository']].dig('snapshot', 'operation', 'featureHead')
        end
      state_path = File.join(engine.workspace, 'work', owner['slug'], 'state.md')
      state = File.read(state_path)
      ledger = engine.store.read
      settled = value['schema'] != 2 && ledger['jobs'].values.none? { |job| job['ownerId'] == owner['id'] && !TERMINAL.include?(job['state']) } &&
        ledger['reservations'].values.none? { |reservation| reservation['ownerId'] == owner['id'] && reservation['state'] != 'released' }
      File.write(state_path, state.sub(/\A---\nlifecycle: active\n---/, "---\nlifecycle: complete\n---")) if exact && settled
    end

    def verify_tracking_binding
      root = File.join(engine.workspace, 'work', owner['slug'])
      stat = File.lstat(root)
      manifest = YAML.safe_load(File.binread(File.join(root, 'portal.yml')), permitted_classes: [], aliases: false)
      identity = WorkspaceCoordination.digest([engine.workspace, owner['workspaceName'], owner['slug'], stat.dev, stat.ino,
        manifest.dig('codex', 'thread_id'), WorkspaceCoordination.creation_identity(manifest)])
      current = engine.store.read['owners'].fetch(owner['id'])
      raise Error.new('completion tracking incarnation changed', 4) unless identity == owner['identity'] && current['identity'] == identity && !current['fence']
    end
  end
end
