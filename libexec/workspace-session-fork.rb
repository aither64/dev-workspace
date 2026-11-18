# frozen_string_literal: true

# Schema 5 is an explicit no-input creation branch. These readers preserve its
# immutable copy origin; they grant no native execution or callback authority.
module WorkspaceSessionBackends
  module_function
  FORK_INTENT_KEYS = (INTENT_KEYS - %w[goal_sha256 initial_input_id] + %w[fork]).freeze

  def intent_keys(record) = record['schema'] == 5 ? FORK_INTENT_KEYS : INTENT_KEYS

  def fork_intent!(record, workspace: nil, slug: nil)
    exact!(record, FORK_INTENT_KEYS)
    unless record['schema'] == 5 && record['backend'] == 'pi' && record['backend_contract'] == 1 &&
      scalar?(record['workspace']) && Pathname.new(record['workspace']).absolute? && File.expand_path(record['workspace']) == record['workspace'] &&
      SLUG.match?(record['slug'].to_s) && (!workspace || workspace == record['workspace']) && (!slug || slug == record['slug']) &&
      %w[receipt_id deletion_history_sha256 session_incarnation].all? { |key| hex?(record[key]) } &&
      %w[model effort provider].all? { |key| scalar?(record[key]) } && [true, false].include?(record['sensitive'])
      raise Error, 'invalid Pi fork identity'
    end
    fork = record['fork']
    exact!(fork, %w[source source_creation_id source_binding_revision source_roster_revision selected_member lead_access runtime_request_id members])
    source = fork['source']; exact!(source, %w[Workspace Slug Incarnation])
    unless source['Workspace'] == record['workspace'] && SLUG.match?(source['Slug'].to_s) && source['Slug'] != record['slug'] && hex?(source['Incarnation']) &&
      hex?(fork['source_creation_id']) && fork['source_binding_revision'].is_a?(Integer) && fork['source_binding_revision'].positive? &&
      fork['source_roster_revision'].is_a?(Integer) && fork['source_roster_revision'] >= 0 && member?(fork['selected_member']) &&
      %w[read_only workspace_write].include?(fork['lead_access']) && fork['runtime_request_id'].to_s.match?(/\A[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\z/)
      raise Error, 'invalid Pi fork source'
    end
    conversations = record['conversations']; members = fork['members']
    unless conversations.is_a?(Array) && conversations.length.between?(1, 65) && conversations.first['address'] == 'lead' && members.is_a?(Array) && members.length == conversations.length
      raise Error, 'Pi fork lost a member'
    end
    conversations.each { |c| exact!(c, %w[address incarnation]); raise Error, 'invalid Pi fork reservation' unless member?(c['address']) && hex?(c['incarnation']) }
    unless conversations.map { |c| c['address'] }.uniq.length == conversations.length && conversations.map { |c| c['incarnation'] }.uniq.length == conversations.length
      raise Error, 'duplicate Pi fork reservation'
    end
    native_ids = []
    members.each_with_index do |member, index|
      exact!(member, %w[address source_incarnation creation_id state added_at source_policy_stored], %w[native_id removed_at origin reserved_tool_policy source_selection selection_source source_catalog_digest source_policy_revision])
      unless member['address'] == conversations[index]['address'] && hex?(member['source_incarnation']) && member['source_incarnation'] != conversations[index]['incarnation'] && hex?(member['creation_id']) &&
        %w[ready creating removed].include?(member['state']) && scalar?(member['added_at'])
        raise Error, 'invalid Pi fork member'
      end
      if member['state'] == 'ready'
        origin = member['origin']; exact!(origin, %w[Source Snapshot])
        handle = origin['Source']; exact!(handle, %w[Conversation Generation])
        conversation = handle['Conversation']; exact!(conversation, %w[Session Backend Member Incarnation NativeID])
        snapshot = origin['Snapshot']; raise Error, 'missing Pi fork tool policy field' unless snapshot.is_a?(Hash) && snapshot.key?('ToolPolicy'); exact!(snapshot.reject { |k, _| k == 'ToolPolicy' }, %w[Native HistoryRef Settings Instructions])
        native = snapshot['Native']; exact!(native, %w[sourceFile sourceNativeId sourceCwd entriesDigest entryCount leafId])
        settings = snapshot['Settings']; exact!(settings, %w[Provider AccountRef Model Thinking PolicyRevision Revision])
        unless conversation['Session'] == source && conversation['Backend'] == 'pi' && conversation['Member'] == member['address'] && conversation['Incarnation'] == member['source_incarnation'] &&
          hex?(handle['Generation']) && scalar?(conversation['NativeID']) && native['sourceNativeId'] == conversation['NativeID'] && hex?(native['entriesDigest']) && native['entryCount'].is_a?(Integer) && native['entryCount'].positive? &&
          %w[sourceFile sourceCwd].all? { |key| scalar?(native[key]) && Pathname.new(native[key]).absolute? && File.expand_path(native[key]) == native[key] } && scalar?(native['leafId']) &&
          hex?(snapshot['HistoryRef']) && snapshot['Instructions'].is_a?(String) && %w[Provider AccountRef Model Thinking].all? { |key| scalar?(settings[key]) } && hex?(settings['PolicyRevision']) && settings['Revision'].is_a?(Integer) && settings['Revision'].positive? &&
          member['native_id'].to_s.match?(/\A[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\z/) && member['native_id'] != conversation['NativeID']
          raise Error, 'invalid Pi copied-history origin'
        end
        if snapshot['ToolPolicy']
          policy = snapshot['ToolPolicy']; exact!(policy, %w[Mode Revision Digest Active RegisteredCeiling])
          raise Error, 'invalid inherited Pi tool policy' unless %w[default plan].include?(policy['Mode']) && policy['Revision'].is_a?(Integer) && policy['Revision'].positive? && hex?(policy['Digest']) && policy['Active'].is_a?(Array) && policy['RegisteredCeiling'].is_a?(Array)
        end
        native_ids << member['native_id']
      elsif member.key?('origin') || member.key?('native_id')
        raise Error, 'unstarted or removed member has native proof'
      end
      next if member['state'] == 'removed'
      unless hex?(member['source_catalog_digest']) && member['source_policy_revision'].is_a?(Integer) && member['source_policy_revision'].positive? &&
        %w[session_default member_override].include?(member['selection_source']) && member['source_selection'].is_a?(Hash)
        raise Error, 'Pi fork lacks source policy provenance'
      end
    end
    unless native_ids.uniq == native_ids && members.first['state'] == 'ready' && members.any? { |m| m['address'] == fork['selected_member'] && m['state'] == 'ready' }
      raise Error, 'Pi fork selected member is not materialized'
    end
    active = conversations.reject.with_index { |_, index| members[index]['state'] == 'removed' }
    selection!(record['provider_selection'], record['workspace'], record['slug'], record['session_incarnation'], active)
    team = record['direct_team']; exact!(team, %w[id name description roles catalogDigest teamDigest leadModel leadEffort members], %w[leadInstructions])
    unless team['roles'] == conversations.map { |c| c['address'] } && team['members'].is_a?(Array) && team['members'].length + 1 == conversations.length && hex?(team['catalogDigest']) && hex?(team['teamDigest']) && team['leadModel'] == record['model'] && team['leadEffort'] == record['effort']
      raise Error, 'Pi fork saved team changed'
    end
    team['members'].each_with_index do |member, index|
      exact!(member, %w[role address model reasoningEffort behavior access], %w[purpose instructions])
      unless member['address'] == conversations[index + 1]['address'] && %w[read_only workspace_write].include?(member['access'])
        raise Error, 'Pi fork saved member changed'
      end
    end
    record['provider_selection']['members'].each do |desired|
      copied = members.find { |m| m['address'] == desired['address'] }
      desired_policy = record['provider_selection']['policy']
      selected = desired_policy['overrides'].find { |o| o['member'] == desired.slice('address', 'incarnation') }&.fetch('value') || desired_policy['default']
      unless copied['source_selection']['selection'] == selected['selection']
        raise Error, 'Pi fork changed a saved selection chain'
      end
      next unless copied['origin']
      applied = copied['origin']['Snapshot']['Settings']; settings = desired['settings']
      unless %w[provider account_ref model thinking].zip(%w[Provider AccountRef Model Thinking]).all? { |a, b| settings[a] == applied[b] }
        raise Error, 'Pi fork desired and inherited settings disagree'
      end
    end
    record
  end

  def fork_receipt_intent!(record, workspace: nil, slug: nil)
    exact!(record, %w[schema workspace request receiptId deletionHistorySha256 attempt state phase startedAt updatedAt validated model effort directTeam pi], %w[error])
    request = record['request']; pi = record['pi']
    exact!(request, %w[kind slug backend source selectedMember], %w[sensitive browserNotificationsEnabled])
    exact!(pi, %w[backendContract sessionIncarnation conversations provider selection fork])
    unless request['kind'] == 'fork' && request['backend'] == 'pi' && record['validated'] == true && record['attempt'].is_a?(Integer) && record['attempt'].positive? &&
      %w[running paused failed ready conflict].include?(record['state']) && scalar?(record['phase']) && scalar?(record['startedAt']) && scalar?(record['updatedAt']) &&
      (!request.key?('sensitive') || request['sensitive'] == true) && (!request.key?('browserNotificationsEnabled') || [true, false].include?(request['browserNotificationsEnabled']))
      raise Error, 'invalid Pi fork receipt'
    end
    intent = {'schema' => 5, 'workspace' => record['workspace'], 'slug' => request['slug'], 'backend' => 'pi', 'backend_contract' => pi['backendContract'],
      'receipt_id' => record['receiptId'], 'deletion_history_sha256' => record['deletionHistorySha256'], 'session_incarnation' => pi['sessionIncarnation'], 'conversations' => pi['conversations'],
      'model' => record['model'], 'effort' => record['effort'], 'provider' => pi['provider'], 'provider_selection' => pi['selection'], 'direct_team' => record['directTeam'], 'fork' => pi['fork'], 'sensitive' => request['sensitive'] == true}
    fork_intent!(intent, workspace:, slug:)
    raise Error, 'Pi fork source changed' unless request['source'] == pi['fork']['source']['Slug'] && request['selectedMember'] == pi['fork']['selected_member']
    intent
  end

  def fork_completion!(record, intent, selected, journal)
    exact!(record, COMPLETION_KEYS - %w[initialInputId initialSubmissionDigest goalSha256] + %w[fork])
    expected = {'schema' => 5, 'workspace' => intent['workspace'], 'slug' => intent['slug'], 'backend' => 'pi', 'backendContract' => 1, 'receiptId' => intent['receipt_id'],
      'sessionIncarnation' => intent['session_incarnation'], 'conversations' => intent['conversations'], 'nativeId' => journal.fetch('native').fetch('native_id'),
      'model' => intent['model'], 'effort' => intent['effort'], 'provider' => intent['provider'], 'providerSelection' => intent['provider_selection'],
      'catalogDigest' => intent['direct_team']['catalogDigest'], 'teamDigest' => intent['direct_team']['teamDigest'], 'team' => intent['direct_team']['id'], 'fork' => intent['fork']}
    unless expected.all? { |key, value| record[key] == value } && record['bindingRevision'].is_a?(Integer) && record['bindingRevision'].positive? && (!selected || record['bindingRevision'] <= selected['revision']) &&
      record['trackingDevice'].is_a?(Integer) && record['trackingDevice'] >= 0 && record['trackingInode'].is_a?(Integer) && record['trackingInode'].positive?
      raise Error, 'Pi fork completion evidence changed'
    end
    record
  end
end

module WorkspaceSessionBackends
  module_function
  def require_fork_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    raise Error, 'target package cannot preserve Pi forks' unless contract['piFork'] == 1 && contract['piCreationSchema'] == 5 && contract.dig('piTeamRecords', 'roster') == 4
  end
end
