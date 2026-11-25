# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class ProviderBindingsTest < Minitest::Test
  B = DevWorkspaceHost::ProviderBindings
  CORPUS = JSON.parse(File.binread(File.expand_path('../fixtures/codex-provider-bindings/corpus.json', __dir__))).freeze

  CORPUS.fetch('forkCases').each do |test|
    define_method("test_fork_journal_#{test.fetch('name').tr('-', '_')}") do
      doc = Marshal.load(Marshal.dump(CORPUS.fetch('fork')))
      test.fetch('patches').each { |p| patch(doc.fetch('journal'), p.fetch('path'), p['value'], p['remove']) }
      source = B.decode_binding_state(JSON.generate(doc.fetch('source')), workspace: 'work', root: '/workspace', socket: '/runtime/codex.sock', catalog: CORPUS['base']['catalog'])
      error = nil
      begin
        B.decode_fork_journal(test['rawJournal'] || JSON.generate(doc.fetch('journal')), source: source, catalog: CORPUS['base']['catalog'])
      rescue B::Error => e
        error = e
      end
      assert_equal test.fetch('valid'), error.nil?, error&.message
    end
  end

  def patch(value, path, replacement, remove)
    key = value.is_a?(Array) ? Integer(path.first) : path.first
    if path.length == 1
      remove ? value.delete(key) : value[key] = replacement
    else
      patch(value.fetch(key), path.drop(1), replacement, remove)
    end
  end

  CORPUS.fetch('cases').each do |test|
    define_method("test_shared_#{test.fetch('name').tr('-', '_')}") do
      doc = Marshal.load(Marshal.dump(CORPUS.fetch('base')))
      test.fetch('patches').each do |p|
        patch(p['target'] == 'document' ? doc : doc.fetch(p['target']), p.fetch('path'), p['value'], p['remove'])
      end
      state = nil
      before = nil
      error = nil
      begin
        state = B.decode_binding_state(test['rawState'] || JSON.generate(doc.fetch('state')), workspace: 'work', root: '/workspace', socket: '/runtime/codex.sock', catalog: doc['catalog'])
        before = Marshal.dump(state)
        case test.fetch('action')
        when 'bounds'
          bound_control(state, doc, test.fetch('bound'))
        when 'decode'
        when 'resolve'
          resolved = B.resolve_current(state, doc.fetch('current'))
          assert_equal test['thread'], resolved['threadId'] if test['thread']
        when 'receipts'
          targets = B.receipt_targets(state, doc.fetch('key'), doc.fetch('bindingRequired'), ->(id) { doc.fetch('evidence').fetch(id) { raise B::Error, 'missing sealed operation' } })
          assert_equal test['targets'], targets.map { |v| v['threadId'] }
        when 'commit'
          c = doc.fetch('commit')
          candidate = B.prepare_binding_commit(state, expected_revision: c.fetch('expectedRevision'), participants: c.fetch('participants'), updates: c.fetch('updates'), selection: c.fetch('selection'), evidence: doc['commitEvidence'], catalog: doc['catalog'])
          state.fetch('bindings', {}).each do |slug, session|
            session.fetch('conversations').each do |origin, prior|
              affected = c['updates'].any? { |u| u['key']['slug'] == slug && u['key']['originalThreadId'] == origin }
              assert_equal prior, candidate.dig('bindings', slug, 'conversations', origin) unless affected
            end
          end
          if test['roundTrip']
            encoded = JSON.generate(candidate)
            decoded = B.decode_binding_state(encoded, workspace: 'work', root: '/workspace', socket: '/runtime/codex.sock', catalog: doc['catalog'])
            if candidate['schema'] == 3
              assert_equal({}, decoded.fetch('bindings'))
              assert_equal 3, decoded.fetch('bindingRevision')
            else
              refute decoded.key?('bindings')
              refute decoded.key?('bindingRevision')
            end
          end
          assert_equal test['schema'], candidate['schema'] if test['schema']
          assert_equal state, candidate if test['schema'] == 1
          assert_equal test['revision'], candidate.fetch('bindingRevision', 0)
          if state.dig('bindings', 'beta').nil?
            assert_nil candidate.dig('bindings', 'beta')
          else
            assert_equal state.dig('bindings', 'beta'), candidate.dig('bindings', 'beta')
          end
          assert_equal state['receipts'], candidate['receipts']
          assert_equal state['bindings'], candidate['bindings'] if test['revision'] == state['bindingRevision']
        else
          flunk 'unknown corpus action'
        end
      rescue B::Error => e
        error = e
      end
      assert_equal before, Marshal.dump(state), 'pure operation mutated input' if before
      assert_equal test.fetch('valid'), error.nil?, error&.message
    end
  end

  def bound_control(input, doc, bound)
    state = Marshal.load(Marshal.dump(input))
    case bound
    when 'state'
      B.decode_binding_state(JSON.generate(state) + ' ' * B::STATE_LIMIT, workspace: 'work', root: '/workspace', socket: '/runtime/codex.sock', catalog: doc['catalog'])
    when 'evidence'
      B.decode_evidence(state, ' ' * (B::EVIDENCE_LIMIT + 1), 'x', 'a' * 64)
    when 'receipts'
      state['receipts'] = (0...2049).to_h { |n| [format('%08x-aaaa-4aaa-8aaa-%012x', n, n), {'target' => 'openai', 'catalogDigest' => 'a' * 64}] }
      B.validate_bindings(state, catalog: doc['catalog'])
    when 'sessions'
      state['bindings'] = (0...4097).to_h { |n| ["s#{n}", {}] }
      B.validate_bindings(state, catalog: doc['catalog'])
    when 'lineage'
      key = doc['key']
      [B::RETENTION_LIMIT, B::RETENTION_LIMIT + 1].each do |count|
        evidence = {}
        old_id, operation, digest = key['originalThreadId'], nil, nil
        (1..count).each do |n|
          next_id, next_op = "long-#{n}", format('%08x-aaaa-4aaa-8aaa-%012x', n, n)
          row = {'key' => key, 'oldThreadId' => old_id, 'newThreadId' => next_id, 'oldProvider' => 'openai', 'newProvider' => 'openai'}
          row.merge!('previousOperationId' => operation, 'previousEvidenceDigest' => digest) if operation
          e = {'schema' => 1, 'operationId' => next_op, 'workspace' => state['workspace'], 'root' => state['root'], 'socket' => state['socket'], 'previousBindingRevision' => n, 'committedBindingRevision' => n + 1, 'snapshotDigest' => 'a' * 64, 'participants' => [row]}
          bytes = JSON.generate(e)
          evidence[next_op] = bytes
          old_id, operation, digest = next_id, next_op, Digest::SHA256.hexdigest(bytes)
        end
        state['bindingRevision'] = state['bindings'][key['slug']]['revision'] = count + 1
        state['bindings'][key['slug']]['conversations'][key['originalThreadId']] = {'member' => key['member'], 'threadId' => old_id, 'provider' => 'openai', 'operationId' => operation, 'evidenceDigest' => digest}
        targets = B.receipt_targets(state, key, true, ->(id) { evidence.fetch(id) })
        assert_equal count + 1, targets.length if count == B::RETENTION_LIMIT
      end
    else
      flunk 'unknown shared bound'
    end
  end

  def test_production_admission_remains_off
    Dir.mktmpdir('provider-bindings-test-') do |dir|
      host = DevWorkspaceHost::Host.allocate
      host.instance_variable_set(:@state, dir)
      host.define_singleton_method(:instance_runtime) { |_entry| {codex: '/runtime/codex.sock'} }
      entry = {'name' => 'work', 'root' => '/workspace'}
      path = File.join(dir, 'providers/work/state.json')
      FileUtils.mkdir_p(File.dirname(path), mode: 0o700)
      [1, 2, 3].each do |schema|
        state = Marshal.load(Marshal.dump(CORPUS.fetch('base').fetch('state')))
        state['schema'] = schema
        state.delete('bindingRevision') if schema < 3
        state.delete('bindings') if schema < 3
        state.delete('sessions') if schema == 1
        File.write(path, JSON.generate(state), mode: 'w', perm: 0o600)
        if schema == 3
          assert_raises(DevWorkspaceHost::Error) { host.send(:provider_state, entry) }
        else
          assert_equal schema, host.send(:provider_state, entry)['schema']
        end
      end
    end
  end

  def test_bounds_and_failed_candidate_preserve_input
    doc = Marshal.load(Marshal.dump(CORPUS.fetch('base')))
    assert_raises(B::Error) { B.decode_binding_state(JSON.generate(doc['state']) + ' ' * B::STATE_LIMIT, workspace: 'work', root: '/workspace', socket: '/runtime/codex.sock', catalog: doc['catalog']) }
    assert_raises(B::Error) { B.decode_evidence(doc['state'], ' ' * (B::EVIDENCE_LIMIT + 1), 'x', 'a' * 64) }
    c = doc['commit']
    e = JSON.parse(doc['commitEvidence'])
    c['updates'][0]['threadId'] = e['participants'][0]['newThreadId'] = 'beta-a'
    before = Marshal.dump(doc['state'])
    assert_raises(B::Error) { B.prepare_binding_commit(doc['state'], expected_revision: c['expectedRevision'], participants: c['participants'], updates: c['updates'], selection: c['selection'], evidence: JSON.generate(e), catalog: doc['catalog']) }
    assert_equal before, Marshal.dump(doc['state'])
  end

  def test_seal_hashes_exact_stored_bytes
    doc = CORPUS.fetch('base')
    assert_raises(B::Error) { B.receipt_targets(doc['state'], doc['key'], true, ->(id) { doc['evidence'].fetch(id) + "\n" }) }
  end
end

class ProviderBindingsLegacyPresenceTest < Minitest::Test
  B = DevWorkspaceHost::ProviderBindings

  def test_prospective_contract_does_not_relax_actual_legacy_ruby_readers
    Dir.mktmpdir('provider-bindings-legacy-') do |dir|
      host = DevWorkspaceHost::Host.allocate
      host.instance_variable_set(:@state, dir)
      host.define_singleton_method(:instance_runtime) { |_entry| {codex: '/runtime/codex.sock'} }
      entry = {'name' => 'work', 'root' => '/workspace'}
      path = File.join(dir, 'providers/work/state.json')
      FileUtils.mkdir_p(File.dirname(path), mode: 0o700)
      [1, 2].each do |schema|
        state = {'schema' => schema, 'workspace' => 'work', 'root' => '/workspace', 'socket' => '/runtime/codex.sock', 'active' => 'openai', 'receipts' => {'44444444-4444-4444-8444-444444444444' => {'target' => 'pool', 'catalogDigest' => 'd' * 64, 'session' => '', 'inherit' => false}}}
        File.write(path, JSON.generate(state), mode: 'w', perm: 0o600)
        assert_raises(DevWorkspaceHost::Error) { host.send(:provider_state, entry) }
        state.delete('receipts')
        if schema == 2
          state['sessions'] = {'alpha' => {'provider' => 'openai', 'rootThreadId' => '', 'creationId' => 'e' * 64}}
          File.write(path, JSON.generate(state), mode: 'w', perm: 0o600)
          assert_raises(DevWorkspaceHost::Error) { host.send(:provider_state, entry) }
        end
      end
    end
  end
end
