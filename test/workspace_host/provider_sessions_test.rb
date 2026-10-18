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

class WorkspaceHostTest
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
