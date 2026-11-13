# frozen_string_literal: true

require 'fileutils'
require 'minitest/autorun'
require 'tmpdir'
load File.expand_path('../libexec/workspace-pi-host', __dir__)

class PiRuntimeHostTest < Minitest::Test
  INSTANCE = '11111111-1111-4111-8111-111111111111'
  REQUEST = '22222222-2222-4222-8222-222222222222'
  SUCCESSOR = '33333333-3333-4333-8333-333333333333'
  BOOT = '44444444-4444-4444-8444-444444444444'
  INVOCATION = 'a' * 32

  class FakeManager
    attr_accessor :observation, :empty, :fail_start, :fail_observe, :before_start
    attr_reader :calls

    def initialize
      @calls = []
      @empty = true
      @observation = { 'boot_id' => BOOT, 'load_state' => 'loaded',
                       'active_state' => 'inactive', 'sub_state' => 'dead', 'job' => '' }
    end

    def observe(unit)
      raise WorkspacePiHost::Error, 'manager_unavailable' if fail_observe
      calls << ['observe', unit]
      observation.dup
    end

    def start(unit)
      before_start&.call
      calls << ['start', unit]
      raise WorkspacePiHost::Error, 'start_reply_lost' if fail_start
      @empty = false
      observation.merge!('active_state' => 'active', 'sub_state' => 'running',
                         'invocation_id' => INVOCATION, 'main_pid' => 91,
                         'main_pid_start' => '100',
                         'control_group' => "/system.slice/#{unit}")
    end

    def stop(unit)
      calls << ['stop', unit]
      observation['active_state'] = 'inactive'
      observation['sub_state'] = 'dead'
      @empty = true
    end

    def empty?(_cgroup)
      raise WorkspacePiHost::Error, 'cgroup_observation_unavailable' if empty == :unknown
      empty
    end
  end

  def setup
    @temporary = Dir.mktmpdir('pi-host-test-')
    @root = File.join(@temporary, 'state')
    @directory = File.join(@root, 'instances', INSTANCE)
    %w[tmp var-tmp receipts].each { |name| FileUtils.mkdir_p(File.join(@directory, name)) }
    File.write(File.join(@directory, 'launch.lock'), '')
    @store = File.join(@temporary, 'store')
    @package = File.join(@store, "#{'0' * 32}-application")
    FileUtils.mkdir_p(File.join(@package, 'share/dev-workspace'))
    FileUtils.mkdir_p(File.join(@package, 'bin'))
    File.write(File.join(@package, 'share/dev-workspace/pi-runtime.json'), JSON.generate(WorkspacePiHost::CAPABILITY))
    File.write(File.join(@package, 'bin/workspace-portal'), "#!/bin/sh\nexit 0\n")
    File.chmod(0o555, File.join(@package, 'bin/workspace-portal'))
    File.symlink(@package, File.join(@directory, 'package-root'))
    @request = {
      'schema' => 1, 'host_launcher_contract' => 1,
      'scope' => { 'workspace' => @temporary, 'slug' => '2026-11-12-fixture',
                   'session_incarnation' => 'b' * 64, 'backend' => 'pi',
                   'backend_contract' => 1, 'binding_revision' => 1 },
      'instance_id' => INSTANCE, 'request_id' => REQUEST, 'generation' => 'c' * 64,
      'app_store_path' => @package, 'profile_path' => '/home/example/profile',
      'profile_generation' => 'profile-1'
    }
    @layout = WorkspacePiHost::Layout.new(root: @root, instance: INSTANCE,
                                         uid: Process.uid, store_prefix: @store)
    @manager = FakeManager.new
    @controller = WorkspacePiHost::Controller.new(layout: @layout, manager: @manager, receipt_uid: Process.uid)
    publish
  end

  def teardown
    FileUtils.remove_entry(@temporary)
  end

  def publish
    @bytes = JSON.pretty_generate(@request) + "\n"
    File.binwrite(File.join(@directory, 'launch.json'), @bytes)
    @digest = Digest::SHA256.hexdigest(@bytes)
  end

  def call(verb = 'start', **options)
    @controller.call(verb: verb, instance: INSTANCE, request: @request['request_id'], digest: @digest, **options)
  end

  def saved
    JSON.parse(File.read(File.join(@directory, 'receipts/host.json')))
  end

  def test_intent_precedes_start_and_retries_never_repeat_start
    @manager.before_start = -> { assert_equal 'starting', saved['requests'][REQUEST]['phase'] }
    assert_equal 'running', call['outcome']
    2.times { assert_equal 'running', call['outcome'] }
    assert_equal 1, @manager.calls.count { |item| item.first == 'start' }
    assert_equal 0o600, File.stat(File.join(@directory, 'receipts/host.json')).mode & 0o777
  end

  def test_lost_start_response_and_pre_call_crash_stay_unknown_without_relaunch
    @manager.fail_start = true
    assert_equal 'unknown', call['outcome']
    @manager.fail_start = false
    assert_equal 'unknown', call['outcome']
    assert_equal 1, @manager.calls.count { |item| item.first == 'start' }
    # The conservative crash window is identical to an unwritten manager call:
    # durable intent is present but no native invocation has ever been observed.
    record = saved
    record['requests'][REQUEST]['phase'] = 'starting'
    @controller.persist(record)
    assert_equal 'unknown', call['outcome']
    assert_equal 1, @manager.calls.count { |item| item.first == 'start' }
  end

  def test_running_inspect_is_read_only_and_manager_failure_is_unknown
    call
    before = File.binread(File.join(@directory, 'receipts/host.json'))
    @manager.fail_observe = true
    assert_equal 'unknown', call('inspect')['outcome']
    assert_equal before, File.binread(File.join(@directory, 'receipts/host.json'))
  end

  def test_overlapping_inspects_share_read_lock_without_writes
    assert_equal 'running', call['outcome']
    descriptor = File.binread(@layout.path('launch.json'))
    receipt = File.binread(@layout.path('receipts/host.json'))
    before = @manager.calls.dup
    second_manager = FakeManager.new
    second_manager.observation = @manager.observation.dup
    second_manager.empty = @manager.empty
    second_layout = WorkspacePiHost::Layout.new(root: @root, instance: INSTANCE,
                                               uid: Process.uid, store_prefix: @store)
    second = WorkspacePiHost::Controller.new(layout: second_layout, manager: second_manager,
                                             receipt_uid: Process.uid)
    digest = @digest
    observe = @manager.method(:observe)
    inner = nil
    entered = false
    @manager.define_singleton_method(:observe) do |unit|
      unless entered
        entered = true
        inner = second.call(verb: 'inspect', instance: INSTANCE, request: REQUEST, digest: digest)
      end
      observe.call(unit)
    end
    outer = call('inspect')
    assert_equal 'running', outer['outcome']
    assert_equal descriptor, File.binread(@layout.path('launch.json'))
    assert_equal receipt, File.binread(@layout.path('receipts/host.json'))
    assert_equal before.reject { |item| item.first == 'observe' },
                 @manager.calls.reject { |item| item.first == 'observe' }
    assert_empty second_manager.calls.reject { |item| item.first == 'observe' }
    puts "OVERLAPPING_INSPECTS #{JSON.generate('outer' => outer, 'inner' => inner,
                                             'first_calls_before' => before, 'first_calls_after' => @manager.calls,
                                             'second_calls' => second_manager.calls, 'files_unchanged' => true)}"
    assert_equal 'running', inner['outcome']
    [outer, inner].each do |response|
      assert_equal [INSTANCE, REQUEST, @digest], response.values_at('instance_id', 'request_id', 'request_sha256')
    end
    assert_equal 1, @manager.calls.length - before.length
    assert_equal ['observe'], second_manager.calls.map(&:first)
    File.open(@layout.path('launch.lock'), File::RDWR) do |file|
      assert file.flock(File::LOCK_EX | File::LOCK_NB), 'inspect did not release its lock'
    end
  end

  def test_inspect_shared_lock_excludes_writers
    assert_equal 'running', call['outcome']
    descriptor = File.binread(@layout.path('launch.json'))
    receipt = File.binread(@layout.path('receipts/host.json'))
    before = @manager.calls.dup
    other_manager = FakeManager.new
    other_layout = WorkspacePiHost::Layout.new(root: @root, instance: INSTANCE,
                                              uid: Process.uid, store_prefix: @store)
    other = WorkspacePiHost::Controller.new(layout: other_layout, manager: other_manager,
                                            receipt_uid: Process.uid)
    digest = @digest
    observe = @manager.method(:observe)
    refusals = []
    publication = nil
    entered = false
    @manager.define_singleton_method(:observe) do |unit|
      unless entered
        entered = true
        refusals << other.call(verb: 'start', instance: INSTANCE, request: REQUEST, digest: digest)
        refusals << other.call(verb: 'stop', instance: INSTANCE, request: REQUEST, digest: digest,
                               boot_id: BOOT, invocation_id: INVOCATION)
        File.open(other_layout.path('launch.lock'), File::RDWR) do |file|
          publication = file.flock(File::LOCK_EX | File::LOCK_NB)
        end
      end
      observe.call(unit)
    end
    assert_equal 'running', call('inspect')['outcome']
    refusals.each do |response|
      assert_equal ['rejected', 'instance_busy', INSTANCE, REQUEST],
                   response.values_at('outcome', 'error_code', 'instance_id', 'request_id')
    end
    assert_equal false, publication
    assert_empty other_manager.calls
    assert_equal before + [['observe', "workspace-pi-session@#{INSTANCE}.service"]], @manager.calls
    assert_equal descriptor, File.binread(@layout.path('launch.json'))
    assert_equal receipt, File.binread(@layout.path('receipts/host.json'))
    File.open(@layout.path('launch.lock'), File::RDWR) do |file|
      assert file.flock(File::LOCK_EX | File::LOCK_NB), 'inspect did not release its lock'
    end
    puts "SHARED_INSPECT_EXCLUDES_WRITERS #{JSON.generate('refusals' => refusals, 'publication' => publication, 'other_calls' => other_manager.calls)}"
  end

  def test_exclusive_launch_lock_refuses_all_host_verbs
    descriptor = File.binread(@layout.path('launch.json'))
    refute File.exist?(@layout.path('receipts/host.json'))
    responses = []
    File.open(@layout.path('launch.lock'), File::RDWR) do |file|
      assert file.flock(File::LOCK_EX | File::LOCK_NB)
      responses << call('inspect')
      responses << call('start')
      responses << call('stop', boot_id: BOOT, invocation_id: INVOCATION)
    end
    responses.each do |response|
      assert_equal ['rejected', 'instance_busy', INSTANCE, REQUEST],
                   response.values_at('outcome', 'error_code', 'instance_id', 'request_id')
    end
    assert_empty @manager.calls
    assert_equal descriptor, File.binread(@layout.path('launch.json'))
    refute File.exist?(@layout.path('receipts/host.json'))
    File.open(@layout.path('launch.lock'), File::RDWR) do |file|
      assert file.flock(File::LOCK_EX | File::LOCK_NB), 'exclusive owner did not release its lock'
    end
    puts "EXCLUSIVE_LOCK_REFUSALS #{JSON.generate(responses)}"
  end

  def test_inspect_rejection_envelopes_are_read_only
    # This literal is also used by the Go post-Output diagnostic control.
    busy = '{"schema":1,"outcome":"rejected","error_code":"instance_busy","instance_id":"11111111-1111-4111-8111-111111111111","request_id":"22222222-2222-4222-8222-222222222222"}'
    descriptor = File.binread(@layout.path('launch.json'))
    receipt = @layout.path('receipts/host.json')
    refute File.exist?(receipt)
    File.open(@layout.path('launch.lock'), File::RDWR) do |file|
      assert file.flock(File::LOCK_EX | File::LOCK_NB)
      assert_equal busy, JSON.generate(call('inspect'))
      assert_empty @manager.calls
      assert_equal descriptor, File.binread(@layout.path('launch.json'))
      refute File.exist?(receipt)
    end
    assert_equal busy.sub('instance_busy', 'unknown_request'), JSON.generate(call('inspect'))
    refute File.exist?(receipt)
    invalid = JSON.generate('schema' => 0, 'instance_id' => INSTANCE,
                            'current_request_id' => nil, 'requests' => {})
    File.binwrite(receipt, invalid)
    File.chmod(0o600, receipt)
    assert_equal busy.sub('instance_busy', 'invalid_receipt'), JSON.generate(call('inspect'))
    assert_equal invalid, File.binread(receipt)
    assert_equal descriptor, File.binread(@layout.path('launch.json'))
    assert_empty @manager.calls
  end

  def test_invalid_ids_verbs_fields_and_byte_changes_refuse_without_native_effects
    assert_equal 'rejected', call('restart')['outcome']
    assert_raises(WorkspacePiHost::Error) do
      WorkspacePiHost::Layout.new(root: @root, instance: '../escape', uid: Process.uid)
    end
    @request['schema'] = 1.0
    publish
    assert_equal 'rejected', call['outcome']
    @request['schema'] = 1
    @request['scope']['backend'] = 'codex'
    publish
    assert_equal 'rejected', call['outcome']
    @request['scope']['backend'] = 'pi'
    @request['command'] = ['/bin/sh']
    publish
    assert_equal 'rejected', call['outcome']
    @request.delete('command')
    publish
    assert_equal 'running', call['outcome']
    File.write(File.join(@directory, 'launch.json'), @bytes + "\n")
    assert_equal 'request_digest_mismatch', call['error_code']
    assert_equal 1, @manager.calls.count { |item| item.first == 'start' }
  end

  def test_duplicate_json_null_trailing_data_and_unsupported_capability
    [@bytes.sub('"schema": 1', '"schema": 1, "schema": 1'),
     @bytes + '{}', @bytes.sub('"schema": 1', '"schema": null')].each do |bytes|
      File.write(File.join(@directory, 'launch.json'), bytes)
      result = @controller.call(verb: 'start', instance: INSTANCE, request: REQUEST,
                                digest: Digest::SHA256.hexdigest(bytes))
      assert_equal 'rejected', result['outcome']
    end
    publish
    File.write(File.join(@package, 'share/dev-workspace/pi-runtime.json'), '{}')
    assert_equal 'unsupported_application', call['error_code']
    assert_empty @manager.calls
    refute File.exist?(File.join(@directory, 'receipts/host.json'))
  end

  def test_stop_requires_live_boot_and_invocation_and_positive_drain
    call
    assert_equal 'stop_fence_mismatch', call('stop', boot_id: BOOT, invocation_id: 'd' * 32)['error_code']
    assert_equal 0, @manager.calls.count { |item| item.first == 'stop' }
    assert_equal 'stopped', call('stop', boot_id: BOOT, invocation_id: INVOCATION)['outcome']
    assert saved['requests'][REQUEST]['drain_proof']['empty']
    @manager.fail_observe = true
    assert_equal 'stopped', call['outcome']
    assert_equal 1, @manager.calls.count { |item| item.first == 'start' }
  end

  def test_empty_unknown_and_populated_cgroup_are_not_interchangeable
    call
    @manager.observation['active_state'] = 'inactive'
    @manager.observation['sub_state'] = 'dead'
    @manager.empty = :unknown
    assert_equal 'unknown', call['outcome']
    @manager.empty = false
    assert_equal 'unknown', call['outcome']
    @manager.empty = true
    assert_equal 'stopped', call['outcome']
    assert_equal 'exited', saved['requests'][REQUEST]['result']
  end

  def test_terminal_replay_after_successor_does_not_inspect_or_stop_successor
    call
    call('stop', boot_id: BOOT, invocation_id: INVOCATION)
    old_digest = @digest
    @request['request_id'] = SUCCESSOR
    @request['generation'] = 'e' * 64
    @request['replaces_request_id'] = REQUEST
    publish
    assert_equal 'running', call['outcome']
    @manager.fail_observe = true
    result = @controller.call(verb: 'stop', instance: INSTANCE, request: REQUEST,
                              digest: old_digest, boot_id: BOOT, invocation_id: INVOCATION)
    assert_equal 'stopped', result['outcome']
    assert_equal 2, @manager.calls.count { |item| item.first == 'start' }
    assert_equal 1, @manager.calls.count { |item| item.first == 'stop' }
  end

  def test_successor_refuses_live_old_same_generation_scope_change_and_skipped_predecessor
    call
    old_digest = @digest
    @request['request_id'] = SUCCESSOR
    @request['replaces_request_id'] = REQUEST
    publish
    assert_equal 'predecessor_not_drained', call['error_code']
    # Restore the frozen predecessor before explicit stop.
    @request['request_id'] = REQUEST
    @request.delete('replaces_request_id')
    publish
    assert_equal old_digest, @digest
    call('stop', boot_id: BOOT, invocation_id: INVOCATION)
    @request['request_id'] = SUCCESSOR
    @request['replaces_request_id'] = REQUEST
    publish
    assert_equal 'predecessor_not_drained', call['error_code']
    @request['generation'] = 'e' * 64
    @request['scope']['session_incarnation'] = 'f' * 64
    publish
    assert_equal 'predecessor_not_drained', call['error_code']
    @request['scope']['session_incarnation'] = 'b' * 64
    @request['replaces_request_id'] = '55555555-5555-4555-8555-555555555555'
    publish
    assert_equal 'predecessor_not_drained', call['error_code']
  end

  def test_concurrent_publication_lock_refuses
    File.open(@layout.path('launch.lock'), File::RDWR) do |file|
      file.flock(File::LOCK_EX)
      assert_equal 'instance_busy', call['error_code']
    end
    assert_empty @manager.calls
  end

  def test_missing_host_unit_and_capacity_refuse_before_native_start
    @manager.observation['load_state'] = 'not-found'
    assert_equal 'instance_not_idle', call['error_code']
    @manager.observation['load_state'] = 'loaded'
    def @controller.persist(_record)
      raise WorkspacePiHost::Error, 'receipt_capacity'
    end
    assert_equal 'receipt_capacity', call['error_code']
    assert_equal 0, @manager.calls.count { |item| item.first == 'start' }
  end

  def test_failed_post_start_receipt_write_returns_unknown_with_retained_intent
    count = 0
    controller = @controller
    original = controller.method(:persist)
    controller.define_singleton_method(:persist) do |record|
      count += 1
      raise Errno::EIO if count == 2
      original.call(record)
    end
    assert_equal 'unknown', call['outcome']
    assert_equal 'starting', saved['requests'][REQUEST]['phase']
    assert_equal 'running', call['outcome']
    assert_equal 1, @manager.calls.count { |item| item.first == 'start' }
  end

  def test_missing_unit_metadata_is_not_drain_proof
    call
    @manager.observation['load_state'] = 'not-found'
    @manager.observation['active_state'] = 'inactive'
    @manager.empty = true
    assert_equal 'unknown', call['outcome']
  end

  def test_entry_executes_only_fixed_capability_argv_after_uid_drop
    skip 'entry deliberately refuses root' if Process.uid.zero?
    argv = nil
    config = { 'owner_uid' => Process.uid, 'state_root' => @root }
    WorkspacePiHost::Entry.run(config, INSTANCE, layout: @layout, execer: ->(args) { argv = args })
    assert_equal [File.join(@package, 'bin/workspace-portal'), 'pi-runtime', 'supervise',
                  '--launch-request', @layout.path('launch.json'), '--instance', INSTANCE,
                  '--request', REQUEST, '--digest', @digest], argv
    assert_raises(WorkspacePiHost::Error) do
      WorkspacePiHost::Entry.run(config.merge('owner_uid' => Process.uid + 1), INSTANCE,
                                 layout: @layout, execer: ->(_args) { flunk 'wrong UID executed app' })
    end
  end

  def test_standalone_entry_loads_immutable_host_code_before_configuration_refusal
    entry = File.expand_path('../libexec/workspace-pi-session-entry', __dir__)
    _output, error, status = Open3.capture3(RbConfig.ruby, entry, INSTANCE)
    refute status.success?
    assert_includes error, 'Pi session bootstrap refused:'
    refute_includes error, 'LoadError'
  end

  def test_manager_argv_is_fixed_and_cgroup_populated_includes_descendants
    executable = File.join(@temporary, 'systemctl')
    log = File.join(@temporary, 'argv.json')
    File.write(executable, "#!#{RbConfig.ruby}\nrequire 'json'\nFile.write(#{log.inspect}, JSON.generate(ARGV))\n")
    File.chmod(0o755, executable)
    manager = WorkspacePiHost::Manager.new(systemctl: executable, cgroup_root: @temporary)
    manager.start('workspace-pi-session@fixture.service')
    assert_equal ['--system', '--no-pager', '--no-block', 'start', '--',
                  'workspace-pi-session@fixture.service'], JSON.parse(File.read(log))
    directory = File.join(@temporary, 'system.slice/fixture')
    FileUtils.mkdir_p(directory)
    File.write(File.join(directory, 'cgroup.events'), "populated 1\nfrozen 0\n")
    refute manager.empty?('/system.slice/fixture')
    File.write(File.join(directory, 'cgroup.events'), "populated 0\nfrozen 0\n")
    assert manager.empty?('/system.slice/fixture')
    File.unlink(File.join(directory, 'cgroup.events'))
    assert_raises(WorkspacePiHost::Error) { manager.empty?('/system.slice/fixture') }
    FileUtils.rmdir(directory)
    assert manager.empty?('/system.slice/fixture')
  end

  def test_cross_language_byte_fixture_hash
    path = File.join(__dir__, 'fixtures/pi-host-launch-v1.json')
    expected = File.read(path + '.sha256').strip
    assert_equal expected, Digest::SHA256.file(path).hexdigest
    refute_equal expected, Digest::SHA256.hexdigest(JSON.generate(JSON.parse(File.read(path))))
  end
end
