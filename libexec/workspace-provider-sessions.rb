# frozen_string_literal: true

# Schema-1 requests retain their original recovery engine. New requests use a
# shared daemon and reserve only their session scope, never the package lock.
module DevWorkspaceHost
  module CodexProviderHost
    private

    class ProviderCanceled < StandardError; end

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
      output = capture!(File.join(package_root, 'bin/workspace-portal'), *args)
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
