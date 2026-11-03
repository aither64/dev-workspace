# frozen_string_literal: true

module WorkspaceCoordination
  MACHINE_CLAIM_CONTRACT = {'schema' => 1, 'claimRecordSchemas' => [1, 2], 'machineIdentityVersion' => 1, 'resolverProtocol' => 1}.freeze

  # Portable identity has one encoding. Source paths, revisions and adapters
  # prove admission, but never enter this key or its human-readable identifier.
  module MachineIdentity
    def self.text!(value)
      text = value.is_a?(String) && value.dup.force_encoding(Encoding::UTF_8)
      unless text && text.valid_encoding? &&
             text.bytesize.between?(1, 256) && !text.match?(/[[:cntrl:]]|\A[[:space:]]|[[:space:]]\z/)
        raise Error, 'machine names and shared labels require bounded UTF-8 text without controls or surrounding whitespace'
      end
      text
    end

    def self.origin!(value, fixtures: false)
      unless value.is_a?(String) && value.valid_encoding? &&
             ((fixtures && value.start_with?('fixture:') && !value.include?("\0")) ||
              WorkspaceCoordination.remote_identity("ssh://git@#{value}.git") == value)
        raise Error, 'machine repository origin must be its canonical registered SSH identity'
      end
      value
    end

    def self.validate!(identity, fixtures: false)
      raise Error, 'invalid structured machine identity' unless identity.is_a?(Hash)
      case identity['kind']
      when 'repository'
        WorkspaceCoordination.keys!(identity, %w[kind origin name])
        origin!(identity['origin'], fixtures:)
        text!(identity['name'])
      when 'shared'
        WorkspaceCoordination.keys!(identity, %w[kind id])
        text!(identity['id'])
      else
        raise Error, 'unsupported machine identity kind'
      end
      WorkspaceCoordination.canonical(identity)
    end

    def self.resource(identity)
      "machine:logical:v1:#{Digest::SHA256.hexdigest(JSON.generate(WorkspaceCoordination.canonical(['machine-identity', 1, identity])))}"
    end

    def self.claim_contract?(value)
      value == MACHINE_CLAIM_CONTRACT && %w[schema machineIdentityVersion resolverProtocol].all? { |key| value[key].is_a?(Integer) } &&
        value['claimRecordSchemas'].all? { |schema| schema.is_a?(Integer) }
    end

    def self.escape(value)
      value.bytes.map { |byte| byte.chr.match?(/[A-Za-z0-9._~\/-]/) ? byte.chr : format('%%%02X', byte) }.join
    end

    def self.identifier(identity)
      identity['kind'] == 'shared' ? "machine:shared:#{escape(identity.fetch('id'))}" :
        "machine:#{escape(identity.fetch('origin'))}:#{escape(identity.fetch('name'))}"
    end
  end

  # Immutable package compatibility only; ordinary machines need no entry.
  class MachinePolicy
    attr_reader :data, :catalog, :explicit

    def initialize(package, catalog:, fixtures: false)
      @catalog, @fixtures = catalog, fixtures
      path = File.join(package, 'share/dev-workspace/machine-identity-policy.json')
      @explicit = begin
        File.lstat(path)
        true
      rescue Errno::ENOENT
        false
      end
      @data = explicit ? WorkspaceCoordination.parse(File.binread(path, MAX_REQUEST + 1)) : legacy_defaults
      WorkspaceCoordination.keys!(data, %w[schema profileTarget legacyAliases])
      unless data['schema'].is_a?(Integer) && data['schema'] == 1 && data['legacyAliases'].is_a?(Array)
        raise Error.new('unsupported machine identity policy', 5)
      end
      raise Error.new('invalid profile machine identity', 5) unless data['profileTarget'].nil? || data['profileTarget'].is_a?(Hash)
      MachineIdentity.validate!(data['profileTarget'], fixtures:) if data['profileTarget']
      @aliases, @identities = {}, {}
      data['legacyAliases'].each do |item|
        WorkspaceCoordination.keys!(item, %w[resource identity])
        resource = item['resource']
        unless resource.is_a?(String) && resource.match?(/\Amachine:.+\z/) && !resource.match?(/[[:cntrl:]]/)
          raise Error.new('invalid legacy machine resource', 5)
        end
        identity = MachineIdentity.validate!(item['identity'], fixtures:)
        target = MachineIdentity.resource(identity)
        raise Error.new('ambiguous legacy machine mapping', 5) if @aliases.key?(resource) && @aliases[resource] != target
        @aliases[resource], @identities[target] = target, identity
      end
      @identities[MachineIdentity.resource(data['profileTarget'])] = data['profileTarget'] if data['profileTarget']
      @aliases.each_key { |resource| canonical(resource) }
    rescue SystemCallError, Error => error
      raise Error.new("invalid machine identity policy: #{error.message}", 5)
    end

    def legacy_identity(machine)
      repository = catalog.repository(machine.fetch('configurationRepository'))
      {'kind' => 'repository', 'origin' => WorkspaceCoordination.remote_identity(repository.fetch('remote'), fixtures: @fixtures),
       'name' => machine.fetch('selector')}
    end

    def legacy_defaults
      machines = catalog.data.fetch('machines')
      target = catalog.data['profileMachine']
      {'schema' => 1, 'profileTarget' => target && legacy_identity(catalog.machine(target)),
       'legacyAliases' => machines.values.map { |machine| {'resource' => "machine:#{machine['id']}", 'identity' => legacy_identity(machine)} }}
    end

    def canonical(resource)
      seen = {}
      while @aliases.key?(resource)
        raise Error.new('cyclic legacy machine mapping', 5) if seen[resource]
        seen[resource] = true
        resource = @aliases.fetch(resource)
      end
      resource
    end

    def normalize(resources)
      Engine.normalized_resources(resources).map { |resource| canonical(resource) }.uniq
    end

    def identity(resource)
      key = canonical(resource)
      @identities[key] || @identities.values.find { |value| canonical(MachineIdentity.resource(value)) == key }
    end

    def profile_resource
      if !explicit && (name = catalog.data['profileMachine'])
        "machine:#{catalog.machine(name)['id']}"
      elsif data['profileTarget']
        MachineIdentity.resource(data['profileTarget'])
      end
    end

    def identifier(resource, identities = [])
      value = identity(resource) || identities.find { |item| canonical(MachineIdentity.resource(item)) == canonical(resource) }
      value ? MachineIdentity.identifier(value) : resource.delete_prefix('machine:')
    end

    def legacy_bindings
      catalog.data.fetch('machines').values.map do |machine|
        {'repositoryOrigin' => legacy_identity(machine).fetch('origin'), 'selector' => machine.fetch('selector'),
         'adapter' => machine.fetch('adapter'), 'resource' => "machine:#{machine.fetch('id')}"}
      end
    end
  end

  # Resolve once outside the ledger lock. Recheck only bounded local metadata
  # inside admission, including loose or packed refs; never run Git/Nix there.
  class MachineSource
    attr_reader :repository, :ref, :revision

    def initialize(owner, name, catalog:, env: ENV, fixtures: false)
      @repository = RepositoryIdentity.new(owner, name, catalog:, env:, fixtures:)
      @source_path = File.realpath(repository.path)
      @env = env.to_h
      branch = repository.registration&.fetch('branch', nil)
      unless branch.is_a?(String) && !branch.empty? && branch.bytesize <= 1024 && !branch.match?(/[[:cntrl:]]/)
        raise Error.new('machine source requires its registered feature branch', 4)
      end
      @ref = "refs/heads/#{branch}"
      command('check-ref-format', ref)
      @revision = command('rev-parse', '--verify', "#{ref}^{commit}").strip
      raise Error.new('machine source has no exact committed feature revision', 3) unless revision.match?(SHA)
      command('cat-file', '-e', "#{revision}:flake.nix")
      recheck!
    end

    def command(*args)
      output, _error, status, overflow = Command.run(['git', '-C', repository.path, *args], env: @env, limit: MAX_REQUEST)
      raise Error.new('machine source branch, commit or flake is unavailable; prepare the registered feature source', 3) unless status.success? && !overflow
      output
    end

    def local_revision
      loose = File.join(repository.common, ref)
      begin
        bytes = File.binread(loose, 1025)
        raise Error.new('machine source ref is malformed', 3) unless bytes.match?(/\A[0-9a-f]{40}\n?\z/)
        return bytes.strip
      rescue Errno::ENOENT
        # A packed branch has no loose ref. Other I/O failures are not absence.
      end
      packed = File.binread(File.join(repository.common, 'packed-refs'), MAX_REQUEST + 1)
      raise Error.new('machine source packed refs exceed their bound', 3) if packed.bytesize > MAX_REQUEST
      line = packed.lines.find { |row| row.chomp.end_with?(" #{ref}") }
      line&.match(/\A([0-9a-f]{40}) /)&.captures&.first
    end

    def recheck!
      repository.recheck!
      raise Error.new('machine source ref changed during resolution; try again', 3) unless local_revision == revision
    rescue SystemCallError
      raise Error.new('machine source ref changed during resolution; try again', 3)
    end

    def resolve(selector, adapter, catalog:, diagnostics: nil)
      MachineIdentity.text!(selector)
      request = {'schema' => 1, 'operation' => 'resolve-machine', 'selector' => selector,
        'source' => {'path' => @source_path, 'revision' => revision}}
      output, _error, status, overflow = Command.run([catalog.adapter(adapter).fetch('command')], env: @env,
        input: JSON.generate(request), limit: MAX_RESULT, log: diagnostics)
      raise Error.new('machine metadata resolution failed; prepare local locked inputs and retry', 3) unless status.success? && !overflow
      response = WorkspaceCoordination.parse(output, limit: MAX_RESULT)
      WorkspaceCoordination.keys!(response, %w[schema name sharedLockId])
      unless response['schema'].is_a?(Integer) && response['schema'] == 1 && response['name'] == selector
        raise Error, 'machine resolver returned a different selector or protocol'
      end
      MachineIdentity.text!(response['name'])
      label = response['sharedLockId']
      label.nil? ? {'kind' => 'repository', 'origin' => repository.identity, 'name' => selector} :
        {'kind' => 'shared', 'id' => MachineIdentity.text!(label)}
    end
  end
end
