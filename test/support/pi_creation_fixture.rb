# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'time'

# Synthetic host-format evidence only. Native readiness/input proof is exercised
# through P3's typed fake verifier by the Go creation tests, never by this helper.
module PiCreationFixture
  def pi_creation_records(workspace, slug: 'example', ready: true)
    conversations = [{'address' => 'lead', 'incarnation' => 'b' * 64}, {'address' => 'implementer0', 'incarnation' => 'c' * 64}]
    policy = {'session' => {'workspace' => workspace, 'slug' => slug, 'incarnation' => 'a' * 64}, 'revision' => 1,
      'default' => {'revision' => 1, 'selection' => {'chain' => [{'provider_id' => 'host', 'account_ref' => 'account', 'model_id' => 'model', 'thinking' => 'high'}],
        'budget' => {'max_extra_attempts' => 0, 'max_hops' => 0}, 'fallback_on' => []}}, 'overrides' => []}
    selection = {'schema' => 1, 'catalog_digest' => '1' * 64, 'policy' => policy,
      'members' => conversations.map { |member| member.merge('source' => 'session_default', 'policy_digest' => 'f' * 64,
        'settings' => {'provider' => 'native', 'account_ref' => 'account', 'model' => 'model', 'thinking' => 'high', 'policy_revision' => 'f' * 64, 'revision' => 1}) }}
    team = {'id' => 'delegated', 'name' => 'Fixture', 'description' => 'Fixture team', 'roles' => %w[lead implementer0], 'catalogDigest' => '2' * 64, 'teamDigest' => '3' * 64,
      'leadModel' => 'model', 'leadEffort' => 'high', 'members' => [{'role' => 'implementer', 'address' => 'implementer0', 'model' => 'model', 'reasoningEffort' => 'high', 'behavior' => 'implementer', 'access' => 'workspace_write'}]}
    goal = 'The original real request'
    intent = {'schema' => 4, 'workspace' => workspace, 'slug' => slug, 'backend' => 'pi', 'backend_contract' => 1, 'receipt_id' => 'd' * 64,
      'deletion_history_sha256' => Digest::SHA256.hexdigest('[]'), 'session_incarnation' => 'a' * 64, 'conversations' => conversations, 'goal_sha256' => Digest::SHA256.hexdigest(goal),
      'model' => 'model', 'effort' => 'high', 'provider' => 'native', 'provider_selection' => selection, 'direct_team' => team, 'initial_input_id' => '11111111-1111-4111-8111-111111111111', 'sensitive' => false}
    state = ready ? 'ready' : 'creating'
    binding = {'schema' => 1, 'backend' => 'pi', 'backend_contract' => 1, 'workspace' => workspace, 'slug' => slug, 'session_incarnation' => 'a' * 64,
      'creation_receipt_id' => 'd' * 64, 'revision' => ready ? 2 : 1, 'state' => state,
      'conversations' => conversations.map.with_index { |member, index| member.merge('state' => ready && index.zero? ? 'ready' : 'creating', **(ready && index.zero? ? {'native_id' => 'native-lead'} : {})) }}
    journal = intent.merge('state' => state, 'native_create_started' => ready)
    journal.merge!('native_generation' => 'e' * 64, 'native' => {'native_id' => 'native-lead', 'history_ref' => '9' * 64}) if ready
    now = Time.now.utc.iso8601
    receipt = {'schema' => 4, 'workspace' => workspace, 'request' => {'kind' => 'new', 'backend' => 'pi', 'slug' => slug, 'goal' => goal, 'team' => team['id'], 'catalogDigest' => team['catalogDigest']},
      'receiptId' => intent['receipt_id'], 'deletionHistorySha256' => intent['deletion_history_sha256'], 'attempt' => 1, 'state' => ready ? 'ready' : 'running', 'phase' => 'Fixture phase',
      'startedAt' => now, 'updatedAt' => now, 'validated' => true, 'goal' => goal, 'model' => 'model', 'effort' => 'high', 'directTeam' => team,
      'pi' => {'backendContract' => 1, 'sessionIncarnation' => 'a' * 64, 'conversations' => conversations, 'provider' => 'native', 'initialInputId' => intent['initial_input_id'], 'selection' => selection}}
    completion = {'schema' => 4, 'workspace' => workspace, 'slug' => slug, 'backend' => 'pi', 'backendContract' => 1, 'receiptId' => intent['receipt_id'], 'sessionIncarnation' => 'a' * 64,
      'bindingRevision' => 2, 'conversations' => conversations, 'nativeId' => 'native-lead', 'initialInputId' => intent['initial_input_id'], 'initialSubmissionDigest' => '4' * 64,
      'goalSha256' => intent['goal_sha256'], 'model' => 'model', 'effort' => 'high', 'provider' => 'native', 'providerSelection' => selection,
      'catalogDigest' => team['catalogDigest'], 'teamDigest' => team['teamDigest'], 'team' => team['id'], 'trackingDevice' => 1, 'trackingInode' => 2}
    {intent:, binding:, journal:, receipt:, completion:, goal:}
  end

  def write_pi_creation_records(root, workspace, records, receipt: true)
    slug = records.fetch(:intent).fetch('slug')
    receipt_path, journal_path = WorkspaceSessionBackends.creation_paths(root, workspace, slug)
    evidence = File.join(File.dirname(receipt_path), "#{slug}.#{records[:intent]['receipt_id']}.complete.json")
    paths = {binding: File.join(WorkspaceSessionBackends.binding_directory(root, workspace), "#{slug}.json"), journal: journal_path,
      intent: evidence + '.request', completion: evidence}
    paths[:receipt] = receipt_path if receipt
    paths.each do |key, path|
      FileUtils.mkdir_p(File.dirname(path), mode: 0700)
      File.write(path, JSON.generate(records.fetch(key)), mode: 'w', perm: 0600)
    end
    paths
  end
end
