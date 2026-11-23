# frozen_string_literal: true

require 'digest'
require 'json'
require 'pathname'

# Shared strict readers and legacy-writer fences. Schema-4 creation authoring
# stays with the exact receipt owner; compatibility checks never grant dispatch.
module WorkspaceSessionBackends
  class Error < StandardError; end
  class StrictObject < Hash
    def []=(key, value)
      raise Error, "duplicate backend evidence key: #{key}" if key?(key)
      super
    end
  end

  FIELDS = %w[sessionBackendBindingSchema sessionBackendBindingPolicy runtimeAuthoritySchema piCreationSchema].zip([1, 1, 2, 4]).to_h.freeze
  TEAM_FIELDS = {'policy' => 1, 'roster' => 2, 'monitorOperation' => 2, 'communication' => 2, 'progressObserver' => 2, 'utilityReceipt' => 2}.freeze
  WORKFLOW_FIELDS = {'piQuestionsSchema' => 1, 'piPlanSchema' => 1,
    'piToolPolicy' => {'policy' => 1, 'conversationSchema' => 2}.freeze,
    'hostCallbacks' => 1, 'linearLineage' => 1, 'workflowObservations' => 1}.freeze
  WORKFLOW_SOURCES = %w[questions plans tool_policy role_instructions fork].freeze
  BINDING_KEYS = %w[schema backend backend_contract workspace slug session_incarnation creation_receipt_id revision state conversations].freeze
  HEX = /\A[0-9a-f]{64}\z/
  SLUG = /\A[A-Za-z0-9][A-Za-z0-9_-]*\z/
  MEMBER = /\A[a-z][a-z0-9]{0,31}[0-9]+\z/

  module_function

  # Existing web.piUploadThread/piWorkflowNamespace projection. The caller owns
  # the validated terminal capture; native IDs and generation are not key inputs.
  def pi_upload_threads(capture)
    binding = capture.fetch('binding')
    capture.fetch('members').map do |member|
      slot = member.fetch('Slot')
      values = ['pi-workflow-v1', 'pi', binding.fetch('workspace'), binding.fetch('slug'),
                binding.fetch('session_incarnation'), slot.fetch('Member'), slot.fetch('Incarnation')]
      # Match Go encoding/json's HTML and JavaScript separator escaping.
      encoded = JSON.generate(values).gsub(/[<>&\u2028\u2029]/) { |char| format('\\u%04x', char.ord) }
      'pi:pi-workflow-v1:' + Digest::SHA256.hexdigest(encoded)
    end.uniq
  end

  def present?(path)
    File.lstat(path)
    true
  rescue Errno::ENOENT
    false
  end

  def private_directory!(path)
    stat = File.lstat(path)
    unless stat.directory? && stat.uid == Process.euid && (stat.mode & 0o777) == 0o700
      raise Error, "backend evidence directory is not private and owned: #{path}"
    end
  end

  def read!(path, maximum: 64 * 1024, private_state: true)
    stat = File.lstat(path)
    unless stat.file? && stat.size <= maximum &&
      (!private_state || stat.uid == Process.euid && (stat.mode & 0o777) == 0o600)
      raise Error, "backend evidence is not a private bounded regular file: #{path}"
    end
    data = File.binread(path, maximum + 1).force_encoding(Encoding::UTF_8)
    raise Error, "invalid backend evidence encoding or size: #{path}" unless data.valid_encoding? && data.bytesize <= maximum
    value = JSON.parse(data, object_class: StrictObject)
    raise Error, "backend evidence is not an object: #{path}" unless value.is_a?(Hash)
    value
  rescue JSON::ParserError, SystemCallError => e
    raise Error, "cannot read backend evidence #{path}: #{e.message}"
  end

  def scalar?(value)
    value.is_a?(String) && value.valid_encoding? && value.bytesize.between?(1, 4096) && !value.include?("\0")
  end

  def hex?(value) = value.is_a?(String) && HEX.match?(value)
  def member?(value) = scalar?(value) && (value == 'lead' || MEMBER.match?(value))

  def binding_directory(root, workspace)
    File.join(root, 'session-backends', Digest::SHA256.hexdigest(workspace))
  end

  def binding(root, workspace, slug)
    raise Error, 'invalid backend scope' unless scalar?(slug) && SLUG.match?(slug) &&
      scalar?(workspace) && Pathname.new(workspace).absolute? && File.expand_path(workspace) == workspace
    directory = binding_directory(root, workspace)
    return nil unless present?(directory)
    private_directory!(directory)
    path = File.join(directory, "#{slug}.json")
    return nil unless present?(path)
    value = read!(path)
    unless value.keys.sort == BINDING_KEYS.sort && value['schema'].is_a?(Integer) && value['schema'] == 1 &&
      value['backend'] == 'pi' && value['backend_contract'].is_a?(Integer) && value['backend_contract'] == 1 &&
      value['workspace'] == workspace && value['slug'] == slug && hex?(value['session_incarnation']) &&
      scalar?(value['creation_receipt_id']) && value['revision'].is_a?(Integer) && value['revision'].between?(1, 2**64 - 1) &&
      %w[creating ready].include?(value['state']) && value['conversations'].is_a?(Array) && !value['conversations'].empty?
      raise Error, "unsupported or invalid session backend binding: #{path}"
    end
    addresses, native_ids = [], []
    value['conversations'].each do |conversation|
      unless conversation.is_a?(Hash) &&
        (conversation.keys.sort == %w[address incarnation state].sort || conversation.keys.sort == %w[address incarnation state native_id].sort) &&
        member?(conversation['address']) && hex?(conversation['incarnation']) && %w[creating ready].include?(conversation['state']) &&
        (!conversation.key?('native_id') || scalar?(conversation['native_id'])) &&
        (conversation['state'] != 'ready' || scalar?(conversation['native_id']))
        raise Error, "invalid backend conversation: #{path}"
      end
      addresses << conversation['address']
      native_ids << conversation['native_id'] if conversation['state'] == 'ready'
    end
    lead = value['conversations'].find { |conversation| conversation['address'] == 'lead' }
    unless lead && addresses.uniq == addresses && native_ids.uniq == native_ids && (value['state'] != 'ready' || lead['state'] == 'ready')
      raise Error, "conflicting backend conversations: #{path}"
    end
    value
  end

  def binding_slugs(root, workspace)
    directory = binding_directory(root, workspace)
    return [] unless present?(directory)
    private_directory!(directory)
    Dir.children(directory).sort.filter_map do |name|
      next unless name.end_with?('.json')
      slug = name.delete_suffix('.json')
      binding(root, workspace, slug)
      slug
    end
  rescue SystemCallError => e
    raise Error, "cannot inspect session backend state: #{e.message}"
  end

  def creation_paths(root, workspace, slug)
    identifier = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
    [File.join(root, 'portal', identifier, 'creations', "#{slug}.json"),
     File.join(workspace, 'worktrees', '.locks', "#{slug}.creation.json")]
  end

  def refuse_pi_creation!(path)
    return unless present?(path)
    record = read!(path, maximum: 12 * 20_000 + 16_384)
    schema = record['schema']
    unless schema.is_a?(Integer) && [1, 2, 3].include?(schema) && !record.key?('backend') &&
      !(record['request'].is_a?(Hash) && record['request'].key?('backend'))
      raise Error, "Pi/unsupported creation evidence requires full schema-4 validation, unavailable in this package: #{path}"
    end
  end

  INTENT_KEYS = %w[schema workspace slug backend backend_contract receipt_id deletion_history_sha256 session_incarnation conversations goal_sha256 model effort provider provider_selection direct_team initial_input_id sensitive].freeze
  PI_KEYS = %w[backendContract sessionIncarnation conversations provider initialInputId selection].freeze
  POLICY_CATEGORIES = %w[configuration authentication refresh_storage model_unavailable rate_limit overload quota_billing transport structured_policy context_limit text_refusal cancelled unknown].freeze

  def exact!(value, required, optional = [])
    unless value.is_a?(Hash) && (required - value.keys).empty? && (value.keys - required - optional).empty? && value.values.none?(&:nil?)
      raise Error, 'Pi creation object has missing, null or unknown fields'
    end
  end

  def selection!(value, workspace, slug, incarnation, conversations)
    exact!(value, %w[schema catalog_digest policy members])
    raise Error, 'invalid Pi provider selection identity' unless value['schema'].is_a?(Integer) && value['schema'] == 1 && hex?(value['catalog_digest'])
    policy = value['policy']
    exact!(policy, %w[session revision default overrides])
    exact!(policy['session'], %w[workspace slug incarnation])
    unless policy['session'] == {'workspace' => workspace, 'slug' => slug, 'incarnation' => incarnation} && policy['revision'].is_a?(Integer) && policy['revision'] == 1 && policy['overrides'].is_a?(Array)
      raise Error, 'Pi frozen policy session changed'
    end
    validate_selection = lambda do |versioned|
      exact!(versioned, %w[revision selection])
      raise Error, 'invalid initial Pi policy revision' unless versioned['revision'].is_a?(Integer) && versioned['revision'] == 1
      selection = versioned['selection']
      exact!(selection, %w[chain budget fallback_on])
      unless selection['chain'].is_a?(Array) && selection['chain'].length.between?(1, 8) && selection['fallback_on'].is_a?(Array) && selection['fallback_on'].uniq == selection['fallback_on'] && (selection['fallback_on'] - POLICY_CATEGORIES).empty?
        raise Error, 'invalid frozen Pi chain or categories'
      end
      selection['chain'].each do |target|
        exact!(target, %w[provider_id account_ref model_id thinking])
        raise Error, 'invalid frozen Pi target' unless target.values.all? { |item| scalar?(item) }
      end
      budget = selection['budget']
      exact!(budget, %w[max_extra_attempts max_hops])
      unless budget.values.all? { |item| item.is_a?(Integer) && item.between?(0, 7) } && budget['max_hops'] <= budget['max_extra_attempts']
        raise Error, 'invalid frozen Pi fallback budget'
      end
    end
    validate_selection.call(policy['default'])
    override_keys = []
    policy['overrides'].each do |override|
      exact!(override, %w[member value]); exact!(override['member'], %w[address incarnation])
      raise Error, 'Pi override has no reserved member' unless conversations.include?(override['member'])
      override_keys << override['member']; validate_selection.call(override['value'])
    end
    raise Error, 'duplicate Pi override' unless override_keys.uniq == override_keys
    members = value['members']
    unless members.is_a?(Array) && members.length == conversations.length
      raise Error, 'Pi provider selection lost a reserved member'
    end
    members.each_with_index do |member, index|
      exact!(member, %w[address incarnation source policy_digest settings])
      expected = conversations.fetch(index)
      source = override_keys.include?(expected) ? 'member_override' : 'session_default'
      settings = member['settings']
      exact!(settings, %w[provider account_ref model thinking policy_revision revision])
      unless member['address'] == expected['address'] && member['incarnation'] == expected['incarnation'] && member['source'] == source && hex?(member['policy_digest']) && settings['policy_revision'] == member['policy_digest'] && settings['revision'].is_a?(Integer) && settings['revision'] == 1 && settings.values_at('provider', 'account_ref', 'model', 'thinking').all? { |item| scalar?(item) }
        raise Error, 'Pi member provider selection changed'
      end
    end
    value
  end

  def intent!(record, workspace: nil, slug: nil)
    return fork_intent!(record, workspace:, slug:) if record['schema'] == 5
    exact!(record, INTENT_KEYS)
    unless record['schema'].is_a?(Integer) && record['schema'] == 4 && record['backend'] == 'pi' && record['backend_contract'].is_a?(Integer) && record['backend_contract'] == 1 &&
      scalar?(record['workspace']) && Pathname.new(record['workspace']).absolute? && File.expand_path(record['workspace']) == record['workspace'] && scalar?(record['slug']) && SLUG.match?(record['slug']) &&
      (!workspace || record['workspace'] == workspace) && (!slug || record['slug'] == slug) &&
      %w[receipt_id deletion_history_sha256 session_incarnation goal_sha256].all? { |key| hex?(record[key]) } &&
      %w[model effort provider].all? { |key| scalar?(record[key]) } && [true, false].include?(record['sensitive']) &&
      record['initial_input_id'].is_a?(String) && record['initial_input_id'].match?(/\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/)
      raise Error, 'invalid Pi creation intent identity'
    end
    conversations = record['conversations']
    unless conversations.is_a?(Array) && conversations.length.between?(1, 65) && conversations.first.is_a?(Hash) && conversations.first['address'] == 'lead'
      raise Error, 'invalid Pi reserved conversation set'
    end
    conversations.each do |member|
      exact!(member, %w[address incarnation])
      raise Error, 'invalid reserved Pi conversation' unless member?(member['address']) && hex?(member['incarnation'])
    end
    unless conversations.map { |v| v['address'] }.uniq.length == conversations.length && conversations.map { |v| v['incarnation'] }.uniq.length == conversations.length
      raise Error, 'duplicate reserved Pi conversation'
    end
    selection!(record['provider_selection'], record['workspace'], record['slug'], record['session_incarnation'], conversations)
    team = record['direct_team']
    exact!(team, %w[id name description roles catalogDigest teamDigest leadModel leadEffort members], %w[leadInstructions])
    unless %w[id name description leadModel leadEffort].all? { |key| scalar?(team[key]) } && hex?(team['catalogDigest']) && hex?(team['teamDigest']) &&
      team['roles'] == conversations.map { |v| v['address'] } && team['members'].is_a?(Array) && team['members'].length + 1 == conversations.length &&
      team['leadModel'] == record['model'] && team['leadEffort'] == record['effort']
      raise Error, 'Pi direct team does not match creation intent'
    end
    team['members'].each_with_index do |member, index|
      exact!(member, %w[role address model reasoningEffort behavior access], %w[purpose instructions])
      saved = record['provider_selection']['members'].fetch(index + 1)
      unless member['address'] == conversations.fetch(index + 1)['address'] && member['model'] == saved['settings']['model'] && member['reasoningEffort'] == saved['settings']['thinking'] &&
        %w[read_only workspace_write].include?(member['access']) && %w[designer implementer reviewer general].include?(member['behavior']) && member.values.all? { |v| scalar?(v) }
        raise Error, 'Pi retained member settings changed'
      end
    end
    lead = record['provider_selection']['members'].first.fetch('settings')
    unless lead['provider'] == record['provider'] && lead['model'] == record['model'] && lead['thinking'] == record['effort']
      raise Error, 'Pi lead provider selection changed'
    end
    record
  end

  def journal!(record, workspace: nil, slug: nil)
    exact!(record, intent_keys(record) + %w[state native_create_started], %w[native_generation native])
    intent!(record.slice(*intent_keys(record)), workspace:, slug:)
    unless %w[creating ready].include?(record['state']) && [true, false].include?(record['native_create_started'])
      raise Error, 'invalid Pi creation journal state'
    end
    if record['native_create_started']
      raise Error, 'Pi create intent lacks generation' unless hex?(record['native_generation'])
    elsif record.key?('native_generation') || record.key?('native')
      raise Error, 'Pi native result precedes create intent'
    end
    if record.key?('native')
      exact!(record['native'], %w[native_id history_ref])
      raise Error, 'invalid Pi native result' unless record['native'].values.all? { |v| scalar?(v) }
    end
    if record['state'] == 'ready' && (!record['native_create_started'] || !record['native'])
      raise Error, 'Pi ready journal lacks native identity'
    end
    record
  end

  def receipt_intent!(record, workspace: nil, slug: nil)
    return fork_receipt_intent!(record, workspace:, slug:) if record['schema'] == 5
    exact!(record, %w[schema workspace request receiptId deletionHistorySha256 attempt state phase startedAt updatedAt validated goal model effort directTeam pi], %w[error])
    request = record['request']; pi = record['pi']
    exact!(request, %w[kind slug goal backend], %w[sensitive model effort provider team catalogDigest browserNotificationsEnabled])
    exact!(pi, PI_KEYS)
    unless record['schema'].is_a?(Integer) && record['schema'] == 4 && request['kind'] == 'new' && request['backend'] == 'pi' && record['validated'] == true &&
      record['attempt'].is_a?(Integer) && record['attempt'].positive? && %w[running paused failed ready conflict].include?(record['state']) &&
      scalar?(record['phase']) && record['startedAt'].is_a?(String) && record['updatedAt'].is_a?(String) &&
      record['goal'].is_a?(String) && record['goal'].bytesize.between?(1, 20_000) && request['goal'].is_a?(String) && record['goal'] == request['goal'].gsub(/\A[ \t\n\v\f\r\0]+|[ \t\n\v\f\r\0]+\z/, '') &&
      (!request.key?('sensitive') || request['sensitive'] == true) && (!request.key?('browserNotificationsEnabled') || [true, false].include?(request['browserNotificationsEnabled']))
      raise Error, 'invalid Pi accepted creation receipt'
    end
    intent = {'schema' => 4, 'workspace' => record['workspace'], 'slug' => request['slug'], 'backend' => 'pi', 'backend_contract' => pi['backendContract'],
      'receipt_id' => record['receiptId'], 'deletion_history_sha256' => record['deletionHistorySha256'], 'session_incarnation' => pi['sessionIncarnation'], 'conversations' => pi['conversations'],
      'goal_sha256' => Digest::SHA256.hexdigest(record['goal']), 'model' => record['model'], 'effort' => record['effort'], 'provider' => pi['provider'], 'provider_selection' => pi['selection'],
      'direct_team' => record['directTeam'], 'initial_input_id' => pi['initialInputId'], 'sensitive' => request['sensitive'] == true}
    intent!(intent, workspace:, slug:)
    {'model' => 'model', 'effort' => 'effort', 'provider' => 'provider', 'team' => 'id', 'catalogDigest' => 'catalogDigest'}.each do |key, frozen_key|
      next unless request.key?(key)
      expected = %w[team catalogDigest].include?(key) ? intent['direct_team'][frozen_key] : intent[frozen_key]
      raise Error, 'Pi request options changed' unless request[key] == expected
    end
    intent
  end

  def validate_creation_owner!(root, workspace, slug, receipt_id, evidence, attempt: nil)
    receipt_path, = creation_paths(root, workspace, slug)
    expected_evidence = File.join(File.dirname(receipt_path), "#{slug}.#{receipt_id}.complete.json")
    raise Error, 'Pi evidence path does not match receipt' unless evidence == expected_evidence
    receipt = read!(receipt_path, maximum: 12 * 20_000 + 16_384)
    intent = receipt_intent!(receipt, workspace:, slug:)
    unless intent['receipt_id'] == receipt_id && receipt['state'] == 'running' && (!attempt || receipt['attempt'] == attempt)
      raise Error, 'Pi receipt owner changed'
    end
    companion = read!(evidence + '.request', maximum: 1 << 20); intent!(companion, workspace:, slug:)
    raise Error, 'Pi creation records disagree' unless companion == intent
    intent
  end

  def validate_creation_bundle!(root, workspace, slug, receipt_id, evidence)
    intent = validate_creation_owner!(root, workspace, slug, receipt_id, evidence)
    _, journal_path = creation_paths(root, workspace, slug)
    journal = read!(journal_path, maximum: 1 << 20); journal!(journal, workspace:, slug:)
    unless journal.slice(*intent_keys(journal)) == intent
      raise Error, 'Pi creation records disagree'
    end
    selected = binding(root, workspace, slug)
    unless selected && selected['creation_receipt_id'] == receipt_id && selected['session_incarnation'] == intent['session_incarnation'] && selected['conversations'].map { |v| v.slice('address', 'incarnation') } == intent['conversations']
      raise Error, 'Pi durable binding disagrees with creation'
    end
    [intent, journal, selected]
  end

  COMPLETION_KEYS = %w[schema workspace slug backend backendContract receiptId sessionIncarnation bindingRevision conversations nativeId initialInputId initialSubmissionDigest goalSha256 model effort provider providerSelection catalogDigest teamDigest team trackingDevice trackingInode].freeze

  def completion!(record, intent, selected, journal)
    return fork_completion!(record, intent, selected, journal) if intent['schema'] == 5
    exact!(record, COMPLETION_KEYS)
    expected = {'schema' => 4, 'workspace' => intent['workspace'], 'slug' => intent['slug'], 'backend' => 'pi', 'backendContract' => 1,
      'receiptId' => intent['receipt_id'], 'sessionIncarnation' => intent['session_incarnation'], 'conversations' => intent['conversations'],
      'nativeId' => journal.fetch('native').fetch('native_id'), 'initialInputId' => intent['initial_input_id'], 'goalSha256' => intent['goal_sha256'],
      'model' => intent['model'], 'effort' => intent['effort'], 'provider' => intent['provider'], 'providerSelection' => intent['provider_selection'],
      'catalogDigest' => intent['direct_team']['catalogDigest'], 'teamDigest' => intent['direct_team']['teamDigest'], 'team' => intent['direct_team']['id']}
    unless expected.all? { |key, value| record[key] == value } && hex?(record['initialSubmissionDigest']) &&
      record['bindingRevision'].is_a?(Integer) && record['bindingRevision'].between?(1, 2**64 - 1) && (!selected || record['bindingRevision'] <= selected['revision']) &&
      record['trackingDevice'].is_a?(Integer) && record['trackingDevice'] >= 0 && record['trackingInode'].is_a?(Integer) && record['trackingInode'].positive?
      raise Error, 'Pi completion evidence disagrees with retained creation'
    end
    record
  end

  # Observational lifetime proof only. The caller must separately supply exact
  # validated domain references before accepting a retired binding's fence.
  # No current binding/runtime or native readiness is inferred from these files.
  def retained_creation_fence!(root, workspace, slug)
    receipt_path, journal_path = creation_paths(root, workspace, slug)
    journal = read!(journal_path, maximum: 1 << 20)
    journal!(journal, workspace:, slug:)
    raise Error, 'retained Pi dependency requires its ready creation journal' unless journal['state'] == 'ready'
    intent = journal.slice(*intent_keys(journal))
    evidence = File.join(File.dirname(receipt_path), "#{slug}.#{intent.fetch('receipt_id')}.complete.json")
    companion = read!(evidence + '.request', maximum: 1 << 20)
    intent!(companion, workspace:, slug:)
    raise Error, 'retained Pi immutable request changed' unless companion == intent
    if present?(receipt_path)
      receipt = receipt_intent!(read!(receipt_path, maximum: 12 * 20_000 + 16_384), workspace:, slug:)
      raise Error, 'retained Pi cache receipt changed' unless receipt == intent
    end
    selected = binding(root, workspace, slug)
    if selected && (selected['state'] != 'ready' || selected['creation_receipt_id'] != intent['receipt_id'] ||
      selected['session_incarnation'] != intent['session_incarnation'] ||
      selected['conversations'].map { |v| v.slice('address', 'incarnation') } != ready_conversations!(root, intent, selected) ||
      selected['conversations'].first['native_id'] != journal.fetch('native').fetch('native_id'))
      raise Error, 'retained Pi binding and creation fence disagree'
    end
    completion = completion!(read!(evidence, maximum: 12 * 20_000 + 16_384), intent, selected, journal)
    {intent:, journal:, completion:}
  end

  # Package compatibility is a durable shape/cross-record check, not permission
  # to start a worker or dispatch.
  def runtime_authority!(record, selected, workspace, slug)
    exact!(record, %w[schema backend backend_contract workspace slug session_incarnation binding_revision generation state])
    unless record['schema'].is_a?(Integer) && record['schema'] == 2 && record['backend'] == 'pi' && record['backend_contract'].is_a?(Integer) && record['backend_contract'] == 1 &&
      record['workspace'] == workspace && record['slug'] == slug && hex?(record['generation']) && %w[starting ready].include?(record['state']) && selected &&
      record['session_incarnation'] == selected['session_incarnation'] && record['binding_revision'].is_a?(Integer) && record['binding_revision'] == selected['revision']
      raise Error, 'Pi live authority has no matching current durable binding'
    end
    record
  end

  def inspect_pi_state!(root, workspace, slug, retained: false, stopped_creations: nil)
    receipt_path, journal_path = creation_paths(root, workspace, slug)
    receipt = present?(receipt_path) && read!(receipt_path, maximum: 12 * 20_000 + 16_384)
    journal = present?(journal_path) && read!(journal_path, maximum: 1 << 20)
    selected = binding(root, workspace, slug)
    pi_receipt = receipt && [4, 5].include?(receipt['schema'])
    pi_journal = journal && [4, 5].include?(journal['schema'])
    unless selected || pi_receipt || pi_journal
      [receipt_path, journal_path].each { |path| refuse_pi_creation!(path) }
      return false
    end
    if receipt && !pi_receipt || journal && !pi_journal
      raise Error, 'Pi backend conflicts with legacy creation evidence'
    end
    intent = pi_receipt && receipt_intent!(receipt, workspace:, slug:)
    if pi_journal
      journal!(journal, workspace:, slug:)
      frozen = journal.slice(*intent_keys(journal))
      raise Error, 'Pi receipt and journal disagree' if intent && intent != frozen
      intent ||= frozen
    end
    raise Error, "session backend conflict: Pi binding has no retained creation fence: #{slug}" unless intent
    if !selected && retained
      retained_creation_fence!(root, workspace, slug)
      return true
    end
    if selected
      unless selected['creation_receipt_id'] == intent['receipt_id'] && selected['session_incarnation'] == intent['session_incarnation'] && selected['conversations'].map { |v| v.slice('address', 'incarnation') } == ready_conversations!(root, intent, selected)
        raise Error, 'Pi binding and retained creation disagree'
      end
    end
    return true unless pi_journal
    evidence = File.join(File.dirname(receipt_path), "#{slug}.#{intent.fetch('receipt_id')}.complete.json")
    companion = read!(evidence + '.request', maximum: 1 << 20); intent!(companion, workspace:, slug:)
    raise Error, 'Pi immutable companion changed' unless companion == intent
    if journal['state'] == 'ready'
      unless selected && selected['state'] == 'ready' && selected['conversations'].first['state'] == 'ready' && selected['conversations'].first['native_id'] == journal.fetch('native').fetch('native_id')
        raise Error, 'Pi ready journal has no matching ready binding'
      end
      completion!(read!(evidence, maximum: 12 * 20_000 + 16_384), intent, selected, journal)
      yield selected, journal if block_given?
    elsif !receipt
      raise Error, 'Pi creating request lost its accepted receipt'
    elsif journal['native'] && selected && selected['conversations'].first['native_id'] && selected['conversations'].first['native_id'] != journal['native']['native_id']
      raise Error, 'Pi native creation result changed'
    end
    # A profile transition can preserve a settled attempt before native Create.
    # Return the complete validated bundle so the host accounts for its journal
    # and receipt together, and can compare the same records after waiting.
    if stopped_creations && receipt && %w[failed paused].include?(receipt['state']) &&
      journal['state'] == 'creating' && journal['native_create_started'] == false &&
      !journal.key?('native') && !journal.key?('native_generation') &&
      selected && selected['revision'] == 1 && selected['state'] == 'creating' &&
      selected['conversations'].all? { |conversation| conversation['state'] == 'creating' && !conversation.key?('native_id') } &&
      !present?(evidence)
      stopped_creations[[workspace, slug]] = {receipt:, journal:, companion:, binding: selected}
    end
    true
  end

  # The Go reader owns runtime.json. Join its validated current projection to
  # the already validated original journal and current live authority here.
  def current_runtime_projection!(selected, journal, reference, authority)
    scope = {'workspace' => selected.fetch('workspace'), 'slug' => selected.fetch('slug'), 'incarnation' => selected.fetch('session_incarnation')}
    unless reference && reference.fetch('session') == scope && reference.fetch('phase') == 'starting'
      raise Error, 'Pi ready binding has no settled current runtime projection'
    end
    generation = reference.fetch('generation')
    if authority
      runtime_authority!(authority, selected, selected.fetch('workspace'), selected.fetch('slug'))
      unless authority.fetch('state') == 'ready' && authority.fetch('generation') == generation
        raise Error, 'Pi current runtime and ready authority disagree'
      end
    elsif generation != journal.fetch('native_generation')
      raise Error, 'Pi successor runtime requires its ready authority'
    end
  end

  def require_codex!(root, workspace, slug)
    selected = binding(root, workspace, slug)
    paths = creation_paths(root, workspace, slug)
    paths.each { |path| refuse_pi_creation!(path) }
    return unless selected
    if paths.none? { |path| present?(path) }
      raise Error, "session backend conflict: Pi binding has no retained creation fence: #{slug}"
    end
    raise Error, "session backend conflict: Pi binding cannot use a legacy Codex writer: #{slug}"
  rescue SystemCallError => e
    raise Error, "cannot inspect session backend state: #{e.message}"
  end

  def require_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    unless FIELDS.all? { |field, expected| contract[field].is_a?(Integer) && (contract[field] == expected || field == 'piCreationSchema' && contract[field] == 5) }
      raise Error, 'target package cannot preserve Pi backend bindings and creation evidence'
    end
  end

  def require_team_contract!(package, required: 2)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    support = contract['piTeamRecords']
    unless support.is_a?(Hash) && support.keys.sort == TEAM_FIELDS.keys.sort &&
      TEAM_FIELDS.all? { |field, expected| support[field].is_a?(Integer) && (field == 'roster' ? [2, 3, 4, 5].include?(support[field]) && support[field] >= required : support[field] == expected) }
      raise Error, 'target package cannot preserve retained Pi team records'
    end
  end

  def pi_roster(root, workspace, slug)
    id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
    path = File.join(root, 'codex-teams', id, "#{slug}.json")
    present?(path) ? read!(path, maximum: 256 * 1024) : nil
  end

  # Initial creation stays exact. Additional tuples must join their immutable
  # explicit Add provenance; no catalog/default can manufacture missing entries.
  def ready_conversations!(root, intent, selected)
    original = intent.fetch('conversations')
    roster = pi_roster(root, intent.fetch('workspace'), intent.fetch('slug'))
    return original if !roster && selected['conversations'].length == original.length
    unless roster && [2, 3, 4, 5].include?(roster['schema']) && roster['workspace'] == intent['workspace'] && roster['slug'] == intent['slug'] &&
      roster['sessionIncarnation'] == intent['session_incarnation'] && roster['leadIncarnation'] == original.first['incarnation'] && roster['members'].is_a?(Array) &&
      roster['members'].length + 1 == selected['conversations'].length
      raise Error, 'Pi roster and binding reservation counts disagree'
    end
    if intent['schema'] == 5
      raise Error, 'Pi fork roster provenance changed' unless [4, 5].include?(roster['schema']) && roster['forkCreationId'] == intent['receipt_id']
    elsif ![2, 3, 5].include?(roster['schema']) || roster.key?('forkCreationId')
      raise Error, 'Pi original creation requires its original roster format'
    end
    expected = original.dup
    roster['members'].each_with_index do |member, index|
      if index + 1 < original.length
        frozen = intent['schema'] == 5 ? intent['fork']['members'].fetch(index + 1) : nil
        creation_id = frozen ? frozen['creation_id'] : intent['receipt_id']
        unless member.values_at('address', 'incarnation', 'creationId') == original[index + 1].values_at('address', 'incarnation') + [creation_id] && !member.key?('addition')
          raise Error, 'Pi original member tuple changed'
        end
        if frozen
          expected_fork = frozen['origin'] ? {'rootCreationId' => intent['receipt_id'], 'nativeId' => frozen['native_id']} : nil
          raise Error, 'Pi member fork reference changed' unless member['fork'] == expected_fork
          bound = selected['conversations'].fetch(index + 1)
          if frozen['origin'] && (bound['state'] != 'ready' || bound['native_id'] != frozen['native_id']) ||
            frozen['state'] == 'removed' && (bound['state'] != 'creating' || bound.key?('native_id'))
            raise Error, 'Pi fork member binding changed'
          end
        end
        next
      end
      a = member['addition']
      unless [3, 4, 5].include?(roster['schema']) && !member.key?('fork') && a.is_a?(Hash) && a['rootCreationId'] == intent['receipt_id'] && hex?(member['creationId']) && member['creationId'] != intent['receipt_id'] &&
        hex?(member['incarnation']) && member?(member['address']) && a['priorBindingRevision'].is_a?(Integer) && a['priorBindingRevision'].positive? && a['priorBindingRevision'] < selected['revision'] &&
        hex?(a['payloadDigest']) && hex?(a['generation']) && a['requestId'].is_a?(String) && /\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/.match?(a['requestId']) &&
        a['selection'].is_a?(Hash) && a['selection'].values_at('address', 'incarnation') == member.values_at('address', 'incarnation')
        raise Error, 'Pi appended member has no exact Add reservation'
      end
      expected << member.slice('address', 'incarnation')
    end
    unless expected == selected['conversations'].map { |v| v.slice('address', 'incarnation') }
      raise Error, 'Pi binding and ordered Add reservations disagree'
    end
    expected
  end

  def required_roster_version(team, root)
    team.fetch('sessions').map do |claim|
      scope = claim.fetch('session')
      roster = pi_roster(root, scope.fetch('workspace'), scope.fetch('slug'))
      roster ? roster.fetch('schema') : 2
    end.max || 2
  end

  def require_workflow_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    valid = WORKFLOW_FIELDS.all? do |field, expected|
      value = contract[field]
      if expected.is_a?(Hash)
        value.is_a?(Hash) && value.keys.sort == expected.keys.sort &&
          expected.all? { |key, version| value[key].is_a?(Integer) && value[key] == version }
      else
        value.is_a?(Integer) && value == expected
      end
    end
    raise Error, 'target package cannot preserve retained Pi workflow records' unless valid
  end

  def require_role_instructions_contract!(package)
    require_workflow_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    unless contract['piRoleInstructions'].is_a?(Integer) && contract['piRoleInstructions'] == 1
      raise Error, 'target package cannot preserve retained Pi role instructions'
    end
  end

  def require_input_delivery_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    unless contract['piInputDelivery'].is_a?(Integer) && contract['piInputDelivery'] == 1
      raise Error, 'target package cannot preserve retained Pi native input delivery'
    end
  end

  def require_input_recovery_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    unless contract['piInputRecovery'].is_a?(Integer) && contract['piInputRecovery'] == 1
      raise Error, 'target package cannot preserve retained Pi input recovery'
    end
  end

  def require_preparation_contract!(package, required: 2)
    require_contract!(package)
    require_role_instructions_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    unless contract['piPreparationSchema'].is_a?(Integer) && [2, 3].include?(contract['piPreparationSchema']) && contract['piPreparationSchema'] >= required
      raise Error, 'target package cannot preserve retained Pi preparation requests'
    end
  end

  TEAM_SOURCES = (%w[roster monitor communication_grant communication_message observer utility_mapping utility_receipt input_delivery input_recovery lifecycle] + WORKFLOW_SOURCES).freeze
  TEAM_INPUT_ID = /\A(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\z/

  def require_empty_team_namespaces!(root)
    pending = %w[codex-teams monitor-operations session-communication progress-observers pi-runtime pi-questions pi-plans].map { |name| File.join(root, name) }
    visited = 0
    until pending.empty?
      path = pending.pop
      next unless present?(path)
      visited += 1
      raise Error, 'orphan retained namespace inventory exceeds limit' if visited > 65_536
      private_directory!(path)
      Dir.children(path).each do |name|
        child = File.join(path, name)
        raise Error, 'retained Pi state has no registered workspace' unless File.lstat(child).directory?
        pending << child
      end
    end
  rescue SystemCallError => e
    raise Error, "cannot inspect unregistered retained Pi state: #{e.message}"
  end

  # The owning Go codecs provide structural record claims. This boundary binds
  # those claims to original creation fences; it grants no ready/native access.
  def team_preflight!(data, root:, workspaces:)
    unless data.is_a?(String) && data.bytesize <= 4 * 1024 * 1024 && data.dup.force_encoding(Encoding::UTF_8).valid_encoding?
      raise Error, 'invalid Pi team preflight output size or encoding'
    end
    result = JSON.parse(data, object_class: StrictObject)
    raise Error, 'invalid Pi team preflight output contract' unless result.is_a?(Hash)
    schema = result['schema']
    keys = case schema
    when 3 then %w[schema required sessions piPreparations piRuntimes]
    when 2 then %w[schema required sessions piPreparations]
    else %w[schema required sessions]
    end
    exact!(result, keys)
    unless result['schema'].is_a?(Integer) && [1, 2, 3].include?(schema) && [true, false].include?(result['required']) &&
      result['sessions'].is_a?(Array) && result['sessions'].length <= 65_536 && result['required'] == !result['sessions'].empty?
      raise Error, 'invalid Pi team preflight output contract'
    end
    if [2, 3].include?(schema)
      preparations = result['piPreparations']
      unless preparations.is_a?(Array) && preparations.length.between?(schema == 3 ? 0 : 1, 65_536)
        raise Error, 'invalid retained Pi preparation inventory'
      end
      keys = preparations.map do |reference|
        exact!(reference, schema == 3 ? %w[workspace requestId schema] : %w[workspace requestId])
        raise Error, 'invalid Pi preparation schema' if schema == 3 && (!reference['schema'].is_a?(Integer) || ![2, 3].include?(reference['schema']))
        unless workspaces.include?(reference['workspace']) && reference['requestId'].is_a?(String) &&
          /\A[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\z/.match?(reference['requestId'])
          raise Error, 'invalid retained Pi preparation identity'
        end
        reference.values_at('workspace', 'requestId')
      end
      raise Error, 'unordered or duplicate Pi preparations' unless keys == keys.sort.uniq
    end
    proofs, identities, members = {}, {}, {}
    if schema == 3
      runtimes = result['piRuntimes']
      unless runtimes.is_a?(Array) && runtimes.length <= 65_536
        raise Error, 'invalid retained Pi runtime inventory'
      end
      keys = runtimes.map do |reference|
        exact!(reference, %w[session generation phase], %w[inputDelivery])
        if reference.key?('inputDelivery') && reference['inputDelivery'] != true
          raise Error, 'invalid retained Pi input delivery requirement'
        end
        scope = reference['session']
        exact!(scope, %w[workspace slug incarnation])
        unless workspaces.include?(scope['workspace']) && scalar?(scope['slug']) && SLUG.match?(scope['slug']) && hex?(scope['incarnation']) &&
          hex?(reference['generation']) && %w[preparing starting rejected].include?(reference['phase'])
          raise Error, 'invalid retained Pi runtime identity'
        end
        identity = scope.values_at('workspace', 'slug', 'incarnation')
        if identities[identity] && identities[identity] != scope['incarnation']
          raise Error, 'conflicting retained Pi session incarnations'
        end
        identities[identity] = scope['incarnation']
        scope.values_at('workspace', 'slug', 'incarnation')
      end
      raise Error, 'unordered or duplicate Pi runtimes' unless keys == keys.sort.uniq
    end
    result['sessions'].each do |claim|
      exact!(claim, %w[source session members], %w[lead])
      exact!(claim['source'], %w[kind id])
      unless TEAM_SOURCES.include?(claim['source']['kind']) && scalar?(claim['source']['id']) && claim['source']['id'].bytesize <= 256
        raise Error, 'invalid retained Pi source identity'
      end
      if WORKFLOW_SOURCES.include?(claim['source']['kind']) && !hex?(claim['source']['id'])
        raise Error, 'invalid retained workflow source identity'
      end
      scope = claim['session']
      exact!(scope, %w[workspace slug incarnation])
      unless workspaces.include?(scope['workspace']) && scalar?(scope['slug']) && SLUG.match?(scope['slug']) && hex?(scope['incarnation'])
        raise Error, 'retained Pi source has an unregistered or malformed session'
      end
      key = scope.values_at('workspace', 'slug', 'incarnation')
      if identities[key] && identities[key] != scope['incarnation']
        raise Error, 'conflicting retained Pi session incarnations'
      end
      identities[key] = scope['incarnation']
      current_binding = binding(root, scope['workspace'], scope['slug'])
      capture = lifecycle_captures(root, scope['workspace'], scope['slug']).find do |value|
        value['binding']['session_incarnation'] == scope['incarnation'] &&
          (!File.directory?(File.join(scope['workspace'], 'work', scope['slug'])) || !current_binding || current_binding['session_incarnation'] != scope['incarnation']) &&
          (!current_binding || current_binding['session_incarnation'] != scope['incarnation'] || current_binding == value['binding'])
      end
      if capture
        checks = claim.fetch('members').dup
        checks << claim.fetch('lead').merge('address' => 'lead') if claim['lead']
        checks.each do |member|
          saved = capture.fetch('members').find { |value| value.fetch('Slot').fetch('Member') == member['address'] }
          slot = saved && saved.fetch('Slot')
          native = saved && saved['Conversation'] && saved['Conversation']['NativeID']
          unless slot && member['incarnation'] == slot['Incarnation'] &&
            (!member.key?('nativeId') || member['nativeId'] == native) &&
            (!member.key?('creationId') || member['creationId'] == slot['CreationID']) &&
            (!member.key?('initialInputId') || member['initialInputId'] == slot['InitialInputID'])
            raise Error, 'retained Pi claim differs from its original lifecycle capture'
          end
        end
        next
      end
      proof = proofs[key] ||= retained_creation_fence!(root, scope['workspace'], scope['slug'])
      intent = proof.fetch(:intent)
      unless intent['session_incarnation'] == scope['incarnation']
        raise Error, 'retained Pi source disagrees with original creation identity'
      end
      selected = binding(root, scope['workspace'], scope['slug'])
      checks = claim['members']
      unless checks.is_a?(Array) && checks.length <= 65 && checks.all? { |member| member.is_a?(Hash) } && checks.map { |member| member['address'] }.uniq.length == checks.length
        raise Error, 'invalid retained Pi member constraints'
      end
      checks = checks.dup
      if claim.key?('lead')
        lead = claim['lead']
        exact!(lead, %w[incarnation], %w[nativeId])
        checks << lead.merge('address' => 'lead')
      end
      if WORKFLOW_SOURCES.include?(claim['source']['kind']) &&
        (checks.length != 1 || !checks.first.key?('nativeId') ||
          %w[tool_policy role_instructions].include?(claim['source']['kind']) && (!checks.first.key?('creationId') || claim.key?('lead')))
        raise Error, 'invalid retained workflow conversation claim'
      end
      checks.each do |member|
        exact!(member, %w[address incarnation], %w[nativeId creationId initialInputId])
        unless member?(member['address']) && hex?(member['incarnation']) &&
          (!member.key?('nativeId') || scalar?(member['nativeId'])) &&
          (!member.key?('creationId') || hex?(member['creationId'])) &&
          (!member.key?('initialInputId') || member['initialInputId'].is_a?(String) && TEAM_INPUT_ID.match?(member['initialInputId']))
          raise Error, 'invalid retained Pi member identity'
        end
        reserved = intent['conversations'].find { |conversation| conversation['address'] == member['address'] }
        creation_id = intent['schema'] == 5 ? intent['fork']['members'].find { |m| m['address'] == member['address'] }&.fetch('creation_id') : intent['receipt_id']
        unless reserved
          roster = pi_roster(root, scope['workspace'], scope['slug'])
          added = roster && roster.fetch('members').find { |entry| entry['address'] == member['address'] && entry['addition'] }
          if added && selected
            ready_conversations!(root, intent, selected)
            reserved, creation_id = added, added['creationId']
          end
        end
        unless reserved && reserved['incarnation'] == member['incarnation'] && (!member.key?('creationId') || member['creationId'] == creation_id)
          raise Error, 'retained Pi member is outside its original creation tuple'
        end
        if member['address'] == 'lead'
          if member.key?('nativeId') && member['nativeId'] != proof.fetch(:completion)['nativeId'] ||
            member.key?('initialInputId') && member['initialInputId'] != intent['initial_input_id']
            raise Error, 'retained Pi lead proof changed'
          end
        elsif member.key?('nativeId') && selected
          current = selected['conversations'].find { |conversation| conversation['address'] == member['address'] }
          if current && current['native_id'] && current['native_id'] != member['nativeId']
            raise Error, 'retained Pi member native identity conflicts with binding'
          end
        end
        member_key = key + [member['address']]
        previous = members[member_key] ||= {}
        member.each do |field, value|
          raise Error, 'conflicting retained Pi member claims' if previous.key?(field) && previous[field] != value
          previous[field] = value
        end
      end
    end
    result
  rescue JSON::ParserError => e
    raise Error, "invalid Pi team preflight JSON: #{e.message}"
  end
end

require_relative 'workspace-session-fork'

module WorkspaceSessionBackends
  module_function

  def lifecycle_captures(root, workspace, slug)
    id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
    paths = %w[archive removal revive].map { |kind| File.join(workspace, 'worktrees', '.locks', "#{slug}.#{kind}.json") }
    paths.concat(Dir.glob(File.join(root, 'archived-worktrees', id, slug, '*', 'lifecycle.json')))
    paths.concat(Dir.glob(File.join(root, 'removed', id, "*-#{slug}-*", 'recovery.json')))
    raise Error, 'Pi lifecycle inventory exceeds limit' if paths.length > 65_536
    paths.filter_map do |path|
      next unless present?(path)
      value = read!(path)
      next unless value.key?('pi')
      unless value['workspace'] == workspace && value['slug'] == slug && [2, 5, 6].include?(value['schema']) && hex?(value['operation_id'])
        raise Error, 'Pi lifecycle recovery scope changed'
      end
      lifecycle_capture!(value.fetch('pi'), workspace:, slug:)
    end
  end

  def require_lifecycle_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    raise Error, 'target package cannot preserve retained Pi lifecycle records' unless contract['piLifecycle'].is_a?(Integer) && contract['piLifecycle'] == 1
  end

  def lifecycle_capture!(capture, workspace:, slug:)
    required = %w[kind contract binding authority runtime roster_revision members selectors]
    unless capture.is_a?(Hash) && (required - capture.keys).empty? && (capture.keys - required - %w[terminal successor callbacks]).empty? &&
      capture['kind'] == 'pi' && capture['contract'] == 1 && capture['binding'].is_a?(Hash) &&
      capture['binding'].keys.sort == BINDING_KEYS.sort && capture['binding']['workspace'] == workspace && capture['binding']['slug'] == slug &&
      capture['authority'].is_a?(Hash) && capture['runtime'].is_a?(Hash) && capture['members'].is_a?(Array) &&
      capture['members'].length == capture['binding'].fetch('conversations').length &&
      capture['runtime']['Session'] == {'Workspace' => workspace, 'Slug' => slug, 'Incarnation' => capture['binding']['session_incarnation']} &&
      capture['authority']['session_incarnation'] == capture['binding']['session_incarnation'] &&
      capture['authority']['binding_revision'] == capture['binding']['revision'] &&
      capture['authority']['generation'] == capture['runtime']['Generation'] && hex?(capture['runtime']['Generation']) && hex?(capture['runtime']['Digest'])
      raise Error, 'invalid Pi lifecycle capture'
    end
    if (terminal = capture['terminal'])
      unless terminal.is_a?(Hash) && terminal.values_at('instance_id', 'request_id', 'generation', 'request_sha256') ==
        capture['runtime'].values_at('InstanceID', 'RequestID', 'Generation', 'Digest') &&
        terminal['phase'] == 'terminal' && terminal['outcome'] == 'stopped' && terminal['drain_proof'].is_a?(Hash) && terminal['drain_proof']['empty'] == true
        raise Error, 'invalid Pi lifecycle terminal proof'
      end
    end
    capture['members'].each_with_index do |member, index|
      bound = capture['binding']['conversations'].fetch(index)
      slot = member.fetch('Slot')
      unless slot.values_at('Member', 'Incarnation', 'NativeID') == bound.values_at('address', 'incarnation', 'native_id').map { |value| value || '' } && scalar?(slot['CreationID'])
        raise Error, 'Pi lifecycle reserved member changed'
      end
      native = member['Conversation']
      if native && (native['Session'] != capture['runtime']['Session'] || native['Member'] != slot['Member'] || native['Incarnation'] != slot['Incarnation'] || !scalar?(native['NativeID']))
        raise Error, 'Pi lifecycle native member changed'
      end
    end
    capture
  rescue KeyError, NoMethodError
    raise Error, 'invalid Pi lifecycle capture'
  end
end

module DevSession
  module PiLifecycle
    def pi_lifecycle_selected?(slug)
      return true if WorkspaceSessionBackends.binding(@workspace_state_root, workspace, slug)
      %w[archive delete revive].any? do |operation|
        path = lifecycle_journal_file(slug, operation)
        next false unless path_exists?(path)
        read_private_json_object(path, 'lifecycle journal', max_size: AUTHORITY_MAX_SIZE, mode: 0o600).key?('pi')
      end
    end

    def pi_lifecycle_validate!(capture, slug)
      WorkspaceSessionBackends.lifecycle_capture!(capture, workspace:, slug:)
    rescue WorkspaceSessionBackends::Error => e
      raise Error, e.message
    end

    def pi_lifecycle_command(slug, action, capture = nil)
      raise Error, 'workspace-portal is required for Pi lifecycle operations' unless @portal_command && @authority_dir && @codex_socket
      selected = capture ? capture.fetch('binding') : WorkspaceSessionBackends.binding(@workspace_state_root, workspace, slug)
      raise Error, 'Pi lifecycle binding is unavailable' unless selected
      runtime_path = File.join(@workspace_state_root, 'pi-runtime', Digest::SHA256.hexdigest(workspace), slug, selected.fetch('session_incarnation'), 'runtime.json')
      records = WorkspaceSessionBackends.read!(runtime_path, maximum: 1 << 20)
      record = records['Schema'] == 2 ? records.fetch('Requests').fetch(records.fetch('CurrentRequestID')) : records
      unless [1, 2].include?(records['Schema'] || records['schema']) && record.fetch('Runtime').fetch('Session') == {'Workspace' => workspace, 'Slug' => slug, 'Incarnation' => selected.fetch('session_incarnation')}
        raise Error, 'Pi lifecycle runtime scope changed'
      end
      WorkspaceSessionBackends.require_lifecycle_contract!(File.realpath(@host_profile))
      lock = @held_slug_lock
      admitted = lock && lock.fetch(:slug) == slug && lock.fetch(:mode) == File::LOCK_EX && @lifecycle_access && @lifecycle_access[:admitted]
      raise Error, 'Pi lifecycle requires its admitted original owner' unless action == 'preflight' || admitted
      invocation = @communication_invocation
      invocation.arguments if invocation
      environment = @env.dup
      if admitted
        environment.merge!('DEV_SESSION_LIFECYCLE_OPERATION' => @lifecycle_access.fetch(:operation),
          'DEV_SESSION_LIFECYCLE_LOCK_FD' => lock.fetch(:file).fileno.to_s,
          'DEV_SESSION_LIFECYCLE_LOCK_PATH' => lock.fetch(:path))
      else
        %w[DEV_SESSION_LIFECYCLE_OPERATION DEV_SESSION_LIFECYCLE_LOCK_FD DEV_SESSION_LIFECYCLE_LOCK_PATH].each { |key| environment.delete(key) }
      end
      passed = admitted ? [lock.fetch(:file)] : []
      portal_command = @portal_command
      if @env.key?('DEV_SESSION_PI_DELETE_RECOVERY')
        unless admitted && %w[check finalize-authority].include?(action) && @lifecycle_access[:operation] == 'delete' && invocation
          raise Error, 'Pi candidate recovery only supports terminal deletion'
        end
        recovery = pi_delete_recovery_context!(slug, @lifecycle_access[:operation_id], load_removal_journal(slug).fetch('force'))
        portal_command = [File.join(recovery.fetch('candidate'), 'bin/workspace-portal')]
        passed << invocation.file
      end
      configuration = {'workspace' => workspace, 'workspace-name' => File.basename(File.dirname(@authority_dir)),
        'user-state-root' => @workspace_state_root, 'authority-dir' => @authority_dir, 'host-profile' => @host_profile,
        'transition-lock' => File.join(@workspace_state_root, 'transition.lock'),
        'registration-marker' => File.join(File.dirname(@codex_socket), 'registration.json'), 'codex-socket' => @codex_socket,
        'pi-auth-dir' => record.fetch('AuthDirectory'), 'pi-host-state-root' => record.fetch('HostStateRoot'), 'slug' => slug, 'action' => action}
      command = [*portal_command, 'pi-runtime', 'lifecycle', *configuration.flat_map { |key, value| ["--#{key}", value] }]
      output, = @command_runner.capture(command, env: environment, pass_fds: passed, input: capture ? JSON.generate(capture) : nil)
      invocation.arguments if invocation
      result = JSON.parse(output, object_class: StrictJSONHash)
      pi_lifecycle_validate!(result, slug)
    rescue WorkspaceSessionBackends::Error, JSON::ParserError, KeyError => e
      raise Error, "Pi lifecycle proof failed: #{e.message}"
    end

    def pi_lifecycle_preserve_selectors!(slug, removal)
      capture = pi_lifecycle_validate!(removal.fetch('pi'), slug)
      raise Error, 'Pi selectors require terminal drain' unless capture['terminal']
      directory = File.join(removal.fetch('recovery'), 'pi-selectors')
      FileUtils.mkdir_p(directory, mode: 0o700)
      WorkspaceSessionBackends.private_directory!(directory)
      capture.fetch('selectors').each do |selector|
        name, source, digest = selector.values_at('Name', 'Path', 'SHA256')
        raise Error, 'invalid saved Pi selector' unless %w[binding creation receipt completion companion team authority].include?(name) && WorkspaceSessionBackends.hex?(digest)
        target = File.join(directory, name + '.json')
        [source, target].each do |path|
          next unless path_exists?(path)
          WorkspaceSessionBackends.read!(path, maximum: 1 << 20)
          raise Error, 'saved Pi selector changed' unless Digest::SHA256.file(path).hexdigest == digest
        end
        if name == 'authority'
          unless source == authority_file(slug)
            raise Error, 'saved Pi authority selector path changed'
          end
          raise Error, 'Pi selector has two owners' if path_exists?(source) && path_exists?(target)
          # The original capture owns this ephemeral projection. A legacy
          # same-device recovery copy remains audit; new retirement makes none.
          next
        end
        if path_exists?(source)
          raise Error, 'Pi selector has two owners' if path_exists?(target)
          move_to_removal_recovery!(source, directory, name + '.json')
        elsif !path_exists?(target)
          raise Error, 'Pi selector lost its saved source and recovery'
        end
      end
      fsync_directory(directory)
      pi_lifecycle_command(slug, 'finalize-authority', capture)
    end

    def pi_lifecycle_archived_proof!(slug, operation_id)
      id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      path = File.join(@workspace_state_root, 'archived-worktrees', id, slug, operation_id, 'lifecycle.json')
      proof = read_private_json_object(path, 'Pi archive lifecycle proof', max_size: AUTHORITY_MAX_SIZE, mode: 0o600)
      unless proof['schema'] == 5 && proof['slug'] == slug && proof['workspace'] == workspace && proof['operation_id'] == operation_id && proof['phase'] == 'archived'
        raise Error, 'Pi archive lifecycle proof changed'
      end
      pi_lifecycle_validate!(proof.fetch('pi'), slug)
      raise Error, 'Pi archive runtime has no drain proof' unless proof['pi']['terminal']
      proof
    end

    def pi_lifecycle_capture!(slug, readonly: false, force_delete: false)
      if !readonly && !path_exists?(authority_file(slug)) && path_exists?(archive_dir(slug))
        manifest = load_portal_manifest(File.join(archive_dir(slug), PORTAL_MANIFEST), required: true, expected_slug: slug)
        proof = pi_lifecycle_archived_proof!(slug, manifest.fetch('archive_operation_id'))
        capture = Marshal.load(Marshal.dump(proof.fetch('pi')))
        # Archive already retired the live authority, and the creation retry
        # cache may have expired. The sealed archive still owns their identity.
        capture.fetch('selectors').reject! do |selector|
          missing = !path_exists?(selector.fetch('Path'))
          if missing && !%w[authority receipt].include?(selector.fetch('Name'))
            raise Error, 'archived Pi selector is missing without retirement proof'
          end
          missing
        end
        return pi_lifecycle_validate!(capture, slug)
      end
      WorkspaceSessionBackends.inspect_pi_state!(@workspace_state_root, workspace, slug)
      pi_lifecycle_command(slug, readonly ? 'preflight' : (force_delete ? 'capture-force-delete' : 'capture'))
    end

    def pi_lifecycle_retire!(slug, journal, operation:)
      capture = pi_lifecycle_validate!(journal.fetch('pi'), slug)
      action = operation == 'archive' && journal['mode'] == 'archived' ? 'archive-retire' : 'retire'
      if operation == 'delete' && journal['force']
        action = capture.fetch('callbacks', []).empty? ? 'delete-retire' : 'terminal-delete'
      end
      result = pi_lifecycle_command(slug, action, capture)
      raise Error, 'Pi runtime drain is not complete' unless result['terminal']
      journal['pi'] = result
      if operation == 'delete'
        write_removal_metadata!(journal.fetch('recovery'), journal)
        write_removal_journal!(slug, journal)
      else
        write_lifecycle_journal(lifecycle_journal_file(slug, operation), journal, command: operation, create: false)
      end
      result
    end
  end
end
