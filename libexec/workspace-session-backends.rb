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
  WORKFLOW_SOURCES = %w[questions plans tool_policy role_instructions].freeze
  BINDING_KEYS = %w[schema backend backend_contract workspace slug session_incarnation creation_receipt_id revision state conversations].freeze
  HEX = /\A[0-9a-f]{64}\z/
  SLUG = /\A[A-Za-z0-9][A-Za-z0-9_-]*\z/
  MEMBER = /\A[a-z][a-z0-9]{0,31}[0-9]+\z/

  module_function

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
    exact!(record, INTENT_KEYS + %w[state native_create_started], %w[native_generation native])
    intent!(record.slice(*INTENT_KEYS), workspace:, slug:)
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

  def validate_creation_bundle!(root, workspace, slug, receipt_id, evidence)
    receipt_path, journal_path = creation_paths(root, workspace, slug)
    expected_evidence = File.join(File.dirname(receipt_path), "#{slug}.#{receipt_id}.complete.json")
    raise Error, 'Pi evidence path does not match receipt' unless evidence == expected_evidence
    receipt = read!(receipt_path, maximum: 12 * 20_000 + 16_384)
    intent = receipt_intent!(receipt, workspace:, slug:)
    raise Error, 'Pi receipt owner changed' unless intent['receipt_id'] == receipt_id && receipt['state'] == 'running'
    companion = read!(evidence + '.request'); intent!(companion, workspace:, slug:)
    journal = read!(journal_path); journal!(journal, workspace:, slug:)
    unless companion == intent && journal.slice(*INTENT_KEYS) == intent
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
    journal = read!(journal_path)
    journal!(journal, workspace:, slug:)
    raise Error, 'retained Pi dependency requires its ready creation journal' unless journal['state'] == 'ready'
    intent = journal.slice(*INTENT_KEYS)
    evidence = File.join(File.dirname(receipt_path), "#{slug}.#{intent.fetch('receipt_id')}.complete.json")
    companion = read!(evidence + '.request')
    intent!(companion, workspace:, slug:)
    raise Error, 'retained Pi immutable request changed' unless companion == intent
    if present?(receipt_path)
      receipt = receipt_intent!(read!(receipt_path, maximum: 12 * 20_000 + 16_384), workspace:, slug:)
      raise Error, 'retained Pi cache receipt changed' unless receipt == intent
    end
    selected = binding(root, workspace, slug)
    if selected && (selected['state'] != 'ready' || selected['creation_receipt_id'] != intent['receipt_id'] ||
      selected['session_incarnation'] != intent['session_incarnation'] ||
      selected['conversations'].map { |v| v.slice('address', 'incarnation') } != intent['conversations'] ||
      selected['conversations'].first['native_id'] != journal.fetch('native').fetch('native_id'))
      raise Error, 'retained Pi binding and creation fence disagree'
    end
    completion = completion!(read!(evidence, maximum: 12 * 20_000 + 16_384), intent, selected, journal)
    {intent:, journal:, completion:}
  end

  # Package compatibility is a durable shape/cross-record check, not permission
  # to start a worker or dispatch. Pending creation is separately refused by
  # the host's existing unfinished-operation preflight.
  def runtime_authority!(record, selected, workspace, slug)
    exact!(record, %w[schema backend backend_contract workspace slug session_incarnation binding_revision generation state])
    unless record['schema'].is_a?(Integer) && record['schema'] == 2 && record['backend'] == 'pi' && record['backend_contract'].is_a?(Integer) && record['backend_contract'] == 1 &&
      record['workspace'] == workspace && record['slug'] == slug && hex?(record['generation']) && %w[starting ready].include?(record['state']) && selected &&
      record['session_incarnation'] == selected['session_incarnation'] && record['binding_revision'].is_a?(Integer) && record['binding_revision'] == selected['revision']
      raise Error, 'Pi live authority has no matching current durable binding'
    end
    record
  end

  def inspect_pi_state!(root, workspace, slug, retained: false)
    receipt_path, journal_path = creation_paths(root, workspace, slug)
    receipt = present?(receipt_path) && read!(receipt_path, maximum: 12 * 20_000 + 16_384)
    journal = present?(journal_path) && read!(journal_path)
    selected = binding(root, workspace, slug)
    pi_receipt = receipt && receipt['schema'] == 4
    pi_journal = journal && journal['schema'] == 4
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
      frozen = journal.slice(*INTENT_KEYS)
      raise Error, 'Pi receipt and journal disagree' if intent && intent != frozen
      intent ||= frozen
    end
    raise Error, "session backend conflict: Pi binding has no retained creation fence: #{slug}" unless intent
    if !selected && retained
      retained_creation_fence!(root, workspace, slug)
      return true
    end
    if selected
      unless selected['creation_receipt_id'] == intent['receipt_id'] && selected['session_incarnation'] == intent['session_incarnation'] && selected['conversations'].map { |v| v.slice('address', 'incarnation') } == intent['conversations']
        raise Error, 'Pi binding and retained creation disagree'
      end
    end
    return true unless pi_journal
    evidence = File.join(File.dirname(receipt_path), "#{slug}.#{intent.fetch('receipt_id')}.complete.json")
    companion = read!(evidence + '.request'); intent!(companion, workspace:, slug:)
    raise Error, 'Pi immutable companion changed' unless companion == intent
    if journal['state'] == 'ready'
      unless selected && selected['state'] == 'ready' && selected['conversations'].first['state'] == 'ready' && selected['conversations'].first['native_id'] == journal.fetch('native').fetch('native_id')
        raise Error, 'Pi ready journal has no matching ready binding'
      end
      completion!(read!(evidence, maximum: 12 * 20_000 + 16_384), intent, selected, journal)
    elsif !receipt
      raise Error, 'Pi creating request lost its accepted receipt'
    elsif journal['native'] && selected && selected['conversations'].first['native_id'] && selected['conversations'].first['native_id'] != journal['native']['native_id']
      raise Error, 'Pi native creation result changed'
    end
    true
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
    unless FIELDS.all? { |field, expected| contract[field].is_a?(Integer) && contract[field] == expected }
      raise Error, 'target package cannot preserve Pi backend bindings and creation evidence'
    end
  end

  def require_team_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    support = contract['piTeamRecords']
    unless support.is_a?(Hash) && support.keys.sort == TEAM_FIELDS.keys.sort &&
      TEAM_FIELDS.all? { |field, expected| support[field].is_a?(Integer) && support[field] == expected }
      raise Error, 'target package cannot preserve retained Pi team records'
    end
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

  TEAM_SOURCES = (%w[roster monitor communication_grant communication_message observer utility_mapping utility_receipt] + WORKFLOW_SOURCES).freeze
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
    exact!(result, %w[schema required sessions])
    unless result['schema'].is_a?(Integer) && result['schema'] == 1 && [true, false].include?(result['required']) &&
      result['sessions'].is_a?(Array) && result['sessions'].length <= 65_536 && result['required'] == !result['sessions'].empty?
      raise Error, 'invalid Pi team preflight output contract'
    end
    proofs, identities, members = {}, {}, {}
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
      key = scope.values_at('workspace', 'slug')
      if identities[key] && identities[key] != scope['incarnation']
        raise Error, 'conflicting retained Pi session incarnations'
      end
      identities[key] = scope['incarnation']
      proof = proofs[key] ||= retained_creation_fence!(root, *key)
      intent = proof.fetch(:intent)
      unless intent['session_incarnation'] == scope['incarnation']
        raise Error, 'retained Pi source disagrees with original creation identity'
      end
      selected = binding(root, *key)
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
        unless reserved && reserved['incarnation'] == member['incarnation'] && (!member.key?('creationId') || member['creationId'] == intent['receipt_id'])
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
