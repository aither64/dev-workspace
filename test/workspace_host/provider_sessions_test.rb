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
         'thread' => {'id' => id, 'slug' => 'one', 'cwd' => File.join(entry['root'], 'work', 'one')}}
      end
      candidate = host.send(:provider_fork_origin_reservation, state, participants.map { |p| p['current'] })
      candidate['operation'] = o.merge('phase' => 'applying', 'stage' => 'preparing', 'snapshot' => path)
      proof = {'schema' => 1, 'strategy' => 'fork-v1', 'operationId' => operation, 'workspace' => entry['name'], 'root' => entry['root'], 'socket' => socket,
        'sourcePackage' => '/nix/store/frozen-package', 'catalogDigest' => 'a' * 64, 'previousStateDigest' => Digest::SHA256.hexdigest(raw),
        'socketIdentity' => 'synthetic-generation', 'rosters' => {'one' => {'rootThreadId' => 'root-one', 'incarnation' => 'c' * 64, 'revision' => 0}},
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
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_state, entry) } # Fork dispatch stays off even in schema 2.
      once = true
      host.after_write = ->(p, _) { if once && p.end_with?('.journal'); once = false; raise 'crash after journal'; end }
      assert_raises(RuntimeError) { host.send(:provider_fork_reserve, entry, operation, path, digest) }
      assert_equal 2, JSON.parse(File.binread(host.send(:provider_state_path, entry)))['schema']
      host.send(:provider_fork_reserve, entry, operation, path, digest)
      state, = host.send(:provider_fork_state, entry)
      assert_equal [3, 1, 'openai'], state.values_at('schema', 'bindingRevision', 'active')
      assert_equal %w[member-one root-one], state['bindings']['one']['conversations'].keys.sort
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_state, entry) } # Production stays off.
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

  def test_scoped_provider_switch_never_stops_the_shared_daemon_or_other_sessions
    with_scoped_provider_host do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      provider_session_fixture(host, entry, 'two', 'root-two')
      host.send(:provider_command, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
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
      host.send(:provider_command, ['request', 'example', 'openai', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
      assert_equal 'applied', host.send(:provider_state, entry).dig('operation', 'phase')
      host.send(:provider_command, ['request', 'example', 'pool', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'])
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      state = host.send(:provider_state, entry)
      assert_equal ['two'], state.dig('operation', 'sessions')
      assert_equal 'pool', state['active']
      assert_equal 'openai', host.send(:provider_session_selection, entry, state, 'one')
      host.send(:provider_command, ['request', 'example', 'pool', 'cccccccc-cccc-4ccc-8ccc-cccccccccccc', '--session', 'one', '--inherit'])
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
      host.send(:provider_command, ['request', 'example', 'pool', id, '--session', 'one'])
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
      host.send(:provider_command, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
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
      host.send(:provider_command, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
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
      host.send(:provider_command, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'])
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
        host.send(:provider_command, ['request', 'example', 'pool', id, '--session', 'one'])
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
      host.send(:provider_command, ['request', 'example', 'pool', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '--session', 'one'])
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      assert_match(/preparation has ended/, host.late_cancel_error)
      assert_equal 'applied', host.send(:provider_state, entry).dig('operation', 'phase')
    end
  end

  def test_cancel_intent_survives_stale_writes_and_worker_restart
    with_scoped_provider_host(host_class: CancelPreparationProviderHost) do |host, entry|
      provider_session_fixture(host, entry, 'one', 'root-one')
      id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      host.send(:provider_command, ['request', 'example', 'pool', id, '--session', 'one'])
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
      host.send(:provider_command, ['request', 'example', 'pool', id, '--session', 'one'])
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
