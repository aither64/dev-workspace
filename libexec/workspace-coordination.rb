# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'open3'
require 'securerandom'
require 'socket'
require 'time'
require 'yaml'
require_relative 'workspace-profile-identity'
require_relative 'workspace-coordination-command'
require_relative 'workspace-coordination-timing'

# The ledger mutex protects JSON transactions only. External commands and
# physical locks always belong outside it. Admitted workers publish separate
# receipts so selecting a new package never grants an old scheduler authority.
module WorkspaceCoordination
  UUID = /\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/
  SHA = /\A[0-9a-f]{40}\z/
  PART = /\A[A-Za-z0-9][A-Za-z0-9_-]{0,127}\z/
  TERMINAL = %w[succeeded failed canceled].freeze
  CLAIMED = %w[claimed running reconciling].freeze
  RESERVATIONS = %w[requested held deployed_review closing released blocked_recovery].freeze
  STATES = %w[waiting_approval waiting_dependency waiting_resource ready claimed running needs_refresh reconciling succeeded failed canceled].freeze
  CONTRACT = {
    'schema' => 1, 'ledgerSchema' => 1, 'snapshotSchema' => 1,
    'attemptSchema' => 1, 'adapterProtocols' => [1],
    'notificationSchema' => 1, 'transitionPolicy' => 1
  }.freeze
  INTEGRATION_CONTRACT = {
    'schema' => 1, 'journalSchemas' => [1, 2], 'certificationSchema' => 1
  }.freeze
  MAX_REQUEST = 256 * 1024
  MAX_LEDGER = 16 * 1024 * 1024
  MAX_RESULT = 16 * 1024
  MAX_COUNTER = (1 << 63) - 1
  RECOVERY_RESERVE = 2 * 1024 * 1024
  JOB_EVENT_STATES = %w[ready needs_refresh reconciling succeeded failed].freeze

  class Error < StandardError
    attr_reader :code
    def initialize(message, code = 2)
      super(message)
      @code = code
    end
  end
  class StrictHash < Hash
    def []=(key, value)
      raise JSON::ParserError, "duplicate JSON key: #{key}" if key?(key)
      super
    end
  end

  def self.parse(payload, limit: MAX_REQUEST)
    raise Error, 'coordination JSON exceeds its size limit' if payload.bytesize > limit
    canonical(JSON.parse(payload, object_class: StrictHash, max_nesting: 48))
  rescue JSON::ParserError => e
    raise Error, "invalid coordination JSON: #{e.message}"
  end

  def self.keys!(data, required, optional = [])
    unless data.is_a?(Hash) && (required - data.keys).empty? && (data.keys - required - optional).empty?
      raise Error, "invalid fields; expected #{(required + optional).join(', ')}"
    end
  end

  def self.canonical(data)
    case data
    when Hash then data.keys.sort.to_h { |key| [key, canonical(data.fetch(key))] }
    when Array then data.map { |value| canonical(value) }
    else data
    end
  end

  def self.digest(data)
    "sha256:#{Digest::SHA256.hexdigest(JSON.generate(canonical(data)))}"
  end

  def self.diagnostic(value)
    text = value.to_s.b.byteslice(0, 4096).force_encoding(Encoding::UTF_8).scrub('')
    text = text.byteslice(0, text.bytesize / 2).force_encoding(Encoding::UTF_8).scrub('') while JSON.generate(text).bytesize > 4096
    text
  end

  def self.event_id(owner, item, state)
    token = Digest::SHA256.hexdigest(JSON.generate(['coordination-event-v1', owner['id'], item['id'], item.dig('snapshot', 'id'), state]))
    [token[0, 8], token[8, 4], token[12, 4], token[16, 4], token[20, 12]].join('-')
  end

  def self.private_read(path, limit: MAX_LEDGER)
    stat = File.lstat(path)
    unless stat.file? && stat.uid == Process.euid && (stat.mode & 0o777) == 0o600 && stat.size <= limit
      raise Error.new("coordination file is not a bounded private file: #{path}", 5)
    end
    parse(File.binread(path, limit + 1), limit:)
  end

  def self.atomic(path, data, limit: MAX_LEDGER)
    encoded = JSON.generate(canonical(data)) + "\n"
    raise Error.new('coordination file exceeds its persisted size bound', 5) if encoded.bytesize > limit
    FileUtils.mkdir_p(File.dirname(path), mode: 0o700)
    temporary = "#{path}.#{SecureRandom.hex(8)}.tmp"
    begin
      File.open(temporary, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
        file.write(encoded)
        file.flush
        file.fsync
      end
      File.rename(temporary, path)
      File.open(File.dirname(path), File::RDONLY) { |directory| directory.fsync }
    ensure
      File.unlink(temporary) if File.exist?(temporary)
    end
  end

  def self.remote_identity(remote, fixtures: false)
    return "fixture:#{File.realpath(remote)}" if fixtures && remote.start_with?('/')
    match = /\A(?:git@([^:]+):|ssh:\/\/git@([^\/]+)\/)([^\s]+?)\/?\z/.match(remote)
    raise Error, 'coordination writes require a canonical SSH Git remote' unless match
    host = (match[1] || match[2]).downcase
    path = match[3].delete_suffix('.git')
    raise Error, 'invalid SSH repository path' unless path.match?(%r{\A[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+\z})
    path = path.downcase if host == 'github.com'
    "#{host}/#{path}"
  end

  def self.creation_identity(manifest)
    # Revival rewrites tracking proofs, not the retained conversation identity.
    manifest['creation']&.reject { |key, _| %w[tracking_origin tracking_plan_sha256 tracking_state_sha256].include?(key) }
  end

  class Store
    attr_reader :root
    def initialize(state)
      @root = File.join(File.expand_path(state), 'coordination')
    end

    def lock(name = 'ledger', nonblock: false, mode: File::LOCK_EX)
      FileUtils.mkdir_p(root, mode: 0o700)
      stat = File.lstat(root)
      raise Error.new('coordination directory has unsafe identity', 5) unless stat.directory? && stat.uid == Process.euid && (stat.mode & 0o777) == 0o700
      path = File.join(root, "#{name}.lock")
      raise Error.new('coordination lock is a symlink', 5) if File.symlink?(path)
      File.open(path, File::RDWR | File::CREAT, 0o600) do |file|
        return false unless file.flock(mode | (nonblock ? File::LOCK_NB : 0))
        yield
      end
    end

    def empty
      {'schema' => 1, 'revision' => 0, 'nextSequence' => 1, 'suspended' => false,
       'owners' => {}, 'approvals' => {}, 'reservations' => {}, 'jobs' => {},
       'outbox' => {}, 'receipts' => {}, 'frontier' => {}, 'lifecycles' => {}}
    end

    def read
      path = File.join(root, 'ledger.json')
      return empty unless File.exist?(path) || File.symlink?(path)
      data = WorkspaceCoordination.private_read(path)
      validate!(data)
      data
    end

    def transaction
      lock do
        data = read
        before = WorkspaceCoordination.digest(data)
        previous_states = Timing.states(data)
        result = yield(data)
        if WorkspaceCoordination.digest(data) != before
          data['revision'] += 1
          validate!(data)
          payload = JSON.generate(data)
          raise Error.new('coordination ledger is full; retain unresolved state', 5) if payload.bytesize + 1 > MAX_LEDGER
          WorkspaceCoordination.atomic(File.join(root, 'ledger.json'), data)
          Timing.new(root).publish(previous_states, data)
          wake
        end
        sync_claims_lifecycle(data)
        result
      end
    end

    # Best effort latency improvement; the scheduler's five-second tick is the
    # durable fallback when it is restarting or its datagram queue is full.
    def wake
      socket = Socket.new(Socket::AF_UNIX, Socket::SOCK_DGRAM, 0)
      socket.sendmsg_nonblock('.', 0, Socket.sockaddr_un(File.join(root, 'wake.sock')))
    rescue SystemCallError, IOError, ArgumentError
      nil
    ensure
      socket&.close
    end

    def validate!(data)
      WorkspaceCoordination.keys!(data, %w[schema revision nextSequence suspended owners approvals reservations jobs outbox receipts frontier lifecycles])
      unless data['schema'] == 1 && data['revision'].is_a?(Integer) && data['revision'] >= 0 &&
             data['nextSequence'].is_a?(Integer) && data['nextSequence'] > 0 && [true, false].include?(data['suspended']) &&
             %w[owners approvals reservations jobs outbox receipts frontier lifecycles].all? { |key| data[key].is_a?(Hash) }
        raise Error.new('unsupported or malformed coordination ledger', 5)
      end
      raise Error.new('coordination counters exceed their bound', 5) unless %w[revision nextSequence].all? { |key| data[key] <= MAX_COUNTER }
      data['jobs'].each do |id, job|
        WorkspaceCoordination.keys!(job, %w[id ownerId kind dependsOn approvalId reservationId snapshot resources state reason revision sequence readySequence attempt result history coverageContracts])
        unless id.match?(UUID) && job['id'] == id && STATES.include?(job['state']) &&
               data['owners'].key?(job['ownerId']) && job['revision'].is_a?(Integer) &&
               job['resources'].is_a?(Array) && job['dependsOn'].is_a?(Array) &&
               job['revision'] > 0 && %w[merge machine_activate profile_activate].include?(job['kind']) && job['history'].is_a?(Array) &&
               job['resources'].all? { |resource| resource.is_a?(String) } && job['resources'].uniq == job['resources'] &&
               job['sequence'].is_a?(Integer) && job['sequence'] > 0 &&
               job['coverageContracts'].is_a?(Array) &&
               (!job['attempt'] || job['attempt'].is_a?(String) && job['attempt'].match?(UUID)) &&
               (!CLAIMED.include?(job['state']) || job['attempt']) && job['snapshot'].is_a?(Hash) &&
               job['snapshot']['id'] == WorkspaceCoordination.digest(job['snapshot'].reject { |key, _| key == 'id' }) &&
               job['dependsOn'].all? { |dependency| data['jobs'][dependency]&.fetch('ownerId') == job['ownerId'] && data['jobs'][dependency]['sequence'] < job['sequence'] }
          raise Error.new('invalid coordination job record', 5)
        end
        raise Error.new('operation counters exceed their bound', 5) unless %w[revision sequence readySequence].all? { |key| job[key].nil? || job[key] <= MAX_COUNTER }
        %w[result reason].each do |key|
          raise Error.new("operation #{key} exceeds its recovery budget", 5) if JSON.generate(job[key]).bytesize > MAX_RESULT
        end
        job['coverageContracts'].each do |contract|
          WorkspaceCoordination.keys!(contract, %w[repository target owned remoteIdentity])
          unless contract['repository'].is_a?(String) && contract['repository'].match?(PART) &&
                 contract['target'].is_a?(String) && contract['target'].start_with?('refs/heads/') && [true, false].include?(contract['owned']) && contract['remoteIdentity'].is_a?(String)
            raise Error.new('invalid sealed coverage contract', 5)
          end
        end
      end
      data['reservations'].each do |id, reservation|
        WorkspaceCoordination.keys!(reservation, %w[id ownerId machine target state revision sequence mode approvalId integrationObligations history], %w[completion])
        unless id.match?(UUID) && reservation['id'] == id && RESERVATIONS.include?(reservation['state']) &&
               data['owners'].key?(reservation['ownerId']) && reservation['history'].is_a?(Array) &&
               reservation['revision'].is_a?(Integer) && reservation['revision'] > 0 && reservation['sequence'].is_a?(Integer) &&
               reservation['machine'].is_a?(String) && %w[deploy_first integrated].include?(reservation['mode']) && reservation['integrationObligations'].is_a?(Array)
          raise Error.new('invalid coordination reservation record', 5)
        end
        reservation['history'].each do |record|
          WorkspaceCoordination.keys!(record, %w[attempt kind artifact coverage coverageContracts observed outcome], %w[dispositions])
          dispositions = record.fetch('dispositions', [])
          unless record['coverage'].is_a?(Array) && record['coverageContracts'].is_a?(Array) &&
                 dispositions.is_a?(Array) && dispositions.length <= 1 &&
                 JSON.generate(record['observed']).bytesize <= MAX_RESULT &&
                 dispositions.all? { |item| JSON.generate(item).bytesize <= MAX_RESULT }
            raise Error.new('deployment history exceeds its recovery budget', 5)
          end
        end
      end
      data['owners'].each do |id, owner|
        WorkspaceCoordination.keys!(owner, %w[id identity workspace workspaceName slug thread socket portalUrl bootstrap fence])
        unless id.match?(UUID) && owner['id'] == id && owner['workspace'].is_a?(String) && owner['workspace'].start_with?('/') &&
               owner['slug'].is_a?(String) && owner['slug'].match?(PART) && [true, false].include?(owner['bootstrap']) &&
               (owner['bootstrap'] ? owner['thread'].nil? : owner['thread'].is_a?(String) && !owner['thread'].empty?)
          raise Error.new('invalid coordination owner record', 5)
        end
        %w[portalUrl thread].each do |key|
          raise Error.new('coordination notification identity exceeds its bound', 5) if JSON.generate(owner[key]).bytesize > 1024
        end
        if owner['fence']
          WorkspaceCoordination.keys!(owner['fence'], %w[id command completed operationId])
          raise Error.new('invalid lifecycle fence', 5) unless owner['fence']['id'].match?(UUID) && %w[archive delete].include?(owner['fence']['command']) && [true, false].include?(owner['fence']['completed'])
          raise Error.new('invalid lifecycle operation binding', 5) unless owner['fence']['operationId'].nil? || owner['fence']['operationId'].match?(/\A[0-9a-f]{64}\z/)
        end
      end
      data['approvals'].each do |id, approval|
        WorkspaceCoordination.keys!(approval, %w[schema requestId kind scope source reviewedSnapshot id ownerId revision], %w[iterationScope])
        raise Error.new('invalid approval record', 5) unless id.match?(UUID) && approval['id'] == id && data['owners'].key?(approval['ownerId']) && approval['scope'].is_a?(Array) && %w[merge deploy].include?(approval['kind'])
      end
      data['outbox'].each do |id, event|
        WorkspaceCoordination.keys!(event, %w[id ownerId thread text messageId actionContext state options receipt attempts retryAt], %w[payloadDigest error])
        unless id.match?(UUID) && event['id'] == id && data['owners'][event['ownerId']]&.fetch('thread') == event['thread'] &&
               %w[pending retry held delivered].include?(event['state']) && event['messageId'].match?(UUID) && event['text'].is_a?(String) &&
               event['actionContext'] == "coordination:#{event['ownerId']}:#{id}" && event['attempts'].is_a?(Integer) && event['retryAt'].is_a?(Integer)
          raise Error.new('invalid coordination notification', 5)
        end
        unless event['options'].nil? || event['options'].is_a?(String) && event['options'].match?(/\Asha256:[0-9a-f]{64}\z/)
          raise Error.new('invalid notification options artifact', 5)
        end
        unless event['receipt'].nil? || event['receipt'].is_a?(String) && event['receipt'].match?(/\Asha256:[0-9a-f]{64}\z/)
          raise Error.new('invalid notification receipt artifact', 5)
        end
        raise Error.new('notification counters exceed their bound', 5) unless event['attempts'].between?(0, 1_000_000_000) && event['retryAt'].between?(0, MAX_COUNTER)
        raise Error.new('notification record exceeds its recovery budget', 5) if JSON.generate(event).bytesize > MAX_RESULT
        if event['payloadDigest'] && event['payloadDigest'] != WorkspaceCoordination.digest(event.slice('thread', 'text', 'messageId', 'actionContext', 'options'))
          raise Error.new('notification payload changed after preparation', 5)
        end
      end
      claims = data['jobs'].values.select { |job| CLAIMED.include?(job['state']) }.flat_map { |job| job['resources'] }
      raise Error.new('overlapping coordination claims', 5) unless claims.uniq == claims
      held = data['reservations'].values.reject { |item| %w[requested released].include?(item['state']) }.map { |item| item['machine'] }
      raise Error.new('overlapping machine reservations', 5) unless held.uniq == held
      data['receipts'].each do |id, receipt|
        WorkspaceCoordination.keys!(receipt, %w[digest result])
        raise Error.new('invalid request receipt', 5) unless id.match?(UUID) && receipt['digest'].match?(/\Asha256:[0-9a-f]{64}\z/) && receipt['result'].is_a?(Hash)
        raise Error.new('request receipt exceeds its recovery budget', 5) if JSON.generate(receipt['result']).bytesize > MAX_RESULT
      end
      data['lifecycles'].each do |id, receipt|
        WorkspaceCoordination.keys!(receipt, %w[workspace slug command owners completed], %w[archiveOperationId])
        unless id.match?(/\A[0-9a-f]{64}\z/) && %w[archive delete revive].include?(receipt['command']) &&
               [true, false].include?(receipt['completed']) && receipt['owners'].is_a?(Array) &&
               receipt['owners'].all? { |binding| binding.keys.sort == %w[id identity] && data['owners'][binding['id']]&.fetch('identity') == binding['identity'] }
          raise Error.new('invalid lifecycle coordination receipt', 5)
        end
      end
      data['frontier'].each_value do |items|
        raise Error.new('invalid machine integration frontier', 5) unless items.is_a?(Array)
        items.each do |item|
          WorkspaceCoordination.keys!(item, %w[repository target head kind])
          raise Error.new('invalid integrated frontier head', 5) unless item['head'].is_a?(String) && item['head'].match?(SHA)
        end
      end
    rescue NoMethodError, KeyError, TypeError => e
      raise Error.new("malformed coordination record: #{e.message}", 5)
    end

    def integration_journals
      Dir[File.join(root, 'integrations', '*.json')].map do |path|
        value = WorkspaceCoordination.private_read(path)
        unless [1, 2].include?(value['schema']) && value['phase'].is_a?(String) && value['id'].is_a?(String) && value['id'].match?(UUID) &&
               File.basename(path) == "#{value['id']}.json"
          raise Error.new('unsupported integration journal requires forward recovery', 5)
        end
        if value['schema'] == 2
          continuation = value['continuation']
          unless value['ownerId'].is_a?(String) && value['ownerId'].match?(UUID) && value['identity'].is_a?(String) &&
                 value['steps'].is_a?(Hash) && value['jobs'].is_a?(Hash) &&
                 continuation.is_a?(Hash) && %w[runnable waiting paused held needs_attention complete].include?(continuation['state']) &&
                 continuation['generation'].is_a?(Integer) && continuation['generation'].between?(0, MAX_COUNTER) &&
                 (continuation['observedRevision'].nil? || (continuation['observedRevision'].is_a?(Integer) && continuation['observedRevision'].between?(0, MAX_COUNTER)))
            raise Error.new('invalid background integration continuation', 5)
          end
          if (handoff = value['handoff'])
            unless handoff.is_a?(Hash) && %w[pending acknowledged supervised canceled].include?(handoff['state']) &&
                   handoff['eventId'].is_a?(String) && handoff['eventId'].match?(UUID) &&
                   handoff['recipient'].is_a?(String) && handoff['recipient'].match?(/\A[0-9A-Za-z-]+\z/) &&
                   handoff['recipient'].bytesize.between?(1, 256) &&
                   %w[generation reminders nextReminderAt leaseUntil].all? { |key| handoff[key].is_a?(Integer) && handoff[key].between?(0, MAX_COUNTER) }
              raise Error.new('invalid integration handoff metadata', 5)
            end
          end
        end
        value
      end
    end

    # Step identities precede admission. Receipts therefore route events even
    # if submission succeeded before its reply/journal update was lost.
    def background_integration(data, owner_id, item_id)
      integration_journals.find do |run|
        next false unless run['schema'] == 2 && run['ownerId'] == owner_id
        next true if run['reservationId'] == item_id
        run.fetch('steps').values.any? do |step|
          result = data.dig('receipts', step['requestId'], 'result')
          result && (result['id'] == item_id || Array(result['jobs']).any? { |job| job['id'] == item_id })
        end
      end
    end

    def background_stopped?(run)
      return false unless run
      marker = File.join(root, 'integrations', "#{run['id']}.pause")
      if File.exist?(marker)
        unless WorkspaceCoordination.private_read(marker) == {'id' => run['ownerId'], 'identity' => run['identity']}
          raise Error.new('background pause belongs to another owner', 4)
        end
        return true
      end
      %w[paused held needs_attention complete].include?(run.dig('continuation', 'state'))
    end

    def integration_summary(run)
      result = run.slice('id', 'ownerId', 'phase', 'reason', 'continuation', 'outcome', 'handoff')
      if run.dig('continuation', 'state') != 'held' && File.exist?(File.join(root, 'integrations', "#{run['id']}.pause")) && background_stopped?(run)
        result['continuation'] = result['continuation'].merge('state' => 'paused')
      end
      result
    end

    def request(data, owner_id, action, payload)
      WorkspaceCoordination.keys!(payload, %w[schema requestId], payload.keys - %w[schema requestId])
      raise Error, 'request needs schema 1 and a lowercase UUID requestId' unless payload['schema'] == 1 && payload['requestId'].is_a?(String) && payload['requestId'].match?(UUID)
      key = payload.fetch('requestId')
      fingerprint = WorkspaceCoordination.digest([owner_id, action, payload])
      if (receipt = data['receipts'][key])
        raise Error.new('requestId was reused for different input', 3) unless receipt.fetch('digest') == fingerprint
        return receipt.fetch('result')
      end
      recovery = %w[cancel finish].include?(Array(action).first)
      if recovery && JSON.generate(payload).bytesize > MAX_RESULT
        raise Error, 'completion/cancellation request exceeds its recovery budget'
      end
      result = yield
      raise Error, 'request receipt exceeds its recovery budget' if JSON.generate(result).bytesize > MAX_RESULT
      data['receipts'][key] = {'digest' => fingerprint, 'result' => result}
      admission_capacity!(data) unless recovery
      result
    end

    # Reserve every retained class that an accepted operation may still add.
    # Large immutable native options live in checksummed artifacts, not here.
    def recovery_growth(data)
      bytes = ->(value) { JSON.generate(value).bytesize }
      pending = data['jobs'].values.reject { |job| TERMINAL.include?(job['state']) }
      envelopes = data['outbox'].values.reject { |event| event['state'] == 'delivered' }.sum { |event| MAX_RESULT - bytes.call(event) }
      future_events = []
      growth = pending.sum do |job|
        owner = data['owners'].fetch(job['ownerId'])
        future_events.concat(JOB_EVENT_STATES.map { |state| WorkspaceCoordination.event_id(owner, job, state) }) if owner['thread']
        amount = [MAX_RESULT - bytes.call(job['result']), 0].max + [MAX_RESULT - bytes.call(job['reason']), 0].max + MAX_RESULT + 256 + 1024
        if job['reservationId']
          existing = data['reservations'].fetch(job['reservationId'])['history'].find { |item| item['attempt'] == job['attempt'] }
          skeleton = {'attempt' => 'a' * 36, 'kind' => job['kind'], 'artifact' => job.dig('snapshot', 'operation', 'preparedArtifact'),
            'coverage' => job.dig('snapshot', 'operation', 'coverage'), 'coverageContracts' => job['coverageContracts'],
            'observed' => nil, 'outcome' => 'unknown', 'dispositions' => []}
          amount += [bytes.call(skeleton) + 2 * MAX_RESULT + 256 - (existing ? bytes.call(existing) : 0), 0].max
        end
        amount
      end
      data['reservations'].each_value do |reservation|
        next if reservation['state'] == 'released'
        owner = data['owners'].fetch(reservation['ownerId'])
        future_events.concat(%w[held released].map { |state| WorkspaceCoordination.event_id(owner, reservation, state) }) if owner['thread']
        history = reservation['history'] + pending.select { |job| job['reservationId'] == reservation['id'] && !reservation['history'].any? { |item| item['attempt'] == job['attempt'] } }.map do |job|
          {'coverage' => job.dig('snapshot', 'operation', 'coverage'), 'coverageContracts' => job['coverageContracts']}
        end
        coverage = history.flat_map { |item| item.fetch('coverage') }
        contracts = history.flat_map { |item| item.fetch('coverageContracts') }.uniq.reject { |item| item['owned'] }
        unowned = contracts.flat_map do |contract|
          coverage.select { |item| item['repository'] == contract['repository'] }.map do |item|
            contract.slice('repository', 'target', 'owned').merge('head' => item['revision'], 'remoteHead' => 'a' * 40)
          end
        end
        proofs = 2 * MAX_RESULT + bytes.call(unowned)
        growth += MAX_RESULT + 3 * proofs + MAX_RESULT + 256 + 1024
      end
      future_events.concat(claim_notification_events(data))
      background = integration_journals.count { |run| run['schema'] == 2 && run.dig('continuation', 'state') != 'complete' }
      growth + envelopes + background * (2 * MAX_RESULT + 128) + (future_events.uniq - data['outbox'].keys).length * (MAX_RESULT + 64)
    end

    def admission_capacity!(data, extra: 0)
      prospective = JSON.generate(data.merge('revision' => data['revision'] + 1)).bytesize + 1 + extra
      if data['receipts'].length > 16_384 || prospective + recovery_growth(data) > MAX_LEDGER - RECOVERY_RESERVE
        raise Error, 'coordination admission capacity exhausted'
      end
    end

    def sequence(data)
      value = data.fetch('nextSequence')
      data['nextSequence'] = value + 1
      value
    end

    def session_blockers(workspace, slug)
      data = read
      owners = data['owners'].values.select { |owner| owner['workspace'] == workspace && owner['slug'] == slug }
      ids = owners.map { |owner| owner.fetch('id') }
      jobs = data['jobs'].values.select { |job| ids.include?(job['ownerId']) && !TERMINAL.include?(job['state']) }
      reservations = data['reservations'].values.select { |item| ids.include?(item['ownerId']) && item['state'] != 'released' }
      deliveries = data['outbox'].values.select { |item| ids.include?(item['ownerId']) && item['state'] != 'delivered' }
      resource_blocks = claims['claims'].values.select { |item| ids.include?(item['ownerId']) && CLAIM_ACTIVE.include?(item['state']) }.map { |item| "resource claim #{item['id']} (#{item['state']})" }
      resource_blocks + jobs.map { |item| "operation #{item['id']} (#{item['state']})" } +
        reservations.map { |item| "machine #{item['machine']} (#{item['state']})" } +
        deliveries.map { |item| "notification #{item['id']} (#{item['state']})" } +
        integration_journals.filter_map { |run| "background integration #{run['id']} (#{run.dig('continuation', 'state')})" if ids.include?(run['ownerId']) && run['schema'] == 2 && run.dig('continuation', 'state') != 'complete' } +
        owners.filter_map { |owner| "coordination #{owner['fence']['command']} intent" if owner['fence'] && !owner['fence']['completed'] }
    end

    def fence(workspace, slug, command, owner_ids: nil)
      transaction do |data|
        owners = data['owners'].values.select { |owner| owner['workspace'] == workspace && owner['slug'] == slug && (!owner_ids || owner_ids.include?(owner['id'])) }
        fence_owners!(data, owners, command)
      end
    end

    def fence_owners!(data, owners, command)
      require_idle_owners!(data, owners.map { |owner| owner['id'] })
      owners.each do |owner|
        if owner['fence'] && owner['fence']['command'] != command && !owner['fence']['completed']
          raise Error.new("coordination lifecycle intent requires #{owner['fence']['command']}", 5)
        end
        jobs = data['jobs'].values.select { |job| job['ownerId'] == owner['id'] }
        integration_journals.each do |run|
          next unless run['schema'] == 2 && run['ownerId'] == owner['id'] && run['phase'] != 'complete'
          # A separate marker survives a preparing worker's journal publication.
          # Revival preserves the hold until explicit background resumption.
          WorkspaceCoordination.atomic(File.join(root, 'integrations', "#{run['id']}.pause"), owner.slice('id', 'identity'))
        end
        owner['fence'] = {'id' => SecureRandom.uuid, 'command' => command, 'completed' => false, 'operationId' => nil} unless owner['fence'] && owner['fence']['command'] == command
        jobs.each do |job|
          next if TERMINAL.include?(job['state'])
          job['state'] = 'canceled'
          job['revision'] += 1
        end
        data['reservations'].each_value do |reservation|
          next unless reservation['ownerId'] == owner['id'] && reservation['state'] != 'released'
          if command == 'archive'
            reservation['state'] = 'blocked_recovery' unless reservation['state'] == 'requested'
          else
            reservation['state'] = reservation['history'].empty? ? 'released' : 'blocked_recovery'
          end
          reservation['revision'] += 1
        end
        data['outbox'].each_value do |event|
          event['state'] = 'held' if event['ownerId'] == owner['id'] && event['state'] != 'delivered'
        end
      end
    end

    # Ordinary lifecycle journals are the recovery authority. This private
    # receipt binds their exact operation to retained owner incarnations.
    def lifecycle_owners(data, workspace, slug)
      root = [File.join(workspace, 'work', slug), File.join(workspace, 'archive', slug)].find { |path| File.directory?(path) }
      return [] unless root && File.file?(File.join(root, 'portal.yml'))
      stat = File.lstat(root)
      manifest = YAML.safe_load(File.binread(File.join(root, 'portal.yml')), permitted_classes: [], aliases: false)
      owners = data['owners'].values.select do |owner|
        owner['workspace'] == workspace && owner['slug'] == slug && owner['identity'] ==
          WorkspaceCoordination.digest([workspace, owner['workspaceName'], slug, stat.dev, stat.ino,
            manifest.dig('codex', 'thread_id'), WorkspaceCoordination.creation_identity(manifest)])
      end
      if block_given?
        # The established session-observation-v1 contract also binds browser
        # caches to this exact directory/root. Use the same stat and manifest
        # that proved ownership, before another incarnation can replace them.
        observation = Digest::SHA256.hexdigest(JSON.generate(['session-observation-v1', workspace, slug,
          stat.dev, stat.ino, manifest.dig('codex', 'thread_id')]))
        owners.each { |owner| yield owner, observation }
      end
      owners
    end

    def require_lifecycle_idle!(workspace, slug)
      data = read
      ids = lifecycle_owners(data, workspace, slug).map { |owner| owner['id'] }
      require_idle_owners!(data, ids)
    end

    def require_idle_owners!(data, ids)
      claim_lifecycle_idle!(ids)
      active = data['jobs'].values.find { |job| ids.include?(job['ownerId']) && CLAIMED.include?(job['state']) }
      raise Error.new("lifecycle waits for coordinated operation #{active['id']}", 5) if active
      uncertain = data['reservations'].values.any? do |reservation|
        ids.include?(reservation['ownerId']) && reservation['state'] != 'released' &&
          reservation['history'].any? { |record| record['outcome'] == 'unknown' }
      end
      raise Error.new('lifecycle waits for uncertain deployment recovery', 5) if uncertain
    end

    def require_lifecycle_capacity!(workspace, slug, command)
      data = read
      owners = lifecycle_owners(data, workspace, slug)
      return if owners.empty?
      receipt = {'workspace' => workspace, 'slug' => slug, 'command' => command,
        'owners' => owners.map { |owner| owner.slice('id', 'identity') }, 'completed' => false,
        'archiveOperationId' => 'a' * 64}
      # Early refusal avoids expensive preparation. Publication itself must
      # occur inside the locked binding after its actual admission check.
      admission_capacity!(data, extra: JSON.generate(receipt).bytesize + 128 + owners.length * 256)
    end

    def bind_lifecycle(workspace, slug, command, operation_id)
      transaction do |data|
        saved = data['lifecycles'][operation_id]
        if saved
          unless saved.slice('workspace', 'slug', 'command') == {'workspace' => workspace, 'slug' => slug, 'command' => command}
            raise Error.new('lifecycle receipt identity changed', 5)
          end
          yield if block_given?
          next
        end
        owners = lifecycle_owners(data, workspace, slug)
        if owners.empty?
          yield if block_given?
          next
        end
        fence_owners!(data, owners, command)
        owners.each do |owner|
          bound = owner.fetch('fence')
          raise Error.new('lifecycle fence belongs to another operation', 5) if bound['operationId'] && bound['operationId'] != operation_id
          bound['operationId'] = operation_id
        end
        data['lifecycles'][operation_id] = {'workspace' => workspace, 'slug' => slug, 'command' => command,
          'owners' => owners.map { |owner| owner.slice('id', 'identity') }, 'completed' => false}
        admission_capacity!(data)
        yield if block_given?
      end
    end

    def bind_revival(workspace, slug, operation_id:, archive_operation_id: nil)
      transaction do |data|
        saved = data['lifecycles'][operation_id]
        if saved
          unless saved.slice('workspace', 'slug', 'command') == {'workspace' => workspace, 'slug' => slug, 'command' => 'revive'}
            raise Error.new('revival receipt identity changed', 5)
          end
          yield if block_given?
          next
        end
        unless archive_operation_id
          candidates = lifecycle_owners(data, workspace, slug).filter_map { |owner| owner.dig('fence', 'operationId') if owner.dig('fence', 'command') == 'archive' }.uniq
          raise Error.new('revival has ambiguous archive ownership', 5) if candidates.length > 1
          archive_operation_id = candidates.first
        end
        archive = data['lifecycles'][archive_operation_id]
        unless archive
          yield if block_given?
          next
        end
        unless archive['completed'] && archive.slice('workspace', 'slug', 'command') == {'workspace' => workspace, 'slug' => slug, 'command' => 'archive'}
          raise Error.new('revival archive identity changed', 5)
        end
        data['lifecycles'][operation_id] = {'workspace' => workspace, 'slug' => slug, 'command' => 'revive',
          'owners' => archive.fetch('owners'), 'archiveOperationId' => archive_operation_id, 'completed' => false}
        admission_capacity!(data)
        yield if block_given?
      end
    end

    def lifecycle_completed(workspace, slug, command, operation_id:)
      transaction do |data|
        receipt = data['lifecycles'][operation_id]
        next unless receipt # lifecycle begun before coordination existed
        unless receipt.slice('workspace', 'slug', 'command') == {'workspace' => workspace, 'slug' => slug, 'command' => command}
          raise Error.new('lifecycle completion identity changed', 5)
        end
        # A completed archive closes deployment review too. Rechecking this
        # on retry also releases holds left by a predecessor executor.
        receipt['owners'].each do |binding|
          owner = data['owners'].fetch(binding['id'])
          unless owner['identity'] == binding['identity'] && owner.dig('fence', 'operationId') == operation_id
            next if receipt['completed']

            raise Error.new('lifecycle completion owner changed', 5)
          end
          if command == 'archive'
            require_idle_owners!(data, [owner['id']])
            reservations = data['reservations'].values.select { |item| item['ownerId'] == owner['id'] && item['state'] != 'released' }
            reservations.each do |reservation|
              reservation['state'] = 'released'
              reservation['revision'] += 1
            end
          end
          owner['fence']['completed'] = true
        end
        receipt['completed'] = true
      end
    end

    def revived(workspace, slug, operation_id:, archive_operation_id: nil)
      bind_revival(workspace, slug, operation_id:, archive_operation_id:)
      transaction do |data|
        saved = data['lifecycles'][operation_id]
        next unless saved
        unless saved.slice('workspace', 'slug', 'command') == {'workspace' => workspace, 'slug' => slug, 'command' => 'revive'}
          raise Error.new('revival receipt identity changed', 5)
        end
        next if saved['completed']
        saved['owners'].each do |binding|
          owner = data['owners'].fetch(binding['id'])
          unless owner['identity'] == binding['identity'] && owner.dig('fence', 'operationId') == saved['archiveOperationId'] && owner.dig('fence', 'completed')
            raise Error.new('revival owner incarnation changed', 5)
          end
          owner['fence'] = nil
          data['outbox'].each_value { |event| event['state'] = 'pending' if event['ownerId'] == owner['id'] && event['state'] == 'held' }
        end
        saved['completed'] = true
      end
    end

    def require_compatible!(package)
      require_claim_capability!(package)
      return unless File.exist?(File.join(root, 'ledger.json'))
      data = read
      path = File.join(package, 'share/dev-workspace/coordination-contract.json')
      unless File.file?(path) && WorkspaceCoordination.parse(File.binread(path)) == CONTRACT
        raise Error.new('target package cannot preserve coordination state; recover forward with a compatible package', 5)
      end
      runs = integration_journals
      needed = runs.select { |run| run['schema'] == 2 || run['phase'] != 'complete' }.map { |run| run['schema'] }.max
      if needed
        metadata_path = File.join(package, 'share/dev-workspace/package.json')
        metadata = File.file?(metadata_path) && WorkspaceCoordination.parse(File.binread(metadata_path))
        capability = metadata && metadata['integration']
        # Keep the legacy descriptor exact for predecessors with pending manual
        # runs. Additional readers are advertised outside strict package.json.
        schemas = [1]
        contract_path = File.join(package, 'share/dev-workspace/integration-contract.json')
        if File.file?(contract_path)
          contract = WorkspaceCoordination.parse(File.binread(contract_path))
          versions = contract.is_a?(Hash) && contract['journalSchemas']
          valid = contract.is_a?(Hash) && contract.keys.sort == INTEGRATION_CONTRACT.keys.sort &&
            contract['schema'] == 1 && contract['certificationSchema'] == 1 &&
            versions.is_a?(Array) && versions.include?(1) && versions.uniq == versions &&
            versions.all? { |version| version.is_a?(Integer) && version.positive? }
          schemas = valid ? versions : []
        end
        unless capability == {'journalSchema' => 1, 'certificationSchema' => 1} && schemas.include?(needed)
          raise Error.new('candidate cannot resume retained integration journals', 5)
        end
      end
      catalog = Catalog.new(package)
      data['reservations'].each_value do |reservation|
        next if reservation['state'] == 'released'
        target = catalog.machine(reservation.fetch('target'))
        raise Error.new('candidate changes retained machine identity', 5) unless target['id'] == reservation['machine']
        catalog.adapter(target.fetch('adapter'))
      end
      data['jobs'].each_value do |job|
        next if TERMINAL.include?(job['state'])
        snapshot = job.fetch('snapshot')
        if job['kind'] == 'machine_activate'
          target = catalog.machine(snapshot.fetch('operation').fetch('target'))
          raise Error.new('candidate lacks retained adapter protocol', 5) unless catalog.adapter(target.fetch('adapter')).fetch('protocol') == 1
          artifact_path = File.join(root, 'sources', snapshot.dig('operation', 'preparedArtifact').delete_prefix('sha256:'), 'artifact.json')
          artifact = WorkspaceCoordination.private_read(artifact_path, limit: MAX_REQUEST)
          unless artifact.dig('machineConfig', 'name') == target['selector'] && artifact['configurationRepository'] == target['configurationRepository']
            raise Error.new('candidate changes retained deployment mapping', 5)
          end
        end
      end
      # Retained deployed artifacts are needed by later recovery, even after
      # their job succeeds. Check them through the candidate adapter's protocol.
      artifacts = data['reservations'].values.reject { |item| item['state'] == 'released' }.flat_map do |reservation|
        reservation['history'].select { |item| item['kind'] == 'machine_activate' }.map { |item| [reservation['target'], item['artifact']] }
      end
      artifacts += data['jobs'].values.select { |job| job['kind'] == 'machine_activate' && !TERMINAL.include?(job['state']) }.map { |job| [job.dig('snapshot', 'operation', 'target'), job.dig('snapshot', 'operation', 'preparedArtifact')] }
      artifacts.uniq.each do |target, id|
        machine = catalog.machine(target)
        artifact = WorkspaceCoordination.private_read(File.join(root, 'sources', id.delete_prefix('sha256:'), 'artifact.json'), limit: MAX_REQUEST)
        request = {'schema' => 1, 'operation' => 'transition-check', 'target' => target, 'machine' => machine, 'artifact' => artifact}
        output, error, status, overflow = Command.run([catalog.adapter(machine.fetch('adapter')).fetch('command')], input: JSON.generate(request))
        unless status.success? && !overflow && WorkspaceCoordination.parse(output) == {'schema' => 1, 'compatible' => true}
          raise Error.new("candidate adapter cannot retain deployment artifact: #{WorkspaceCoordination.diagnostic(error)}", 5)
        end
      end
    end
  end

  class Catalog
    attr_reader :data, :digest
    def initialize(package, data: nil, fixtures: false)
      installed = File.join(package, 'share/dev-workspace/coordination.json')
      source = File.join(package, 'nix/coordination-empty.json')
      payload = if data
        JSON.generate(data)
      elsif File.file?(installed) || File.file?(source)
        File.binread(File.file?(installed) ? installed : source, MAX_REQUEST + 1)
      else
        JSON.generate('schema' => 1, 'adapters' => {}, 'machines' => {}, 'repositories' => {}, 'profileMachine' => nil)
      end
      @data = WorkspaceCoordination.parse(payload)
      WorkspaceCoordination.keys!(@data, %w[schema adapters machines repositories profileMachine])
      raise Error, 'invalid coordination catalog schema' unless @data['schema'] == 1
      raise Error, 'invalid catalog maps' unless %w[adapters machines repositories].all? { |key| @data[key].is_a?(Hash) }
      @digest = "sha256:#{Digest::SHA256.hexdigest(payload)}"
      @fixtures = fixtures
      aliases = {}
      @data.fetch('machines').each do |name, machine|
        WorkspaceCoordination.keys!(machine, %w[id aliases adapter configurationRepository selector])
        raise Error, 'invalid machine ID' unless machine['id'].is_a?(String) && machine['id'].match?(/\A[a-zA-Z0-9:_-]{1,128}\z/)
        raise Error, 'invalid machine aliases' unless machine['aliases'].is_a?(Array) && machine['aliases'].all? { |item| item.is_a?(String) && !item.empty? && item.bytesize <= 256 } && machine['aliases'].uniq == machine['aliases']
        raise Error, 'machine configuration repository is absent from catalog' unless @data['repositories'].key?(machine['configurationRepository'])
        raise Error, 'invalid exact machine selector' unless machine['selector'].is_a?(String) && machine['selector'].match?(%r{\A[A-Za-z0-9_.:/-]+\z})
        ([name, machine['id']] + machine.fetch('aliases')).each do |key|
          raise Error, 'ambiguous machine alias' if aliases.key?(key) && aliases[key] != name
          aliases[key] = name
        end
        adapter(machine.fetch('adapter'))
      end
      @aliases = aliases
      if @data['profileMachine'] && !@aliases.key?(@data['profileMachine'])
        raise Error, 'profile machine is not in the coordination catalog'
      end
      @data.fetch('repositories').each_value do |repository|
        WorkspaceCoordination.keys!(repository, %w[remote], %w[inputPaths defaultTarget])
        WorkspaceCoordination.remote_identity(repository.fetch('remote'), fixtures:)
        if repository['defaultTarget'] && !repository['defaultTarget'].match?(%r{\Arefs/heads/[A-Za-z0-9][A-Za-z0-9_./-]*\z})
          raise Error, 'invalid configured default target'
        end
      end
    rescue Errno::ENOENT => e
      raise Error, "coordination catalog is missing: #{e.message}"
    end

    def machine(name)
      data.fetch('machines').fetch(@aliases.fetch(name) { raise Error, "unknown coordinated machine: #{name}" })
    end

    def adapter(name)
      value = data.fetch('adapters').fetch(name) { raise Error, "unknown coordination adapter: #{name}" }
      WorkspaceCoordination.keys!(value, %w[protocol command])
      unless value['protocol'] == 1 && value['command'].is_a?(String) &&
             (@fixtures || value['command'].start_with?('/nix/store/')) && File.executable?(value['command'])
        raise Error, 'coordination adapter must be a protocol-1 immutable executable'
      end
      value
    end

    def repository(name)
      data.fetch('repositories').fetch(name) { raise Error, "repository is absent from coordination catalog: #{name}" }
    end
  end

  # A local identity proof crosses the ledger transaction boundary. Git commands
  # run only during resolution; admission rechecks bounded local files instead.
  class RepositoryIdentity
    attr_reader :path, :common, :identity, :registration

    def self.registration(owner, name, internal_workspace: false)
      root = File.join(owner.fetch('workspace'), 'work', owner.fetch('slug'))
      manifest_path = File.join(root, 'portal.yml')
      payload = File.binread(manifest_path, MAX_REQUEST + 1)
      raise Error.new('repository registration exceeds its size bound', 4) if payload.bytesize > MAX_REQUEST
      manifest = YAML.safe_load(payload, permitted_classes: [], aliases: false)
      entries = manifest.is_a?(Hash) && manifest['repositories']
      unless manifest.is_a?(Hash) && manifest['slug'] == owner['slug'] && entries.is_a?(Array) &&
             entries.all? { |item| item.is_a?(Hash) && %w[name project].all? { |key| item[key].is_a?(String) && item[key].match?(PART) } }
        raise Error.new('invalid repository registration; check session repository names and projects', 4)
      end
      if entries.map { |item| item['name'] }.uniq.length != entries.length
        raise Error.new('ambiguous repository registration; remove duplicate repository names', 4)
      end
      stat = File.stat(root)
      retained = WorkspaceCoordination.digest([owner.fetch('workspace'), owner.fetch('workspaceName'), owner.fetch('slug'), stat.dev, stat.ino,
        owner['thread'], WorkspaceCoordination.creation_identity(manifest)])
      raise Error.new('repository registration belongs to a changed session incarnation', 4) unless retained == owner['identity']
      reg = entries.find { |item| item['name'] == name }
      return nil if !reg && internal_workspace && name == 'workspace'
      raise Error.new("repository is not registered to this session: #{name}", 4) unless reg
      unless (name == 'workspace') == (reg['project'] == 'workspace') && reg['default_branch'].is_a?(String) &&
             !reg['default_branch'].empty? && reg['default_branch'].bytesize <= 1024 && !reg['default_branch'].match?(/[[:cntrl:]]/) &&
             (!reg.key?('github') || reg['github'].is_a?(String) && reg['github'].match?(%r{\A[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\z}))
        raise Error.new("invalid repository registration for #{name}; check project, default_branch and github", 4)
      end
      reg
    rescue Errno::ENOENT, Psych::Exception, TypeError, KeyError
      raise Error.new('repository registration is unavailable or invalid; check the current session manifest', 4)
    end

    def initialize(owner, name, catalog:, env: ENV, fixtures: false, internal_workspace: false)
      @owner, @name, @env, @fixtures = owner, name, env.to_h, fixtures
      @registration = self.class.registration(owner, name, internal_workspace:)
      @path = name == 'workspace' ? owner.fetch('workspace') : File.join(owner.fetch('workspace'), 'repos', "#{registration.fetch('project')}.git")
      @common = File.realpath(git('rev-parse', '--path-format=absolute', '--git-common-dir').strip)
      @real_path = File.realpath(path)
      @proof = local_proof
      if name == 'workspace'
        unless File.realpath(git('rev-parse', '--show-toplevel').strip) == File.realpath(owner.fetch('workspace'))
          raise Error.new('registered workspace must be its canonical Git root', 4)
        end
      elsif git('rev-parse', '--is-bare-repository').strip != 'true' || common != @real_path
        raise Error.new("registered repository #{name} needs its canonical bare clone in repos/<project>.git", 4)
      end
      if registration
        branch = registration.fetch('default_branch')
        git('check-ref-format', "refs/heads/#{branch}")
        raise Error.new("invalid registered default_branch for #{name}", 4) if branch == 'HEAD' || branch.start_with?('-')
      end
      # Includes and global configuration cannot replace the canonical clone's
      # direct origin. Its exact local config is covered by the admission proof.
      origins = git('config', '--local', '--no-includes', '--get-all', 'remote.origin.url').lines.map(&:strip)
      raise Error.new("registered repository #{name} needs exactly one canonical SSH origin", 4) unless origins.length == 1
      @identity = WorkspaceCoordination.remote_identity(origins.first, fixtures:)
      if registration && registration['github'] && identity != "github.com/#{registration['github'].downcase.delete_suffix('.git')}"
        raise Error.new("registered github identity for #{name} differs from its canonical origin", 4)
      end
      configured = catalog.data.fetch('repositories')[name]
      if configured && identity != WorkspaceCoordination.remote_identity(configured.fetch('remote'), fixtures:)
        raise Error.new("coordination catalog remote for #{name} differs from its registered canonical origin", 4)
      end
      unless File.realpath(git('rev-parse', '--path-format=absolute', '--git-common-dir').strip) == common
        raise Error.new('repository mapping changed during resolution; try again', 3)
      end
      recheck!
    rescue SystemCallError
      raise Error.new("canonical clone for registered repository #{name} is unavailable; check repos/<project>.git", 4)
    end

    def values
      [path, common, identity, registration]
    end

    def resources
      ["git:#{common}", "repository:#{identity}"]
    end

    def recheck!
      raise Error.new('repository registration or local Git identity changed; try again', 3) unless local_proof == @proof
    rescue SystemCallError
      raise Error.new('repository registration or local Git identity changed; try again', 3)
    end

    private

    def git(*args)
      # Read only local metadata, never fetch or invoke a credential helper.
      output, _error, status, overflow = Command.run(['git', '-C', path, *args], env: @env, limit: MAX_REQUEST)
      unless status.success? && !overflow
        raise Error.new("could not verify registered repository #{@name}; check its canonical clone, origin and default_branch", 4)
      end
      output
    end

    def local_proof
      paths = [File.join(@owner.fetch('workspace'), 'work', @owner.fetch('slug'), 'portal.yml'), File.join(common, 'config')]
      paths << File.join(path, '.git') if @name == 'workspace'
      files = paths.map do |file|
        stat = File.stat(file)
        if stat.directory?
          [File.realpath(file), stat.dev, stat.ino]
        else
          bytes = File.binread(file, MAX_REQUEST + 1)
          raise Error.new('repository identity metadata exceeds its size bound', 4) if bytes.bytesize > MAX_REQUEST
          [File.realpath(file), stat.dev, stat.ino, stat.size, stat.mtime, stat.ctime, Digest::SHA256.hexdigest(bytes)]
        end
      end
      [File.realpath(path), File.realpath(common), files]
    end
  end

  class Engine
    def validate_obligations!(owner, values)
      raise Error, 'integration obligations must be a unique list' unless values.is_a?(Array) && values.uniq == values
      values.each do |item|
        WorkspaceCoordination.keys!(item, %w[repository target])
        reg = registration(owner, item.fetch('repository'))
        raise Error, 'integration target differs from registered default branch' unless item['target'] == "refs/heads/#{reg.fetch('default_branch')}"
      end
    end

    def validate_approval!(owner, request)
      scope = request.fetch('scope')
      raise Error, 'approval scope must be a nonempty unique list' unless scope.is_a?(Array) && !scope.empty? && scope.uniq == scope
      if request['kind'] == 'merge'
        validate_obligations!(owner, scope)
      else
        scope.each do |item|
          WorkspaceCoordination.keys!(item, %w[machine actions reboot])
          raise Error, 'approval must name a canonical machine ID' unless catalog.machine(item.fetch('machine'))['id'] == item['machine']
          unless item['reboot'] == false && item['actions'].is_a?(Array) && !item['actions'].empty? && (item['actions'] - %w[switch profile_activate]).empty? && item['actions'].uniq == item['actions']
            raise Error, 'only no-reboot switch and profile activation approval are supported'
          end
        end
      end
      unless request['reviewedSnapshot'].is_a?(String) && request['reviewedSnapshot'].match?(/\Asha256:[0-9a-f]{64}\z/)
        raise Error, 'approval must bind a reviewed snapshot digest'
      end
      if request.key?('iterationScope') && (!request['iterationScope'].is_a?(String) || request['iterationScope'].strip.empty? || request['iterationScope'].bytesize > 4096)
        raise Error, 'iteration scope requires the explicitly authorized change scope'
      end
    end

    def notification_payload(owner, event, action)
      options = event && event['options'] && artifact!(event['options'])
      if options
        WorkspaceCoordination.keys!(options, %w[id kind options])
        raise Error.new('notification options artifact has wrong kind', 5) unless options['kind'] == 'notification_options' && options['options'].is_a?(Hash)
      end
      # Status messages tolerate duplicate delivery. Retain the logical event,
      # but do not retry an uncertain native submission forever with the same ID.
      message_id = event && event['messageId']
      if event && action == 'send' && event.fetch('attempts', 0).positive?
        message_id = WorkspaceCoordination.event_id(owner, event, "notification-delivery:#{event.fetch('attempts')}")
      end
      payload = {'schema' => 1, 'action' => action, 'workspace' => owner.fetch('workspace'), 'slug' => owner.fetch('slug'),
        'socket' => owner.fetch('socket'), 'thread' => owner.fetch('thread'), 'stateRoot' => File.dirname(store.root),
        'text' => event && event['text'], 'messageId' => message_id,
        'actionContext' => event && event['actionContext'], 'options' => options && options['options']}
      raise Error, 'notification request exceeds its protocol bound' if JSON.generate(payload).bytesize + 1 > MAX_REQUEST
      payload
    end

    def notification_call(owner, event, action)
      payload = notification_payload(owner, event, action)
      directory = File.join(store.root, 'notifications')
      path = File.join(directory, "#{SecureRandom.uuid}.json")
      WorkspaceCoordination.atomic(path, payload, limit: MAX_REQUEST)
      WorkspaceCoordination.parse(capture(File.join(package, 'bin/workspace-portal'), 'coordination-notify', '--request', path))
    ensure
      File.unlink(path) if path && File.file?(path)
    end

    def deliver_notifications(owner_id: nil)
      current_generation!
      ledger = store.read
      return if ledger['suspended']
      eligible = ledger['outbox'].values.select do |event|
        owner = ledger['owners'].fetch(event['ownerId'])
        %w[pending retry].include?(event['state']) && event['retryAt'] <= Time.now.to_i &&
          owner['workspace'] == workspace && !owner['fence'] && (!owner_id || owner['id'] == owner_id)
      end
      eligible.first(2).each do |event|
        owner = ledger['owners'].fetch(event['ownerId'])
        next unless owner['workspace'] == workspace && !owner['fence'] && (!owner_id || owner['id'] == owner_id)
        begin
          unless event['options']
            response = notification_call(owner, event, 'prepare')
            options = save_artifact('kind' => 'notification_options', 'options' => response.fetch('options'))
            notification_payload(owner, event.merge('options' => options.fetch('id')), 'send')
            store.transaction do |data|
              current = data['outbox'].fetch(event['id'])
              next if data['owners'][owner['id']]['fence'] || current['state'] == 'held'
              current['options'] ||= options.fetch('id')
              current['payloadDigest'] = WorkspaceCoordination.digest(current.slice('thread', 'text', 'messageId', 'actionContext', 'options'))
            end
          end
          event = store.read['outbox'].fetch(event['id'])
          next if event['state'] == 'held'
          response = notification_call(owner, event, 'send')
          receipt = save_artifact('kind' => 'notification_receipt', 'receipt' => response.fetch('receipt'))
          store.transaction do |data|
            current = data['outbox'].fetch(event['id'])
            current['receipt'], current['state'], current['error'] = receipt.fetch('id'), 'delivered', nil
          end
        rescue Error => e
          store.transaction do |data|
            current = data['outbox'].fetch(event['id'])
            next if current['state'] == 'held'
              current['attempts'] = [current['attempts'] + 1, 1_000_000_000].min
            current['state'] = 'retry'
              current['error'] = WorkspaceCoordination.diagnostic(e.message)
            current['retryAt'] = Time.now.to_i + [1, 2, 5, 15, 60][[current['attempts'] - 1, 4].min]
          end
        end
      end
    end

    attr_reader :store, :catalog, :package, :profile, :workspace, :entry
    def initialize(state:, package:, profile:, workspace:, entry:, env: ENV, fixtures: false, catalog: nil)
      @store = Store.new(state)
      @package = File.realpath(package)
      @profile = File.expand_path(profile)
      @workspace = File.realpath(workspace)
      @entry = entry
      @env = env.to_h
      @fixtures = fixtures
      @catalog = catalog || Catalog.new(@package, fixtures:)
      @profile_token = DevWorkspaceProfileIdentity.token(@profile)
    end

    def capture(*argv, cwd: nil, input: nil, limit: MAX_REQUEST, digest: false)
      output, error, status, overflow = Command.run(argv, env: @env, cwd:, input:, limit:, digest:, log: @diagnostics)
      raise Error.new("#{File.basename(argv.first)} failed: #{WorkspaceCoordination.diagnostic(error)}", 75) unless status.success?
      raise Error.new('command protocol output exceeds its bound; complete evidence is required', 5) if overflow
      output
    end

    def diagnostics=(value)
      @diagnostics = value
    end

    def current_generation?
      @fixtures || (File.symlink?(profile) && File.realpath(profile) == package && DevWorkspaceProfileIdentity.token(profile) == @profile_token) ||
        (@bootstrap_token && DevWorkspaceProfileIdentity.token(profile) == @bootstrap_token)
    end

    def current_generation!
      unless current_generation?
        raise Error.new('coordinator belongs to a superseded package generation; rerun the stable command', 3)
      end
    end

    def owner(slug, bootstrap: false)
      value = verified_owner_tracking(slug, bootstrap:)
      store.transaction do |data|
        existing = data['owners'].values.find { |item| item['identity'] == value['identity'] }
        next existing if existing
        data['owners'][value['id']] = value
        store.admission_capacity!(data)
        value
      end
    end

    # Delivery may finish after tracking completion, but cannot register a new
    # owner or admit ordinary operations for a completed session.
    def notification_owner(owner)
      current_generation!
      data = store.read
      raise Error.new('coordination is suspended', 4) if data['suspended']
      verified = verified_owner_tracking(owner.fetch('slug'), completed: true)
      retained = data['owners'][owner.fetch('id')]
      unless retained && !retained['fence'] && !retained['bootstrap'] && retained['identity'] == verified['identity'] &&
             retained.slice('workspace', 'workspaceName', 'slug', 'thread', 'socket') == verified.slice('workspace', 'workspaceName', 'slug', 'thread', 'socket')
        raise Error.new('notification owner changed or has lifecycle recovery', 4)
      end
      retained
    end

    def verified_owner_tracking(slug, bootstrap: false, completed: false)
      raise Error, 'invalid session slug' unless slug.match?(PART)
      root = File.join(workspace, 'work', slug)
      stat = File.lstat(root)
      raise Error.new('session tracking is not an ordinary directory', 4) unless stat.directory?
      lifecycle = completed ? /\A---\s*\nlifecycle: (?:active|complete)\s*\n---/ : /\A---\s*\nlifecycle: active\s*\n---/
      raise Error.new('coordination requires an eligible session lifecycle', 4) unless File.binread(File.join(root, 'state.md'), MAX_REQUEST).match?(lifecycle)
      manifest = YAML.safe_load(File.binread(File.join(root, 'portal.yml'), 8 * 1024 * 1024 + 1), permitted_classes: [], aliases: false)
      unless manifest.is_a?(Hash) && manifest['slug'] == slug && manifest['repositories'].is_a?(Array) &&
             (!manifest['creation'] || manifest['creation']['state'] == 'ready')
        raise Error.new('coordination requires ready, explicit session tracking', 4)
      end
      thread = manifest.dig('codex', 'thread_id')
      unless bootstrap || (thread.is_a?(String) && !thread.empty? && manifest.dig('codex', 'socket_path') == entry.fetch('socket'))
        raise Error.new('automatic coordination requires the exact retained session conversation', 4)
      end
      raise Error.new('bootstrap owner must be threadless', 4) if bootstrap && thread
      unless bootstrap || @fixtures
        notification_call(value = {'workspace' => workspace, 'slug' => slug, 'socket' => entry.fetch('socket'), 'thread' => thread}, nil, 'verify')
      end
      identity = WorkspaceCoordination.digest([workspace, entry.fetch('name'), slug, stat.dev, stat.ino, thread, WorkspaceCoordination.creation_identity(manifest)])
      {'id' => SecureRandom.uuid, 'identity' => identity, 'workspace' => workspace,
               'workspaceName' => entry.fetch('name'), 'slug' => slug, 'thread' => thread,
               'socket' => entry.fetch('socket'), 'portalUrl' => "#{entry.fetch('portalUrl')}/#{slug}/",
               'bootstrap' => bootstrap, 'fence' => nil}
    rescue Errno::ENOENT, Psych::Exception => e
      raise Error.new("coordination owner cannot be verified: #{e.message}", 4)
    end

    def registration(owner, name)
      RepositoryIdentity.registration(owner, name)
    end

    def existing_bootstrap_owner(slug, id)
      raise Error.new('invalid retained bootstrap owner', 4) unless id.is_a?(String) && id.match?(UUID)
      retained = store.read['owners'][id]
      unless retained && retained['bootstrap'] && retained['thread'].nil? && retained['slug'] == slug &&
             retained['workspace'] == workspace && retained['workspaceName'] == entry['name'] && retained['fence'].nil?
        raise Error.new('bootstrap owner does not match active maintenance scope', 4)
      end
      # Verify the same active incarnation without creating or adopting an owner.
      root = File.join(workspace, 'work', slug)
      stat = File.lstat(root)
      manifest = YAML.safe_load(File.binread(File.join(root, 'portal.yml')), permitted_classes: [], aliases: false)
      raise Error.new('bootstrap maintenance requires active session lifecycle', 4) unless File.binread(File.join(root, 'state.md'), MAX_REQUEST).match?(/\A---\s*\nlifecycle: active\s*\n---/)
      identity = WorkspaceCoordination.digest([workspace, entry.fetch('name'), slug, stat.dev, stat.ino, nil, WorkspaceCoordination.creation_identity(manifest)])
      unless stat.directory? && manifest['slug'] == slug && manifest['repositories'].is_a?(Array) && manifest.dig('codex', 'thread_id').nil? &&
             (!manifest['creation'] || manifest.dig('creation', 'state') == 'ready') && retained['identity'] == identity
        raise Error.new('bootstrap tracking incarnation changed', 4)
      end
      retained
    rescue Errno::ENOENT, Psych::Exception => e
      raise Error.new("retained bootstrap owner cannot be verified: #{e.message}", 4)
    end

    def repository_identity(owner, name)
      RepositoryIdentity.new(owner, name, catalog: catalog, env: @env, fixtures: @fixtures)
    end

    def repository(owner, name)
      repository_identity(owner, name).values
    end

    def snapshot(owner, value, kind)
      WorkspaceCoordination.keys!(value, %w[catalogDigest preparedGeneration evidence operation], %w[id changeScope])
      raise Error.new('prepared catalog changed; prepare again', 3) unless value.fetch('catalogDigest') == catalog.digest
      WorkspaceCoordination.keys!(value.fetch('preparedGeneration'), %w[package profileToken])
      raise Error.new('prepared package generation changed', 3) unless value.dig('preparedGeneration', 'package') == package && value.dig('preparedGeneration', 'profileToken') == DevWorkspaceProfileIdentity.token(profile)
      raise Error, 'snapshot evidence must be a nonempty array' unless value['evidence'].is_a?(Array) && !value['evidence'].empty?
      value.fetch('evidence').each do |evidence|
        WorkspaceCoordination.keys!(evidence, %w[kind path sha256])
        path = File.expand_path(evidence.fetch('path'), workspace)
        unless path.start_with?(File.join(workspace, 'work', owner.fetch('slug')) + '/') && File.file?(path) && !File.symlink?(path) &&
               Digest::SHA256.file(path).hexdigest == evidence.fetch('sha256')
          raise Error.new('prepared verification evidence changed or belongs to another session', 3)
        end
      end
      if value.key?('changeScope') && (!value['changeScope'].is_a?(String) || value['changeScope'].strip.empty? || value['changeScope'].bytesize > 4096)
        raise Error, 'changeScope must name the explicitly approved iteration scope'
      end
      operation = value.fetch('operation')
      resources = case kind
      when 'merge'
        WorkspaceCoordination.keys!(operation, %w[repository target featureBranch featureHead expectedRemoteHead expectedLocalTargetHead comparisonBase patchDigest sourceRef targetCheckout hookArtifact])
        unless operation['sourceRef'].is_a?(String) && operation['sourceRef'].match?(%r{\Arefs/dev-workspace-coordination/[0-9a-f-]{36}\z}) &&
               %w[featureHead expectedRemoteHead comparisonBase].all? { |key| operation[key].is_a?(String) && operation[key].match?(SHA) }
          raise Error, 'invalid retained merge identity'
        end
        path, common, identity, reg = repository(owner, operation.fetch('repository'))
        unless operation['target'] == "refs/heads/#{reg.fetch('default_branch')}" && operation['featureBranch'] == reg.fetch('branch') &&
               %w[featureHead expectedRemoteHead comparisonBase].all? { |key| operation[key].match?(SHA) }
          raise Error.new('merge snapshot differs from its registered feature/target', 4)
        end
        head = capture('git', '-C', path, 'rev-parse', "#{operation.fetch('featureBranch')}^{commit}").strip
        retained = capture('git', '-C', path, 'rev-parse', "#{operation.fetch('sourceRef')}^{commit}").strip
        raise Error.new('prepared feature head changed', 3) unless head == operation['featureHead'] && retained == head
        patch = capture('git', '-C', path, 'diff', operation.fetch('comparisonBase'), head, digest: true)
        raise Error.new('prepared patch digest changed', 3) unless patch == operation['patchDigest']
        unless operation['sourceRef'].match?(%r{\Arefs/dev-workspace-coordination/[0-9a-f-]{36}\z}) &&
               (operation['expectedLocalTargetHead'].nil? || operation['expectedLocalTargetHead'].match?(SHA))
          raise Error, 'invalid retained merge identity'
        end
        if operation['repository'] == 'workspace'
          raise Error, 'shared workspace cannot supply a target checkout' unless operation['targetCheckout'].nil?
        else
          checkout = operation.fetch('targetCheckout')
          unless checkout.is_a?(String) && File.realpath(checkout).start_with?(File.join(store.root, 'sources') + '/') &&
                 File.realpath(capture('git', '-C', checkout, 'rev-parse', '--path-format=absolute', '--git-common-dir').strip) == common &&
                 capture('git', '-C', checkout, 'rev-parse', 'HEAD').strip == operation['expectedRemoteHead'] &&
                 capture('git', '-C', checkout, 'status', '--porcelain').empty?
            raise Error.new('prepared integration checkout changed', 3)
          end
        end
        hooks = artifact!(operation['hookArtifact']) if operation['hookArtifact']
        if hooks && (hooks['kind'] != 'merge_hooks' || hooks['featureHead'] != operation['featureHead'] || hooks['repository'] != operation['repository'])
          raise Error.new('prepared hook environment differs from merge source', 3)
        end
        ["repo:#{identity}:#{operation['target']}", "git:#{common}"]
      when 'machine_activate'
        WorkspaceCoordination.keys!(operation, %w[target preparedArtifact expectedLive coverage configurationHead])
        machine = catalog.machine(operation.fetch('target'))
        artifact = artifact!(operation.fetch('preparedArtifact'))
        unless artifact['kind'] == 'machine' && artifact['machine'] == machine['id'] && artifact['target'] == operation['target'] &&
               artifact['configurationRevision'] == operation['configurationHead'] && artifact['coverage'] == operation['coverage'] &&
               artifact['expectedLive'] == operation['expectedLive']
          raise Error.new('machine snapshot differs from sealed preparation', 3)
        end
        ["machine:#{machine.fetch('id')}"]
      when 'profile_activate'
        WorkspaceCoordination.keys!(operation, %w[preparedArtifact candidatePackage expectedProfileToken coverage])
        raise Error, 'coordinated profile activation requires a configured machine' unless catalog.data['profileMachine']
        artifact = artifact!(operation.fetch('preparedArtifact'))
        unless artifact['candidatePackage'] == operation['candidatePackage'] && artifact['profileToken'] == operation['expectedProfileToken'] && artifact['coverage'] == operation['coverage']
          raise Error.new('profile snapshot differs from sealed preparation', 3)
        end
        ["machine:#{catalog.machine(catalog.data['profileMachine']).fetch('id')}", "profile:#{Digest::SHA256.hexdigest(profile)}"]
      else raise Error, 'only merge, machine_activate and profile_activate are supported'
      end
      if kind != 'merge'
        raise Error, 'deployment coverage must be a unique list' unless operation['coverage'].is_a?(Array) && operation['coverage'].uniq == operation['coverage']
        operation['coverage'].each do |item|
          WorkspaceCoordination.keys!(item, %w[repository revision])
          catalog.repository(item.fetch('repository'))
          raise Error, 'deployment coverage requires exact source commits' unless item['revision'].is_a?(String) && item['revision'].match?(SHA)
        end
      end
      clean = value.reject { |key, _| key == 'id' }
      id = WorkspaceCoordination.digest(clean)
      raise Error, 'snapshot digest does not match its bytes' if value['id'] && value['id'] != id
      [clean.merge('id' => id), resources]
    end

    def artifact!(id)
      raise Error, 'invalid prepared artifact ID' unless id.is_a?(String) && id.match?(/\Asha256:[0-9a-f]{64}\z/)
      path = File.join(store.root, 'sources', id.delete_prefix('sha256:'), 'artifact.json')
      data = WorkspaceCoordination.private_read(path, limit: MAX_REQUEST)
      raise Error.new('prepared artifact digest changed', 5) unless WorkspaceCoordination.digest(data.reject { |key, _| key == 'id' }) == id
      data
    end

    def prepared_snapshot!(owner, id)
      raise Error, 'invalid prepared snapshot ID' unless id.is_a?(String) && id.match?(/\Asha256:[0-9a-f]{64}\z/)
      saved = WorkspaceCoordination.private_read(File.join(store.root, 'sources', id.delete_prefix('sha256:'), 'snapshot.json'), limit: MAX_REQUEST)
      WorkspaceCoordination.keys!(saved, %w[schema ownerId kind snapshot])
      unless saved['schema'] == 1 && saved['ownerId'] == owner['id'] && saved.dig('snapshot', 'id') == id &&
             WorkspaceCoordination.digest(saved['snapshot'].reject { |key, _| key == 'id' }) == id
        raise Error.new('prepared snapshot belongs to another owner or changed', 4)
      end
      saved
    end

    def reply(item, owner)
      {'schema' => 1, 'id' => item.fetch('id'), 'revision' => item.fetch('revision', 1),
       'state' => item.fetch('state', 'approved'), 'reason' => item['reason'], 'portalUrl' => owner.fetch('portalUrl')}
    end

    def event(data, owner, item, state, reason = nil)
      return unless owner['thread']
      return if item['kind'] != 'integration' && store.background_integration(data, owner['id'], item['id'])
      # One semantic event per snapshot/state. Repeated resource churn and
      # reconciliation retain the first logical event identity.
      id = WorkspaceCoordination.event_id(owner, item, state)
      return if data['outbox'].key?(id)
      text = if item['kind'] == 'integration'
        state == 'complete' ? "Workspace coordination: the operations in integration #{item['id']} have finished. Review the results." :
          "Workspace coordination: integration #{item['id']} needs your attention. #{WorkspaceCoordination.diagnostic(reason && reason['message'])[0, 512]}"
      else
        "Workspace coordination: #{item['kind'] || 'machine reservation'} #{item['id']} is #{state}." + (reason ? " #{reason['code']}." : '')
      end
      if item['kind'] == 'integration'
        text += "\nRead integration-status for this run. Acknowledge its handoff with integration-ack using a schema-1 request with a new requestId, eventId #{id}, and generation #{item.dig('snapshot', 'id')}. Acknowledgement lasts five minutes; renew it while handling the action."
      end
      text += "\n#{owner['portalUrl']}\nRead dev-session coordination status for your session and continue its authorized work. This event supplies no merge or deployment approval."
      raise Error, 'notification text exceeds its envelope budget' if JSON.generate(text).bytesize > 4096
      data['outbox'][id] = {'id' => id, 'ownerId' => owner['id'], 'thread' => owner['thread'],
        'text' => text, 'messageId' => SecureRandom.uuid, 'actionContext' => "coordination:#{owner['id']}:#{id}",
        'state' => 'pending', 'options' => nil, 'receipt' => nil, 'attempts' => 0, 'retryAt' => 0}
    end

    def transition(data, job, state, reason = nil)
      return if job['state'] == state && job['reason'] == reason
      job['state'], job['reason'] = state, reason
      job['revision'] += 1
      if JOB_EVENT_STATES.include?(state)
        event(data, data['owners'].fetch(job['ownerId']), job, state, reason)
      end
    end

    def mutate(owner, action, request, job_id: nil)
      current_generation!
      if store.read['receipts'].key?(request['requestId'])
        return store.transaction { |data| store.request(data, owner['id'], [action, job_id], request) { raise Error, 'request receipt disappeared' } }
      end
      preparation_capacity! unless %w[cancel].include?(action)
      Retention.new(store).require_fresh!(request.dig('snapshot', 'id')) if %w[submit refresh].include?(action)
      prepared = snapshot(owner, request.fetch('snapshot'), request.fetch('kind')) if action == 'submit'
      if action == 'refresh'
        job = store.read['jobs'].fetch(job_id) { raise Error, 'unknown coordinated operation' }
        prepared = snapshot(owner, request.fetch('snapshot'), job.fetch('kind'))
      end
      if action == 'approve'
        Retention.new(store).require_fresh!(request['reviewedSnapshot'])
        saved = prepared_snapshot!(owner, request['reviewedSnapshot'])
        raise Error.new('approval kind differs from its prepared operation', 4) unless (saved['kind'] == 'merge') == (request['kind'] == 'merge')
        snapshot(owner, saved.fetch('snapshot'), saved.fetch('kind'))
      end
      if action == 'reserve'
        manifest = YAML.safe_load(File.binread(File.join(workspace, 'work', owner['slug'], 'portal.yml')), permitted_classes: [], aliases: false)
        obligations = request.fetch('integrationObligations') + manifest.fetch('repositories').map { |reg| {'repository' => reg['name'], 'target' => "refs/heads/#{reg['default_branch']}"} }
      end
      contracts = prepared && prepared[0].dig('operation', 'coverage') && coverage_contracts(owner, prepared[0].dig('operation', 'coverage'))
      store.transaction do |data|
        current = data['owners'].fetch(owner.fetch('id'))
        raise Error.new('session is fenced by a lifecycle operation', 5) if current['fence']
        store.request(data, owner['id'], [action, job_id], request) do
          result = case action
          when 'approve'
            WorkspaceCoordination.keys!(request, %w[schema requestId kind scope source reviewedSnapshot], %w[iterationScope])
            WorkspaceCoordination.keys!(request.fetch('source'), %w[kind reference quote])
            unless %w[merge deploy].include?(request['kind']) && request.dig('source', 'kind') == 'user_instruction' &&
                   %w[reference quote].all? { |key| request['source'][key].is_a?(String) && !request['source'][key].strip.empty? }
              raise Error.new('approval requires actual user-instruction evidence and exact scope', 4)
            end
            validate_approval!(owner, request)
            id = SecureRandom.uuid
            value = request.merge('id' => id, 'ownerId' => owner['id'], 'revision' => 1)
            data['approvals'][id] = value
            reply(value, owner)
          when 'reserve'
            WorkspaceCoordination.keys!(request, %w[schema requestId target mode approvalId integrationObligations])
            machine = catalog.machine(request.fetch('target'))
            raise Error, 'invalid reservation mode' unless %w[deploy_first integrated].include?(request['mode'])
            approval = data['approvals'][request['approvalId']]
            unless approval && approval['ownerId'] == owner['id'] && approval['kind'] == 'deploy'
              raise Error.new('machine reservation requires the owner deployment approval', 4)
            end
            unless approval['scope'].any? { |item| item['machine'] == machine['id'] }
              raise Error.new('deployment approval does not cover this machine', 4)
            end
            validate_obligations!(owner, request.fetch('integrationObligations'))
            existing = data['reservations'].values.find { |item| item['ownerId'] == owner['id'] && item['machine'] == machine['id'] && item['state'] != 'released' }
            if existing
              reply(existing, owner)
            else
              id = SecureRandom.uuid
              value = {'id' => id, 'ownerId' => owner['id'], 'machine' => machine['id'], 'target' => request['target'],
                'state' => 'requested', 'revision' => 1, 'sequence' => store.sequence(data), 'mode' => request['mode'],
                'approvalId' => request['approvalId'], 'integrationObligations' => obligations.uniq, 'history' => []}
              data['reservations'][id] = value
              reply(value, owner)
            end
          when 'submit'
            create_job(data, owner, request, prepared, contracts)
          when 'cancel', 'refresh'
            value = data['jobs'].fetch(job_id) { raise Error, 'unknown coordinated operation' }
            raise Error.new('operation belongs to another owner', 4) unless value['ownerId'] == owner['id']
            raise Error.new('operation revision changed', 3) unless request['expectedRevision'] == value['revision']
            raise Error.new('admitted or uncertain operation must be reconciled before cancellation/refresh', 5) if CLAIMED.include?(value['state'])
            raise Error, 'terminal operation cannot be refreshed or canceled' if TERMINAL.include?(value['state'])
            if action == 'refresh'
              WorkspaceCoordination.keys!(request, %w[schema requestId expectedRevision snapshot approvalId])
              value['history'] << {'snapshot' => value['snapshot'], 'result' => value['result']}
              value['snapshot'], value['resources'], value['approvalId'] = prepared[0], prepared[1], request['approvalId']
              value['coverageContracts'] = contracts || []
              value['readySequence'] = nil
              transition(data, value, 'waiting_approval')
            else
              WorkspaceCoordination.keys!(request, %w[schema requestId expectedRevision reason])
              transition(data, value, 'canceled', {'code' => 'owner_canceled', 'message' => request['reason']})
            end
            reply(value, owner)
          else raise Error, "unsupported coordination mutation: #{action}"
          end
          result.merge('requestId' => request.fetch('requestId'))
        end
      end
    end

    def create_job(data, owner, request, prepared, contracts)
      WorkspaceCoordination.keys!(request, %w[schema requestId kind dependsOn approvalId reservationId snapshot])
      raise Error, 'coordination job capacity exhausted' if data['jobs'].values.count { |job| !TERMINAL.include?(job['state']) } >= 4096
      dependencies = request.fetch('dependsOn')
      unless dependencies.is_a?(Array) && dependencies.uniq == dependencies && dependencies.all? { |id| data['jobs'][id]&.fetch('ownerId') == owner['id'] }
        raise Error, 'dependencies must be unique, earlier operations of this owner'
    end
      if request['kind'] != 'merge'
        reservation = data['reservations'][request['reservationId']]
        unless reservation && reservation['ownerId'] == owner['id'] && reservation['state'] != 'released' && prepared[1].include?("machine:#{reservation['machine']}")
          raise Error.new('deployment requires the owner reservation for this machine', 4)
        end
      elsif request['reservationId']
        raise Error, 'merge-only operations do not acquire machine reservations'
    end
      id = SecureRandom.uuid
      value = {'id' => id, 'ownerId' => owner['id'], 'kind' => request['kind'], 'dependsOn' => dependencies,
        'approvalId' => request['approvalId'], 'reservationId' => request['reservationId'], 'snapshot' => prepared[0],
        'resources' => prepared[1], 'state' => 'waiting_approval', 'reason' => nil, 'revision' => 1,
        'sequence' => store.sequence(data), 'readySequence' => nil, 'attempt' => nil, 'result' => nil, 'history' => []}
      value['coverageContracts'] = contracts || []
      data['jobs'][id] = value
      reply(value, owner)
    end


    def submit_batch(owner, request)
      current_generation!
      if store.read['receipts'].key?(request['requestId'])
        return store.transaction { |data| store.request(data, owner['id'], ['submit-batch', nil], request) { raise Error, 'batch receipt disappeared' } }
      end
      WorkspaceCoordination.keys!(request, %w[schema requestId jobs])
      jobs = request.fetch('jobs')
      raise Error, 'batch requires a nonempty list of jobs' unless jobs.is_a?(Array) && !jobs.empty?
      keys = jobs.map { |job| job.fetch('key') }
      unless keys.uniq == keys && keys.all? { |key| key.is_a?(String) && key.match?(PART) && !key.match?(UUID) }
        raise Error, 'batch job keys must be unique names, not UUIDs'
      end
      preparation_capacity!
      prepared = jobs.map do |job|
        WorkspaceCoordination.keys!(job, %w[key kind dependsOn approvalId reservationId snapshot])
        Retention.new(store).require_fresh!(job.dig('snapshot', 'id'))
        snapshot(owner, job.fetch('snapshot'), job.fetch('kind'))
      end
      contracts = prepared.map { |value| value[0].dig('operation', 'coverage') && coverage_contracts(owner, value[0].dig('operation', 'coverage')) }
      store.transaction do |data|
        raise Error.new('session is fenced by a lifecycle operation', 5) if data['owners'].fetch(owner['id'])['fence']
        store.request(data, owner['id'], ['submit-batch', nil], request) do
          ids = {}
          replies = jobs.each_with_index.map do |job, index|
            raise Error, 'batch dependencies must be a list' unless job['dependsOn'].is_a?(Array)
            dependencies = job['dependsOn'].map { |dependency| ids.fetch(dependency, dependency) }
            item = job.reject { |key, _| key == 'key' }.merge('schema' => 1,
              'requestId' => request['requestId'], 'dependsOn' => dependencies)
            value = create_job(data, owner, item, prepared[index], contracts[index])
            ids[job['key']] = value['id']
            value.merge('key' => job['key'])
          end
          {'schema' => 1, 'jobs' => replies, 'requestId' => request['requestId']}
        end
      end
    end

    def approved?(data, job)
      approval = data['approvals'][job['approvalId']]
      return false unless approval && approval['ownerId'] == job['ownerId']
      operation = job.dig('snapshot', 'operation')
      if job['kind'] == 'merge'
        return false unless approval['kind'] == 'merge' && approval['scope'].include?({'repository' => operation['repository'], 'target' => operation['target']})
      else
        reservation = data['reservations'].fetch(job.fetch('reservationId'))
        return false unless approval['kind'] == 'deploy' && approval['scope'].any? { |scope| scope['machine'] == reservation['machine'] && scope['reboot'] == false && scope['actions'].include?(job['kind'] == 'profile_activate' ? 'profile_activate' : 'switch') }
      end
      approval['reviewedSnapshot'] == job.dig('snapshot', 'id') ||
        (approval['iterationScope'].is_a?(String) && !approval['iterationScope'].empty? &&
         approval['iterationScope'] == job.dig('snapshot', 'changeScope'))
    end

    def claim(owner_id: nil)
      current_generation!
      store.lock('preparation', mode: File::LOCK_SH, nonblock: true) { claim_ready(owner_id: owner_id) } || []
    end

    def claim_ready(owner_id:)
      store.transaction do |data|
        next [] if data['suspended']
        active = data['jobs'].values.select { |job| CLAIMED.include?(job['state']) }
        busy = Engine.normalized_resources(active.flat_map { |job| job.fetch('resources') }) + store.held_claim_resources
        # Unknown effects keep their resource claims, but stopped mutation
        # workers must not consume unrelated execution capacity forever.
        slots = [4 - active.count { |job| job['state'] != 'reconciling' }, 0].max
        candidates = data['jobs'].values.reject { |job| TERMINAL.include?(job['state']) || CLAIMED.include?(job['state']) || job['state'] == 'needs_refresh' }.sort_by { |job| [job['sequence'], job['id']] }
        candidates.each do |job|
          owner = data['owners'].fetch(job['ownerId'])
          if store.background_stopped?(store.background_integration(data, owner['id'], job['id']))
            transition(data, job, 'waiting_dependency', {'code' => 'integration_paused'})
            next
          end
          if owner['fence'] || !approved?(data, job)
            transition(data, job, 'waiting_approval', {'code' => 'approval_required'})
            next
          end
          unless job['dependsOn'].all? { |id| data['jobs'].fetch(id)['state'] == 'succeeded' }
            transition(data, job, 'waiting_dependency', {'code' => 'dependency_pending'})
            next
          end
          if job['reservationId']
            reservation = data['reservations'].fetch(job['reservationId'])
            held = data['reservations'].values.find { |item| item['machine'] == reservation['machine'] && item['id'] != reservation['id'] && !%w[requested released].include?(item['state']) }
            earlier = data['reservations'].values.find { |item| item['machine'] == reservation['machine'] && item['state'] == 'requested' && item['sequence'] < reservation['sequence'] && candidates.any? { |candidate| candidate['reservationId'] == item['id'] && !store.background_stopped?(store.background_integration(data, candidate['ownerId'], candidate['id'])) && approved?(data, candidate) && candidate['dependsOn'].all? { |id| data['jobs'][id]['state'] == 'succeeded' } } }
            if held || earlier || reservation['state'] == 'blocked_recovery'
              blocker = held || earlier || reservation
              transition(data, job, 'waiting_resource', {'code' => 'machine_owned', 'ownerId' => blocker['ownerId'], 'portalUrl' => data['owners'][blocker['ownerId']]['portalUrl'], 'resource' => "machine:#{reservation['machine']}"})
              next
            end
          end
          if !(Engine.normalized_resources(job['resources']) & store.held_claim_resources).empty?
            transition(data, job, 'waiting_resource', {'code' => 'resource_claim_held'})
            next
          end
          job['readySequence'] ||= store.sequence(data)
          blocker = active.find { |item| !(item['resources'] & job['resources']).empty? }
          if blocker
            transition(data, job, 'waiting_resource', {'code' => 'execution_busy', 'ownerId' => blocker['ownerId'], 'portalUrl' => data['owners'][blocker['ownerId']]['portalUrl']})
            next
          end
          transition(data, job, 'ready')
        end
        claimed = []
        candidates.select { |job| job['state'] == 'ready' }.sort_by { |job| [job['readySequence'], job['id']] }.each do |job|
          next if slots.zero? || !(Engine.normalized_resources(job['resources']) & busy).empty?
          unless (!owner_id || job['ownerId'] == owner_id) && data['owners'][job['ownerId']]['workspace'] == workspace
            # Another owner's admission runs under its own outer locks. Preserve
            # its earlier place for shared resources without claiming for it.
            busy.concat(Engine.normalized_resources(job['resources']))
            next
          end
          attempt = SecureRandom.uuid
          prospective = job.merge('attempt' => attempt, 'state' => 'claimed', 'reason' => nil, 'revision' => job['revision'] + 1)
          if JSON.generate(worker_request(prospective, data['owners'].fetch(job['ownerId']))).bytesize + 1 > MAX_REQUEST
            transition(data, job, 'needs_refresh', {'code' => 'worker_request_too_large', 'message' => 'prepared operation exceeds the worker request limit; reduce the snapshot and refresh'})
            next
          end
          if job['reservationId']
            reservation = data['reservations'].fetch(job['reservationId'])
            if reservation['state'] == 'requested'
              reservation['state'] = 'held'
              reservation['revision'] += 1
              event(data, data['owners'][job['ownerId']], reservation, 'held')
            end
          end
          job['attempt'] = attempt
          transition(data, job, 'claimed')
          busy.concat(Engine.normalized_resources(job['resources']))
          slots -= 1
          claimed << Marshal.load(Marshal.dump(job))
        end
        claimed
      end
    end

    def status(slug = nil, portal: false)
      data = store.read
      owners = data['owners'].values.select { |owner| owner['workspace'] == workspace && (!slug || owner['slug'] == slug) }
      ids = owners.map { |owner| owner['id'] }
      result = {'schema' => 1, 'revision' => data['revision'], 'suspended' => data['suspended'],
       'owners' => owners.map { |owner| owner.slice('id', 'slug', 'portalUrl', 'bootstrap', 'fence') },
       'integrations' => store.integration_journals.select { |run| ids.include?(run['ownerId']) && run['schema'] == 2 }.map { |run| store.integration_summary(run) },
       'jobs' => data['jobs'].values.select { |job| ids.include?(job['ownerId']) },
       'reservations' => data['reservations'].values.select { |item| ids.include?(item['ownerId']) },
       'notifications' => data['outbox'].values.select { |event| ids.include?(event['ownerId']) }.map { |event| event.reject { |key, _| %w[text options].include?(key) } },
       'foreignReservations' => data['reservations'].values.reject { |item| ids.include?(item['ownerId']) || item['state'] == 'released' }.map { |item| item.slice('machine', 'state').merge('portalUrl' => data['owners'][item['ownerId']]['portalUrl']) }}
      claim_state = store.claims['claims'].values
      result['claims'] = claim_state.select { |item| ids.include?(item['ownerId']) }
      claim_state.select { |item| item['state'] == 'held' }.each do |item|
        item['resources'].grep(/^machine:/).each do |resource|
          machine = resource.delete_prefix('machine:')
          if ids.include?(item['ownerId'])
            result['reservations'] << item.slice('id', 'ownerId', 'state', 'revision', 'sequence', 'createdAt', 'updatedAt').merge('machine' => machine, 'mode' => 'claim', 'history' => [])
          else
            result['foreignReservations'] << {'machine' => machine, 'state' => 'held', 'portalUrl' => data['owners'].fetch(item['ownerId'])['portalUrl']}
          end
        end
      end
      if portal
        timing = Timing.new(store.root)
        dates = timing.read
        observations = {}
        owners.map { |owner| owner['slug'] }.uniq.each do |name|
          store.lifecycle_owners(data, workspace, name) { |owner, observation| observations[owner['id']] = observation }
        end
        # A session pane must not attribute retained jobs to a recreated slug.
        if slug
          ids.select! { |id| observations.key?(id) }
          %w[owners jobs reservations notifications integrations claims].each { |kind| result[kind].select! { |item| ids.include?(kind == 'owners' ? item['id'] : item['ownerId']) } }
        end
        result['owners'].each { |owner| owner['observationIdentity'] = observations[owner['id']] }
        machine_details = lambda do |item|
          name = catalog.data.fetch('machines').find { |_key, value| value['id'] == item['machine'] }&.first
          item.slice('id', 'ownerId', 'machine', 'state', 'revision', 'mode').merge(
            'shortName' => name || item['machine'].split(':').last,
            'deployments' => item.fetch('history', []).length,
            'timestamps' => item['mode'] == 'claim' ? {'createdAt' => item['createdAt'], 'heldAt' => item['createdAt'], 'stateChangedAt' => item['updatedAt']} : timing.matching(dates, 'reservations', item))
        end
        held = result['reservations'].reject { |item| %w[requested released].include?(item['state']) }.group_by { |item| item['ownerId'] }
        result['reservationHolders'] = []
        owners.select { |owner| held.key?(owner['id']) }.map { |owner| owner['slug'] }.uniq.each do |name|
          store.lifecycle_owners(data, workspace, name) do |owner, observation|
            next unless held.key?(owner['id'])
            result['reservationHolders'] << owner.slice('slug', 'portalUrl').merge('ownerId' => owner['id'],
              'observationIdentity' => observation,
              'machines' => held.fetch(owner['id']).sort_by { |item| item['machine'] }.map { |item| machine_details.call(item) })
          end
        end
        result['counts'] = result.slice('owners', 'jobs', 'reservations', 'notifications', 'integrations', 'foreignReservations', 'claims').transform_values(&:length)
        result['claims'] = result['claims'].sort_by { |item| [CLAIM_ACTIVE.include?(item['state']) ? 0 : 1, -item['sequence']] }.first(128).map do |item|
          item.slice('id', 'ownerId', 'state', 'revision', 'sequence', 'createdAt', 'updatedAt').merge(
            'label' => item['request']['label'], 'repositories' => item['request']['repositories'].first(8),
            'machines' => item['request']['machines'].first(8))
        end
        result['owners'] = result['owners'].first(128)
        result['jobs'] = result['jobs'].sort_by { |job| [TERMINAL.include?(job['state']) ? 1 : 0, -job['sequence']] }.first(128).map do |job|
          job.slice('id', 'ownerId', 'kind', 'state', 'reason', 'revision', 'sequence', 'readySequence', 'dependsOn', 'reservationId').merge(
            'snapshotId' => job.dig('snapshot', 'id'), 'source' => job.dig('snapshot', 'operation').slice('repository', 'target', 'featureHead', 'configurationHead', 'candidatePackage'),
            'timestamps' => timing.matching(dates, 'jobs', job),
            'result' => job['result'] && job['result'].slice('outcome', 'observed', 'phase', 'error'))
        end
        result['reservations'] = result['reservations'].sort_by { |item| [item['state'] == 'released' ? 1 : 0, -item['sequence']] }.first(128).map { |item| machine_details.call(item) }
        result['notifications'] = result['notifications'].last(128)
        result['integrations'] = result['integrations'].sort_by { |run| run.dig('continuation', 'state') == 'complete' ? 1 : 0 }.first(128)
        result['foreignReservations'] = result['foreignReservations'].first(128)
      end
      result
    end
  end
end

require_relative 'workspace-coordination-claims'
