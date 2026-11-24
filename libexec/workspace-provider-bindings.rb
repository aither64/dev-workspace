# frozen_string_literal: true

require 'json'
require 'digest'

module DevWorkspaceHost
  # Pure schema-3 preparation. Loading this module does not enable production
  # provider-state admission, authorize callers, or perform native/file effects.
  module ProviderBindings
    class Error < StandardError; end
    class UniqueObject < Hash
      def []=(key, value)
        raise Error, 'duplicate binding JSON key' if key?(key)
        super
      end
    end

    STATE_LIMIT = 1024 * 1024
    EVIDENCE_LIMIT = 16 * 1024 * 1024
    RETENTION_LIMIT = 2048
    UINT64_MAX = (1 << 64) - 1
    ID = /\A[a-z0-9][a-z0-9_-]{0,62}\z/
    DIGEST = /\A[0-9a-f]{64}\z/
    OPERATION = /\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/
    KEY = {'workspace' => String, 'slug' => String, 'incarnation' => String, 'rootThreadId' => String, 'member' => String, 'originalThreadId' => String}.freeze
    CONVERSATION = {'member' => String, 'threadId' => String, 'provider' => String, 'operationId?' => String, 'evidenceDigest?' => String}.freeze
    SESSION = {'rootThreadId' => String, 'incarnation' => String, 'revision' => :uint, 'conversations' => [:map, CONVERSATION]}.freeze
    SELECTION = {'provider' => String, 'rootThreadId?' => String, 'creationId?' => String}.freeze
    RECEIPT = {'target' => String, 'catalogDigest' => String, 'session?' => String, 'inherit?' => :bool}.freeze
    OP = {'id' => String, 'target' => String, 'catalogDigest' => String, 'phase' => String, 'blockers' => [:array, String], 'createdAt' => String, 'updatedAt' => String,
          'snapshot?' => String, 'message?' => String, 'stage?' => String, 'previousActive?' => String, 'strategy?' => String, 'session?' => String, 'sessions?' => [:array, String], 'inherit?' => :bool, 'roots?' => [:map, String]}.freeze
    STATE = {'schema' => Integer, 'workspace' => String, 'root' => String, 'socket' => String, 'active' => String, 'operation?' => OP, 'receipts?' => [:map, RECEIPT], 'sessions?' => [:map, SELECTION], 'bindingRevision?' => :uint, 'bindings?' => [:map, SESSION]}.freeze
    CURRENT = {'key' => KEY, 'bindingRequired' => :bool, 'expectedRevision' => :uint, 'provider' => String, 'socket' => String, 'eligible' => :bool}.freeze
    PARTICIPANT = {'key' => KEY, 'oldThreadId' => String, 'newThreadId' => String, 'oldProvider' => String, 'newProvider' => String, 'previousOperationId?' => String, 'previousEvidenceDigest?' => String}.freeze
    EVIDENCE = {'schema' => Integer, 'operationId' => String, 'workspace' => String, 'root' => String, 'socket' => String, 'previousBindingRevision' => :uint, 'committedBindingRevision' => :uint, 'snapshotDigest' => String, 'participants' => [:array, PARTICIPANT]}.freeze

    module_function

    def refuse!(message = 'invalid provider binding') = raise(Error, message)
    def id?(v) = v.is_a?(String) && ID.match?(v)
    def digest?(v) = v.is_a?(String) && DIGEST.match?(v)
    def thread?(v) = v.is_a?(String) && !v.empty? && v.bytesize <= 512 && !v.match?(/[\x00\t\r\n ]/)
    def path?(v) = v.is_a?(String) && v.start_with?('/') && v.bytesize <= 4096 && File.expand_path(v) == v && !v.match?(/[\x00\r\n]/)
    def pair?(op, digest) = (op.nil? && digest.nil?) || (op.is_a?(String) && OPERATION.match?(op) && digest?(digest))
    def key?(k)
      path?(k['workspace']) && id?(k['slug']) && digest?(k['incarnation']) && thread?(k['rootThreadId']) && thread?(k['originalThreadId']) && id?(k['member']) && (k['member'] != 'lead' || k['rootThreadId'] == k['originalThreadId'])
    end

    def shape!(value, specification)
      case specification
      when Hash
        refuse!('binding object required') unless value.is_a?(Hash)
        names = specification.keys.map { |k| k.delete_suffix('?') }
        refuse!('unknown binding field') unless (value.keys - names).empty?
        specification.each do |key, type|
          name = key.delete_suffix('?')
          refuse!('missing binding field') unless key.end_with?('?') || value.key?(name)
          shape!(value[name], type) if value.key?(name)
        end
      when Array
        kind, type = specification
        refuse!('binding container required') unless kind == :map ? value.is_a?(Hash) : value.is_a?(Array)
        (kind == :map ? value.values : value).each { |v| shape!(v, type) }
      when :uint
        refuse!('binding revision is not uint64') unless value.is_a?(Integer) && value.between?(0, UINT64_MAX)
      when :bool
        refuse!('binding boolean required') unless [true, false].include?(value)
      else
        refuse!('binding field type changed') unless value.is_a?(specification)
      end
    end

    def parse(data, maximum, specification)
      refuse!('binding JSON exceeds bounds or invalid UTF-8') unless data.is_a?(String) && data.bytesize <= maximum && data.dup.force_encoding(Encoding::UTF_8).valid_encoding?
      value = JSON.parse(data, object_class: UniqueObject, max_nesting: 64, create_additions: false)
      shape!(value, specification)
      value
    rescue JSON::ParserError, JSON::NestingError => e
      raise Error, "invalid binding JSON: #{e.class}"
    end

    def decode_binding_state(data, workspace:, root:, socket:, catalog:)
      state = parse(data, STATE_LIMIT, STATE)
      refuse!('binding workspace identity changed') unless state.values_at('workspace', 'root', 'socket') == [workspace, root, socket]
      validate_bindings(state, catalog: catalog)
      state
    end

    def validate_bindings(state, catalog:)
      shape!(state, STATE)
      refuse! unless [1, 2, 3].include?(state['schema']) && id?(state['workspace']) && path?(state['root']) && path?(state['socket'])
      providers = catalog.fetch('providers').map { |p| p.fetch('id') }
      refuse!('provider unavailable') unless providers.include?(state['active'])
      receipts = state.fetch('receipts', {})
      refuse! if receipts.length > 2048
      receipts.each do |id, r|
        refuse! unless OPERATION.match?(id) && id?(r['target']) && digest?(r['catalogDigest']) && (!r.key?('session') || id?(r['session'])) && (!r['inherit'] || r['session'])
      end
      selections = state.fetch('sessions', {})
      refuse! if selections.length > 4096 || state['schema'] == 1 && !selections.empty?
      selections.each do |slug, v|
        refuse! unless id?(slug) && providers.include?(v['provider']) && (!v.key?('rootThreadId') || thread?(v['rootThreadId'])) && (!v.key?('creationId') || digest?(v['creationId'])) && (v['rootThreadId'] || v['creationId'])
      end
      if (o = state['operation'])
        refuse! unless id?(o['target']) && OPERATION.match?(o['id']) && digest?(o['catalogDigest']) && %w[waiting applying applied canceled failed recovery-required].include?(o['phase'])
        if o['strategy'] && !o['strategy'].empty?
          refuse! unless [2, 3].include?(state['schema']) && o['strategy'] == 'scoped-v1' && (!o['session'] || id?(o['session']))
          reserved = %w[applying recovery-required].include?(o['phase'])
          refuse! unless (%w[accepted waiting-creation waiting-package].include?(o['stage']) && !reserved) || %w[quiescing preparing switching resuming restoring-terminals].include?(o['stage'])
          sessions, roots = o.fetch('sessions', []), o.fetch('roots', {})
          refuse! unless sessions.length <= 4096 && sessions.uniq == sessions && roots.length == sessions.length && (!o['inherit'] || o['session']) && (!o['session'] || sessions == [o['session']]) && sessions.all? { |slug| id?(slug) && thread?(roots[slug]) }
        end
      end
      if state['schema'] != 3
        refuse!('legacy state has binding fields') if state.key?('bindings') || state.key?('bindingRevision')
        return true
      end
      revision, bindings = state.values_at('bindingRevision', 'bindings')
      refuse! unless revision&.positive? && bindings && bindings.length <= 4096
      originals, active = {}, {}
      bindings.each do |slug, session|
        refuse! unless id?(slug) && thread?(session['rootThreadId']) && digest?(session['incarnation']) && session['revision'].positive? && session['revision'] <= revision && !session['conversations'].empty?
        refuse!('missing original lead binding') unless session['conversations'].dig(session['rootThreadId'], 'member') == 'lead'
        session['conversations'].each do |original, c|
          k = {'workspace' => state['root'], 'slug' => slug, 'incarnation' => session['incarnation'], 'rootThreadId' => session['rootThreadId'], 'member' => c['member'], 'originalThreadId' => original}
          refuse! unless key?(k) && !originals[original] && thread?(c['threadId']) && !active[c['threadId']] && providers.include?(c['provider']) && pair?(c['operationId'], c['evidenceDigest']) && ((c['threadId'] == original) == !c.key?('operationId'))
          originals[original] = true
          active[c['threadId']] = true
        end
      end
      bindings.each_value { |v| v['conversations'].each { |original, c| refuse!('cross-conversation alias') if c['threadId'] != original && originals[c['threadId']] } }
      refuse!('binding state exceeds bounds') if JSON.generate(state).bytesize > STATE_LIMIT
      true
    end

    def binding(state, key)
      shape!(key, KEY)
      refuse!('binding key identity changed') unless key?(key) && key['workspace'] == state['root']
      session = state.fetch('bindings', {})[key['slug']]
      return [nil, 0] unless session
      refuse! unless session.values_at('rootThreadId', 'incarnation') == key.values_at('rootThreadId', 'incarnation')
      c = session['conversations'][key['originalThreadId']]
      refuse!('conversation binding identity changed') unless c && c['member'] == key['member']
      [c, session['revision']]
    end

    def resolve_current(state, current)
      shape!(current, CURRENT)
      refuse!('current binding preconditions changed') unless current['eligible'] && current['socket'] == state['socket'] && id?(current['provider'])
      selection = state.fetch('sessions', {})[current['key']['slug']]
      effective = selection && selection['rootThreadId'] == current['key']['rootThreadId'] ? selection['provider'] : state['active']
      refuse!('current provider selection changed') unless effective == current['provider']
      c, revision = binding(state, current['key'])
      unless c
        k = current['key']
        selection = state.fetch('sessions', {})[k['slug']]
        provider = selection && selection['rootThreadId'] == k['rootThreadId'] ? selection['provider'] : state['active']
        refuse!('required binding missing') if current['bindingRequired'] || current['expectedRevision'] != 0 || provider != current['provider']
        c = {'threadId' => k['originalThreadId'], 'provider' => provider}
      end
      refuse!('stale binding revision or provider') unless revision == current['expectedRevision'] && c['provider'] == current['provider']
      {'key' => current['key'].dup, 'threadId' => c['threadId'], 'provider' => c['provider'], 'revision' => revision, 'socket' => state['socket']}
    end

    def decode_evidence(state, data, operation, digest)
      refuse!('sealed evidence exceeds bounds') unless data.is_a?(String) && data.bytesize <= EVIDENCE_LIMIT
      refuse!('sealed evidence digest changed') unless Digest::SHA256.hexdigest(data) == digest
      e = parse(data, EVIDENCE_LIMIT, EVIDENCE)
      refuse! unless e['schema'] == 1 && e['operationId'] == operation && OPERATION.match?(operation) && e.values_at('workspace', 'root', 'socket') == state.values_at('workspace', 'root', 'socket') && digest?(e['snapshotDigest']) && e['previousBindingRevision'] < UINT64_MAX && e['committedBindingRevision'] == e['previousBindingRevision'] + 1 && e['committedBindingRevision'] <= state['bindingRevision'] && !e['participants'].empty?
      keys, old, targets = [], {}, {}
      e['participants'].each do |p|
        k = p['key']
        refuse! unless key?(k) && k['workspace'] == state['root'] && !keys.include?(k) && thread?(p['oldThreadId']) && thread?(p['newThreadId']) && p['oldThreadId'] != p['newThreadId'] && !old[p['oldThreadId']] && !targets[p['newThreadId']] && id?(p['oldProvider']) && id?(p['newProvider']) && pair?(p['previousOperationId'], p['previousEvidenceDigest']) && ((p['oldThreadId'] == k['originalThreadId']) == !p.key?('previousOperationId'))
        keys << k
        old[p['oldThreadId']] = targets[p['newThreadId']] = true
      end
      refuse!('conflicting evidence targets') unless (old.keys & targets.keys).empty?
      e
    end

    def receipt_targets(state, key, binding_required, evidence_reader)
      shape!(binding_required, :bool)
      c, revision = binding(state, key)
      unless c
        refuse!('receipt binding missing') if binding_required
        selection = state.fetch('sessions', {})[key['slug']]
        provider = selection && selection['rootThreadId'] == key['rootThreadId'] ? selection['provider'] : state['active']
        return [{'threadId' => key['originalThreadId'], 'provider' => provider}]
      end
      result, seen = [], {}
      loop do
        refuse!('binding lineage exceeds bounds or cycles') if (result.length >= RETENTION_LIMIT && c['threadId'] != key['originalThreadId']) || seen[c['threadId']]
        seen[c['threadId']] = true
        result << c.slice('threadId', 'provider', 'operationId', 'evidenceDigest')
        if c['threadId'] == key['originalThreadId']
          refuse! if c.key?('operationId') || c.key?('evidenceDigest')
          return result
        end
        refuse!('binding lineage missing') unless evidence_reader && c['operationId'] && pair?(c['operationId'], c['evidenceDigest'])
        e = decode_evidence(state, evidence_reader.call(c['operationId']), c['operationId'], c['evidenceDigest'])
        refuse!('lineage revisions do not decrease') unless e['committedBindingRevision'] <= revision
        p = e['participants'].find { |v| v['key'] == key }
        refuse!('lineage target or key changed') unless p && p.values_at('newThreadId', 'newProvider') == c.values_at('threadId', 'provider')
        c = {'threadId' => p['oldThreadId'], 'provider' => p['oldProvider']}
        c.merge!('operationId' => p['previousOperationId'], 'evidenceDigest' => p['previousEvidenceDigest']) if p.key?('previousOperationId')
        revision = e['previousBindingRevision']
      end
    end

    def prepare_binding_commit(state, expected_revision:, participants:, updates:, selection:, evidence: nil, catalog:)
      validate_bindings(state, catalog: catalog)
      shape!(expected_revision, :uint)
      shape!(participants, [:array, CURRENT])
      shape!(updates, [:array, {'key' => KEY, 'threadId' => String, 'provider' => String}])
      refuse! unless expected_revision == state.fetch('bindingRevision', 0) && !participants.empty? && participants.length == updates.length
      shape!(selection, {'scope' => String, 'session?' => String, 'provider' => String, 'inherit?' => :bool})
      provider = selection['provider']
      refuse! unless catalog['providers'].any? { |p| p['id'] == provider } && %w[session workspace].include?(selection['scope']) && (selection['scope'] != 'session' || id?(selection['session'])) && (selection['scope'] != 'workspace' || !selection.key?('session') && !selection['inherit']) && (!selection['inherit'] || provider == state['active'])
      current, changes = {}, {}
      participants.each do |p|
        k = p['key']
        refuse! if current.key?(k) || selection['scope'] == 'session' && k['slug'] != selection['session']
        explicit = state.fetch('sessions', {})[k['slug']]
        refuse!('explicit session outside inherited scope') if selection['scope'] == 'workspace' && explicit && explicit['rootThreadId'] == k['rootThreadId']
        current[k] = resolve_current(state, p)
      end
      current.keys.group_by { |k| k['slug'] }.each_value do |keys|
        refuse! unless keys.any? { |k| k['member'] == 'lead' } && keys.map { |k| [k['incarnation'], k['rootThreadId'], current[k]['revision']] }.uniq.length == 1
      end
      changed = false
      updates.each do |u|
        shape!(u, {'key' => KEY, 'threadId' => String, 'provider' => String})
        k = u['key']; r = current[k]
        refuse! unless r && !changes[k] && thread?(u['threadId']) && u['provider'] == provider && (u['threadId'] != r['threadId'] || u['provider'] == r['provider'])
        changed ||= u['threadId'] != r['threadId']
        changes[k] = u
      end
      candidate = JSON.parse(JSON.generate(state))
      if selection['scope'] == 'workspace'
        candidate['active'] = provider
      elsif selection['inherit']
        candidate.fetch('sessions', {}).delete(selection['session'])
      else
        candidate['sessions'] ||= {}
        candidate['sessions'][selection['session']] = {'provider' => provider, 'rootThreadId' => current.keys.first['rootThreadId']}
      end
      candidate['schema'] = 2 if candidate['schema'] == 1 && !candidate.fetch('sessions', {}).empty?
      if changed
        refuse! unless state.fetch('bindingRevision', 0) < UINT64_MAX && evidence
        candidate['schema'] = 3
        candidate['bindingRevision'] = state.fetch('bindingRevision', 0) + 1
        candidate['bindings'] ||= {}
        digest = Digest::SHA256.hexdigest(evidence)
        operation = JSON.parse(evidence)['operationId']
        e = decode_evidence(candidate, evidence, operation, digest)
        refuse! unless e['previousBindingRevision'] == state.fetch('bindingRevision', 0) && e['participants'].length == current.length
        e['participants'].each do |p|
          k = p['key']; r = current[k]; u = changes[k]
          refuse! unless r && u && p.values_at('oldThreadId', 'newThreadId', 'oldProvider', 'newProvider') == [r['threadId'], u['threadId'], r['provider'], u['provider']]
          previous = state.fetch('bindings', {}).dig(k['slug'], 'conversations', k['originalThreadId']) || {}
          refuse! unless p.values_at('previousOperationId', 'previousEvidenceDigest') == previous.values_at('operationId', 'evidenceDigest')
          session = candidate['bindings'][k['slug']] ||= {'rootThreadId' => k['rootThreadId'], 'incarnation' => k['incarnation'], 'conversations' => {}}
          session['revision'] = candidate['bindingRevision']
          session['conversations'][k['originalThreadId']] = {'member' => k['member'], 'threadId' => u['threadId'], 'provider' => u['provider'], 'operationId' => operation, 'evidenceDigest' => digest}
        end
      else
        refuse!('selection-only commit has switch evidence') if evidence
      end
      validate_bindings(candidate, catalog: catalog)
      candidate
    rescue JSON::ParserError => e
      raise Error, "invalid binding evidence: #{e.class}"
    end
  end
end
