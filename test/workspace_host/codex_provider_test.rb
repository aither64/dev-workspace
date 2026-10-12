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

class WorkspaceHostTest
  def with_provider_host
    Dir.mktmpdir('provider-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(name: 'example', root:, hostname: 'workspace.example.test', aliases: [], replace: false)
      host = CodexProviderTestHost.new(env: host_environment(directory, config:), out: StringIO.new, err: StringIO.new)
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

  def test_provider_cancel_loses_after_applying_is_durable
    with_provider_host do |host, entry|
      identity = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      host.send(:provider_command, ['request', 'example', 'pool', identity])
      host.send(:provider_locked, entry) do
        state = host.send(:provider_state, entry)
        state['operation']['phase'] = 'applying'
        host.send(:provider_write_state, entry, state)
      end
      assert_raises(DevWorkspaceHost::Error) { host.send(:provider_command, ['cancel', 'example', identity]) }
      assert_equal 'applying', host.send(:provider_state, entry).dig('operation', 'phase')
    end
  end

  def test_waiting_provider_request_does_not_take_exclusive_transition_lock
    with_provider_host do |host, entry|
      identity = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
      host.send(:provider_command, ['request', 'example', 'pool', identity])
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
      host.send(:provider_command, ['request', 'example', 'pool', identity])
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
      host.send(:provider_command, ['request', 'example', 'openai', first])
      observed = false
      host.idle_blockers = ['Still busy.']
      host.on_pause = lambda do
        unless observed
          observed = true
          host.send(:provider_command, ['request', 'example', 'pool', second])
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
      host.send(:provider_command, ['request', 'example', 'pool', id])
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
      host.send(:provider_command, ['request', 'example', 'pool', id])
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
end
