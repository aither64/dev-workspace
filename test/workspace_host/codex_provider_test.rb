# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'
require 'socket'

class CodexProviderTestHost < DevWorkspaceHost::Host
  attr_accessor :catalog, :processes, :idle_blockers, :cancel_on_sleep, :on_pause, :before_write, :after_write, :helper_failure, :on_restore, :service_states, :binaries, :children
  attr_reader :calls, :lock_modes

  def initialize(**arguments)
    super
    @calls, @lock_modes, @processes, @idle_blockers = [], [], [123], []
  end

  private

  def provider_catalog = @catalog
  def with_current_generation_lock(mode: File::LOCK_EX, **)
    @lock_modes << mode
    yield
  end
  def system!(*arguments) = @calls << arguments
  def provider_native_binary = '/proved/native/codex'
  def codex_version(_command) = '0.160.0'
  def provider_process_binary(pid)
    return @children.fetch(pid).fetch(:binary) if @children&.key?(pid)
    @binaries&.shift || '/proved/native/codex'
  end
  def provider_process_parent(pid) = @children.fetch(pid).fetch(:parent)
  def provider_process_arguments(pid) = @children.fetch(pid).fetch(:arguments)
  def load_registration_marker(_entry) = nil
  def provider_cgroup_processes(_group) = @processes
  def capture!(*arguments)
    @calls << arguments
    return @service_states.shift if arguments.include?('--property=MainPID,ActiveState') && @service_states
    arguments.include?('--value') ? "123\n" : "MainPID=123\nControlGroup=/owned/codex.service\n"
  end
  def require_no_unfinished_lifecycle_operations!(*, **) = nil
  def provider_helper(entry, action, **)
    raise 'injected helper failure' if @helper_failure == action
    @calls << ['helper', action]
    return {'verified' => true} if action == 'health'
    raise "unexpected helper #{action}" unless action == 'inventory'
    {'schema' => 1, 'workspace' => entry.fetch('root'), 'threads' => [], 'loaded' => [], 'blockers' => @idle_blockers}
  end
  def provider_write_state(entry, state)
    @before_write&.call
    super
    @after_write&.call
  end
  def provider_restore_terminals(entry, snapshot)
    @on_restore&.call(entry, snapshot)
  end
  def provider_worker_pause
    return @on_pause.call if @on_pause
    if @cancel_on_sleep
      provider_command(['cancel', *@cancel_on_sleep])
      @cancel_on_sleep = nil
    else
      throw :worker_stopped
    end
  end
  def sleep(_seconds) = nil
end

class ProviderDiagnosticTestHost < CodexProviderTestHost
  attr_accessor :faults, :real_readiness
  attr_reader :written_states

  def initialize(**arguments)
    super
    @faults, @written_states = {}, []
  end

  private

  def fault(point)
    error = @faults[point]&.shift
    raise error if error
  end

  def provider_helper(entry, action, **)
    @calls << ['helper', action]
    fault(['helper', action])
    return {'schema' => 1, 'workspace' => entry.fetch('root'), 'threads' => [], 'loaded' => [], 'blockers' => []} if %w[inventory capture].include?(action)
    {'verified' => true}
  end

  def provider_private_server(_entry, provider)
    @calls << ['private', provider]
    fault(['private', provider])
    yield '/private/synthetic-socket'
  end

  def quiesce_sessions(*, **) = @calls << ['quiesce']

  def provider_wait_shared_server(entry, diagnostic: nil)
    @calls << ['readiness']
    @checking_readiness = true
    super if @real_readiness
  ensure
    @checking_readiness = false
  end

  def provider_process_binary(pid)
    fault(['identity']) if @checking_readiness
    super
  end

  def capture!(*arguments)
    fault(['readiness-properties']) if arguments.include?('--property=MainPID,ActiveState')
    super
  end

  def provider_write_state(entry, state)
    super
    @written_states << [state.dig('operation', 'phase'), state.dig('operation', 'stage'), state['active']]
  end
end

class WorkspaceHostTest
  def with_provider_host(host_class: CodexProviderTestHost)
    Dir.mktmpdir('provider-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(name: 'example', root:, hostname: 'workspace.example.test', aliases: [], replace: false)
      host = host_class.new(env: host_environment(directory, config:), out: StringIO.new, err: StringIO.new)
      host.catalog = {'digest' => 'a' * 64, 'catalog' => {'schema' => 1, 'initialProvider' => 'openai', 'providers' => [
        {'id' => 'openai', 'label' => 'Subscription', 'kind' => 'subscription'},
        {'id' => 'pool', 'label' => 'Pool', 'kind' => 'codex-lb', 'baseURL' => 'http://127.0.0.1:2455/v1', 'tokenFile' => '/private/token'}
      ]}}
      yield host, DevWorkspaceHost::Registry.new(config).find('example')
    end
  end

  def test_provider_idempotency_does_not_resurrect_a_canceled_request
    with_provider_host do |host, entry|
      first, second = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'
      host.send(:provider_command, ['request', 'example', 'pool', first])
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_command, ['request', 'example', 'openai', second]) }
      host.send(:provider_command, ['cancel', 'example', first])
      host.send(:provider_command, ['request', 'example', 'openai', second])
      host.send(:provider_command, ['request', 'example', 'pool', first])
      state = host.send(:provider_state, entry)
      assert_equal second, state.dig('operation', 'id')
      assert_equal 'applied', state.dig('operation', 'phase')
      assert_equal 'openai', state['active']
    end
  end

  def test_legacy_provider_cancel_loses_after_applying_is_durable
    with_provider_host do |host, entry|
      identity = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      host.send(:legacy_provider_command, ['request', 'example', 'pool', identity])
      host.send(:provider_locked, entry) do
        state = host.send(:provider_state, entry)
        state['operation']['phase'] = 'applying'
        state['operation']['stage'] = 'quiescing'
        host.send(:provider_write_state, entry, state)
      end
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_command, ['cancel', 'example', identity]) }
      assert_equal 'applying', host.send(:provider_state, entry).dig('operation', 'phase')
    end
  end

  def test_legacy_waiting_provider_request_does_not_take_exclusive_transition_lock
    with_provider_host do |host, entry|
      identity = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      host.send(:legacy_provider_command, ['request', 'example', 'pool', identity])
      host.idle_blockers = ['A thread has an active goal.']
      host.cancel_on_sleep = ['example', identity]
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      assert host.lock_modes.all? { |mode| mode == File::LOCK_SH }
      assert_equal 'canceled', host.send(:provider_state, entry).dig('operation', 'phase')
      refute host.calls.any? { |call| call.include?('stop') }
    end
  end

  def test_provider_cgroup_scan_preserves_literal_systemd_paths_and_descendants
    with_provider_host do |host, _entry|
      Dir.mktmpdir('provider-cgroup-test') do |directory|
        group = File.join(directory, 'app-workspace\\x2dcodex.slice', 'workspace-codex@fixture.service')
        FileUtils.mkdir_p(File.join(group, 'child'))
        File.write(File.join(group, 'cgroup.procs'), "123\n456\n")
        File.write(File.join(group, 'child', 'cgroup.procs'), "456\n789\n")
        scan = DevWorkspaceHost::Host.instance_method(:provider_cgroup_processes).bind(host)
        assert_equal [123, 456, 789], scan.call(group).sort
      end
    end
  end

  def test_provider_restart_refuses_an_additional_vm_process
    with_provider_host do |host, entry|
      host.send(:require_provider_restart_safe!, entry)
      host.processes = [123, 456]
      host.children = {456 => {binary: '/unowned/qemu', parent: 123, arguments: ['/unowned/qemu']}}
      error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry) }
      assert_match(/additional or unproved processes/, error.message)
      refute host.calls.any? { |call| call.include?('stop') }
    end
  end

  def test_recovery_before_daemon_stop_leaves_native_work_running
    with_provider_host do |host, entry|
      identity = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      host.send(:legacy_provider_command, ['request', 'example', 'pool', identity])
      state = host.send(:provider_state, entry)
      snapshot = File.join(host.send(:provider_directory, entry), "#{identity}.snapshot.json")
      host.send(:atomic_private_file, snapshot, JSON.generate('schema' => 1, 'threads' => []))
      state['operation'].merge!('phase' => 'recovery-required', 'stage' => 'quiescing', 'snapshot' => snapshot)
      host.send(:provider_write_state, entry, state)
      host.processes = [123, 456]
      host.send(:provider_recover, entry)
      assert_equal 'failed', host.send(:provider_state, entry).dig('operation', 'phase')
      assert_equal 'openai', host.send(:provider_state, entry)['active']
      refute host.calls.any? { |call| call.include?('stop') }
    end
  end

  def test_provider_request_and_receipt_survive_the_same_atomic_commit
    [:before_write, :after_write].each do |fault|
      with_provider_host do |host, entry|
        id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
        host.public_send("#{fault}=", -> { raise 'simulated interruption' })
        assert_raises(RuntimeError) { host.send(:provider_command, ['request', 'example', 'pool', id]) }
        saved = host.send(:provider_state, entry)
        assert_equal(fault == :after_write, saved.dig('receipts', id) != nil)
        assert_equal(fault == :after_write, saved.dig('operation', 'id') == id)
        host.public_send("#{fault}=", nil)
        host.send(:provider_command, ['request', 'example', 'pool', id])
        saved = host.send(:provider_state, entry)
        assert_equal id, saved.dig('operation', 'id')
        assert_equal 'pool', saved.dig('receipts', id, 'target')
      end
    end
  end

  def test_worker_observes_request_accepted_after_its_terminal_observation
    with_provider_host do |host, entry|
      first, second = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'
      host.send(:legacy_provider_command, ['request', 'example', 'openai', first])
      observed = false
      host.idle_blockers = ['Still busy.']
      host.on_pause = lambda do
        unless observed
          observed = true
          host.send(:legacy_provider_command, ['request', 'example', 'pool', second])
          next
        end
        assert_equal second, host.send(:provider_state, entry).dig('operation', 'id')
        assert_equal ['Still busy.'], host.send(:provider_state, entry).dig('operation', 'blockers')
        throw :worker_stopped
      end
      catch(:worker_stopped) { host.send(:run_provider_worker, ['example']) }
      assert host.calls.include?(['helper', 'inventory'])
    end
  end

  def test_member_environment_keeps_the_member_address
    with_provider_host do |host, entry|
      root = host.send(:provider_thread_environment, entry, 'example')
      member = host.send(:provider_thread_environment, entry, 'example', 'implementer2')
      refute root.key?('DEV_SESSION_MEMBER_ADDRESS')
      assert_equal 'implementer2', member.delete('DEV_SESSION_MEMBER_ADDRESS')
      assert_equal root, member
    end
  end

  def test_pre_stop_capture_refusal_leaves_daemon_and_terminals_untouched
    with_provider_host do |host, entry|
      id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      host.send(:legacy_provider_command, ['request', 'example', 'pool', id])
      state = host.send(:provider_state, entry)
      snapshot = File.join(host.send(:provider_directory, entry), "#{id}.snapshot.json")
      host.send(:atomic_private_file, snapshot, JSON.generate('schema' => 1, 'threads' => []))
      state['operation'].merge!('phase' => 'applying', 'stage' => 'capturing', 'snapshot' => snapshot)
      host.send(:provider_write_state, entry, state)
      host.helper_failure = 'capture'
      host.send(:provider_apply, entry, state, snapshot)
      assert_equal 'failed', host.send(:provider_state, entry).dig('operation', 'phase')
      refute host.calls.any? { |call| call.include?('stop') }
      refute File.exist?(snapshot + '.terminals')
    end
  end

  def test_terminal_restoration_notice_survives_process_interruption
    with_provider_host do |host, entry|
      id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      host.send(:legacy_provider_command, ['request', 'example', 'pool', id])
      state = host.send(:provider_state, entry)
      snapshot = File.join(host.send(:provider_directory, entry), "#{id}.snapshot.json")
      host.send(:atomic_private_file, snapshot, JSON.generate('schema' => 1, 'threads' => []))
      state['operation'].merge!('phase' => 'applying', 'stage' => 'quiescing', 'snapshot' => snapshot)
      host.send(:provider_write_state, entry, state)
      host.on_restore = ->(*) { raise SystemExit }
      assert_raises(SystemExit) { host.send(:provider_recover, entry) }
      saved = host.send(:provider_state, entry)
      assert_equal 'failed', saved.dig('operation', 'phase')
      assert_match(/restoration is pending/, saved.dig('operation', 'message'))
    end
  end

  def test_shared_readiness_waits_for_native_protocol_and_refuses_early_exit
    with_provider_host do |host, entry|
      socket = host.send(:instance_runtime, entry).fetch(:codex)
      FileUtils.mkdir_p(File.dirname(socket))
      server = UNIXServer.new(socket)
      host.service_states = ["MainPID=123\nActiveState=active\n"] * 2
      host.binaries = ['/startup/wrapper', '/proved/native/codex']
      host.send(:provider_wait_shared_server, entry)
      assert_equal 1, host.calls.count(['helper', 'health'])
      host.service_states = ["MainPID=0\nActiveState=failed\n"]
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_wait_shared_server, entry) }
    ensure
      server&.close
    end
  end

  def test_idle_helpers_require_exact_process_and_member_identity
    with_provider_host do |host, entry|
      command = File.join(host.send(:package_root), 'bin/workspace-portal')
      args = ['team-mcp', '--workspace', entry.fetch('root'), '--member-address', 'implementer1']
      inventory = {'blockers' => [], 'threads' => [{'memberAddress' => 'implementer1', 'settings' => {'policy' => {'mcpServer' => {'Args' => args}}}}]}
      host.processes = [123, 456, 789]
      host.children = {
        456 => {binary: command, parent: 123, arguments: [command, *args]},
        789 => {binary: '/proved/native/codex-code-mode-host', parent: 123, arguments: ['/proved/native/codex-code-mode-host']}
      }
      host.send(:require_provider_restart_safe!, entry, inventory)
      host.children[456][:arguments] = [command, 'team-mcp', '--member-address', 'foreign']
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry, inventory) }
      host.children[456][:arguments] = [command, *args]
      host.children[456][:parent] = 789
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry, inventory) }
      inventory['blockers'] = ['active turn']
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry, inventory) }
    end
  end

  def test_daemon_reporting_helpers_require_exact_validated_launch_and_idle_parent
    with_provider_host do |host, entry|
      directory = File.dirname(host.instance_variable_get(:@state))
      package = make_package(directory, 'reporting-launch')
      contract = File.join(package, 'share/workspace-portal/runtime-contract.json')
      File.write(contract, JSON.generate(JSON.parse(File.read(contract)).merge('workspaceReportingPolicy' => 1)))
      host.define_singleton_method(:package_root) { package }
      host.instance_variable_set(:@profile, File.join(directory, 'custom-profile'))
      host.define_singleton_method(:load_registration_marker) do |selected|
        DevWorkspaceHost::Host.instance_method(:load_registration_marker).bind_call(self, selected)
      end
      command = File.join(package, 'bin/workspace-portal')
      args = ['team-mcp', '--user-state-root', host.instance_variable_get(:@state), '--workspace', entry.fetch('root'),
        '--host-profile', host.instance_variable_get(:@profile)]
      values = {'command' => command, 'args' => args, 'required' => true, 'enabled_tools' => ['report_to_lead'],
        'tools.report_to_lead.approval_mode' => 'approve'}
      argv = values.flat_map { |key, value| ['-c', "mcp_servers.team_reporting.#{key}=#{JSON.generate(value)}"] }
      plan = DevWorkspaceHost::RegistrationPlan.new(package_root: package, argv: argv, digest: 'a' * 64,
        policy: 1, required_native_child_threads: 0, states: [])
      host.send(:write_registration_marker, entry, '/proved/native/codex', plan)
      host.processes = [123, 456]
      host.children = {456 => {binary: command, parent: 123, arguments: [command, *args]}}
      inventory = {'blockers' => [], 'threads' => []}
      host.send(:require_provider_restart_safe!, entry, inventory)
      # Exact old launch remains owned while a compatible selected package is
      # completing forward recovery; the marker retains its original command.
      replacement = make_package(directory, 'new-selected')
      host.define_singleton_method(:package_root) { replacement }
      host.send(:require_provider_restart_safe!, entry, inventory)
      host.define_singleton_method(:package_root) { package }
      [2, 4, 6].each do |position|
        changed = args.dup; changed[position] = '/foreign'
        host.children[456][:arguments] = [command, *changed]
        assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry, inventory) }
      end
      host.children[456][:arguments] = [command, *args, '--unexpected']
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry, inventory) }
      host.children[456][:arguments] = [command, *args]
      host.children[456][:parent] = 999
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry, inventory) }
      host.children[456][:parent] = 123
      inventory['blockers'] = ['active member']
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry, inventory) }
      inventory['blockers'] = []
      marker_path = host.send(:registration_marker_path, entry)
      original = File.binread(marker_path)
      File.write(marker_path, '{}')
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry, inventory) }
      File.write(marker_path, original)
      host.children[456][:binary] = '/foreign/workspace-portal'
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_provider_restart_safe!, entry, inventory) }
      refute host.calls.any? { |call| call.include?('stop') }
    end
  end

  def prepare_diagnostic_apply(host, entry)
    id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
    host.send(:legacy_provider_command, ['request', 'example', 'pool', id])
    state = host.send(:provider_state, entry)
    snapshot = File.join(host.send(:provider_directory, entry), "#{id}.snapshot.json")
    host.send(:atomic_private_file, snapshot, JSON.generate('schema' => 1, 'threads' => []))
    state['operation'].merge!('phase' => 'applying', 'stage' => 'capturing', 'snapshot' => snapshot, 'previousActive' => 'openai')
    host.send(:provider_write_state, entry, state)
    host.calls.clear
    host.written_states.clear
    [state, snapshot]
  end

  def provider_failure_records(host)
    host.instance_variable_get(:@err).string.lines.map do |line|
      assert_operator line.bytesize, :<=, 2048
      assert line.start_with?('PROVIDER_FAILURE ')
      refute_match(/SECRET_CANARY|private|https:|Bearer|argv|snapshot|rpc-payload/, line)
      data = JSON.parse(line.delete_prefix('PROVIDER_FAILURE '))
      assert_operator data.fetch('frames').length, :<=, 4
      assert data.fetch('frames').all? { |frame| %w[workspace-host workspace-codex-providers.rb].include?(frame.fetch('source')) && frame.fetch('line').is_a?(Integer) }
      data
    end
  end

  def test_provider_failure_diagnostics_separate_forward_and_recovery_without_changing_order
    outcomes = []
    [:working, :broken_sink, :broken_formatter].each do |sink|
      with_provider_host(host_class: ProviderDiagnosticTestHost) do |host, entry|
        state, snapshot = prepare_diagnostic_apply(host, entry)
        forward = Errno::EIO.new('SECRET_CANARY /private/token argv Bearer rpc-payload' * 4096)
        recovery = Class.new(StandardError).new('SECRET_CANARY https://private/endpoint')
        if sink == :broken_sink
          broken = Object.new
          broken.define_singleton_method(:write) { |_| raise IOError, 'SECRET_CANARY sink' }
          host.instance_variable_set(:@err, broken)
        elsif sink == :broken_formatter
          [forward, recovery].each { |error| error.define_singleton_method(:backtrace_locations) { raise 'SECRET_CANARY formatter' } }
        end
        host.faults[['helper', 'verify']] = [forward, recovery]
        error = assert_raises(DevWorkspaceHost::Error) { host.send(:provider_apply, entry, state, snapshot) }
        assert_equal 'provider recovery could not be verified', error.message
        saved = host.send(:provider_state, entry)
        assert_equal 'recovery-required', saved.dig('operation', 'phase')
        assert_equal 'openai', saved['active']
        outcomes << [host.calls, host.written_states, saved.dig('operation', 'message')]
        if sink == :working
          records = provider_failure_records(host)
          assert_equal %w[forward recovery], records.map { |record| record.fetch('direction') }
          assert_equal %w[verify verify], records.map { |record| record.fetch('step') }
          assert_equal ['Errno::EIO', 'OtherError'], records.map { |record| record.fetch('exception') }
          assert_equal Errno::EIO::Errno, records.first.fetch('errno')
          assert records.all? { |record| record.fetch('operation') == state.dig('operation', 'id') && record.fetch('stage') == 'shared-start' }
          assert records.all? { |record| record.fetch('elapsedMs').is_a?(Integer) }
          refute records.any? { |record| record.key?('readiness') }
        elsif sink == :broken_formatter
          assert_empty provider_failure_records(host)
        end
      end
    end
    assert_equal outcomes.first, outcomes[1]
    assert_equal outcomes.first, outcomes[2]
    calls, states = outcomes.first
    assert_equal %w[stop start stop start], calls.select { |call| call.first == 'systemctl' && %w[stop start].include?(call[2]) }.map { |call| call[2] }
    assert_equal ['capture', 'inventory', 'resume', 'verify', 'inventory', 'resume', 'verify'], calls.select { |call| call.first == 'helper' }.map(&:last)
    assert_equal [
      ['applying', 'quiescing', 'openai'], ['applying', 'daemon-stop', 'openai'],
      ['applying', 'shared-start', 'pool'], ['recovery-required', 'shared-start', 'pool'],
      ['recovery-required', 'shared-start', 'openai'], ['recovery-required', 'shared-start', 'openai']
    ], states
  end

  def test_provider_failure_diagnostics_leave_success_and_compensation_state_compatible
    with_provider_host(host_class: ProviderDiagnosticTestHost) do |host, entry|
      state, snapshot = prepare_diagnostic_apply(host, entry)
      host.send(:provider_apply, entry, state, snapshot)
      assert_empty provider_failure_records(host)
      assert_equal 'applied', host.send(:provider_state, entry).dig('operation', 'phase')
      state, snapshot = prepare_diagnostic_apply(host, entry)
      host.faults[['helper', 'verify']] = [RuntimeError.new('SECRET_CANARY')]
      host.send(:provider_apply, entry, state, snapshot)
      saved = host.send(:provider_state, entry)
      assert_equal 'failed', saved.dig('operation', 'phase')
      assert_equal 'openai', saved['active']
      assert_equal ['forward'], provider_failure_records(host).map { |record| record['direction'] }
      assert_equal %w[blockers catalogDigest createdAt id phase previousActive snapshot stage target updatedAt message].sort, saved.fetch('operation').keys.sort
    end
  end

  def test_provider_failure_readiness_keeps_only_current_attempt_observations
    cases = [
      {fault: ['readiness-properties'], error: RuntimeError.new('SECRET_CANARY'), expected: {'mainPID' => 'unavailable', 'activeState' => 'unavailable', 'executableMatch' => 'unavailable', 'socketPresent' => 'unavailable', 'healthError' => 'unavailable'}},
      {fault: ['identity'], error: Errno::ENOENT.new('SECRET_CANARY'), expected: {'mainPID' => 123, 'activeState' => 'active', 'executableMatch' => 'unavailable', 'socketPresent' => 'unavailable', 'healthError' => 'unavailable'}},
      {fault: ['helper', 'health'], error: IOError.new('SECRET_CANARY'), expected: {'mainPID' => 123, 'activeState' => 'active', 'executableMatch' => true, 'socketPresent' => true, 'healthError' => 'IOError'}}
    ]
    cases.each do |test|
      with_provider_host(host_class: ProviderDiagnosticTestHost) do |host, entry|
        state, snapshot = prepare_diagnostic_apply(host, entry)
        host.real_readiness = true
        host.faults[test.fetch(:fault)] = [test.fetch(:error)]
        host.service_states = ["MainPID=123\nActiveState=active\n"] * 2
        # Existing deadline expires after the first failed health check, with no
        # actual wait, service process or socket. Recovery gets its own context.
        tick = 0
        clock = ->(*) { tick += 1; tick == 3 ? 31.0 : 0.0 }
        original_clock = Process.method(:clock_gettime)
        original_socket = File.method(:socket?)
        begin
          Process.define_singleton_method(:clock_gettime, &clock)
          File.define_singleton_method(:socket?) { |_| true }
          host.send(:provider_apply, entry, state, snapshot)
        ensure
          Process.define_singleton_method(:clock_gettime, original_clock)
          File.define_singleton_method(:socket?, original_socket)
        end
        records = provider_failure_records(host)
        assert_equal 1, records.length
        assert_equal 'shared-readiness', records.first.fetch('step')
        assert_equal test.fetch(:expected), records.first.fetch('readiness')
        assert records.first.fetch('frames').any? if test.fetch(:fault) == ['helper', 'health']
        assert_equal 'failed', host.send(:provider_state, entry).dig('operation', 'phase')
      end
    end
  end

  def test_provider_failure_readiness_does_not_reuse_forward_values_in_recovery
    with_provider_host(host_class: ProviderDiagnosticTestHost) do |host, entry|
      state, snapshot = prepare_diagnostic_apply(host, entry)
      host.real_readiness = true
      host.service_states = ["MainPID=123\nActiveState=active\n"]
      host.faults[['identity']] = [Errno::ENOENT.new('SECRET_CANARY')]
      host.faults[['readiness-properties']] = [nil, RuntimeError.new('SECRET_CANARY')]
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_apply, entry, state, snapshot) }
      forward, recovery = provider_failure_records(host)
      assert_equal 123, forward.dig('readiness', 'mainPID')
      assert_equal 'active', forward.dig('readiness', 'activeState')
      assert_equal ['unavailable'], recovery.fetch('readiness').values.uniq
      assert_equal %w[forward recovery], [forward, recovery].map { |record| record['direction'] }
    end
  end

  def test_provider_failure_readiness_retains_last_observation_when_next_read_fails
    with_provider_host(host_class: ProviderDiagnosticTestHost) do |host, entry|
      state, snapshot = prepare_diagnostic_apply(host, entry)
      host.real_readiness = true
      host.service_states = ["MainPID=123\nActiveState=active\n"] * 2
      host.faults[['helper', 'health']] = [IOError.new('SECRET_CANARY')]
      host.faults[['readiness-properties']] = [nil, RuntimeError.new('SECRET_CANARY')]
      original_socket = File.method(:socket?)
      begin
        File.define_singleton_method(:socket?) { |_| true }
        host.send(:provider_apply, entry, state, snapshot)
      ensure
        File.define_singleton_method(:socket?, original_socket)
      end
      record = provider_failure_records(host).fetch(0)
      assert_equal({'mainPID' => 123, 'activeState' => 'active', 'executableMatch' => true, 'socketPresent' => true, 'healthError' => 'IOError'}, record['readiness'])
      assert_equal 'failed', host.send(:provider_state, entry).dig('operation', 'phase')
    end
  end

  def test_provider_failure_projection_bounds_untrusted_error_metadata
    with_provider_host do |host, _entry|
      operation = {'id' => 'SECRET_CANARY' * 4096, 'stage' => 'SECRET_CANARY'}
      diagnostic = {step: 'verify', started: Process.clock_gettime(Process::CLOCK_MONOTONIC)}
      host.send(:provider_failure_diagnostic, operation, 'forward', diagnostic, Class.new(StandardError).new('SECRET_CANARY' * 4096))
      record = provider_failure_records(host).fetch(0)
      assert_equal 'unavailable', record['operation']
      assert_equal 'unavailable', record['stage']
      assert_equal 'OtherError', record['exception']
      assert_equal 'unavailable', record['errno']
    end
  end

  def test_recovery_vm_wait_returns_terminal_to_python_and_keeps_expected_recovery_phases
    fixture = File.expand_path('../../nix/tests/session-recovery-boot.nix', __dir__)
    script = <<~'PY'
      import ast, contextlib, io, json, re, shlex, sys, textwrap
      from pathlib import Path
      source = Path(sys.argv[1]).read_text()
      driver = source.split("  testScript = ''\n", 1)[1].rsplit("  '';", 1)[0]
      driver = textwrap.dedent(re.sub(r"\$\{[^}]+\}", "/synthetic-store/path", driver))
      tree = ast.parse(driver)
      names = {'wait_provider_result', 'wait_phase', 'collect_provider_diagnostics'}
      functions = ast.Module(body=[node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in names], type_ignores=[])
      class Machine:
          def __init__(self):
              self.phase = 'waiting'
              self.calls = 0
              self.collection = []
              self.collection_timeouts = []
              self.fail_collection = False
          def wait_until_succeeds(self, command):
              self.calls += 1
              expression = shlex.split(shlex.split(command)[-1])[-1]
              def safe_open(path):
                  assert path == '/fixture/state.json'
                  return io.StringIO(json.dumps({'operation': {'phase': self.phase, 'blockers': ['synthetic']}}))
              output = io.StringIO()
              with contextlib.redirect_stdout(output):
                  exec(expression, {'open': safe_open})
              return output.getvalue()
          def execute(self, command, *, timeout):
              assert timeout == 10
              self.collection.append(command)
              self.collection_timeouts.append(timeout)
              if self.fail_collection:
                  raise TimeoutError('SECRET_CANARY collection')
              return 0, 'safe-fixture-evidence'
      machine = Machine()
      scope = dict(machine=machine, json=json, shlex=shlex, state_path='/fixture/state.json', user_prefix='', host='/synthetic-host', user=lambda command: '{}', diagnostic_start='123', worker='workspace-codex-provider@fixture.service', codex_unit='workspace-codex@fixture.service')
      exec(compile(functions, 'selected-vm-functions', 'exec'), scope)
      wait = scope['wait_phase']
      for phase in ['applied', 'failed', 'recovery-required', 'canceled']:
          machine.phase = phase
          wait(phase)  # Expected phase is checked before terminal classification.
      assert not machine.collection
      for phase, expected in [('failed', 'applied'), ('canceled', 'applied'), ('applied', 'waiting')]:
          machine.phase = phase
          machine.fail_collection = True
          before = machine.calls
          output = io.StringIO()
          try:
              with contextlib.redirect_stdout(output):
                  wait(expected)
              raise RuntimeError('unexpected terminal accepted')
          except AssertionError as error:
              assert str(error) == 'provider reached an unexpected terminal phase'
          assert machine.calls == before + 1  # Terminal shell return was successful.
          assert 'SECRET_CANARY' not in output.getvalue()
      assert len(machine.collection) == 9
      assert machine.collection_timeouts == [10] * 9
      assert sum(machine.collection_timeouts[:3]) == 30
      journal = machine.collection[0]
      assert '--unit=workspace-codex-provider@fixture.service' in journal
      assert '--unit=workspace-codex@fixture.service' in journal
      assert 'tail -c 65536' in journal
      # Nonterminal phases keep the existing retry path (simulated first attempt).
      for phase in ['waiting', 'applying', 'recovery-required']:
          machine.phase = phase
          try:
              with contextlib.redirect_stdout(io.StringIO()):
                  wait('applied')
              raise RuntimeError('intermediate incorrectly accepted')
          except AssertionError as error:
              assert str(error) != 'provider reached an unexpected terminal phase'
      assert source.count('with subtest(') == 6
      assert source.count('machine.reboot()') == 2
      assert "wait_phase(\"recovery-required\")" in source
      assert source.count('wait_phase("failed")') == 2
      metadata = textwrap.dedent(source.split('metadata = pkgs.writeText "recovery-fixture-metadata.py" \'\'\n', 1)[1].split("  '';\nin", 1)[0])
      compile(metadata, 'fixture-metadata', 'exec')
      assert 'os.O_NOFOLLOW' in metadata and 'os.O_NONBLOCK' in metadata
      assert 'info.st_uid != 1000' in metadata and 'op.get("snapshot") == BASE + snapshot_name' in metadata
      assert 'threadCount' in metadata and 'terminalCount' in metadata
      print('VM wait expected/unexpected/intermediate phases and bounded collection: PASS')
    PY
    stdout, stderr, status = Open3.capture3('python3', '-c', script, fixture)
    assert status.success?, stderr
    assert_includes stdout, 'PASS'
  end
end
