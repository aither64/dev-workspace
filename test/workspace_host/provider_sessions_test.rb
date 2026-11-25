# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class ScopedProviderTestHost < DevWorkspaceHost::Host
  attr_accessor :scoped_failure, :catalog
  attr_reader :calls, :lock_modes

  def initialize(**arguments)
    super
    @calls, @lock_modes = [], []
  end

  private

  def provider_catalog = @catalog
  def with_current_generation_lock(mode: File::LOCK_EX, **)
    @lock_modes << mode
    yield
  end
  def system!(*arguments) = @calls << arguments
  def provider_worker_pause = throw(:worker_stopped)
  def provider_restore_terminals(*) = nil

  def provider_scoped_helper(entry, action, _snapshot, operation)
    @calls << ['scoped-helper', action, operation.fetch('sessions')]
    raise 'synthetic scoped failure' if @scoped_failure == action
    return {'schema' => 1, 'workspace' => entry.fetch('root'), 'threads' => [], 'loaded' => [], 'blockers' => []} if action == 'scoped-prepare'
    {'verified' => true}
  end

  def provider_helper(entry, action, **options)
    return {'verified' => true} if action == 'preflight'
    raise "unexpected helper #{action}"
  end

  def provider_private_server(_entry, _provider, **) = yield('/synthetic/private-model-socket')

  def quiesce_sessions(_entries, provider_slugs:, **)
    @calls << ['quiesce-scope', provider_slugs]
  end
end

class ForkProviderTestHost < ScopedProviderTestHost
  attr_accessor :after_write
  attr_reader :fork_writes
  def initialize(**arguments)
    super
    @fork_writes = []
  end
  private
  def require_current_package_generation! = nil
  def preselection_session_package = '/nix/store/frozen-package'
  def atomic_private_file(path, data)
    super
    @fork_writes << JSON.parse(data) if path.end_with?('/state.json')
    @after_write&.call(path, data)
  end
end

class WorkspaceHostTest
  def with_fork_provider
    with_scoped_provider_host(host_class: ForkProviderTestHost) do |host, entry|
      operation = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      socket = host.send(:instance_runtime, entry).fetch(:codex)
      path = File.join(host.send(:provider_directory, entry), "#{operation}.snapshot.json")
      o = {'id' => operation, 'target' => 'pool', 'catalogDigest' => 'a' * 64, 'phase' => 'waiting', 'stage' => 'accepted', 'strategy' => 'fork-v1',
        'session' => 'one', 'sessions' => ['one'], 'roots' => {'one' => 'root-one'}, 'blockers' => [], 'createdAt' => '2026-11-25T00:00:00Z', 'updatedAt' => '2026-11-25T00:00:00Z'}
      state = {'schema' => 2, 'workspace' => entry['name'], 'root' => entry['root'], 'socket' => socket, 'active' => 'openai', 'sessions' => {}, 'receipts' => {}, 'operation' => o}
      host.send(:provider_locked, entry) { host.send(:provider_write_state, entry, state) }
      raw = File.binread(host.send(:provider_state_path, entry))
      participants = [['lead', 'root-one'], ['implementer0', 'member-one']].map do |member, id|
        key = {'workspace' => entry['root'], 'slug' => 'one', 'incarnation' => 'c' * 64, 'rootThreadId' => 'root-one', 'member' => member, 'originalThreadId' => id}
        {'current' => {'key' => key, 'bindingRequired' => false, 'expectedRevision' => 0, 'provider' => 'openai', 'socket' => socket, 'eligible' => true},
         'thread' => {'id' => id, 'slug' => 'one', 'cwd' => File.join(entry['root'], 'work', 'one')}, 'browserRecords' => {'nextSettings'=>nil, 'activityFinal'=>nil}}
      end
      candidate = host.send(:provider_fork_origin_reservation, state, participants.map { |p| p['current'] })
      candidate['operation'] = o.merge('phase' => 'applying', 'stage' => 'preparing', 'snapshot' => path)
      proof = {'schema' => 1, 'strategy' => 'fork-v1', 'operationId' => operation, 'workspace' => entry['name'], 'root' => entry['root'], 'socket' => socket,
        'sourcePackage' => '/nix/store/frozen-package', 'catalogDigest' => 'a' * 64, 'previousStateDigest' => Digest::SHA256.hexdigest(raw),
        'socketIdentity' => 'synthetic-generation', 'rosters' => {'one' => {'rootThreadId' => 'root-one', 'incarnation' => 'c' * 64, 'revision' => 0, 'bindingOriginSchema'=>1}},
        'state' => candidate, 'selection' => {'scope' => 'session', 'session' => 'one', 'provider' => 'pool'}, 'sessions' => ['one'], 'participants' => participants}
      bytes = JSON.generate(proof)
      host.send(:atomic_private_file, path, bytes)
      yield host, entry, operation, path, Digest::SHA256.hexdigest(bytes), proof
    end
  end

  def reserve_fork_fixture(host, entry, operation, path, digest)
    host.send(:provider_fork_reserve, entry, operation, path, digest)
    %w[manifest authority].each { |kind| host.send(:provider_fork_project, entry, operation, path, digest, 'one', kind) {} }
  end

  def verified_fork_fixture(host, entry, operation, path, digest, source)
    reserve_fork_fixture(host, entry, operation, path, digest)
    host.send(:provider_fork_first_intent, entry, operation, path, digest)
    journal = JSON.parse(File.binread(path + '.journal'))
    proofs = journal['participants'].map do |p|
      p['state'], p['childThreadId'] = 'verified', p['sourceThreadId'] + '-child'
      bytes = JSON.generate('key' => p['current']['key'], 'sourceThreadId' => p['sourceThreadId'], 'childThreadId' => p['childThreadId'], 'provider' => 'pool',
        'historyDigest' => 'b' * 64, 'settingsDigest' => 'd' * 64, 'titleDigest' => 'e' * 64)
      p['verificationDigest'] = Digest::SHA256.hexdigest(bytes)
      bytes
    end
    journal['stage'] = 'verifying'
    host.send(:atomic_private_file, path + '.journal', JSON.generate(journal))
    verified = source.slice('schema', 'strategy', 'operationId', 'workspace', 'root', 'socket', 'sourcePackage', 'catalogDigest').merge('sourceSnapshotDigest' => digest, 'proofs' => proofs)
    bytes = JSON.generate(verified)
    host.send(:atomic_private_file, path + '.verified', bytes)
    Digest::SHA256.hexdigest(bytes)
  end

  def test_fork_origin_reservation_and_journal_crash_recover_without_selection
    with_fork_provider do |host, entry, operation, path, digest, _source|
      assert_equal(2, host.send(:provider_state, entry).fetch('schema'))
      once = true
      host.after_write = ->(p, _) { if once && p.end_with?('.journal'); once = false; raise 'crash after journal'; end }
      assert_raises(RuntimeError) { host.send(:provider_fork_reserve, entry, operation, path, digest) }
      assert_equal 2, JSON.parse(File.binread(host.send(:provider_state_path, entry)))['schema']
      host.send(:provider_fork_reserve, entry, operation, path, digest)
      state, = host.send(:provider_fork_state, entry)
      assert_equal [3, 1, 'openai'], state.values_at('schema', 'bindingRevision', 'active')
      assert_equal %w[member-one root-one], state['bindings']['one']['conversations'].keys.sort
      assert_equal(3, host.send(:provider_state, entry).fetch('schema'))
      host.send(:provider_fork_reserve, entry, operation, path, digest)
      assert_equal 1, host.send(:provider_fork_state, entry).first['bindingRevision']
    end
  end

  def test_fork_cancel_and_first_intent_share_the_request_mutex
    %w[cancel-first intent-first intent-journal-crash].each do |fault|
      with_fork_provider do |host, entry, operation, path, digest, _|
        reserve_fork_fixture(host, entry, operation, path, digest)
        if fault == 'cancel-first'
          host.send(:provider_fork_cancel, entry, operation, path, digest)
          assert_raises(DevWorkspaceHost::CodexProviderHost::ProviderCanceled) { host.send(:provider_fork_first_intent, entry, operation, path, digest) }
          assert_equal 'not-attempted', JSON.parse(File.binread(path + '.journal'))['participants'][0]['state']
        else
          if fault == 'intent-journal-crash'
            once = true
            host.after_write = ->(p, _) { if once && p.end_with?('.journal'); once = false; raise 'lost first-intent write'; end }
            assert_raises(RuntimeError) { host.send(:provider_fork_first_intent, entry, operation, path, digest) }
            assert_equal 'preparing', host.send(:provider_fork_state, entry).first.dig('operation', 'stage')
          else
            host.send(:provider_fork_first_intent, entry, operation, path, digest)
          end
          assert_raises(DevWorkspaceHost::Error) { host.send(:provider_fork_cancel, entry, operation, path, digest) }
          assert_raises(DevWorkspaceHost::Error) { host.send(:provider_fork_first_intent, entry, operation, path, digest) }
          assert_equal 'intent', JSON.parse(File.binread(path + '.journal'))['participants'][0]['state']
        end
      end
    end
  end

  def test_fork_collective_commit_seal_crash_and_forward_terminal_projection
    with_fork_provider do |host, entry, operation, path, digest, source|
      verified_digest = verified_fork_fixture(host, entry, operation, path, digest, source)
      state, = host.send(:provider_fork_state, entry)
      state['bindingRevision'] = 2
      state['bindings']['unrelated'] = {'rootThreadId' => 'other-root', 'incarnation' => 'f' * 64, 'revision' => 2,
        'conversations' => {'other-root' => {'member' => 'lead', 'threadId' => 'other-root', 'provider' => 'openai'}}}
      host.send(:provider_write_state, entry, state)
      once = true
      host.after_write = ->(p, _) { if once && p.end_with?('.evidence'); once = false; raise 'seal before write crash'; end }
      assert_raises(RuntimeError) { host.send(:provider_fork_commit, entry, operation, path + '.verified', verified_digest) }
      old, = host.send(:provider_fork_state, entry)
      assert_equal 'committing', old.dig('operation', 'stage')
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_fork_binding_write_allowed!, old) }
      seal = File.binread(path + '.evidence')
      committed = host.send(:provider_fork_commit, entry, operation, path + '.verified', verified_digest)
      assert_equal 3, committed['bindingRevision']
      assert_equal old['bindings']['unrelated'], committed['bindings']['unrelated']
      assert_equal seal, File.binread(path + '.evidence')
      assert_equal %w[member-one-child root-one-child], committed['bindings']['one']['conversations'].values.map { |c| c['threadId'] }.sort
      refute JSON.parse(File.binread(path + '.journal')).dig('projections','one','authority')
      host.send(:provider_fork_project, entry, operation, path, digest, 'one', 'authority') {}
      host.send(:provider_fork_project, entry, operation, path, digest, 'one', 'recovery') {}
      assert_raises(RuntimeError) { host.send(:provider_fork_project, entry, operation, path, digest, 'one', 'terminal') { raise 'terminal unavailable' } }
      assert_equal committed['bindings'], host.send(:provider_fork_state, entry).first['bindings']
      host.send(:provider_fork_project, entry, operation, path, digest, 'one', 'terminal') {}
      assert_equal 'applied', host.send(:provider_fork_state, entry).first.dig('operation', 'phase')
      published = host.fork_writes.select { |s| s['schema'] == 3 }.map { |s| s['bindings']['one']['conversations'].values.map { |c| c['threadId'].end_with?('-child') }.uniq }
      assert published.all? { |row| row.length == 1 }, 'partial participants were published'
    end
  end
end

class WorkspaceHostTest
  def test_fork_commit_lost_ack_reuses_exact_seal_and_rejects_changed_evidence
    with_fork_provider do |host, entry, operation, path, digest, source|
      verified_digest = verified_fork_fixture(host, entry, operation, path, digest, source)
      once = true
      host.after_write = lambda do |file, bytes|
        if once && file.end_with?('/state.json') && JSON.parse(bytes).dig('operation', 'stage') == 'restoring-terminals'
          once = false
          raise 'lost collective commit acknowledgement'
        end
      end
      assert_raises(RuntimeError) { host.send(:provider_fork_commit, entry, operation, path + '.verified', verified_digest) }
      seal = File.binread(path + '.evidence')
      committed = host.send(:provider_fork_commit, entry, operation, path + '.verified', verified_digest)
      assert_equal 2, committed['bindingRevision']
      assert_equal seal, File.binread(path + '.evidence')
      bad = JSON.parse(seal)
      bad['participants'][0]['oldProvider'] = 'pool'
      host.send(:atomic_private_file, path + '.evidence', JSON.generate(bad))
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_fork_commit, entry, operation, path + '.verified', verified_digest) }
      assert_equal committed, host.send(:provider_fork_state, entry).first
    end
  end

  def test_fork_projection_releases_request_mutex_and_preserves_unrelated_writes
    with_fork_provider do |host, entry, operation, path, digest, source|
      verified_digest = verified_fork_fixture(host, entry, operation, path, digest, source)
      host.send(:provider_fork_commit, entry, operation, path + '.verified', verified_digest)
      host.send(:provider_fork_project, entry, operation, path, digest, 'one', 'authority') {}
      host.send(:provider_fork_project, entry, operation, path, digest, 'one', 'recovery') do
        File.open(File.join(host.send(:provider_directory, entry), 'request.lock'), File::RDWR) do |lock|
          assert lock.flock(File::LOCK_EX | File::LOCK_NB), 'projection held request mutex'
          state, = host.send(:provider_fork_state, entry)
          state['bindingRevision'] += 1
          state['bindings']['unrelated'] = {'rootThreadId' => 'other', 'incarnation' => 'e' * 64, 'revision' => state['bindingRevision'],
            'conversations' => {'other' => {'member' => 'lead', 'threadId' => 'other', 'provider' => 'openai'}}}
          host.send(:provider_write_state, entry, state)
        end
      end
      host.send(:provider_fork_project, entry, operation, path, digest, 'one', 'terminal') {}
      final = host.send(:provider_fork_state, entry).first
      assert_equal 3, final['bindingRevision']
      assert_equal 'other', final.dig('bindings', 'unrelated', 'conversations', 'other', 'threadId')
      assert_equal 'applied', final.dig('operation', 'phase')
    end
  end

  def test_fork_selection_only_default_session_inherit_and_noop_stay_legacy
    %w[default session inherit noop].each do |scope|
      with_fork_provider do |host, entry, operation, _path, _digest, source|
        state = source['state']
        state = state.reject { |key, _| %w[bindings bindingRevision operation].include?(key) }.merge('schema' => 2)
        state['sessions']['one'] = {'provider' => 'openai', 'rootThreadId' => 'root-one'} if scope == 'inherit'
        host.send(:provider_write_state, entry, state)
        target = scope == 'default' ? 'pool' : 'openai'
        selection = {'scope' => scope == 'default' ? 'workspace' : 'session', 'provider' => target}
        selection.merge!('session' => 'one') unless scope == 'default'
        selection['inherit'] = true if scope == 'inherit'
        op = source['state']['operation'].merge('target' => target)
        participants = scope == 'default' ? [] : source['participants'].map { |p| p['current'] }
        result = host.send(:provider_fork_selection_only, entry, op, selection, participants)
        assert_equal 2, result['schema']
        refute result.key?('bindings')
        refute result.key?('bindingRevision')
        assert_equal target, result['active'] if scope == 'default'
        refute result['sessions'].key?('one') if scope == 'inherit'
        assert_equal result, host.send(:provider_fork_selection_only, entry, op, selection, participants)
        assert_equal 'applied', result.dig('operation', 'phase')
      end
    end
  end

  def test_fork_corrupt_journal_cannot_admit_cancellation_or_intent
    with_fork_provider do |host, entry, operation, path, digest, _|
      reserve_fork_fixture(host, entry, operation, path, digest)
      host.send(:atomic_private_file, path + '.journal', '{}')
      assert_raises(DevWorkspaceHost::ProviderBindings::Error) { host.send(:provider_fork_cancel, entry, operation, path, digest) }
      assert_raises(DevWorkspaceHost::ProviderBindings::Error) { host.send(:provider_fork_first_intent, entry, operation, path, digest) }
      assert_equal 'applying', host.send(:provider_fork_state, entry).first.dig('operation', 'phase')
    end
  end
end

class WorkspaceHostTest
  def test_scoped_provider_subprocess_uses_registered_workspace_codex_home
    keys = %w[CODEX_HOME CODEX_SQLITE_HOME DEV_WORKSPACE_CODEX_HOME]
    saved = keys.to_h { |key| [key, ENV[key]] }
    Dir.mktmpdir('scoped-provider-environment-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'registry.json')
      registry = DevWorkspaceHost::Registry.new(config)
      registry.register(name: 'example', root:, hostname: 'workspace.example.test', aliases: [], replace: false)
      package = File.join(directory, 'package')
      executable = File.join(package, 'bin', 'workspace-portal')
      FileUtils.mkdir_p(File.dirname(executable))
      File.write(executable, <<~RUBY)
        #!#{RbConfig.ruby}
        require 'json'
        puts JSON.generate('homes' => ENV.values_at('CODEX_HOME', 'CODEX_SQLITE_HOME', 'DEV_WORKSPACE_CODEX_HOME'), 'arguments' => ARGV)
      RUBY
      File.chmod(0o755, executable)
      entry = registry.find('example')
      operation = {'target' => 'pool', 'id' => 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
                   'roots' => {'one' => 'fixture-root'}, 'sessions' => ['one']}
      [nil, File.join(directory, 'incidental-home')].each do |incidental|
        keys.each { |key| incidental ? ENV[key] = incidental : ENV.delete(key) }
        environment = host_environment(directory, config:)
        keys.each { |key| environment[key] = incidental } if incidental
        host = DevWorkspaceHost::Host.new(env: environment, out: StringIO.new, err: StringIO.new)
        host.define_singleton_method(:package_root) { package }
        result = host.send(:provider_scoped_helper, entry, 'scoped-prepare', File.join(directory, 'snapshot.json'), operation)
        expected_home = File.join(environment.fetch('DEV_WORKSPACES_STATE'), 'codex-homes', entry.fetch('name'))
        assert_equal [expected_home] * keys.length, result.fetch('homes')
        arguments = result.fetch('arguments')
        assert_equal ['codex-provider', 'scoped-prepare'], arguments.first(2)
        assert_equal root, arguments[arguments.index('--workspace') + 1]
        assert_equal JSON.generate(operation.fetch('roots')), arguments[arguments.index('--roots') + 1]
        assert_equal 'one', arguments[arguments.index('--sessions') + 1]
      end
    end
  ensure
    saved&.each { |key, value| value ? ENV[key] = value : ENV.delete(key) }
  end

  def test_archived_bound_manifest_requires_all_three_exact_package_capabilities
    with_scoped_provider_host do |host, entry|
      package = make_package(File.dirname(entry.fetch('root')), 'binding-package')
      directory = File.join(entry.fetch('root'), 'archive', 'bound')
      FileUtils.mkdir_p(directory)
      manifest = {'schema' => 6, 'slug' => 'bound', 'repositories' => [],
        'archive_operation_id' => 'd' * 64, 'finalized_at' => '2026-11-25T12:00:00Z',
        'codex' => {'thread_id' => 'origin', 'socket_path' => host.send(:instance_runtime, entry).fetch(:codex),
          'client_version' => '0.172.0', 'binding_schema' => 3, 'binding_origin_schema' => 3}}
      path = File.join(directory, 'portal.yml')
      File.write(path, YAML.dump(manifest))
      contract_path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      base = JSON.parse(File.read(contract_path)).merge('codexProviderScopedStateSchema' => 2, 'codexProviderScopedTransitionPolicy' => 1)
      required = {'codexProviderBindingStateSchema' => 3, 'codexProviderForkTransitionPolicy' => 1, 'codexRuntimeAuthoritySchema' => 3}
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_provider_state!, package) }
      complete = base.merge(required)
      required.each do |key, value|
        [nil, value.to_s, value.to_f, value + 1].each do |wrong|
          File.write(contract_path, JSON.generate(complete.merge(key => wrong)))
          assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_provider_state!, package) }
        end
        File.write(contract_path, JSON.generate(complete.reject { |k, _| k == key }))
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_provider_state!, package) }
      end
      File.write(contract_path, JSON.generate(complete))
      host.send(:require_compatible_provider_state!, package)
      File.write(path, YAML.dump(manifest.merge('archive_operation_id' => '')))
      assert_raises(DevSession::Error) { host.send(:require_compatible_provider_state!, package) }
    end
  end

  def test_bound_monitor_saved_target_requires_same_three_package_capabilities
    with_scoped_provider_host do |host, entry|
      package = make_package(File.dirname(entry.fetch('root')), 'monitor-package')
      directory = File.join(host.instance_variable_get(:@state), 'monitor-operations', 'workspace')
      FileUtils.mkdir_p(directory, mode: 0700)
      path = File.join(directory, 'operation.json')
      File.write(path, JSON.generate('owner' => {'runtimeSchema' => 3, 'rootThreadId' => 'origin', 'leadThreadId' => 'child'}), mode: 'w', perm: 0600)
      contract_path = File.join(package, 'share/workspace-portal/runtime-contract.json')
      base = JSON.parse(File.read(contract_path)).merge('workspaceReportingPolicy' => 4,
        'monitorOperationStateSchema' => 1, 'monitorOperationPolicy' => 1)
      File.write(contract_path, JSON.generate(base))
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_monitor_operations!, package) }
      File.write(contract_path, JSON.generate(base.merge('codexProviderBindingStateSchema' => 3,
        'codexProviderForkTransitionPolicy' => 1, 'codexRuntimeAuthoritySchema' => 3)))
      host.send(:require_compatible_monitor_operations!, package)
      observer = File.join(directory, 'observers', 'observer.json')
      FileUtils.mkdir_p(File.dirname(observer), mode: 0700)
      File.rename(path, observer)
      File.write(contract_path, JSON.generate(base))
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_monitor_operations!, package) }
      File.write(contract_path, JSON.generate(base.merge('codexProviderBindingStateSchema' => 3,
        'codexProviderForkTransitionPolicy' => 1, 'codexRuntimeAuthoritySchema' => 3,
        'monitorObserverStateSchema' => 1, 'monitorObserverLifecyclePolicy' => 1)))
      host.send(:require_compatible_monitor_operations!, package)
      File.write(observer, '{')
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_monitor_operations!, package) }
    end
  end

  def test_retained_workflow_sources_require_complete_target_support
    require_relative '../support/pi_creation_fixture'
    extend PiCreationFixture
    %w[questions plans tool_policy role_instructions].each do |kind|
      with_transition_host do |host, paths|
        workspace = host.send(:registry).entries.first.fetch('root')
        state = host.instance_variable_get(:@state)
        saved = write_pi_creation_records(state, workspace, pi_creation_records(workspace), receipt: false)
        File.unlink(saved.fetch(:binding))
        member = {'address' => 'lead', 'incarnation' => 'b' * 64, 'nativeId' => 'native-lead', 'creationId' => 'd' * 64}
        claim = {'source' => {'kind' => kind, 'id' => 'f' * 64},
          'session' => {'workspace' => workspace, 'slug' => 'example', 'incarnation' => 'a' * 64}, 'members' => [member]}
        host.team_preflight_output = JSON.generate('schema' => 1, 'required' => true, 'sessions' => [claim])
        path = File.join(host.candidate, 'share/workspace-portal/runtime-contract.json')
        complete = JSON.parse(File.read(path)).merge(WorkspaceSessionBackends::FIELDS).merge('piTeamRecords' => WorkspaceSessionBackends::TEAM_FIELDS).merge(WorkspaceSessionBackends::WORKFLOW_FIELDS)
        complete['piRoleInstructions'] = 1 if kind == 'role_instructions'
        File.write(path, JSON.generate(complete))
        host.send(:require_compatible_session_backends!, host.candidate)
        if kind == 'role_instructions'
          [nil, false, '1', 1.0, 0, 2].each do |version|
            File.write(path, JSON.generate(complete.merge('piRoleInstructions' => version)))
            error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
            assert_match(/role instructions/, error.message)
          end
          File.write(path, JSON.generate(complete.reject { |key, _| key == 'piRoleInstructions' }))
          assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
        end
        WorkspaceSessionBackends::WORKFLOW_FIELDS.each_key do |field|
          File.write(path, JSON.generate(complete.reject { |key, _| key == field }))
          error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_backends!, host.candidate) }
          assert_match(/workflow/, error.message)
        end
        refute(File.exist?(host.instance_variable_get(:@profile)))
        refute_includes(host.events, [:sessions_quiesced])
        refute_includes(host.events, [:configured])
      end
    end
  end

  def with_scoped_provider_host(host_class: ScopedProviderTestHost)
    Dir.mktmpdir('scoped-provider-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      registry = DevWorkspaceHost::Registry.new(config)
      registry.register(name: 'example', root:, hostname: 'workspace.example.test', aliases: [], replace: false)
      host = host_class.new(env: host_environment(directory, config:), out: StringIO.new, err: StringIO.new)
      host.catalog = {'digest' => 'a' * 64, 'catalog' => {'schema' => 1, 'initialProvider' => 'openai', 'providers' => [
        {'id' => 'openai', 'label' => 'Subscription', 'kind' => 'subscription'},
        {'id' => 'pool', 'label' => 'Pool', 'kind' => 'codex-lb', 'baseURL' => 'http://127.0.0.1:2455/v1', 'tokenFile' => '/private/token'}
      ]}}
      yield host, registry.find('example')
    end
  end
  def provider_session_fixture(host, entry, slug, root)
    directory = File.join(entry.fetch('root'), 'work', slug)
    FileUtils.mkdir_p(directory)
    File.write(File.join(directory, 'portal.yml'), YAML.dump({
      'schema' => 1, 'slug' => slug, 'repositories' => [], 'artifacts' => [],
      'codex' => {'thread_id' => root, 'socket_path' => host.send(:instance_runtime, entry).fetch(:codex), 'client_version' => '0.163.0'},
      'creation' => {'state' => 'ready'}
    }))
  end

  # These controls retain pre-upgrade scoped-v1 recovery. New public requests
  # use fork-v1; only the fixture's accepted strategy is restored here.
  def accept_retained_scoped_request(host, entry, arguments)
    host.send(:provider_command, arguments)
    host.send(:provider_locked, entry) do
      state = host.send(:provider_state, entry)
      if state.dig('operation', 'strategy') == 'fork-v1'
        state.fetch('operation')['strategy'] = 'scoped-v1'
        host.send(:provider_write_state, entry, state)
      end
    end
  end

  def test_public_provider_request_uses_fork_and_keeps_pending_replay_and_empty_scope
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      arguments = ['request', 'example', 'pool', id, '--session', 'one']
      host.send(:provider_command, arguments)
      before = File.binread(host.send(:provider_state_path, entry))
      state = host.send(:provider_state, entry)
      assert_equal('fork-v1', state.dig('operation', 'strategy'))
      assert_equal('waiting', state.dig('operation', 'phase'))
      host.send(:provider_command, arguments)
      assert_equal(before, File.binread(host.send(:provider_state_path, entry)))
      host.send(:provider_command, ['cancel', 'example', id])
      assert_equal('canceled', host.send(:provider_state, entry).dig('operation', 'phase'))
    end
    with_scoped_provider_host do |host, entry|
      profile = host.instance_variable_get(:@profile)
      FileUtils.mkdir_p(File.dirname(profile))
      File.symlink(host.send(:package_root), profile)
      host.send(:provider_command, ['request', 'example', 'pool', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'])
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      state = host.send(:provider_state, entry)
      assert_equal('pool', state.fetch('active'))
      assert_equal('applied', state.dig('operation', 'phase'))
      refute(state.key?('bindings'))
      refute(host.calls.any? { |call| call.first == 'scoped-helper' })
    end
  end

  def test_scoped_provider_switch_never_stops_the_shared_daemon_or_other_sessions
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      provider_session_fixture(host, entry, 'two', 'root-two')
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      state = host.send(:provider_state, entry)
      assert_equal 2, state['schema']
      assert_equal 'applied', state.dig('operation', 'phase')
      assert_equal 'openai', state['active']
      assert_equal({'provider' => 'pool', 'rootThreadId' => 'root-one'}, state.dig('sessions', 'one'))
      refute state.fetch('sessions').key?('two')
      assert_includes host.calls, ['quiesce-scope', ['one']]
      refute host.calls.any? { |call| call.include?('stop') || call.include?('restart') }
      assert host.lock_modes.all? { |mode| (mode & File::LOCK_EX).zero? }
    end
  end

  def test_workspace_default_preserves_explicit_selection_and_inherit_clears_it
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      provider_session_fixture(host, entry, 'two', 'root-two')
      accept_retained_scoped_request(host, entry, ['request', 'example', 'openai', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
      assert_equal 'applied', host.send(:provider_state, entry).dig('operation', 'phase')
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'])
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      state = host.send(:provider_state, entry)
      assert_equal ['two'], state.dig('operation', 'sessions')
      assert_equal 'pool', state['active']
      assert_equal 'openai', host.send(:provider_session_selection, entry, state, 'one')
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', 'cccccccc-cccc-4ccc-8ccc-cccccccccccc', '--session', 'one', '--inherit'])
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      state = host.send(:provider_state, entry)
      refute state.fetch('sessions').key?('one')
      assert_equal 'pool', host.send(:provider_session_selection, entry, state, 'one')
    end
  end

  def test_scoped_failure_stays_visible_until_recovery_and_never_compensates_other_threads
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', id, '--session', 'one'])
      host.scoped_failure = 'scoped-switch'
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      assert_equal 'recovery-required', host.send(:provider_state, entry).dig('operation', 'phase')
      calls = host.calls.count { |call| call[0] == 'scoped-helper' }
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      assert_equal calls, host.calls.count { |call| call[0] == 'scoped-helper' }
      host.scoped_failure = nil
      host.send(:provider_command, ['recover', 'example', id])
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      assert_equal 'applied', host.send(:provider_state, entry).dig('operation', 'phase')
      refute host.calls.any? { |call| call[0] == 'private' || call.include?('stop') }
    end
  end

  def test_waiting_request_refuses_a_replacement_root
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'original-root')
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
      provider_session_fixture(host, entry, 'one', 'replacement-root')
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      assert_equal 'failed', host.send(:provider_state, entry).dig('operation', 'phase')
      refute host.calls.any? { |call| %w[scoped-helper quiesce-scope].include?(call.first) }
    end
  end

  def test_creation_provider_is_bound_to_incarnation_and_then_exact_root
    %w[creation fork].each do |kind|
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'original-root')
      locks = File.join(entry.fetch('root'), 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, "one.#{kind}.json")
      File.write(journal, JSON.generate('schema' => kind == 'fork' ? 2 : 1, 'slug' => 'one', 'state' => 'creating', 'tmux_identity' => 'b' * 64, 'provider' => 'pool'))
      File.chmod(0o600, journal)
      host.send(:provider_bind_creation, entry, ['one', 'b' * 64, 'pool'])
      host.send(:provider_bind_creation, entry, ['one', 'b' * 64])
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_bind_creation, entry, ['one', 'b' * 64, 'openai']) }
      host.send(:provider_bind_root, entry, ['one', 'original-root'])
      assert_equal 'pool', host.send(:provider_session_selection, entry, host.send(:provider_state, entry), 'one')
      provider_session_fixture(host, entry, 'one', 'replacement-root')
      File.write(journal, JSON.generate('schema' => 1, 'slug' => 'one', 'state' => 'creating', 'tmux_identity' => 'c' * 64))
      host.send(:provider_bind_creation, entry, ['one', 'c' * 64])
      refute host.send(:provider_state, entry).fetch('sessions').key?('one')
      assert_equal 'openai', host.send(:provider_session_selection, entry, host.send(:provider_state, entry), 'one')
    end
  end
    end

  def test_invalid_scoped_stage_or_root_set_never_reaches_the_worker
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'original-root')
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
      original = host.send(:provider_state, entry)
      [ ['stage', 'unknown'], ['roots', {}], ['sessions', ['one', 'one']] ].each do |key, value|
        state = Marshal.load(Marshal.dump(original))
        state.fetch('operation')[key] = value
        host.send(:provider_write_state, entry, state)
        assert_raises(DevWorkspaceHost::Error) { host.send(:provider_state, entry) }
      end
    end
  end

  def test_default_scope_waits_for_admitted_creation_before_freezing_roots
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'])
      path = File.join(host.send(:provider_directory, entry), 'creation-admission.lock')
      File.open(path, File::RDWR | File::CREAT, 0o600) do |creator|
        creator.flock(File::LOCK_SH)
        catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
        assert_equal 'waiting', host.send(:provider_state, entry).dig('operation', 'phase')
        assert_equal 'waiting-creation', host.send(:provider_state, entry).dig('operation', 'stage')
        provider_session_fixture(host, entry, 'new', 'new-root')
      end
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      state = host.send(:provider_state, entry)
      assert_equal 'applied', state.dig('operation', 'phase')
      assert_equal %w[new one], state.dig('operation', 'sessions').sort
      assert_equal 'new-root', state.dig('operation', 'roots', 'new')
    end
  end
end

class CancelPreparationProviderHost < ScopedProviderTestHost
  attr_accessor :cancel_at, :restore_failure
  attr_reader :restoration_state, :late_cancel_error

  private

  def provider_scoped_helper(entry, action, snapshot, operation)
    provider_control(entry, 'cancel', [operation.fetch('id')]) if @cancel_at == action
    super
  end

  def provider_helper(entry, action, **options)
    provider_control(entry, 'cancel', [provider_state(entry).dig('operation', 'id')]) if @cancel_at == 'preflight' && action == 'preflight'
    super
  end

  def provider_scoped_begin_switch!(entry, state)
    provider_control(entry, 'cancel', [state.dig('operation', 'id')]) if @cancel_at == 'before-fence'
    super
    if @cancel_at == 'after-fence'
      begin
        provider_control(entry, 'cancel', [state.dig('operation', 'id')])
      rescue DevWorkspaceHost::Error => error
        @late_cancel_error = error.message
      end
    end
  end

  def provider_restore_terminals(entry, *)
    @restoration_state = provider_state(entry).fetch('operation').dup
    raise 'synthetic terminal failure' if @restore_failure
  end
end

class WorkspaceHostTest
  def test_cancel_preparation_preserves_reservation_until_terminals_are_restored
    %w[scoped-prepare preflight before-fence].each do |stage|
      with_scoped_provider_host(host_class: CancelPreparationProviderHost) do |host, entry|
        provider_session_fixture(host, entry, 'one', 'root-one')
        id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
        host.cancel_at = stage
        accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', id, '--session', 'one'])
        catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
        state = host.send(:provider_state, entry)
        assert_equal 'canceled', state.dig('operation', 'phase'), stage
        assert_equal 'openai', state['active']
        assert_nil state.dig('sessions', 'one')
        assert_equal 'applying', host.restoration_state['phase']
        assert host.restoration_state['cancelRequested']
        refute host.calls.any? { |call| call[0] == 'scoped-helper' && call[1] == 'scoped-switch' }
      end
    end
  end

  def test_late_cancel_cannot_race_past_the_switching_fence
    with_scoped_provider_host(host_class: CancelPreparationProviderHost) do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      host.cancel_at = 'after-fence'
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      assert_match(/preparation has ended/, host.late_cancel_error)
      assert_equal 'applied', host.send(:provider_state, entry).dig('operation', 'phase')
    end
  end

  def test_cancel_intent_survives_stale_writes_and_worker_restart
    with_scoped_provider_host(host_class: CancelPreparationProviderHost) do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', id, '--session', 'one'])
      stale = host.send(:provider_state, entry)
      stale['operation']['phase'], stale['operation']['stage'] = 'applying', 'preparing'
      host.send(:provider_write_state, entry, stale)
      host.send(:provider_command, ['cancel', 'example', id])
      host.send(:provider_scoped_write_state, entry, stale)
      assert host.send(:provider_state, entry).dig('operation', 'cancelRequested')
      host.restore_failure = true
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      result = host.send(:provider_state, entry)
      assert_equal 'canceled', result.dig('operation', 'phase')
      assert_match(/terminal clients need dev-session sync/, result.dig('operation', 'message'))
      refute host.calls.any? { |call| call[0] == 'scoped-helper' }
    end
  end

  def test_dismiss_preserves_failure_identity_timestamp_and_routing
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      accept_retained_scoped_request(host, entry, ['request', 'example', 'pool', id, '--session', 'one'])
      host.scoped_failure = 'scoped-prepare'
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      before = host.send(:provider_state, entry)
      host.send(:provider_command, ['dismiss', 'example', id])
      after = host.send(:provider_state, entry)
      assert after.dig('operation', 'dismissed')
      persisted = JSON.parse(File.read(host.send(:provider_state_path, entry)))
      refute persisted.fetch('operation').key?('dismissed')
      refute persisted.fetch('operation').key?('cancelRequested')
      assert_equal before['operation'], after['operation'].reject { |key, _| key == 'dismissed' }
      assert_equal 'openai', after['active']
      assert_equal before.dig('operation', 'updatedAt'), host.send(:provider_operation_view, entry, after['operation'])['finishedAt']
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_command, ['dismiss', 'example', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb']) }
    end
  end
end

class WorkspaceHostTest
  def test_creation_writers_preserve_sealed_state_and_refuse_changed_package_or_owner
    original_contract = DevWorkspaceHost::RUNTIME_CONTRACT
    required = {'codexProviderBindingStateSchema' => 3, 'codexProviderForkTransitionPolicy' => 1, 'codexRuntimeAuthoritySchema' => 3}
    faults = %w[explicit inherited generation committing scope selection-incarnation binding-incarnation selection-root selection-provider]
    required.each_key { |key| [nil, 'wrong', 3.0].each { |value| faults << [key, value] } }
    faults.each do |fault|
      with_fork_provider do |host, entry, operation, path, digest, source|
        verified_digest = verified_fork_fixture(host, entry, operation, path, digest, source)
        host.send(:provider_fork_commit, entry, operation, path + '.verified', verified_digest)
        %w[authority recovery terminal].each { |kind| host.send(:provider_fork_project, entry, operation, path, digest, 'one', kind) {} }
        state, = host.send(:provider_fork_state, entry)
        slug, incarnation, root = 'destination', 'b' * 64, 'destination-root'
        slug = 'one' if fault == 'binding-incarnation'
        provider_session_fixture(host, entry, slug, root)
        journal = File.join(entry.fetch('root'), 'worktrees', '.locks', "#{slug}.fork.json")
        FileUtils.mkdir_p(File.dirname(journal))
        intent = {'schema' => 1, 'slug' => slug, 'state' => 'creating', 'tmux_identity' => incarnation}
        intent.merge!('schema' => 2, 'provider' => 'pool') unless fault == 'inherited'
        File.write(journal, JSON.generate(intent), mode: 'w', perm: 0600)
        state['sessions'][slug] = {'provider' => 'pool', 'creationId' => 'f' * 64} if fault == 'selection-incarnation'
        state['sessions'][slug] = {'provider' => 'pool', 'creationId' => incarnation, 'rootThreadId' => 'saved-root'} if fault == 'selection-root'
        state['sessions'][slug] = {'provider' => 'openai', 'creationId' => incarnation} if fault == 'selection-provider'
        if %w[committing scope].include?(fault)
          state['operation'].merge!('phase' => 'applying', 'stage' => fault == 'committing' ? 'committing' : 'preparing')
        end
        host.send(:provider_write_state, entry, state)
        state_path = host.send(:provider_state_path, entry)
        before = File.binread(state_path)
        seal = File.binread(path + '.evidence')
        contract = original_contract.merge(required)
        contract[fault[0]] = fault[1] if fault.is_a?(Array)
        DevWorkspaceHost.send(:remove_const, :RUNTIME_CONTRACT)
        DevWorkspaceHost.const_set(:RUNTIME_CONTRACT, contract.freeze)
        # Exercise the real selected-generation validator; transition locking is
        # doubled by ScopedProviderTestHost, unlike the command composition.
        profile = host.instance_variable_get(:@profile)
        FileUtils.mkdir_p(File.dirname(profile))
        File.symlink(fault == 'generation' ? entry.fetch('root') : host.send(:package_root), profile)
        validator = DevWorkspaceHost::Host.instance_method(:require_current_package_generation!)
        host.define_singleton_method(:require_current_package_generation!) { validator.bind_call(self) }
        host.after_write = lambda do |file, _|
          next unless file == state_path
          File.open(File.join(File.dirname(file), 'request.lock'), File::RDWR) do |contender|
            refute(contender.flock(File::LOCK_EX | File::LOCK_NB), 'writer lost the request mutex')
          end
        end
        if %w[explicit inherited].include?(fault)
          arguments = [slug, incarnation]
          arguments << 'pool' if fault == 'explicit'
          host.send(:provider_bind_creation, entry, arguments)
          host.send(:provider_bind_creation, entry, [slug, incarnation])
          host.send(:provider_bind_root, entry, [slug, root])
          after, = host.send(:provider_fork_state, entry)
          if fault == 'inherited'
            assert_equal(before, File.binread(state_path), 'inherited selection must remain an exact no-op')
          else
            assert_equal({'provider' => 'pool', 'creationId' => incarnation, 'rootThreadId' => root}, after.fetch('sessions').fetch(slug))
            after.fetch('sessions').delete(slug)
            assert_equal(state, after, 'creation writer changed sealed or unrelated state')
          end
        elsif %w[selection-root selection-provider].include?(fault)
          error = assert_raises(DevWorkspaceHost::Error) { host.send(:provider_bind_root, entry, [slug, root]) }
          assert_includes(error.message, 'accepted creation selection')
          assert_equal(before, File.binread(state_path), fault)
        else
          error = assert_raises(DevWorkspaceHost::Error, fault.inspect) { host.send(:provider_bind_creation, entry, [slug, incarnation, 'pool']) }
          expected = fault.is_a?(Array) ? 'consumers are unavailable' : {
            'generation' => 'superseded package generation', 'committing' => 'sealed provider commit', 'scope' => 'scope is fenced',
            'selection-incarnation' => 'another selection incarnation', 'binding-incarnation' => 'existing binding'
          }.fetch(fault)
          assert_includes(error.message, expected, fault.inspect)
          assert_raises(DevWorkspaceHost::Error, fault.inspect) { host.send(:provider_bind_root, entry, [slug, root]) }
          assert_equal(before, File.binread(state_path), fault.inspect)
        end
        assert_equal(seal, File.binread(path + '.evidence'), 'creation changed sealed history')
      end
    end
  ensure
    DevWorkspaceHost.send(:remove_const, :RUNTIME_CONTRACT)
    DevWorkspaceHost.const_set(:RUNTIME_CONTRACT, original_contract)
  end

  def test_bound_fork_retention_reaches_actual_prepare_and_candidate_barriers_first
    %w[active archive-only state-only-pending state-only-completed state-only-retained].each do |kind|
      with_transition_host do |host, paths|
        entry = host.send(:registry).entries.first
        workspace, candidate = entry.fetch('root'), host.candidate
        catalog = {'digest' => 'a' * 64, 'catalog' => {'schema' => 1, 'initialProvider' => 'openai',
          'providers' => [{'id' => 'openai', 'label' => 'OpenAI', 'kind' => 'subscription'}]}}
        host.define_singleton_method(:provider_catalog) { catalog }
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        prepared = host.send(:prepare_profile, ['--source', paths.fetch(:source)])
        profile = host.instance_variable_get(:@profile)
        FileUtils.mkdir_p(File.dirname(profile))
        File.symlink(host.send(:package_root), profile)
        contract_path = File.join(candidate, 'share/workspace-portal/runtime-contract.json')
        base = JSON.parse(File.read(contract_path)).merge('codexProviderScopedStateSchema' => 2,
          'codexProviderScopedTransitionPolicy' => 1, 'codexProviderForkCreationPolicy' => 1)
        socket = host.send(:instance_runtime, entry).fetch(:codex)
        incarnation = 'b' * 64
        state = {'schema' => 3, 'workspace' => entry.fetch('name'), 'root' => workspace, 'socket' => socket,
          'active' => 'openai', 'bindingRevision' => 1, 'bindings' => {
            'source' => {'rootThreadId' => 'origin', 'incarnation' => incarnation, 'revision' => 1,
              'conversations' => {'origin' => {'member' => 'lead', 'threadId' => 'origin', 'provider' => 'openai'}}}}}
        state_path = host.send(:provider_state_path, entry)
        FileUtils.mkdir_p(File.dirname(state_path), mode: 0700)
        File.write(state_path, JSON.generate(state), mode: 'w', perm: 0600)
        retained = [state_path]
        unless kind.start_with?('state-only')
          area = kind == 'active' ? 'work' : 'archive'
          path = File.join(workspace, area, 'source', 'portal.yml')
          FileUtils.mkdir_p(File.dirname(path))
          manifest = {'schema' => 6, 'slug' => 'source', 'repositories' => [], 'artifacts' => [],
            'codex' => {'thread_id' => 'origin', 'socket_path' => socket, 'client_version' => '0.172.0',
              'binding_schema' => 3, 'binding_origin_schema' => 1}}
          manifest.merge!('archive_operation_id' => 'c' * 64, 'finalized_at' => '2026-11-25T12:00:00Z') if area == 'archive'
          File.write(path, YAML.dump(manifest)); retained << path
        end
        source_roster = {'schema' => 1, 'workspace' => workspace, 'slug' => 'source', 'rootThreadId' => 'origin',
          'revision' => 1, 'createdAt' => '2026-11-25T12:00:00Z', 'updatedAt' => '2026-11-25T12:00:00Z', 'members' => []}
        roster_digest = Digest::SHA256.hexdigest(JSON.generate(source_roster))
        capture = {'incarnation' => incarnation, 'revision' => 1, 'threadId' => 'origin', 'rosterDigest' => roster_digest, 'members' => []}
        receipt = legacy_portal_creation_receipt(workspace, 'destination', 'd' * 64)
        receipt['request'].delete('goal')
        receipt['request'].merge!('kind' => 'fork', 'source' => 'source', 'sourceThreadId' => 'origin',
          'sourceIdentity' => 'e' * 64, 'sourceFork' => capture)
        receipt['state'] = kind.end_with?('pending') ? 'failed' : 'ready'
        creations = File.join(host.instance_variable_get(:@state), 'portal',
          "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}", 'creations')
        FileUtils.mkdir_p(creations, mode: 0700)
        receipt_path = File.join(creations, 'destination.json')
        File.write(receipt_path, JSON.generate(receipt), mode: 'w', perm: 0600); retained << receipt_path
        if kind.end_with?('pending') || kind.end_with?('completed')
          path = File.join(workspace, 'worktrees', '.locks', 'destination.fork.json')
          FileUtils.mkdir_p(File.dirname(path))
          journal = {'schema' => 1, 'slug' => 'destination', 'source_slug' => 'source', 'source_thread_id' => 'origin',
            'model' => nil, 'effort' => nil, 'requested_model' => nil, 'requested_effort' => nil, 'state' => 'creating',
            'tmux_identity' => 'f' * 64, 'source_fork' => capture,
            'root_fork' => kind.end_with?('pending') ? {'state' => 'intent'} : {'state' => 'known', 'threadId' => 'destination-root'}}
          File.write(path, JSON.generate(journal), mode: 'w', perm: 0600); retained << path
        else
          # Retained destination snapshot after ordinary outer-journal retirement.
          directory = File.join(host.instance_variable_get(:@state), 'codex-teams',
            "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}")
          FileUtils.mkdir_p(directory, mode: 0700)
          path = File.join(directory, 'destination.json')
          snapshot = source_roster.merge('slug' => 'destination', 'rootThreadId' => 'destination-root',
            'forkSource' => {'workspace' => workspace, 'slug' => 'source', 'rootThreadId' => 'origin',
              'sourceNativeThreadId' => 'origin', 'revision' => 1, 'digest' => roster_digest, 'members' => []})
          File.write(path, JSON.generate(snapshot), mode: 'w', perm: 0600); retained << path
        end
        before = retained.to_h { |path| [path, File.binread(path)] }
        token = host.send(:profile_link_token)
        host.events.clear
        compatibility = host.method(:require_compatible_provider_state!)
        observed = []
        host.define_singleton_method(:require_compatible_provider_state!) do |package|
          path = send(:transition_lock_file)
          FileUtils.mkdir_p(File.dirname(path))
          File.open(path, File::RDWR | File::CREAT, 0600) { |probe| observed << !probe.flock(File::LOCK_SH | File::LOCK_NB) }
          compatibility.call(package)
        end
        operations = [
          -> { host.send(:prepare_profile, ['--source', paths.fetch(:source)]) },
          -> { host.send(:with_transition_lock) { host.send(:activate_candidate, candidate, prepared:) } }
        ]
        operations.each_with_index do |operation, index|
          File.write(contract_path, JSON.generate(base))
          error = assert_raises(DevWorkspaceHost::Error, &operation)
          assert_includes error.message, 'preserve Codex conversation bindings', kind
          assert_equal(index == 1, observed.last, 'activation must repeat under real transition EX')
          token.nil? ? assert_nil(host.send(:profile_link_token)) : assert_equal(token, host.send(:profile_link_token))
          before.each { |path, bytes| assert_equal(bytes, File.binread(path), kind) }
          refute(host.events.any? { |event| %i[profile_set sessions_quiesced configured consumers_restarted].include?(event.first) })
        end
        # The package owner reads retained bindings without enabling ordinary commands.
        File.write(contract_path, JSON.generate(base.merge('codexProviderBindingStateSchema' => 3,
          'codexProviderForkTransitionPolicy' => 1, 'codexRuntimeAuthoritySchema' => 3)))
        host.send(:require_compatible_provider_state!, candidate)
        host.team_preflight_output = JSON.generate('schema' => 1, 'required' => false, 'sessions' => [])
        if kind.end_with?('pending') || kind.end_with?('completed')
          operations.each do |operation|
            error = assert_raises(DevWorkspaceHost::Error, &operation)
            assert_includes(error.message, 'unfinished session operations')
            before.each { |path, bytes| assert_equal(bytes, File.binread(path), kind) }
          end
        else
          prepared = host.send(:prepare_profile, ['--source', paths.fetch(:source)])
          host.send(:with_transition_lock) { host.send(:activate_candidate, candidate, prepared:) }
          assert_equal(candidate, File.realpath(profile))
          before.each { |path, bytes| assert_equal(bytes, File.binread(path), kind) }
        end
        assert_equal(3, host.send(:provider_state, entry).fetch('schema'))
      end
    end
  end
end
