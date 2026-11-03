# frozen_string_literal: true

# Cooperative resource ownership only. Sessions execute and verify their work.
module WorkspaceCoordination
  CLAIM_CONTRACT = {'schema' => 1, 'recordSchema' => 1}.freeze
  REPOSITORY_CLAIM_CONTRACT = {'schema' => 1, 'registeredRepositories' => true}.freeze
  CLAIM_ACTIVE = %w[waiting held].freeze

  class Store
    def claims
      claims_record.last
    end

    def claims_record
      present, data = read_retained_file('claims.json')
      [present, present ? validate_claims!(data) : {'schema' => 1, 'claims' => {}}]
    rescue Error => error
      raise Error.new("cannot read retained resource claims: #{error.message}", 5)
    end

    def validate_claim_request!(request)
      WorkspaceCoordination.keys!(request, %w[schema requestId repositories machines label])
      unless request['schema'].is_a?(Integer) && [1, 2].include?(request['schema']) && request['requestId'].is_a?(String) && request['requestId'].match?(UUID) &&
             request['label'].is_a?(String) && request['label'].bytesize.between?(1, 512) && !request['label'].include?("\0") &&
             request['repositories'].is_a?(Array) && request['repositories'].uniq == request['repositories'] &&
             request['repositories'].all? { |name| name.is_a?(String) && name.match?(PART) } &&
             request['machines'].is_a?(Array) && request['machines'].uniq == request['machines'] && !(request['repositories'] + request['machines']).empty? &&
             JSON.generate(request).bytesize <= MAX_RESULT
        raise Error, 'invalid resource request; supply requestId, label and repository/machine lists'
      end
      request['machines'].each do |machine|
        if request['schema'] == 1
          raise Error, 'invalid legacy machine name' unless machine.is_a?(String) && machine.bytesize.between?(1, 256) && !machine.include?("\0")
        else
          WorkspaceCoordination.keys!(machine, %w[repository selector adapter])
          unless machine['repository'].is_a?(String) && machine['repository'].match?(PART) &&
                 machine['adapter'].is_a?(String) && machine['adapter'].bytesize.between?(1, 256) && !machine['adapter'].include?("\0")
            raise Error, 'typed machines require a registered repository and installed adapter ID'
          end
          MachineIdentity.text!(machine['selector'])
        end
      end
    end

    def validate_claims!(value)
      WorkspaceCoordination.keys!(value, %w[schema claims])
      raise Error.new('unsupported resource claims; recover forward', 5) unless value['schema'].is_a?(Integer) && [1, 2].include?(value['schema']) && value['claims'].is_a?(Hash)
      value['claims'].each do |id, item|
        typed = item.is_a?(Hash) && item.dig('request', 'schema') == 2
        WorkspaceCoordination.keys!(item, %w[id ownerId request resources state revision sequence createdAt updatedAt notificationPending] + (typed ? ['machineBindings'] : []))
        unless id.match?(UUID) && item['id'] == id && item['ownerId'].is_a?(String) && item['ownerId'].match?(UUID) &&
               %w[waiting held released canceled].include?(item['state']) && [true, false].include?(item['notificationPending']) &&
               %w[revision sequence].all? { |key| item[key].is_a?(Integer) && item[key].between?(1, MAX_COUNTER) } &&
               item['resources'].is_a?(Array) && !item['resources'].empty? && item['resources'].all? { |resource| resource.is_a?(String) && resource.match?(%r{\A(?:git:/|repository:|machine:).+\z}) && !resource.include?("\0") } &&
               item['resources'] == item['resources'].sort.uniq &&
               %w[createdAt updatedAt].all? { |key| item[key].is_a?(String) && item[key].match?(/\A\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\z/) } &&
               item['request'].is_a?(Hash) && item['request']['requestId'] == id
          raise Error.new('malformed resource claim', 5)
        end
        validate_claim_request!(item['request'])
        if typed
          raise Error.new('typed resource claims require envelope schema 2', 5) unless value['schema'] == 2
          bindings = item['machineBindings']
          unless bindings.is_a?(Array) && bindings.length == item['request']['machines'].length
            raise Error.new('typed machine bindings are incomplete', 5)
          end
          bindings.zip(item['request']['machines']).each do |binding, requested|
            WorkspaceCoordination.keys!(binding, %w[repository repositoryOrigin selector adapter sourceRevision identity])
            unless binding.slice('repository', 'selector', 'adapter') == requested && binding['sourceRevision'].is_a?(String) && binding['sourceRevision'].match?(SHA)
              raise Error.new('typed machine binding differs from its request', 5)
            end
            MachineIdentity.origin!(binding['repositoryOrigin'], fixtures: @fixtures)
            identity = MachineIdentity.validate!(binding['identity'], fixtures: @fixtures)
            if identity['kind'] == 'repository' && (identity['origin'] != binding['repositoryOrigin'] || identity['name'] != binding['selector'])
              raise Error.new('typed machine binding origin or name changed', 5)
            end
          end
          machines = bindings.map { |binding| MachineIdentity.resource(binding['identity']) }
          unless machines.uniq == machines && item['resources'].grep(/^machine:/) == machines.sort
            raise Error.new('typed machine resources differ from frozen bindings', 5)
          end
        end
      end
      held = value['claims'].values.select { |item| item['state'] == 'held' }.flat_map { |item| normalize_machine_resources(item['resources']) }
      sequences = value['claims'].values.map { |item| item['sequence'] }
      raise Error.new('overlapping resource claims or duplicate sequence', 5) unless held.uniq == held && sequences.uniq == sequences
      value
    rescue Error, NoMethodError, TypeError => error
      raise Error.new("malformed resource claims: #{error.message}", 5)
    end

    def normalize_machine_resources(resources)
      machine_policy ? machine_policy.normalize(resources) : Engine.normalized_resources(resources)
    end

    # Callers hold the existing ledger mutex; old jobs and new claims exclude
    # each other without changing the legacy ledger format.
    def write_claims(value, admission: false)
      validate_claims!(value)
      limit = admission ? MAX_LEDGER - RECOVERY_RESERVE : MAX_LEDGER
      WorkspaceCoordination.atomic(File.join(root, 'claims.json'), value, limit: limit)
      wake
    end

    def held_claim_resources
      normalize_machine_resources(claims['claims'].values.select { |item| item['state'] == 'held' }.flat_map { |item| item['resources'] })
    end

    def claim_notification_events(ledger)
      claims['claims'].values.filter_map do |item|
        next unless item['state'] == 'waiting' || item['notificationPending']
        owner = ledger['owners'].fetch(item['ownerId'])
        WorkspaceCoordination.event_id(owner, item, 'held') if owner['thread']
      end
    end

    def legacy_machine_bindings(ledger)
      bindings = machine_policy ? machine_policy.legacy_bindings : []
      ledger['jobs'].values.select { |job| CLAIMED.include?(job['state']) && job['resources'].any? { |resource| resource.start_with?('machine:') } }.each do |job|
        source_package = job.dig('snapshot', 'preparedGeneration', 'package')
        raise Error.new('retained machine job has no source catalog', 5) unless source_package.is_a?(String) && File.directory?(source_package)
        source_catalog = Catalog.new(source_package, fixtures: @fixtures)
        policy = MachinePolicy.new(source_package, catalog: source_catalog, fixtures: @fixtures)
        bindings.concat(policy.legacy_bindings.select { |binding| job['resources'].include?(binding['resource']) })
      end
      bindings.uniq
    end

    def require_machine_policy!(candidate, value)
      return unless machine_policy
      current_profile, next_profile = machine_policy.profile_resource, candidate.profile_resource
      if current_profile && (!next_profile || candidate.canonical(current_profile) != candidate.canonical(next_profile))
        raise Error.new('target package changes the logical profile machine; preserve ownership and recover forward', 5)
      end
      needed = value['claims'].values.select { |item| CLAIM_ACTIVE.include?(item['state']) }.flat_map { |item| item['resources'].grep(/^machine:/) }
      ledger = read
      needed.concat(ledger['reservations'].values.reject { |item| item['state'] == 'released' }.map { |item| "machine:#{item['machine']}" })
      needed.concat(ledger['jobs'].values.select { |job| CLAIMED.include?(job['state']) }.flat_map { |job| job['resources'].grep(/^machine:/) })
      # Preserve bridges for legacy names the candidate continues to accept.
      needed.concat(candidate.legacy_bindings.map { |binding| binding['resource'] })
      if machine_policy.explicit && needed.any? { |resource| candidate.canonical(machine_policy.canonical(resource)) != candidate.canonical(resource) }
        raise Error.new('target package loses a retained machine identity bridge; recover forward', 5)
      end
      groups = value['claims'].values.select { |item| item['state'] == 'held' }.map { |item| candidate.normalize(item['resources'].grep(/^machine:/)) }
      groups.concat(ledger['reservations'].values.reject { |item| %w[requested released].include?(item['state']) }.map { |item| candidate.normalize(["machine:#{item['machine']}"]) })
      groups.concat(ledger['jobs'].values.select { |job| CLAIMED.include?(job['state']) && !job['reservationId'] }.map { |job| candidate.normalize(job['resources'].grep(/^machine:/)) })
      all = groups.flatten
      raise Error.new('target machine policy collides with retained ownership; preserve all original records', 5) unless all.uniq == all
    end

    def require_claim_capability!(package)
      present, value = claims_record
      catalog = Catalog.new(package, fixtures: @fixtures)
      candidate = MachinePolicy.new(package, catalog:, fixtures: @fixtures)
      require_machine_policy!(candidate, value)
      return unless present
      path = File.join(package, 'share/dev-workspace/claim-contract.json')
      unless File.file?(path) && WorkspaceCoordination.parse(File.binread(path)) == CLAIM_CONTRACT
        raise Error.new('target package cannot preserve resource claims; recover forward with a compatible package', 5)
      end
      if value['schema'] == 2
        path = File.join(package, 'share/dev-workspace/machine-claim-contract.json')
        unless File.file?(path) && MachineIdentity.claim_contract?(WorkspaceCoordination.parse(File.binread(path, MAX_RESULT + 1), limit: MAX_RESULT))
          raise Error.new('target package cannot preserve typed machine claims; recover forward with a compatible package', 5)
        end
      end
      value['claims'].values.select { |item| CLAIM_ACTIVE.include?(item['state']) }.each do |item|
        if item['machineBindings']
          owner = read.fetch('owners').fetch(item['ownerId'])
          item['machineBindings'].each do |binding|
            source = MachineSource.new(owner, binding['repository'], catalog:, fixtures: @fixtures)
            identity = source.resolve(binding['selector'], binding['adapter'], catalog:)
            unless source.repository.identity == binding['repositoryOrigin'] && identity == binding['identity'] &&
                   (!machine_policy || candidate.canonical(MachineIdentity.resource(identity)) == candidate.canonical(machine_policy.canonical(MachineIdentity.resource(identity))))
              raise Error.new('target package changes an active machine binding; recover forward', 5)
            end
            source.recheck!
          end
        else
          item['request']['machines'].each do |target|
            raise Error.new('target package changes a claimed physical machine', 5) unless item['resources'].include?("machine:#{catalog.machine(target)['id']}")
            if machine_policy
              old = machine_policy.catalog.machine(target)
              same_source = machine_policy.legacy_identity(old) == candidate.legacy_identity(catalog.machine(target))
              raise Error.new('target package changes a retained legacy machine mapping', 5) unless same_source && old['adapter'] == catalog.machine(target)['adapter']
            end
          end
        end
        item['request']['repositories'].each do |name|
          owner = read.fetch('owners').fetch(item['ownerId'])
          identity = RepositoryIdentity.new(owner, name, catalog: catalog, internal_workspace: true)
          unless (identity.resources - item['resources']).empty?
            raise Error.new('target package changes a claimed repository identity; recover forward', 5)
          end
          unless catalog.data.fetch('repositories').key?(name)
            dynamic = File.join(package, 'share/dev-workspace/repository-claim-contract.json')
            unless File.file?(dynamic) && WorkspaceCoordination.parse(File.binread(dynamic)) == REPOSITORY_CLAIM_CONTRACT
              raise Error.new('target package cannot preserve registered repository claims; recover forward with a compatible package', 5)
            end
          end
        rescue Error => error
          raise Error.new("target package cannot preserve active repository identity: #{error.message}; recover forward", 5)
        end
      end
    end

    def claim_lifecycle_idle!(ids)
      held = claims['claims'].values.find { |item| ids.include?(item['ownerId']) && item['state'] == 'held' }
      raise Error.new("release resource claim #{held['id']} after stopping and inspecting owned work before lifecycle cleanup", 5) if held
    end

    def sync_claims_lifecycle(ledger)
      return unless File.exist?(File.join(root, 'claims.json'))
      value = claims
      changed = false
      value['claims'].each_value do |item|
        next unless item['state'] == 'waiting' && ledger['owners'].fetch(item['ownerId'])['fence']
        item.merge!('state' => 'canceled', 'notificationPending' => false, 'updatedAt' => Time.now.utc.iso8601)
        item['revision'] += 1
        changed = true
      end
      write_claims(value) if changed
    end
  end

  class Engine
    def claim_owner(slug, read_only: false)
      manifest = YAML.safe_load(File.binread(File.join(workspace, 'work', slug, 'portal.yml')), permitted_classes: [], aliases: false)
      bootstrap = manifest.dig('codex', 'thread_id').nil?
      return owner(slug, bootstrap:) unless read_only
      identity = verified_owner_tracking(slug, bootstrap:)
      store.read['owners'].values.find { |item| item['identity'] == identity['identity'] } || raise(Error.new('session has no retained claim owner', 4))
    end

    def resolve_machine_binding(owner, requested, proofs: [])
      source = MachineSource.new(owner, requested.fetch('repository'), catalog:, env: @env, fixtures: @fixtures)
      identity = source.resolve(requested.fetch('selector'), requested.fetch('adapter'), catalog:, diagnostics: @diagnostics)
      proofs << source
      requested.merge('repositoryOrigin' => source.repository.identity, 'sourceRevision' => source.revision,
        'identity' => MachineIdentity.validate!(identity, fixtures: @fixtures))
    end

    def claim_resources_for(owner, request, proofs: [], bindings: [])
      resources = request['repositories'].flat_map do |name|
        identity = repository_identity(owner, name)
        proofs << identity
        identity.resources
      end
      machines = if request['schema'] == 2
        request['machines'].map do |requested|
          binding = resolve_machine_binding(owner, requested, proofs:)
          bindings << binding
          MachineIdentity.resource(binding['identity'])
        end
      else
        request['machines'].map { |name| "machine:#{catalog.machine(name).fetch('id')}" }
      end
      if request['schema'] == 2 && machine_policy.normalize(machines).length != machines.length
        raise Error, 'duplicate resolved machine identities in one request'
      end
      resources.concat(machines).sort.uniq
    end

    def self.normalized_resources(resources)
      resources.map { |resource| resource.start_with?('repo:') ? "repository:#{resource.delete_prefix('repo:').split(':refs/').first}" : resource }.uniq
    end

    def claim_owner_live!(owner, ledger)
      retained = ledger['owners'][owner['id']]
      unless retained && retained['identity'] == owner['identity'] && !retained['fence'] && !ledger['suspended']
        raise Error.new('resource owner is fenced or coordination is suspended', 4)
      end
    end

    def claim_owned!(value, owner, id)
      raise Error, 'claim requires an exact UUID' unless id.is_a?(String) && id.match?(UUID)
      item = value['claims'][id] || raise(Error, 'unknown resource claim')
      raise Error.new('claim belongs to another session incarnation', 4) unless item['ownerId'] == owner['id']
      item
    end

    def claim_blocked?(ledger, value, item)
      legacy = ledger['jobs'].values.select { |job| CLAIMED.include?(job['state']) }.flat_map { |job| machine_policy.normalize(job['resources']) }
      reservations = ledger['reservations'].values.reject { |hold| %w[requested released].include?(hold['state']) }.map { |hold| "machine:#{hold['machine']}" }
      held = value['claims'].values.select { |other| other['state'] == 'held' }.flat_map { |other| other['resources'] }
      busy = machine_policy.normalize(legacy + reservations + held)
      resources = machine_policy.normalize(item['resources'])
      return true unless (resources & busy).empty?
      value['claims'].values.any? do |other|
        other['id'] != item['id'] && other['state'] == 'waiting' && other['sequence'] < item['sequence'] &&
          (machine_policy.normalize(other['resources']) & busy).empty? && !(resources & machine_policy.normalize(other['resources'])).empty?
      end
    end

    # Called under the ledger lock after local source proofs are rechecked.
    # A bridge establishes contention, never permission to retarget a hold.
    def check_machine_source_pairs!(ledger, value, request, bindings)
      incoming = if request['schema'] == 2
        bindings.map { |binding| binding.merge('resource' => MachineIdentity.resource(binding['identity'])) }
      else
        request['machines'].map do |name|
          machine = catalog.machine(name)
          machine_policy.legacy_bindings.find { |binding| binding['resource'] == "machine:#{machine['id']}" }
        end
      end
      known = store.legacy_machine_bindings(ledger)
      value['claims'].values.select { |item| CLAIM_ACTIVE.include?(item['state']) }.each do |item|
        next unless item['machineBindings']
        known.concat(item['machineBindings'].map { |binding| binding.merge('resource' => MachineIdentity.resource(binding['identity'])) })
      end
      incoming.each do |binding|
        known.each do |prior|
          next unless prior.values_at('repositoryOrigin', 'selector') == binding.values_at('repositoryOrigin', 'selector')
          unless prior['adapter'] == binding['adapter'] && machine_policy.canonical(prior['resource']) == machine_policy.canonical(binding['resource'])
            raise Error.new('active machine source mapping changed; retain ownership and cancel or release the original request before reacquiring', 3)
          end
        end
      end
    end

    def acquire_claim(owner, input, wait: true)
      current_generation!
      store.validate_claim_request!(input)
      # Exact retries must work after release or clone removal. Repeat this
      # check in the admission transaction to cover concurrent identical calls.
      saved = store.lock do
        ledger, value = store.read, store.claims
        claim_owner_live!(owner, ledger)
        existing_claim(value, owner, input)
      end
      return saved if saved
      proofs, bindings = [], []
      resources = claim_resources_for(owner, input, proofs:, bindings:)
      create_resource_claim(owner, input, resources, wait:, proofs:, bindings:)
    end

    def create_resource_claim(owner, input, resources, wait:, proofs: [], bindings: [])
      # The collector rechecks held resources while owning this guard. A new
      # grant must not race the gap between that check and worktree removal.
      result = store.lock('preparation', mode: File::LOCK_SH, nonblock: true) do
        create_resource_claim_ready(owner, input, resources, wait:, grant: true, proofs:, bindings:)
      end
      return result if result
      raise Error.new('retained preparation waits for payload collection', 75) unless wait
      create_resource_claim_ready(owner, input, resources, wait: true, grant: false, proofs:, bindings:)
    end

    def existing_claim(value, owner, input)
      saved = value['claims'][input['requestId']]
      return unless saved
      unless saved['ownerId'] == owner['id'] && saved['request'] == WorkspaceCoordination.canonical(input)
        raise Error.new('claim requestId reused with changed input or owner', 4)
      end
      saved
    end

    def create_resource_claim_ready(owner, input, resources, wait:, grant:, proofs: [], bindings: [])
      request = WorkspaceCoordination.canonical(input)
      store.lock do
        ledger, value = store.read, store.claims
        claim_owner_live!(owner, ledger)
        saved = existing_claim(value, owner, request)
        next saved if saved
        proofs.each(&:recheck!)
        check_machine_source_pairs!(ledger, value, request, bindings)
        now = Time.now.utc.iso8601
        item = {'id' => request['requestId'], 'ownerId' => owner['id'], 'request' => request, 'resources' => resources,
          'state' => 'waiting', 'revision' => 1, 'sequence' => value['claims'].length + 1,
          'createdAt' => now, 'updatedAt' => now, 'notificationPending' => false}
        if request['schema'] == 2
          item['machineBindings'] = bindings
          value['schema'] = 2
        end
        value['claims'][item['id']] = item
        blocked = !grant || claim_blocked?(ledger, value, item)
        raise Error.new('retained preparation waits for resource ownership', 75) if blocked && !wait
        item['state'] = 'held' unless blocked
        store.admission_capacity!(ledger, extra: blocked && owner['thread'] ? MAX_RESULT + 64 : 0)
        store.write_claims(value, admission: true)
        item
      end
    end

    # Only retained internal writers use this scope. Tracking commits may
    # touch the workspace root without registering it as a feature repository.
    # Admission surrounds ownership updates only: retained tracking invokes
    # the ordinary artifact writer, which needs the exclusive session guard.
    def with_repository_claim(owner, name, admission: ->(&block) { block.call })
      return yield unless File.file?(File.join(package, 'share/dev-workspace/claim-contract.json'))
      current_generation!
      resources = if name == 'workspace'
        common = File.realpath(capture('git', '-C', workspace, 'rev-parse', '--path-format=absolute', '--git-common-dir').strip)
        identity = WorkspaceCoordination.remote_identity(capture('git', '-C', workspace, 'remote', 'get-url', 'origin').strip, fixtures: @fixtures)
        configured = WorkspaceCoordination.remote_identity(catalog.repository(name).fetch('remote'), fixtures: @fixtures)
        raise Error.new('tracking repository differs from the coordination catalog', 4) unless identity == configured
        ["git:#{common}", "repository:#{identity}"].sort
      else
        claim_resources_for(owner, {'repositories' => [name], 'machines' => []})
      end
      request = {'schema' => 1, 'requestId' => SecureRandom.uuid, 'repositories' => [name], 'machines' => [], 'label' => 'Retained repository maintenance'}
      item = admission.call { current_generation!; create_resource_claim(owner, request, resources, wait: false) }
      begin
        yield
      ensure
        admission.call { change_claim(owner, item['id'], 'release') }
      end
    end

    def change_claim(owner, id, action)
      current_generation!
      store.lock do
        ledger, value = store.read, store.claims
        claim_owner_live!(owner, ledger)
        item = claim_owned!(value, owner, id)
        destination = action == 'release' ? 'released' : 'canceled'
        next item if item['state'] == destination
        expected = action == 'release' ? 'held' : 'waiting'
        raise Error.new("#{action} requires a #{expected} claim", 5) unless item['state'] == expected
        item.merge!('state' => destination, 'notificationPending' => false, 'updatedAt' => Time.now.utc.iso8601)
        item['revision'] += 1
        store.write_claims(value)
        item
      end
    end

    # Session admission surrounds this call. Ownership is persisted first;
    # delivery uses the existing durable outbox and can never grant resources.
    def tick_claims(owner)
      store.lock('preparation', mode: File::LOCK_SH, nonblock: true) { tick_claims_ready(owner) }
    end

    def tick_claims_ready(owner)
      current_generation!
      store.claims['claims'].values.select { |item| item['ownerId'] == owner['id'] && item['state'] == 'waiting' }.sort_by { |item| item['sequence'] }.each do |saved|
        proofs, bindings = [], []
        resources = claim_resources_for(owner, saved['request'], proofs:, bindings:)
        store.lock do
          ledger, value = store.read, store.claims
          claim_owner_live!(owner, ledger)
          item = claim_owned!(value, owner, saved['id'])
          next unless item['state'] == 'waiting'
          proofs.each(&:recheck!)
          raise Error.new('queued resource mappings changed; cancel and acquire again', 3) unless resources == item['resources']
          if saved['machineBindings'] && bindings.map { |binding| binding.reject { |key, _| key == 'sourceRevision' } } != saved['machineBindings'].map { |binding| binding.reject { |key, _| key == 'sourceRevision' } }
            raise Error.new('queued machine metadata changed; cancel and acquire again', 3)
          end
          check_machine_source_pairs!(ledger, value, item['request'], bindings)
          next if claim_blocked?(ledger, value, item)
          item.merge!('state' => 'held', 'notificationPending' => true, 'updatedAt' => Time.now.utc.iso8601)
          item['revision'] += 1
          store.write_claims(value)
        end
      end
      store.claims['claims'].values.select { |item| item['ownerId'] == owner['id'] && item['notificationPending'] }.each do |item|
        store.transaction do |ledger|
          claim_owner_live!(owner, ledger)
          live = store.claims['claims'].fetch(item['id'])
          event(ledger, owner, live.merge('kind' => 'resource claim'), 'held') if live['state'] == 'held'
        end
        store.lock do
          value = store.claims
          value['claims'].fetch(item['id'])['notificationPending'] = false
          store.write_claims(value)
        end
      end
    end

    def profile_claim!(id, machine)
      item = store.claims['claims'][id]
      retained = item && store.read['owners'][item['ownerId']]
      unless retained && retained['workspace'] == workspace && !retained['fence'] &&
             retained['slug'] == @env['DEV_SESSION_SLUG'] && retained['workspace'] == @env['DEV_SESSION_WORKSPACE'] &&
             claim_owner(retained['slug'], read_only: true)['id'] == retained['id'] && item['state'] == 'held' &&
             machine_policy.normalize(item['resources']).include?(machine_policy.canonical(machine.start_with?('machine:') ? machine : "machine:#{machine}"))
        raise Error.new('profile activation requires the owning session and a held physical machine claim', 4)
      end
      if item['machineBindings']
        binding = item['machineBindings'].find { |value| machine_policy.canonical(MachineIdentity.resource(value['identity'])) == machine_policy.canonical(machine.start_with?('machine:') ? machine : "machine:#{machine}") }
        raise Error.new('profile claim has no matching logical machine binding', 4) unless binding
        if machine_policy.data['profileTarget'] && binding['identity'] != machine_policy.data['profileTarget']
          raise Error.new('held machine binding differs from the configured profile target', 3)
        end
        verify_machine_binding(retained, id, binding['repository'], binding['selector'])
      end
      item
    end

    def verify_machine(owner, id, repository, selector)
      current_generation!
      verify_machine_binding(owner, id, repository, selector)
    end

    # Profile callers already hold their ordinary transition/generation locks,
    # including forward completion after selecting the exact new candidate.
    def verify_machine_binding(owner, id, repository, selector)
      item = store.claims['claims'][id]
      raise Error.new('verify-machine requires an owning held typed machine claim', 4) unless item && item['ownerId'] == owner['id'] && item['state'] == 'held'
      saved = item.fetch('machineBindings', []).find { |binding| binding['repository'] == repository && binding['selector'] == selector }
      raise Error.new('held claim has no exact registered machine binding', 4) unless saved
      proofs = []
      binding = resolve_machine_binding(owner, saved.slice('repository', 'selector', 'adapter'), proofs:)
      unless binding.reject { |key, _| key == 'sourceRevision' } == saved.reject { |key, _| key == 'sourceRevision' }
        raise Error.new('held machine mapping changed; retain the original claim and resolve the configuration change', 3)
      end
      store.lock do
        ledger, value = store.read, store.claims
        claim_owner_live!(owner, ledger)
        live = claim_owned!(value, owner, id)
        raise Error.new('machine claim changed during verification', 3) unless live == item
        proofs.each(&:recheck!)
      end
      resource = machine_policy.canonical(MachineIdentity.resource(saved['identity']))
      {'schema' => 1, 'claimId' => id, 'resource' => resource, 'identifier' => machine_policy.identifier(resource, [saved['identity']]),
       'sourceRevision' => binding['sourceRevision']}
    end
  end
end
