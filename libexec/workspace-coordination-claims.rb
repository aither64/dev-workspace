# frozen_string_literal: true

# Cooperative resource ownership only. Sessions execute and verify their work.
module WorkspaceCoordination
  CLAIM_CONTRACT = {'schema' => 1, 'recordSchema' => 1}.freeze
  REPOSITORY_CLAIM_CONTRACT = {'schema' => 1, 'registeredRepositories' => true}.freeze
  CLAIM_ACTIVE = %w[waiting held].freeze

  class Store
    def claims
      path = File.join(root, 'claims.json')
      return {'schema' => 1, 'claims' => {}} unless File.exist?(path)
      validate_claims!(WorkspaceCoordination.private_read(path))
    end

    def validate_claim_request!(request)
      WorkspaceCoordination.keys!(request, %w[schema requestId repositories machines label])
      unless request['schema'] == 1 && request['requestId'].is_a?(String) && request['requestId'].match?(UUID) &&
             request['label'].is_a?(String) && request['label'].bytesize.between?(1, 512) && !request['label'].include?("\0") &&
             %w[repositories machines].all? { |key| request[key].is_a?(Array) && request[key].uniq == request[key] && request[key].all? { |name| name.is_a?(String) && name.bytesize.between?(1, 256) && !name.include?("\0") } } &&
             request['repositories'].all? { |name| name.match?(PART) } && !(request['repositories'] + request['machines']).empty? &&
             JSON.generate(request).bytesize <= MAX_RESULT
        raise Error, 'invalid resource request; supply requestId, label and repository/machine lists'
      end
    end

    def validate_claims!(value)
      WorkspaceCoordination.keys!(value, %w[schema claims])
      raise Error.new('unsupported resource claims; recover forward', 5) unless value['schema'] == 1 && value['claims'].is_a?(Hash)
      value['claims'].each do |id, item|
        WorkspaceCoordination.keys!(item, %w[id ownerId request resources state revision sequence createdAt updatedAt notificationPending])
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
      end
      held = value['claims'].values.select { |item| item['state'] == 'held' }.flat_map { |item| item['resources'] }
      sequences = value['claims'].values.map { |item| item['sequence'] }
      raise Error.new('overlapping resource claims or duplicate sequence', 5) unless held.uniq == held && sequences.uniq == sequences
      value
    rescue NoMethodError, TypeError => error
      raise Error.new("malformed resource claims: #{error.message}", 5)
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
      claims['claims'].values.select { |item| item['state'] == 'held' }.flat_map { |item| item['resources'] }
    end

    def claim_notification_events(ledger)
      claims['claims'].values.filter_map do |item|
        next unless item['state'] == 'waiting' || item['notificationPending']
        owner = ledger['owners'].fetch(item['ownerId'])
        WorkspaceCoordination.event_id(owner, item, 'held') if owner['thread']
      end
    end

    def require_claim_capability!(package)
      return unless File.exist?(File.join(root, 'claims.json'))
      value = claims
      path = File.join(package, 'share/dev-workspace/claim-contract.json')
      unless File.file?(path) && WorkspaceCoordination.parse(File.binread(path)) == CLAIM_CONTRACT
        raise Error.new('target package cannot preserve resource claims; recover forward with a compatible package', 5)
      end
      catalog = Catalog.new(package)
      value['claims'].values.select { |item| CLAIM_ACTIVE.include?(item['state']) }.each do |item|
        item['request']['machines'].each do |target|
          raise Error.new('target package changes a claimed physical machine', 5) unless item['resources'].include?("machine:#{catalog.machine(target)['id']}")
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
    def claim_owner(slug)
      manifest = YAML.safe_load(File.binread(File.join(workspace, 'work', slug, 'portal.yml')), permitted_classes: [], aliases: false)
      owner(slug, bootstrap: manifest.dig('codex', 'thread_id').nil?)
    end

    def claim_resources_for(owner, request, proofs: [])
      resources = request['repositories'].flat_map do |name|
        identity = repository_identity(owner, name)
        proofs << identity
        identity.resources
      end
      resources.concat(request['machines'].map { |name| "machine:#{catalog.machine(name).fetch('id')}" }).sort.uniq
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
      legacy = ledger['jobs'].values.select { |job| CLAIMED.include?(job['state']) }.flat_map { |job| Engine.normalized_resources(job['resources']) }
      reservations = ledger['reservations'].values.reject { |hold| %w[requested released].include?(hold['state']) }.map { |hold| "machine:#{hold['machine']}" }
      held = value['claims'].values.select { |other| other['state'] == 'held' }.flat_map { |other| other['resources'] }
      busy = legacy + reservations + held
      return true unless (item['resources'] & busy).empty?
      value['claims'].values.any? do |other|
        other['id'] != item['id'] && other['state'] == 'waiting' && other['sequence'] < item['sequence'] &&
          (other['resources'] & busy).empty? && !(item['resources'] & other['resources']).empty?
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
      proofs = []
      resources = claim_resources_for(owner, input, proofs: proofs)
      create_resource_claim(owner, input, resources, wait: wait, proofs: proofs)
    end

    def create_resource_claim(owner, input, resources, wait:, proofs: [])
      # The collector rechecks held resources while owning this guard. A new
      # grant must not race the gap between that check and worktree removal.
      result = store.lock('preparation', mode: File::LOCK_SH, nonblock: true) do
        create_resource_claim_ready(owner, input, resources, wait: wait, grant: true, proofs: proofs)
      end
      return result if result
      raise Error.new('retained preparation waits for payload collection', 75) unless wait
      create_resource_claim_ready(owner, input, resources, wait: true, grant: false, proofs: proofs)
    end

    def existing_claim(value, owner, input)
      saved = value['claims'][input['requestId']]
      return unless saved
      unless saved['ownerId'] == owner['id'] && saved['request'] == WorkspaceCoordination.canonical(input)
        raise Error.new('claim requestId reused with changed input or owner', 4)
      end
      saved
    end

    def create_resource_claim_ready(owner, input, resources, wait:, grant:, proofs: [])
      request = WorkspaceCoordination.canonical(input)
      store.lock do
        ledger, value = store.read, store.claims
        claim_owner_live!(owner, ledger)
        saved = existing_claim(value, owner, request)
        next saved if saved
        proofs.each(&:recheck!)
        now = Time.now.utc.iso8601
        item = {'id' => request['requestId'], 'ownerId' => owner['id'], 'request' => request, 'resources' => resources,
          'state' => 'waiting', 'revision' => 1, 'sequence' => value['claims'].length + 1,
          'createdAt' => now, 'updatedAt' => now, 'notificationPending' => false}
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
        proofs = []
        resources = claim_resources_for(owner, saved['request'], proofs: proofs)
        store.lock do
          ledger, value = store.read, store.claims
          claim_owner_live!(owner, ledger)
          item = claim_owned!(value, owner, saved['id'])
          next unless item['state'] == 'waiting'
          proofs.each(&:recheck!)
          raise Error.new('queued resource mappings changed; cancel and acquire again', 3) unless resources == item['resources']
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
             claim_owner(retained['slug'])['id'] == retained['id'] && item['state'] == 'held' && item['resources'].include?("machine:#{machine}")
        raise Error.new('profile activation requires the owning session and a held physical machine claim', 4)
      end
      item
    end
  end
end
