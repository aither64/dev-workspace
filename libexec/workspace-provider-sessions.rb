# frozen_string_literal: true

# Schema-1 requests retain their original recovery engine. New requests use a
# shared daemon and reserve only their session scope, never the package lock.
module DevWorkspaceHost
  module CodexProviderHost
    private

    class ProviderCanceled < StandardError; end

    # Dormant fork-v1 writer. No command/worker dispatcher calls these methods
    # until the matching consumers and capability boundary are complete.
    def provider_fork_state(entry)
      raw = read_private_json_file!(provider_state_path(entry), 'fork provider state', maximum: ProviderBindings::STATE_LIMIT)
      state = ProviderBindings.decode_binding_state(raw, workspace: entry.fetch('name'), root: entry.fetch('root'),
        socket: instance_runtime(entry).fetch(:codex), catalog: provider_catalog.fetch('catalog'))
      [JSON.parse(JSON.generate(state)), raw]
    end

    def provider_fork_binding_write_allowed!(state, operation = nil)
      o = state['operation']
      if o && o['strategy'] == 'fork-v1' && o['stage'] == 'committing' && o['id'] != operation
        raise Error, 'sealed provider commit requires its original operation'
      end
    end

    def provider_fork_origin_reservation(state, participants)
      b = ProviderBindings
      catalog = provider_catalog.fetch('catalog')
      b.validate_bindings(state, catalog: catalog)
      candidate = JSON.parse(JSON.generate(state))
      changed, keys, sessions = {}, [], {}
      participants.each do |current|
        b.shape!(current, b::CURRENT)
        k = current.fetch('key')
        selection = state.fetch('sessions', {})[k['slug']]
        effective = selection && selection['rootThreadId'] == k['rootThreadId'] ? selection['provider'] : state['active']
        b.refuse!('unproved origin reservation') unless b.key?(k) && k['workspace'] == state['root'] && current['eligible'] &&
          current['socket'] == state['socket'] && !keys.include?(k) && current['provider'] == effective
        keys << k
        old = state.fetch('bindings', {})[k['slug']]
        b.refuse!('origin reservation revision changed') unless current['expectedRevision'] == (old ? old['revision'] : 0)
        if old
          b.refuse!('origin incarnation changed') unless old.values_at('rootThreadId', 'incarnation') == k.values_at('rootThreadId', 'incarnation')
        end
        if old && old['conversations'].key?(k['originalThreadId'])
          b.resolve_current(state, current)
        else
          b.refuse!('required original binding missing') if current['bindingRequired']
          candidate['bindings'] ||= {}
          session = candidate['bindings'][k['slug']] ||= {'rootThreadId' => k['rootThreadId'], 'incarnation' => k['incarnation'], 'conversations' => {}}
          session['conversations'][k['originalThreadId']] = {'member' => k['member'], 'threadId' => k['originalThreadId'], 'provider' => current['provider']}
          changed[k['slug']] = true
        end
        sessions[k['slug']] ||= []
        sessions[k['slug']] << k
      end
      b.refuse!('frozen scope has no original lead') unless !participants.empty? && sessions.values.all? { |rows| rows.any? { |k| k['member'] == 'lead' } }
      unless changed.empty?
        revision = state.fetch('bindingRevision', 0)
        b.refuse!('binding revision exhausted') unless revision < b::UINT64_MAX
        candidate['schema'], candidate['bindingRevision'] = 3, revision + 1
        changed.each_key { |slug| candidate['bindings'][slug]['revision'] = revision + 1 }
      end
      b.validate_bindings(candidate, catalog: catalog)
      # Reserve room for every maximum-size returned ID and seal reference before
      # the first fork. Nothing is pruned to make a transition fit.
      b.refuse!('intended collective binding exceeds bounds') if JSON.generate(candidate).bytesize + 1024 + participants.length * (512 + 36 + 64 + 128) > b::STATE_LIMIT
      candidate
    end

    def provider_fork_proof(entry, operation, path, digest, verified: false)
      expected = File.join(provider_directory(entry), "#{operation}.snapshot.json") + (verified ? '.verified' : '')
      raise Error, 'fork proof identity changed' unless operation.match?(PROVIDER_OPERATION_ID) && path == expected && digest.match?(SHA256_HEX)
      raw = read_private_json_file!(path, 'fork source proof', maximum: ProviderBindings::EVIDENCE_LIMIT)
      raise Error, 'fork proof digest changed' unless Digest::SHA256.hexdigest(raw) == digest
      fields = %w[schema strategy operationId workspace root socket catalogDigest sourcePackage]
      fields += verified ? %w[sourceSnapshotDigest proofs] : %w[previousStateDigest state selection sessions socketIdentity rosters participants]
      proof = ProviderBindings.parse(raw, ProviderBindings::EVIDENCE_LIMIT, Hash)
      raise Error, 'fork proof fields changed' unless proof.keys.sort == fields.sort
      raise Error, 'fork proof ownership changed' unless proof['schema'] == 1 && proof['strategy'] == 'fork-v1' &&
        proof['operationId'] == operation && proof['workspace'] == entry.fetch('name') && proof['root'] == entry.fetch('root') &&
        proof['socket'] == instance_runtime(entry).fetch(:codex) && proof['catalogDigest'] == provider_catalog.fetch('digest') &&
        proof['sourcePackage'] == preselection_session_package
      [JSON.parse(raw), raw]
    end

    def provider_fork_operation!(state, source)
      fields = %w[id strategy target catalogDigest session sessions roots inherit snapshot]
      raise Error, 'fork operation or scope changed' unless state['operation'] && source['state']['operation'] &&
        state['operation'].values_at(*fields) == source['state']['operation'].values_at(*fields)
    end

    def provider_fork_journal(entry, operation, source, digest)
      path = File.join(provider_directory(entry), "#{operation}.snapshot.json.journal")
      raw = read_private_json_file!(path, 'fork journal', maximum: ProviderBindings::EVIDENCE_LIMIT)
      journal = ProviderBindings.decode_fork_journal(raw, source: source.fetch('state'), catalog: provider_catalog.fetch('catalog'))
      raise Error, 'fork journal source changed' unless journal['sourceSnapshotDigest'] == digest && journal['sourcePackage'] == source['sourcePackage']
      [JSON.parse(raw), path]
    end

    # Called synchronously by the Go runtime/team lock owner. This callback owns
    # request.lock only; it never reacquires runtime/team locks or trusts a supplied
    # replacement state. Its acknowledgement follows the durable fence write.
    def provider_fork_reserve(entry, operation, path, digest)
      require_current_package_generation!
      proof, = provider_fork_proof(entry, operation, path, digest)
      provider_locked(entry) do
        state, raw = provider_fork_state(entry)
        provider_fork_binding_write_allowed!(state, operation)
        raise Error, 'fork operation changed' unless state.dig('operation', 'id') == operation && state.dig('operation', 'strategy') == 'fork-v1'
        if state == proof['state']
          journal, = provider_fork_journal(entry, operation, proof, digest)
          return journal # Exact acknowledged reservation; no native replay.
        end
        raise Error, 'fork source changed before reservation' unless Digest::SHA256.hexdigest(raw) == proof['previousStateDigest'] && state.dig('operation', 'phase') == 'waiting'
        participants = proof.fetch('participants').map { |p| p.fetch('current') }
        candidate = provider_fork_origin_reservation(state, participants)
        candidate['operation'] = state.fetch('operation').merge('phase' => 'applying', 'stage' => 'preparing', 'snapshot' => path)
        raise Error, 'fork origin candidate differs from locked proof' unless candidate == proof.fetch('state')
        rows = proof.fetch('participants').map do |p|
          current = JSON.parse(JSON.generate(p.fetch('current')))
          current['bindingRequired'] = true
          current['expectedRevision'] = candidate.fetch('bindings').fetch(current['key']['slug']).fetch('revision')
          resolved = ProviderBindings.resolve_current(candidate, current)
          raise Error, 'fork source thread changed' unless resolved['threadId'] == p.fetch('thread').fetch('id')
          {'current' => current, 'sourceThreadId' => resolved['threadId'], 'state' => 'not-attempted'}
        end
        journal = {'schema' => 1, 'strategy' => 'fork-v1', 'operationId' => operation, 'workspace' => proof['workspace'], 'root' => proof['root'],
          'socket' => proof['socket'], 'catalogDigest' => proof['catalogDigest'], 'sourcePackage' => proof['sourcePackage'], 'selection' => proof.fetch('selection'),
          'sessions' => proof.fetch('sessions'), 'sourceSnapshotDigest' => digest, 'reservationRevision' => candidate.fetch('bindingRevision'),
          'stage' => 'preparing', 'participants' => rows, 'projections' => proof.fetch('sessions').to_h { |slug| [slug, %w[manifest authority recovery terminal].to_h { |key| [key, false] }] }}
        ProviderBindings.decode_fork_journal(JSON.generate(journal), source: candidate, catalog: provider_catalog.fetch('catalog'))
        raise Error, 'intended fork journal exceeds bounds' if JSON.generate(journal).bytesize + rows.length * (512 + 64 + 128) > ProviderBindings::EVIDENCE_LIMIT
        # Journal intent precedes the first binding/requirement mutation. Its
        # exact candidate can be published forward after interruption.
        journal_path = path + '.journal'
        if File.exist?(journal_path)
          saved = JSON.parse(read_private_json_file!(journal_path, 'fork reservation journal', maximum: ProviderBindings::EVIDENCE_LIMIT))
          raise Error, 'fork reservation journal changed' unless saved == journal
        else
          atomic_private_file(journal_path, JSON.generate(journal))
        end
        provider_write_state(entry, candidate)
        journal
      end
    end

    def provider_fork_commit(entry, operation, path, digest)
      require_current_package_generation!
      verified, = provider_fork_proof(entry, operation, path, digest, verified: true)
      source_path = File.join(provider_directory(entry), "#{operation}.snapshot.json")
      source_digest = verified.fetch('sourceSnapshotDigest')
      source, = provider_fork_proof(entry, operation, source_path, source_digest)
      provider_locked(entry) do
        state, = provider_fork_state(entry)
        provider_fork_binding_write_allowed!(state, operation)
        raise Error, 'fork commit operation changed' unless state.dig('operation', 'id') == operation && state.dig('operation', 'strategy') == 'fork-v1'
        journal, journal_path = provider_fork_journal(entry, operation, source, source_digest)
        provider_fork_operation!(state, source)
        rows = journal.fetch('participants')
        raise Error, 'fork verification is incomplete' unless rows.all? { |p| p['state'] == 'verified' } && verified.fetch('proofs').length == rows.length
        rows.zip(verified.fetch('proofs')).each do |p, bytes|
          raise Error, 'fork verification bytes changed' unless Digest::SHA256.hexdigest(bytes) == p['verificationDigest']
          v = ProviderBindings.parse(bytes, ProviderBindings::EVIDENCE_LIMIT,
            {'key' => ProviderBindings::KEY, 'sourceThreadId' => String, 'childThreadId' => String, 'provider' => String,
             'historyDigest' => String, 'settingsDigest' => String, 'titleDigest' => String})
          raise Error, 'fork verification digest is invalid' unless %w[historyDigest settingsDigest titleDigest].all? { |key| ProviderBindings.digest?(v[key]) }
          raise Error, 'fork verification identity changed' unless v.values_at('key', 'sourceThreadId', 'childThreadId', 'provider') ==
            [p['current']['key'], p['sourceThreadId'], p['childThreadId'], journal['selection']['provider']]
        end
        seal_path = source_path + '.evidence'
        if File.exist?(seal_path)
          sealed = read_private_json_file!(seal_path, 'sealed fork evidence', maximum: ProviderBindings::EVIDENCE_LIMIT)
          seal = JSON.parse(sealed)
          evidence_state = state.merge('bindingRevision' => [state.fetch('bindingRevision'), seal.fetch('committedBindingRevision')].max)
          seal = ProviderBindings.decode_evidence(evidence_state, sealed, operation, Digest::SHA256.hexdigest(sealed))
          expected_rows = rows.map do |p|
            k = p['current']['key']; prior = source.fetch('state').fetch('bindings').fetch(k['slug']).fetch('conversations').fetch(k['originalThreadId'])
            row = {'key' => k, 'oldThreadId' => p['sourceThreadId'], 'newThreadId' => p['childThreadId'],
              'oldProvider' => p['current']['provider'], 'newProvider' => journal['selection']['provider']}
            row.merge!('previousOperationId' => prior['operationId'], 'previousEvidenceDigest' => prior['evidenceDigest']) if prior['operationId']
            row
          end
          raise Error, 'fork sealed snapshot or participants changed' unless seal['snapshotDigest'] == digest && seal['participants'] == expected_rows
          committed = rows.all? do |p|
            k = p['current']['key']; c = state.dig('bindings', k['slug'], 'conversations', k['originalThreadId'])
            c && c.values_at('threadId', 'provider', 'operationId', 'evidenceDigest') ==
              [p['childThreadId'], journal['selection']['provider'], operation, Digest::SHA256.hexdigest(sealed)] &&
              state.dig('bindings', k['slug'], 'revision') == seal['committedBindingRevision']
          end
          selection = journal['selection']
          selected = selection['scope'] == 'workspace' ? state['active'] == selection['provider'] :
            (selection['inherit'] ? !state.fetch('sessions', {}).key?(selection['session']) && state['active'] == selection['provider'] :
              state.dig('sessions', selection['session']) == {'rootThreadId' => state['operation']['roots'][selection['session']], 'provider' => selection['provider']})
          if committed && selected && state.dig('operation', 'stage') == 'restoring-terminals'
            journal['stage'] = 'restoring-terminals'
            atomic_private_file(journal_path, JSON.generate(journal))
            return state
          end
          raise Error, 'sealed fork commit has a third state' unless state['bindingRevision'] == seal['previousBindingRevision'] && state.dig('operation', 'stage') == 'committing'
        end
        rows.each { |p| ProviderBindings.resolve_current(state, p['current']) }
        state['operation']['stage'] = 'committing'
        provider_write_state(entry, state) # Survives a seal-before-state crash.
        journal['stage'] = 'committing'
        atomic_private_file(journal_path, JSON.generate(journal))
        unless sealed
          participants = rows.map do |p|
            k = p['current']['key']; prior = state.fetch('bindings').fetch(k['slug']).fetch('conversations').fetch(k['originalThreadId'])
            row = {'key' => k, 'oldThreadId' => p['sourceThreadId'], 'newThreadId' => p['childThreadId'],
              'oldProvider' => p['current']['provider'], 'newProvider' => journal['selection']['provider']}
            row.merge!('previousOperationId' => prior['operationId'], 'previousEvidenceDigest' => prior['evidenceDigest']) if prior['operationId']
            row
          end
          sealed = JSON.generate('schema' => 1, 'operationId' => operation, 'workspace' => source['workspace'], 'root' => source['root'], 'socket' => source['socket'],
            'previousBindingRevision' => state.fetch('bindingRevision'), 'committedBindingRevision' => state.fetch('bindingRevision') + 1,
            'snapshotDigest' => digest, 'participants' => participants)
          # The request mutex and durable committing fence exclude all binding
          # writers. Existing sealed bytes are never rewritten on recovery.
          atomic_private_file(seal_path, sealed)
        end
        updates = rows.map { |p| {'key' => p['current']['key'], 'threadId' => p['childThreadId'], 'provider' => journal['selection']['provider']} }
        candidate = ProviderBindings.prepare_binding_commit(state, expected_revision: state.fetch('bindingRevision'), participants: rows.map { |p| p['current'] },
          updates: updates, selection: journal.fetch('selection'), evidence: sealed, catalog: provider_catalog.fetch('catalog'))
        candidate['operation']['stage'] = 'restoring-terminals'
        provider_write_state(entry, candidate) # The sole collective route write.
        journal['stage'] = 'restoring-terminals'
        atomic_private_file(journal_path, JSON.generate(journal))
        candidate
      end
    end

    def provider_fork_worker_ownership(entry, operation)
      with_current_generation_lock(mode: File::LOCK_SH) do
        directory = provider_directory(entry)
        File.open(File.join(directory, 'fork-worker.lock'), File::RDWR | File::CREAT, 0o600) do |worker|
          raise Error, 'fork worker is already owned' unless worker.flock(File::LOCK_EX | File::LOCK_NB)
          admission = File.open(File.join(directory, 'creation-admission.lock'), File::RDWR | File::CREAT, 0o600) unless operation['session']
          begin
            raise Error, 'session creation is still admitted' if admission && !admission.flock(File::LOCK_EX | File::LOCK_NB)
            # Never hold request.lock across the Go lock owner or native work.
            yield
          ensure
            admission&.close
          end
        end
      end
    end

    def provider_fork_selection_only(entry, operation, selection, participants)
      require_current_package_generation!
      provider_locked(entry) do
        state, = provider_fork_state(entry)
        provider_fork_binding_write_allowed!(state)
        b = ProviderBindings
        b.shape!(selection, b::SELECTION_CHANGE)
        catalog = provider_catalog
        raise Error, 'selection-only operation changed' unless operation['id'].is_a?(String) && operation['id'].match?(PROVIDER_OPERATION_ID) &&
          operation['target'] == selection['provider'] && operation['catalogDigest'] == catalog['digest']
        receipt = {'target' => selection['provider'], 'catalogDigest' => catalog['digest']}
        receipt['session'] = selection['session'] if selection['session']
        receipt['inherit'] = true if selection['inherit']
        if (old = state.fetch('receipts', {})[operation['id']])
          raise Error, 'selection-only UUID reused with another scope' unless old == receipt
          return state
        end
        raise Error, 'another provider operation is unfinished' if %w[waiting applying recovery-required].include?(state.dig('operation', 'phase'))
        if participants.empty?
          raise Error, 'empty selection requires workspace-default scope' unless selection['scope'] == 'workspace' && !selection.key?('session') && !selection['inherit']
          raise Error, 'selection provider unavailable' unless catalog['catalog']['providers'].any? { |p| p['id'] == selection['provider'] }
          candidate = Marshal.load(Marshal.dump(state))
          candidate['active'] = selection['provider']
        else
          updates = participants.map do |current|
            r = b.resolve_current(state, current)
            raise Error, 'selection-only operation changes effective provider' unless r['provider'] == selection['provider']
            {'key' => current['key'], 'threadId' => r['threadId'], 'provider' => r['provider']}
          end
          candidate = b.prepare_binding_commit(state, expected_revision: state.fetch('bindingRevision', 0), participants: participants,
            updates: updates, selection: selection, catalog: catalog['catalog'])
        end
        raise Error, 'provider receipt retention exhausted' if state.fetch('receipts', {}).length >= 2048
        candidate['receipts'] ||= {}
        candidate['receipts'][operation['id']] = receipt
        candidate['operation'] = operation.slice('id', 'target', 'catalogDigest', 'createdAt', 'updatedAt').merge('phase' => 'applied', 'blockers' => [])
        b.validate_bindings(candidate, catalog: catalog['catalog'])
        provider_write_state(entry, candidate)
        candidate
      end
    end

    def provider_fork_control(entry, operation)
      path = File.join(provider_directory(entry), "#{operation}.control.json")
      return {} unless File.exist?(path)
      control = ProviderBindings.parse(read_private_json_file!(path, 'fork control', maximum: 4096), 4096,
        {'operation' => String, 'cancelRequested?' => :bool, 'dismissed?' => :bool})
      raise Error, 'fork control ownership changed' unless control['operation'] == operation
      control
    end

    # The same reserve action has one explicit first-intent step. A replay is
    # never dispatch permission, even if the earlier acknowledgement was lost.
    def provider_fork_first_intent(entry, operation, path, digest)
      require_current_package_generation!
      source, = provider_fork_proof(entry, operation, path, digest)
      provider_locked(entry) do
        state, = provider_fork_state(entry)
        journal, journal_path = provider_fork_journal(entry, operation, source, digest)
        provider_fork_operation!(state, source)
        raise Error, 'fork intent is already unknown; do not repeat it' unless journal['stage'] == 'preparing' && journal['participants'].all? { |p| p['state'] == 'not-attempted' }
        raise ProviderCanceled if provider_fork_control(entry, operation)['cancelRequested'] || state.dig('operation', 'phase') == 'canceled'
        raise Error, 'fork preparing fence changed' unless state.dig('operation', 'id') == operation && state.dig('operation', 'phase') == 'applying' && state.dig('operation', 'stage') == 'preparing'
        raise Error, 'fork origin projections are incomplete' unless journal['projections'].values.all? { |p| p['manifest'] && p['authority'] }
        journal['participants'].each { |p| ProviderBindings.resolve_current(state, p['current']) }
        journal['stage'], journal['participants'].first['state'] = 'forking', 'intent'
        atomic_private_file(journal_path, JSON.generate(journal)) # Cancellation observes this cutoff first.
        state['operation']['stage'] = 'forking'
        provider_write_state(entry, state)
        journal
      end
    end

    def provider_fork_cancel(entry, operation, path, digest)
      source, = provider_fork_proof(entry, operation, path, digest)
      provider_locked(entry) do
        state, = provider_fork_state(entry)
        journal, = provider_fork_journal(entry, operation, source, digest)
        provider_fork_operation!(state, source)
        raise Error, 'fork has crossed its native intent cutoff' unless state.dig('operation', 'id') == operation &&
          journal['participants'].all? { |p| p['state'] == 'not-attempted' }
        atomic_private_file(File.join(provider_directory(entry), "#{operation}.control.json"), JSON.generate('operation' => operation, 'cancelRequested' => true))
        ready = journal['projections'].values.all? { |p| p['manifest'] && p['authority'] }
        provider_finish(state, ready ? 'canceled' : 'recovery-required', ready ? 'Provider switch canceled.' : 'Finish the original origin projections before reopening the canceled scope.')
        provider_write_state(entry, state)
      end
    end

    # R2-C supplies idempotent owning projectors. At this checkpoint no real
    # manifest/authority/recovery/terminal projector or command route is wired.
    def provider_fork_project(entry, operation, path, digest, slug, kind)
      source, = provider_fork_proof(entry, operation, path, digest)
      before_commit = %w[manifest authority].include?(kind)
      check = lambda do |state, journal|
        provider_fork_operation!(state, source)
        raise Error, 'fork projection scope changed' unless journal['sessions'].include?(slug) && %w[manifest authority recovery terminal].include?(kind)
        if before_commit
          raise Error, 'origin projection follows native intent' unless journal['stage'] == 'preparing' && journal['participants'].all? { |p| p['state'] == 'not-attempted' }
          journal['participants'].each { |p| ProviderBindings.resolve_current(state, p['current']) }
        else
          raise Error, 'terminal projection precedes collective commit' unless journal['stage'] == 'restoring-terminals' && state.dig('operation', 'stage') == 'restoring-terminals'
          journal['participants'].each do |p|
            k = p['current']['key']; binding = state.dig('bindings', k['slug'])
            current = binding && binding['conversations'][k['originalThreadId']]
            raise Error, 'committed fork target changed' unless current && binding.values_at('rootThreadId', 'incarnation') == k.values_at('rootThreadId', 'incarnation') &&
              current.values_at('threadId', 'provider', 'operationId') == [p['childThreadId'], journal['selection']['provider'], operation]
          end
        end
        prior = {'authority' => 'manifest', 'recovery' => 'authority', 'terminal' => 'recovery'}[kind]
        raise Error, 'fork projection order changed' if prior && !journal['projections'][slug][prior]
        journal['sessions'].to_h { |session| [session, state.fetch('bindings').fetch(session)] }
      end
      frozen = provider_locked(entry) do
        state, = provider_fork_state(entry)
        journal, = provider_fork_journal(entry, operation, source, digest)
        return if journal['projections'].fetch(slug).fetch(kind)
        check.call(state, journal)
      end
      # The existing runtime/team owner excludes affected mutations. Never keep
      # the request mutex around a terminal or filesystem projection effect.
      yield
      provider_locked(entry) do
        state, = provider_fork_state(entry)
        journal, journal_path = provider_fork_journal(entry, operation, source, digest)
        raise Error, 'fork projection binding changed' unless check.call(state, journal) == frozen
        journal['projections'][slug][kind] = true
        atomic_private_file(journal_path, JSON.generate(journal))
        if !before_commit && journal['projections'].values.all? { |row| row.values.all? }
          provider_finish(state, 'applied')
          provider_write_state(entry, state)
        end
      end
    rescue StandardError
      provider_locked(entry) do
        state, = provider_fork_state(entry)
        if state.dig('operation', 'id') == operation && state.dig('operation', 'stage') == 'restoring-terminals'
          provider_finish(state, 'recovery-required', 'Provider changed. Finish the original projection; terminal clients may need dev-session sync.')
          provider_write_state(entry, state)
        end
      end
      raise
    end

    def provider_scoped_cancelable?(operation)
      operation['phase'] == 'waiting' ||
        operation['strategy'] == 'scoped-v1' && operation['phase'] == 'applying' && %w[quiescing preparing].include?(operation['stage'])
    end

    def provider_control(entry, action, argv)
      raise Error, 'provider control requires the exact operation UUID' unless argv.length == 1 && argv.first.match?(PROVIDER_OPERATION_ID)
      with_current_generation_lock(mode: File::LOCK_SH) do
        provider_locked(entry) do
          state = provider_state(entry)
          operation = state['operation']
          raise Error, 'provider operation identity changed' unless operation && operation['id'] == argv.first
          if action == 'dismiss'
            raise Error, 'an unfinished provider switch cannot be dismissed' unless %w[failed canceled applied].include?(operation['phase'])
            operation['dismissed'] = true
          elsif operation['phase'] == 'waiting'
            provider_finish(state, 'canceled', 'Provider switch canceled.')
          elsif operation['phase'] == 'canceled'
            # An exact cancellation retry is idempotent.
          elsif operation['strategy'] == 'scoped-v1' && provider_scoped_cancelable?(operation)
            operation['cancelRequested'] = true
          else
            raise Error, 'provider preparation has ended; inspect its result or recovery action'
          end
          provider_write_state(entry, state)
        end
      end
      @out.puts JSON.generate(provider_public_status(entry))
    end

    def provider_scoped_check_cancel!(entry, operation)
      current = provider_locked(entry) { provider_state(entry).fetch('operation') }
      raise Error, 'scoped provider operation identity changed' unless current['id'] == operation['id']
      raise ProviderCanceled if current['cancelRequested'] || current['phase'] == 'canceled'
    end

    # This short transaction is the cancellation cutoff. It precedes the first
    # native close and shares the request mutex with cancellation admission.
    def provider_scoped_begin_switch!(entry, state)
      provider_locked(entry) do
        current = provider_state(entry)
        operation = current.fetch('operation')
        raise Error, 'scoped provider operation identity changed' unless operation['id'] == state.dig('operation', 'id')
        raise ProviderCanceled if operation['cancelRequested']
        raise Error, 'provider preparation state changed' unless operation['phase'] == 'applying' && operation['stage'] == 'preparing'
        state.fetch('operation')['stage'] = 'switching'
        current['operation'] = state.fetch('operation')
        provider_write_state(entry, current)
        state.replace(current)
      end
    end

    def provider_thread_identity?(value)
      value.is_a?(String) && !value.empty? && value.bytesize <= 512 && !value.match?(/[\x00-\x20\x7f]/)
    end

    def validate_scoped_provider_operation!(state)
      operation = state.fetch('operation')
      sessions, roots = operation.values_at('sessions', 'roots')
      unless state['schema'] == 2 && operation['strategy'] == 'scoped-v1' &&
             %w[accepted waiting-creation waiting-package quiescing preparing switching resuming restoring-terminals].include?(operation['stage']) &&
             operation['target'].is_a?(String) && operation['target'].match?(PROVIDER_ID) &&
             operation['catalogDigest'].is_a?(String) && operation['catalogDigest'].match?(SHA256_HEX) &&
             %w[cancelRequested dismissed].all? { |key| !operation.key?(key) || [true, false].include?(operation[key]) } &&
             sessions.is_a?(Array) && sessions.length <= 4096 && sessions.uniq == sessions &&
             sessions.all? { |slug| slug.is_a?(String) && slug.match?(PROVIDER_ID) } &&
             roots.is_a?(Hash) && roots.keys.sort == sessions.sort && roots.values.all? { |root| provider_thread_identity?(root) } &&
             (!operation['session'] || sessions == [operation['session']]) &&
             (!operation.key?('inherit') || [true, false].include?(operation['inherit']) && (!operation['inherit'] || operation['session'])) &&
             (!%w[applying recovery-required].include?(operation['phase']) || %w[quiescing preparing switching resuming restoring-terminals].include?(operation['stage']))
        raise Error, 'invalid scoped provider operation'
      end
    end

    def provider_command(argv)
      action, name = argv.first(2)
      return legacy_provider_command(argv) unless %w[request native-args bind-creation bind-root recover models cancel dismiss].include?(action)

      argv = argv.drop(2)
      entry = registry.find(name.to_s)
      raise Error, 'provider operation requires a registered workspace' unless entry
      return provider_control(entry, action, argv) if %w[cancel dismiss].include?(action)
      if action == 'models'
        raise Error, 'provider models requires a catalog provider ID' unless argv.length == 1
        target = argv.first
        raise Error, 'model provider is unavailable' unless provider_catalog.fetch('catalog').fetch('providers').any? { |p| p.fetch('id') == target }
        with_current_generation_lock(mode: File::LOCK_SH) do
          provider_private_server(entry, target, process_group: false) do |socket|
            models = provider_helper(entry, 'models', socket:)
            @out.puts JSON.generate(models)
          end
        end
        return
      end
      if action == 'native-args'
        raise Error, 'provider native-args takes an optional session slug' unless argv.length <= 1
        require_current_package_generation!
        selected = argv.empty? ? nil : provider_session_selection(entry, provider_state(entry), argv.first)
        @out.puts JSON.generate('argv' => provider_argv(entry, selected:))
        return
      end
      return provider_bind_creation(entry, argv) if action == 'bind-creation'
      return provider_bind_root(entry, argv) if action == 'bind-root'
      if action == 'recover'
        raise Error, 'provider recovery takes an optional exact operation UUID' unless argv.empty? || argv.length == 1 && argv.first.match?(PROVIDER_OPERATION_ID)
        with_current_generation_lock(mode: File::LOCK_SH) do
          provider_locked(entry) do
            state = provider_state(entry)
            raise Error, 'provider recovery operation identity changed' if argv.first && state.dig('operation', 'id') != argv.first
            if state.dig('operation', 'strategy') == 'scoped-v1' && state.dig('operation', 'phase') == 'recovery-required'
              state.fetch('operation')['phase'] = 'applying'
              provider_write_state(entry, state)
            end
          end
          system! 'systemctl', '--user', 'start', provider_worker_unit(entry)
        end
        return
      end

      inherit = argv.last == '--inherit'
      argv = argv[0...-1] if inherit
      unless argv.length == 2 || argv.length == 4 && argv[2] == '--session'
        raise Error, 'provider request requires provider ID, operation UUID and optional --session SLUG'
      end
      target, identity, _, slug = argv
      raise Error, 'inheriting the workspace default requires a session' if inherit && !slug
      raise Error, 'invalid provider operation UUID' unless identity.match?(PROVIDER_OPERATION_ID)
      raise Error, 'invalid provider session slug' if slug && !slug.match?(PROVIDER_ID)
      with_current_generation_lock(mode: File::LOCK_SH | File::LOCK_NB) do
        catalog = provider_catalog
        raise Error, 'provider is unavailable' unless catalog.fetch('catalog').fetch('providers').any? { |p| p.fetch('id') == target }
        provider_locked(entry) do
          state = provider_state(entry, catalog)
          receipts = state['receipts'] ||= {}
          if (saved = receipts[identity])
            raise Error, 'provider request UUID has a different target or scope' unless saved['target'] == target && saved['session'] == slug && (saved['inherit'] == true) == inherit
          else
            if %w[waiting applying recovery-required].include?(state.dig('operation', 'phase'))
              raise Error, 'another provider request is pending; inspect its progress or cancel it'
            end
            sessions = slug ? provider_scoped_sessions(entry, state, slug) : {}
            raise Error, 'workspace default changed; refresh before selecting it' if inherit && target != state['active']
            raise Error, 'provider request retention limit reached' if receipts.length >= 2048
            state['schema'] = 2
            receipts[identity] = {'target' => target, 'catalogDigest' => catalog.fetch('digest'), 'session' => slug}.compact
            receipts[identity]['inherit'] = true if inherit
            now = Time.now.utc.iso8601
            state['operation'] = {
              'id' => identity, 'target' => target, 'catalogDigest' => catalog.fetch('digest'),
              'phase' => 'waiting', 'strategy' => 'scoped-v1', 'session' => slug,
              'sessions' => sessions.keys, 'stage' => 'accepted', 'blockers' => [],
              'roots' => sessions,
              'createdAt' => now, 'updatedAt' => now
            }.compact
            state['operation']['inherit'] = true if inherit
            current = slug ? provider_session_selection(entry, state, slug) : state.fetch('active')
            if current == target
              if slug
                state['sessions'] ||= {}
                if inherit
                  state['sessions'].delete(slug)
                else
                  state['sessions'][slug] = {'provider' => target, 'rootThreadId' => sessions.fetch(slug)}
                end
              end
              provider_finish(state, 'applied')
            end
            provider_write_state(entry, state)
          end
        end
        system! 'systemctl', '--user', 'enable', '--now', provider_worker_unit(entry)
      end
      @out.puts JSON.generate(provider_public_status(entry))
    end

    def provider_scoped_sessions(entry, state, selected_slug)
      sessions = {}
      paths = selected_slug ? [File.join(entry.fetch('root'), 'work', selected_slug, 'portal.yml')] : Dir[File.join(entry.fetch('root'), 'work', '*', 'portal.yml')].sort
      paths.each do |path|
        slug = File.basename(File.dirname(path))
        next if selected_slug && slug != selected_slug
        manifest = read_portal_manifest(path)
        root = manifest.dig('codex', 'thread_id')
        next unless root && manifest.dig('codex', 'socket_path') == instance_runtime(entry).fetch(:codex)
        selection = state.fetch('sessions', {})[slug]
        next if !selected_slug && selection && selection['rootThreadId'] == root
        if !selected_slug && selection && selection['creationId'] && manifest.dig('creation', 'state') == 'creating'
          journal = provider_creation_journal(entry, slug)
          next if journal['state'] == 'creating' && journal['tmux_identity'] == selection['creationId']
        end
        sessions[slug] = root
      end
      raise Error, 'selected session has no retained Codex thread' if selected_slug && sessions.empty?
      sessions
    end

    def provider_session_selection(entry, state, slug)
      raise Error, 'invalid provider session slug' unless slug.match?(PROVIDER_ID)
      manifest = read_portal_manifest(File.join(entry.fetch('root'), 'work', slug, 'portal.yml'))
      selection = state.fetch('sessions', {})[slug]
      return state.fetch('active') unless selection
      return selection.fetch('provider') if selection['rootThreadId'] && selection['rootThreadId'] == manifest.dig('codex', 'thread_id')
      if selection['creationId'] && (journal = provider_creation_journal(entry, slug, required: false))
        return selection.fetch('provider') if journal['state'] == 'creating' && journal['tmux_identity'] == selection['creationId']
      end
      state.fetch('active')
    end

    # Forks have their own journal, but share the same provider incarnation.
    def provider_creation_journal(entry, slug, required: true)
      paths = %w[creation fork].map do |kind|
        File.join(entry.fetch('root'), 'worktrees', '.locks', "#{slug}.#{kind}.json")
      end.select { |path| File.exist?(path) }
      return nil if paths.empty? && !required
      raise Error, 'provider creation identity is unavailable or ambiguous' unless paths.length == 1

      journal = JSON.parse(read_private_json_file!(paths.first, 'provider creation identity', maximum: 64 * 1024))
      unless journal.is_a?(Hash) && journal['slug'] == slug && %w[creating ready].include?(journal['state']) &&
             journal['tmux_identity'].is_a?(String) && journal['tmux_identity'].match?(SHA256_HEX)
        raise Error, 'provider creation incarnation changed'
      end
      if paths.first.end_with?('.fork.json') &&
         !([1, 2].include?(journal['schema']) && (journal['schema'] == 1 || journal['provider'].is_a?(String) && journal['provider'].match?(PROVIDER_ID)))
        raise Error, 'invalid provider fork journal'
      end
      journal
    end

    # Called after the creation journal exists and before native creation. A
    # repeated call with no explicit selection retains the accepted incarnation.
    def provider_bind_creation(entry, argv)
      raise Error, 'provider bind-creation requires slug, incarnation and optional provider' unless [2, 3].include?(argv.length)
      slug, incarnation, selected = argv
      raise Error, 'invalid provider creation identity' unless slug.match?(PROVIDER_ID) && incarnation.match?(SHA256_HEX)
      with_current_generation_lock(mode: File::LOCK_SH) do
        provider_locked(entry) do
          state = provider_state(entry)
          journal = provider_creation_journal(entry, slug)
          raise Error, 'provider creation incarnation changed' unless journal['slug'] == slug && journal['tmux_identity'] == incarnation && journal['state'] == 'creating'
          raise Error, 'creation provider differs from its retained intent' if selected && selected != journal['provider']
          selected = journal['provider']
          old = state.fetch('sessions', {})[slug]
          return unless old || selected
          if old && old['creationId'] == incarnation
            raise Error, 'creation retry selected a different provider' if selected && old['provider'] != selected
            return
          end
          catalog = provider_catalog.fetch('catalog')
          if selected
            raise Error, 'creation provider is unavailable' unless catalog.fetch('providers').any? { |p| p.fetch('id') == selected }
          end
          state['schema'] = 2
          state['sessions'] ||= {}
          state['sessions'].delete(slug)
          state['sessions'][slug] = {'provider' => selected, 'creationId' => incarnation} if selected
          provider_write_state(entry, state)
        end
      end
    end

    def provider_operation_view(entry, operation)
      view = operation.slice('id', 'target', 'phase', 'blockers', 'message', 'stage', 'session', 'sessions', 'createdAt', 'updatedAt', 'cancelRequested', 'dismissed')
      view['canCancel'] = provider_scoped_cancelable?(operation) && !operation['cancelRequested']
      view['finishedAt'] = operation['updatedAt'] if %w[applied failed canceled].include?(operation['phase'])
      if operation['strategy'] == 'scoped-v1'
        path = File.join(provider_directory(entry), "#{operation.fetch('id')}.snapshot.json.progress")
        if File.file?(path)
          progress = JSON.parse(read_private_json_file!(path, 'provider progress', maximum: 64 * 1024))
          view.merge!(progress.slice('stage', 'thread', 'threadLabel', 'completed', 'total', 'updatedAt', 'reason')) if progress['operation'] == operation['id']
        end
      end
      view
    end

    def provider_bind_root(entry, argv)
      raise Error, 'provider bind-root requires a slug and retained root ID' unless argv.length == 2
      slug, root = argv
      raise Error, 'invalid provider session slug' unless slug.match?(PROVIDER_ID)
      raise Error, 'invalid provider root thread' unless provider_thread_identity?(root)
      with_current_generation_lock(mode: File::LOCK_SH) do
        provider_locked(entry) do
          state = provider_state(entry)
          selection = state.fetch('sessions', {})[slug]
          return unless selection && selection['creationId']
          manifest = read_portal_manifest(File.join(entry.fetch('root'), 'work', slug, 'portal.yml'))
          journal = provider_creation_journal(entry, slug)
          raise Error, 'provider root has a different creation incarnation' unless journal['state'] == 'creating' && journal['tmux_identity'] == selection['creationId']
          raise Error, 'provider root does not match its creation manifest' unless manifest.dig('codex', 'thread_id') == root && manifest.dig('codex', 'socket_path') == instance_runtime(entry).fetch(:codex)
          selection['rootThreadId'] = root
          provider_write_state(entry, state)
        end
      end
    end

    def provider_scoped_write_state(entry, state, selection: false, expected_phase: nil)
      provider_locked(entry) do
        current = provider_state(entry)
        raise Error, 'scoped provider operation identity changed' unless current.dig('operation', 'id') == state.dig('operation', 'id')
        return false if expected_phase && current.dig('operation', 'phase') != expected_phase
        state.fetch('operation')['cancelRequested'] = true if current.dig('operation', 'cancelRequested')
        current['operation'] = state.fetch('operation')
        if selection
          if state.dig('operation', 'session')
            slug = state.dig('operation', 'session')
            current['sessions'] ||= {}
            if state.dig('operation', 'inherit')
              current['sessions'].delete(slug)
            else
              current['sessions'][slug] = state.fetch('sessions').fetch(slug)
            end
          else
            current['active'] = state.fetch('active')
          end
        end
        provider_write_state(entry, current)
        state.replace(current)
        true
      end
    end

    def provider_scoped_helper(entry, action, snapshot, operation)
      args = ['codex-provider', action, '--workspace', entry.fetch('root'), '--state-root', @state,
              '--socket', instance_runtime(entry).fetch(:codex), '--host-profile', @profile,
              '--authority', instance_runtime(entry).fetch(:authority), '--snapshot', snapshot,
              '--target', operation.fetch('target'), '--operation', operation.fetch('id'),
              '--roots', JSON.generate(operation.fetch('roots')),
              '--sessions', operation.fetch('sessions').join(',')]
      output = capture_env!(codex_environment(entry), File.join(package_root, 'bin/workspace-portal'), *args)
      raise Error, 'scoped provider response exceeds bounds' if output.bytesize > 16 * 1024 * 1024
      JSON.parse(output)
    end

    def provider_scoped_progress(operation, snapshot, stage, thread: nil, completed: 0, total: 0, reason: nil)
      atomic_private_file(snapshot + '.progress', JSON.generate({'operation' => operation.fetch('id'), 'stage' => stage, 'thread' => thread, 'completed' => completed, 'total' => total, 'reason' => reason, 'updatedAt' => Time.now.utc.iso8601}.compact))
    end

    def provider_run_scoped(entry)
      directory = provider_directory(entry)
      File.open(File.join(directory, 'scoped-worker.lock'), File::RDWR | File::CREAT, 0o600) do |lock|
        return unless lock.flock(File::LOCK_EX | File::LOCK_NB)
        state = provider_state(entry)
        operation = state.fetch('operation')
        return unless operation['strategy'] == 'scoped-v1' && %w[waiting applying].include?(operation['phase'])
        snapshot = File.join(provider_directory(entry), "#{operation.fetch('id')}.snapshot.json")
        begin
          if operation['phase'] == 'waiting'
            # Creators hold a shared admission until their journal/root is
            # durable. Freeze inventory and publish the fence under EX so no
            # inherited creator can enter the gap between those two actions.
            admission = File.open(File.join(directory, 'creation-admission.lock'), File::RDWR | File::CREAT, 0o600) unless operation['session']
            if admission && !admission.flock(File::LOCK_EX | File::LOCK_NB)
              admission&.close
              operation['stage'] = 'waiting-creation'
              operation['blockers'] = ['A session creation is still running.']
              provider_scoped_write_state(entry, state, expected_phase: 'waiting')
              return
            end
            begin
            if !operation['session']
              pending = Dir[File.join(entry.fetch('root'), 'worktrees', '.locks', '*.{creation,fork}.json')].filter_map do |path|
                journal = JSON.parse(read_private_json_file!(path, 'provider pending creation', maximum: 64 * 1024))
                next journal.fetch('slug') if path.end_with?('.fork.json')
                slug = File.basename(path, '.creation.json')
                selection = state.fetch('sessions', {})[slug]
                slug if journal['state'] == 'creating' && !selection
              end
              unless pending.empty?
                operation['stage'] = 'waiting-creation'
                operation['blockers'] = pending.map { |slug| "#{slug}: initial conversation creation is still running" }
                provider_scoped_write_state(entry, state, expected_phase: 'waiting')
                return
              end
              roots = provider_scoped_sessions(entry, state, nil)
              operation['sessions'] = roots.keys
              operation['roots'] = roots
            elsif provider_scoped_sessions(entry, state, operation['session']) != operation.fetch('roots')
              provider_finish(state, 'failed', 'The selected session conversation changed. Select its provider again.')
              provider_scoped_write_state(entry, state, expected_phase: 'waiting')
              return
            end
            if provider_catalog.fetch('digest') != operation.fetch('catalogDigest')
              provider_finish(state, 'failed', 'Provider configuration changed. Select the provider again.')
              provider_scoped_write_state(entry, state, expected_phase: 'waiting')
              return
            end
            operation['phase'] = 'applying'
            operation['stage'] = 'quiescing'
            operation['snapshot'] = snapshot
            return unless provider_scoped_write_state(entry, state, expected_phase: 'waiting')
            ensure
              admission&.close
            end
          end
          terminal_path = snapshot + '.terminals'
          provider_scoped_check_cancel!(entry, operation) if %w[quiescing preparing].include?(operation['stage'])
          if operation['stage'] == 'quiescing'
            unless File.exist?(terminal_path)
              atomic_private_file(terminal_path, JSON.generate('schema' => 1, 'workspace' => entry.fetch('name'), 'root' => entry.fetch('root'), 'socket' => instance_runtime(entry).fetch(:codex), 'terminals' => []))
            end
            quiesce_sessions([entry], provider_terminal_snapshot: terminal_path, provider_slugs: operation.fetch('sessions')) do |slug, index, total|
              provider_scoped_check_cancel!(entry, operation)
              provider_scoped_progress(operation, snapshot, 'quiescing', thread: slug, completed: index, total:)
            end
            operation['stage'] = 'preparing'
            provider_scoped_write_state(entry, state)
          end
          if operation['stage'] == 'preparing'
            provider_scoped_check_cancel!(entry, operation)
            prepared = provider_scoped_helper(entry, 'scoped-prepare', snapshot, operation)
            provider_scoped_check_cancel!(entry, operation)
            prepared.fetch('threads').each { |thread| thread['environment'] = provider_thread_environment(entry, thread.fetch('slug'), thread['memberAddress']) }
            atomic_private_file(snapshot, JSON.generate(prepared))
            provider_scoped_progress(operation, snapshot, 'starting-provider-check')
            # Native model discovery is daemon-configured. A read-only private
            # server probes the target without changing the shared daemon.
            provider_private_server(entry, operation.fetch('target')) do |socket|
              provider_helper(entry, 'preflight', snapshot:, target: operation.fetch('target'), socket:)
            end
            provider_scoped_begin_switch!(entry, state)
          end
          if %w[switching resuming].include?(operation['stage'])
            provider_scoped_helper(entry, 'scoped-switch', snapshot, operation)
            if operation['stage'] == 'switching'
              if operation['session']
                roots = provider_scoped_sessions(entry, state, operation.fetch('session'))
                state['sessions'] ||= {}
                state['sessions'][operation.fetch('session')] = {'provider' => operation.fetch('target'), 'rootThreadId' => roots.fetch(operation.fetch('session'))}
              else
                state['active'] = operation.fetch('target')
              end
              operation['stage'] = 'resuming'
              provider_scoped_write_state(entry, state, selection: true)
            end
            provider_scoped_helper(entry, 'scoped-continue', snapshot, operation)
            operation['stage'] = 'restoring-terminals'
            operation['message'] = 'Provider changed. Restoring terminal clients.'
            provider_scoped_write_state(entry, state)
          end
          raise Error, 'scoped provider restoration has an invalid stage' unless operation['stage'] == 'restoring-terminals'
          begin
            provider_restore_terminals(entry, snapshot) do |slug, index, total|
              provider_scoped_progress(operation, snapshot, 'restoring-terminals', thread: slug, completed: index, total:)
            end
            operation.delete('message')
          rescue StandardError
            operation['message'] = 'Provider changed. Some terminal clients need dev-session sync.'
          end
          atomic_private_file(File.join(provider_directory(entry), "#{operation.fetch('id')}.watch.json"), JSON.generate('schema' => 1, 'operation' => operation.fetch('id'), 'workspace' => entry.fetch('root'), 'socket' => instance_runtime(entry).fetch(:codex)))
          provider_finish(state, 'applied')
          provider_scoped_write_state(entry, state)
        rescue StandardError => error
          canceled = error.is_a?(ProviderCanceled)
          if %w[quiescing preparing].include?(operation['stage']) && !canceled
            current = provider_locked(entry) { provider_state(entry).fetch('operation') }
            canceled = current['id'] == operation['id'] && current['cancelRequested']
          end
          if canceled
            message = 'Provider switch canceled.'
            provider_scoped_progress(operation, snapshot, 'restoring-terminals')
            begin
              provider_restore_terminals(entry, snapshot)
            rescue StandardError
              message += ' Some terminal clients need dev-session sync.'
            end
            provider_finish(state, 'canceled', message)
            provider_scoped_progress(operation, snapshot, 'canceled')
            provider_scoped_write_state(entry, state)
            return
          end
          diagnostic = {started: Process.clock_gettime(Process::CLOCK_MONOTONIC), step: 'scoped-transition'}
          provider_failure_diagnostic(operation, 'forward', diagnostic, error)
          # No silent compensation. The journal retains exact native progress
          # and a retry completes the accepted target without touching others.
          if %w[quiescing preparing].include?(operation['stage'])
            provider_finish(state, 'failed', 'Provider preparation failed before any thread was closed. Inspect its current step before retrying.')
            provider_scoped_write_state(entry, state)
            begin
              provider_restore_terminals(entry, snapshot)
            rescue StandardError
              operation['message'] += ' Some terminal clients need dev-session sync.'
            end
          else
            provider_finish(state, 'recovery-required', 'Provider switch paused. Inspect its current step and retry provider recovery; other sessions remain available.')
          end
          provider_scoped_write_state(entry, state)
        end
      end
    end
  end
end
