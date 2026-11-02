# frozen_string_literal: true

require_relative '../support/dev_session_test_case'
require_relative 'lifecycle_commands_test'
require_relative 'ordinary_readiness_test'

class DevSessionTest < Minitest::Test
  # Models only explicitly launched units, with cgroup.events in disposable state.
  # Native systemd/cgroup behavior is covered by session-archive-lifecycle.
  class ServiceManagerFixture
    Status = Struct.new(:ok) do
      def success? = ok
    end
    attr_reader :calls, :units
    attr_accessor :lost_reply, :unavailable, :residual, :before_launch, :stop_observer, :fast_exit

    def initialize(root)
      @root, @calls, @units = root, [], {}
      FileUtils.mkdir_p(root)
      File.write(File.join(root, 'cgroup.controllers'), 'cpu memory')
    end

    def with_timeout(_seconds)
      yield
    end

    def capture(argv, allow_failure: false, env: nil)
      argv = argv.drop(3) if argv.first == 'timeout'
      @calls << argv
      raise 'fixture manager unavailable' if unavailable
      case argv.first
      when 'systemd-run'
        before_launch&.call
        name = argv.find { |arg| arg.start_with?('--unit=') }.delete_prefix('--unit=')
        description = argv.find { |arg| arg.start_with?('--description=') }.delete_prefix('--description=')
        group = "/fixture/app.slice/#{name}"
        FileUtils.mkdir_p(File.join(@root, group))
        File.write(File.join(@root, group, 'cgroup.events'), "populated 1\n")
        @units[name] = { 'Id' => name, 'LoadState' => 'loaded', 'ActiveState' => 'active', 'SubState' => 'running',
          'Description' => description, 'InvocationID' => SecureRandom.hex(16), 'ControlGroup' => group, 'Job' => '',
          'Transient' => 'yes', 'Type' => 'exec', 'Restart' => 'no', 'RemainAfterExit' => 'yes',
          'Slice' => 'app.slice',
          'KillMode' => 'control-group', 'SendSIGKILL' => 'yes', 'TimeoutStopUSec' => '15s', 'Triggers' => '', 'TriggeredBy' => '' }
        if fast_exit
          @units[name]['ControlGroup'] = ''
          @units[name]['SubState'] = 'exited'
          if fast_exit == :failed
            @units[name]['ActiveState'] = 'failed'
            @units[name]['SubState'] = 'failed'
          end
          FileUtils.rm_r(File.join(@root, group))
        end
        raise 'fixture launch response lost' if lost_reply
      when 'systemctl'
        verb = argv[2]
        unit = verb == 'show' ? argv[3] : argv.last
        case verb
        when 'show'
          return ["/fixture\n", '', Status.new(true)] if argv[3] == '--property=ControlGroup'
          values = @units[unit] || { 'LoadState' => 'not-found', 'Job' => '' }
          return [values.map { |key, value| "#{key}=#{value}\n" }.join, '', Status.new(true)]
        when 'stop'
          stop_observer&.call(unit)
          values = @units.fetch(unit)
          values['ActiveState'] = 'inactive'
          values['SubState'] = 'dead'
          group = values.fetch('ControlGroup')
          File.write(File.join(@root, group, 'cgroup.events'), "populated #{residual ? 1 : 0}\n") unless group.empty?
        when 'reset-failed'
          @units.delete(unit)
        else
          raise "unexpected fixture verb: #{verb}"
        end
      else
        raise "unexpected fixture command: #{argv.first}"
      end
      ['', '', Status.new(true)]
    end
  end

  def with_services_fixture(archive: false)
    with_workspace do |workspace|
      slug = '2026-06-06-managed-services'
      runner = archive ? archive_runner_for(workspace) : runner_for(workspace)
      runner.ensure_tracking_files(slug)
      if archive
        commit_archive_fixture(workspace, slug, lifecycle: 'active')
        runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
        configure_workspace_origin(workspace)
      else
        runner.send(:ensure_portal_manifest, slug)
      end
      root = File.join(workspace, '.test-cgroups')
      manager = ServiceManagerFixture.new(root)
      runner.define_singleton_method(:service_command_runner) { manager }
      original = runner.method(:session_services)
      elapsed = 0.0
      runner.define_singleton_method(:session_services) do |selected|
        store = original.call(selected)
        store.define_singleton_method(:cgroup_root) { root }
        # Advance fixture deadlines only during simulated polling. Filesystem
        # fsync and CI scheduling must not consume a 150 ms test deadline.
        store.define_singleton_method(:monotonic) { elapsed }
        store.define_singleton_method(:sleep) { |seconds| elapsed += seconds }
        store.define_singleton_method(:bounded) do |&block|
          previous = @deadline
          @deadline ||= monotonic + 0.15
          block.call
        ensure
          @deadline = previous
        end
        store
      end
      yield runner, manager, workspace, slug
    end
  end

  def launch_service(runner, workspace, slug, id: SecureRandom.uuid, label: 'preview', argv: ['/bin/server', '--token=private-fixture'])
    runner.service_action(slug, as_is: true, action: 'run', id:, label:, cwd: File.join(workspace, 'work', slug), argv:)
  end

  def services_files(runner, slug)
    runner.session_services(slug).files
  end

  def test_session_services_service_registration_is_durable_before_native_launch_and_projects_only_allowlisted_fields
    with_services_fixture do |runner, manager, workspace, slug|
      manager.before_launch = lambda do
        files = services_files(runner, slug)
        marker = JSON.parse(File.read(files[:marker]))
        registry = JSON.parse(File.read(files[:registry]))
        assert_equal 'ready', marker['state']
        assert_equal 'starting', registry.fetch('resources').first['phase']
        assert_equal marker['registry_id'], registry['registry_id']
        assert_equal 0o600, File.stat(files[:registry]).mode & 0o777
      end
      result = launch_service(runner, workspace, slug)
      assert_equal 1, result['count']
      assert_equal true, result['countKnown']
      assert_equal 'running', result['services'].first['state']
      assert_equal %w[error id label observedAt phase retiredAt state], result['services'].first.keys.sort
      refute_includes JSON.generate(result), 'private-fixture'
      refute_includes JSON.generate(result), 'ControlGroup'
      assert_equal({ required: true, pending: [] }, WorkspaceSessionServices.requirements(workspace:, state_root: runner.instance_variable_get(:@workspace_state_root)))
    end
  end

  def test_session_services_service_request_id_reconciles_lost_reply_without_duplicate_launch
    with_services_fixture do |runner, manager, workspace, slug|
      id = SecureRandom.uuid
      manager.lost_reply = true
      assert_raises(DevSession::Error) { launch_service(runner, workspace, slug, id:) }
      manager.lost_reply = false
      result = launch_service(runner, workspace, slug, id:)
      assert_equal 'running', result['services'].first['phase']
      assert_equal 1, manager.calls.count { |argv| argv.first == 'systemd-run' }
      assert_raises(DevSession::Error) { launch_service(runner, workspace, slug, id:, label: 'changed') }
      assert_equal 1, manager.calls.count { |argv| argv.first == 'systemd-run' }
    end
  end

  def test_session_services_services_list_never_initializes_state_or_contacts_manager_for_untouched_session
    with_services_fixture do |runner, manager, workspace, slug|
      before = Dir.glob(File.join(workspace, '**', '*'), File::FNM_DOTMATCH).sort
      result = runner.service_action(slug, as_is: true, action: 'list')
      assert_equal false, result['registered']
      assert_equal 0, result['count']
      assert_empty manager.calls
      assert_equal before, Dir.glob(File.join(workspace, '**', '*'), File::FNM_DOTMATCH).sort
    end
  end

  def test_session_services_services_reader_keeps_known_membership_when_manager_unavailable_and_never_writes
    with_services_fixture do |runner, manager, workspace, slug|
      launch_service(runner, workspace, slug)
      files = services_files(runner, slug)
      before = files.values.select { |path| File.file?(path) }.to_h { |path| [path, [File.binread(path), File.stat(path).mtime]] }
      manager.unavailable = true
      result = runner.service_action(slug, as_is: true, action: 'list')
      assert_equal 1, result['count']
      assert_equal true, result['countKnown']
      assert_equal 'unknown', result['services'].first['state']
      assert_equal before, before.keys.to_h { |path| [path, [File.binread(path), File.stat(path).mtime]] }
    end
  end

  def test_session_services_ready_registration_loss_or_wrong_session_identity_fails_closed_without_discovery
    with_services_fixture do |runner, manager, workspace, slug|
      launch_service(runner, workspace, slug)
      files = services_files(runner, slug)
      original = File.binread(files[:registry])
      File.unlink(files[:registry])
      calls = manager.calls.length
      assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'list') }
      assert_raises(WorkspaceSessionServices::Error) { WorkspaceSessionServices.requirements(workspace:, state_root: runner.instance_variable_get(:@workspace_state_root)) }
      assert_equal calls, manager.calls.length
      File.write(files[:registry], original)
      File.chmod(0o600, files[:registry])
      marker = JSON.parse(File.read(files[:marker]))
      marker['tracking_identity']['ino'] += 1
      File.write(files[:marker], JSON.generate(marker))
      assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'list') }
    end
  end

  def test_session_services_initializing_registration_can_recover_without_losing_ready_missing_state_guarantee
    with_services_fixture do |runner, manager, workspace, slug|
      store = runner.session_services(slug)
      store.send(:initialize_registry!)
      refute File.exist?(store.files[:registry])
      id = SecureRandom.uuid
      launch_service(runner, workspace, slug, id:)
      assert_equal 1, manager.calls.count { |argv| argv.first == 'systemd-run' }
      assert_equal 'ready', JSON.parse(File.read(store.files[:marker]))['state']
    end
  end

  def test_session_services_stop_requires_exact_invocation_and_empty_descendant_cgroup_before_retirement
    with_services_fixture do |runner, manager, workspace, slug|
      result = launch_service(runner, workspace, slug)
      id = result['services'].first['id']
      unit = manager.units.keys.first
      manager.residual = true
      assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'stop', id:) }
      assert_equal 1, runner.service_action(slug, as_is: true, action: 'list')['count']
      original = manager.units[unit]['InvocationID']
      stops = manager.calls.count { |argv| argv[2] == 'stop' }
      [SecureRandom.hex(16), '', '0' * 32].each do |invalid|
        manager.units[unit]['InvocationID'] = invalid
        assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'stop', id:) }
        assert_equal stops, manager.calls.count { |argv| argv[2] == 'stop' }
      end
      manager.units[unit]['InvocationID'] = original
      manager.residual = false
      retired = runner.service_action(slug, as_is: true, action: 'stop', id:)
      assert_equal 0, retired['count']
      assert_equal 'retired', retired['history'].first['phase']
      manager.unavailable = true
      history = runner.service_action(slug, as_is: true, action: 'list')['history'].first
      assert_equal 'stopped', history['state']
      stored = JSON.parse(File.read(services_files(runner, slug)[:registry])).fetch('resources').first
      assert_equal stored['stopped_at'], history['observedAt']
      assert_equal true, WorkspaceSessionServices.requirements(workspace:, state_root: runner.instance_variable_get(:@workspace_state_root))[:required]
    end
  end

  def test_session_services_archive_cleanup_precedes_tracking_prune_and_retains_exact_prelude_on_failure
    with_services_fixture(archive: true) do |runner, manager, workspace, slug|
      id = SecureRandom.uuid
      launch_service(runner, workspace, slug, id:)
      asset = File.join(workspace, 'work', slug, 'launch-script.sh')
      File.write(asset, 'service dependency')
      manager.stop_observer = lambda do |_unit|
        assert File.file?(asset), 'tracking pruned before native stop'
        prelude = JSON.parse(File.read(services_files(runner, slug)[:prelude]))
        assert_equal 'archive', prelude['operation']
        assert_equal [id], prelude['resource_ids']
      end
      manager.residual = true
      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true, explicit: true) }
      assert_instance_of WorkspaceSessionServices::Error, error.cause
      files = services_files(runner, slug)
      pending = JSON.parse(File.read(files[:prelude]))
      assert File.file?(asset)
      refute File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive'))
      assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'run', id: SecureRandom.uuid, label: 'late', cwd: workspace, argv: ['/bin/server']) }
      manager.residual = false
      runner.archive(slug, as_is: true, explicit: true)
      refute File.exist?(files[:prelude])
      refute File.exist?(File.join(workspace, 'archive', slug, 'launch-script.sh'))
      manifest = YAML.safe_load(File.read(File.join(workspace, 'archive', slug, 'portal.yml')))
      assert_equal pending['operation_id'], manifest['archive_operation_id']
      assert_empty manager.units
      calls = manager.calls.length
      assert_equal 0, runner.service_action(slug, as_is: true, action: 'list')['count']
      assert_equal calls, manager.calls.length
      state_root = runner.instance_variable_get(:@workspace_state_root)
      assert_equal true, WorkspaceSessionServices.requirements(workspace:, state_root:)[:required]
      runner.revive(slug, as_is: true)
      assert_equal true, WorkspaceSessionServices.requirements(workspace:, state_root:)[:required]
      assert_equal 0, runner.service_action(slug, as_is: true, action: 'list')['count']
      assert_equal calls, manager.calls.length
    end
  end

  def test_session_services_cleanup_includes_all_registered_candidates_and_does_not_inspect_other_units
    with_services_fixture do |runner, manager, workspace, slug|
      3.times { |index| launch_service(runner, workspace, slug, label: "candidate #{index}") }
      calls_before = manager.calls.length
      store = runner.session_services(slug)
      operation_id = 'a' * 64
      store.prepare_cleanup!(operation: 'archive', operation_id:, mode: 'archived', force: nil, automatic: false)
      store.cleanup!
      assert_equal 3, manager.calls[calls_before..].count { |args| args[2] == 'stop' }
      assert_empty manager.units
      journal = { 'workspace' => workspace, 'slug' => slug, 'operation_id' => operation_id, 'mode' => 'archived' }
      store.handoff_cleanup!(journal:)
      assert_equal 3, store.public_status['history'].length
      manager.calls.each do |args|
        next unless args.first == 'systemctl'
        unit = args[2] == 'show' ? args[3] : args.last
        next if unit == '--property=ControlGroup'
        assert_match(/\Aworkspace-session-[0-9a-f-]+\.service\z/, unit)
      end
    end
  end

  def test_session_services_service_prelude_enforces_lifecycle_identity_and_existing_delete_force_upgrade
    with_services_fixture do |runner, _manager, workspace, slug|
      launch_service(runner, workspace, slug)
      store = runner.session_services(slug)
      options = { operation: 'delete', operation_id: 'a' * 64, mode: nil, force: false, automatic: false }
      store.prepare_cleanup!(**options)
      assert_raises(WorkspaceSessionServices::Error) { store.prepare_cleanup!(**options.merge(operation_id: 'b' * 64)) }
      store.prepare_cleanup!(**options.merge(force: true))
      assert_equal true, store.prelude['force']
      assert_raises(WorkspaceSessionServices::Error) { store.prepare_cleanup!(**options) }
      assert_raises(DevSession::Error) { runner.send(:reject_conflicting_lifecycle_journal!, slug, operation: 'revive') }
      assert_equal 'a' * 64, runner.send(:lifecycle_access_journal, slug, 'delete')
    end
  end

  def test_session_services_managed_launch_changes_inactivity_token_but_reads_and_stop_do_not
    with_services_fixture do |runner, _manager, workspace, slug|
      result = launch_service(runner, workspace, slug)
      store = runner.session_services(slug)
      before = store.activity_token
      runner.service_action(slug, as_is: true, action: 'list')
      runner.service_action(slug, as_is: true, action: 'stop', id: result['services'].first['id'])
      assert_equal before, store.activity_token
      launch_service(runner, workspace, slug, label: 'new candidate')
      refute_equal before, store.activity_token
    end
  end

  def test_session_services_fast_exit_uses_durable_expected_cgroup_without_inventing_an_observation
    [true, :failed].each do |outcome|
      with_services_fixture do |runner, manager, workspace, slug|
        manager.fast_exit = outcome
        result = launch_service(runner, workspace, slug)
        assert_equal outcome == :failed ? 'failed' : 'exited', result['services'].first['state']
        files = services_files(runner, slug)
        row = JSON.parse(File.read(files[:registry])).fetch('resources').first
        assert_nil row['control_group']
        assert_equal '/fixture', row['manager_control_group']
        assert_equal "/fixture/app.slice/#{row['unit']}", row['expected_control_group']
        retired = runner.service_action(slug, as_is: true, action: 'stop', id: row['id'])
        assert_equal 0, retired['count']
      end
    end
  end

  def test_session_services_cgroup_mismatch_or_unobserved_absent_launch_cannot_retire
    with_services_fixture do |runner, manager, workspace, slug|
      manager.lost_reply = true
      id = SecureRandom.uuid
      assert_raises(DevSession::Error) { launch_service(runner, workspace, slug, id:) }
      manager.lost_reply = false
      unit = manager.units.keys.first
      group = manager.units[unit]['ControlGroup']
      manager.units[unit]['ControlGroup'] = '/unexpected/group'
      assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'stop', id:) }
      refute manager.calls.any? { |args| args[2] == 'stop' }
      manager.units.delete(unit)
      FileUtils.rm_r(File.join(workspace, '.test-cgroups', group))
      assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'stop', id:) }
      assert_equal 1, runner.service_action(slug, as_is: true, action: 'list')['count']
    end
  end

  def test_session_services_delete_transfers_retired_records_and_retries_partial_transfer
    with_services_fixture(archive: true) do |runner, manager, workspace, slug|
      launch_service(runner, workspace, slug)
      manager.residual = true
      error = assert_raises(DevSession::Error) { runner.delete(slug, as_is: true, force: true) }
      assert_instance_of WorkspaceSessionServices::Error, error.cause
      assert File.directory?(File.join(workspace, 'work', slug))
      assert File.file?(services_files(runner, slug)[:prelude])
      manager.residual = false
      files = services_files(runner, slug)
      original_fsync = runner.method(:fsync_directory)
      interrupted = false
      runner.define_singleton_method(:fsync_directory) do |path|
        original_fsync.call(path)
        if !interrupted && path == File.dirname(files[:marker]) &&
           !File.exist?(files[:marker]) && File.exist?(files[:registry])
          interrupted = true
          raise DevSession::Error, 'fixture interruption after first evidence unlink'
        end
      end
      assert_raises(DevSession::Error) { runner.delete(slug, as_is: true, force: true) }
      assert interrupted
      state_root = runner.instance_variable_get(:@workspace_state_root)
      assert_raises(WorkspaceSessionServices::Error) { WorkspaceSessionServices.requirements(workspace:, state_root:) }
      runner.delete(slug, as_is: true, force: true)
      files.each_value { |path| refute File.exist?(path) }
      recovery = Dir[File.join(workspace, '.xdg-state', 'dev-workspaces', 'removed', '*', '*')].first
      assert File.file?(File.join(recovery, 'services-marker.json'))
      assert File.file?(File.join(recovery, 'services-registry.json'))
      assert_empty manager.units
      assert_equal({ required: false, pending: [] }, WorkspaceSessionServices.requirements(workspace:, state_root:))
    end
  end

  def test_session_services_expired_stop_is_a_session_error_and_retry_preserves_recovery
    with_services_fixture(archive: true) do |runner, manager, workspace, slug|
      launch_service(runner, workspace, slug)
      manager.stop_observer = lambda do |_unit|
        runner.session_services(slug).send(:sleep, 0.2)
      end

      error = assert_raises(DevSession::Error) { runner.delete(slug, as_is: true, force: true) }
      assert_instance_of WorkspaceSessionServices::Error, error.cause
      assert_equal 'service observation deadline exceeded', error.cause.cause.message
      assert File.directory?(File.join(workspace, 'work', slug))
      assert File.file?(services_files(runner, slug)[:prelude])
      registry = JSON.parse(File.read(services_files(runner, slug)[:registry]))
      assert_equal 'stopping', registry.fetch('resources').first.fetch('phase')

      manager.stop_observer = nil
      runner.delete(slug, as_is: true, force: true)
      assert_empty manager.units
      services_files(runner, slug).each_value { |path| refute File.exist?(path) }
      recovery = removal_recovery(workspace, slug)
      assert File.file?(File.join(recovery, 'services-marker.json'))
      assert File.file?(File.join(recovery, 'services-registry.json'))
    end
  end

  def test_session_services_invalid_prelude_is_a_session_error_before_lifecycle_mutation
    with_services_fixture do |runner, manager, workspace, slug|
      launch_service(runner, workspace, slug)
      files = services_files(runner, slug)
      File.write(files[:prelude], '{}', mode: 'w', perm: 0o600)
      before = files.to_h { |key, path| [key, File.binread(path)] }
      calls = manager.calls.dup

      %w[archive delete].each do |operation|
        error = assert_raises(DevSession::Error) do
          operation == 'archive' ? runner.archive(slug, as_is: true, explicit: true) : runner.delete(slug, as_is: true, force: true)
        end
        assert_instance_of WorkspaceSessionServices::Error, error.cause
        assert_includes error.message, 'invalid service cleanup prelude'
        assert_equal before, files.to_h { |key, path| [key, File.binread(path)] }
        assert_equal calls, manager.calls
        refute File.exist?(runner.send(:lifecycle_journal_file, slug, operation))
      end
    end
  end

  def test_session_services_initialization_interruption_retries_the_same_intent_before_launch
    with_services_fixture do |runner, manager, workspace, slug|
      store = runner.session_services(slug)
      original = store.method(:write)
      interrupted = false
      store.define_singleton_method(:write) do |path, data|
        if !interrupted && path == files[:marker] && data['state'] == 'ready'
          interrupted = true
          raise WorkspaceSessionServices::Error, 'fixture interrupted before ready marker'
        end
        original.call(path, data)
      end
      id = SecureRandom.uuid
      options = { id:, label: 'preview', cwd: File.join(workspace, 'work', slug), argv: ['/bin/server'] }
      assert_raises(WorkspaceSessionServices::Error) { store.run!(**options) }
      refute manager.calls.any? { |args| args.first == 'systemd-run' }
      assert_equal 'initializing', JSON.parse(File.read(store.files[:marker]))['state']
      result = runner.session_services(slug).run!(**options)
      assert_equal id, result['services'].first['id']
      assert_equal 1, manager.calls.count { |args| args.first == 'systemd-run' }
    end
  end

  def test_session_services_busy_reader_and_oversized_launch_preserve_state
    with_services_fixture do |runner, manager, workspace, slug|
      launch_service(runner, workspace, slug)
      lock_path = runner.send(:session_lock_file, slug)
      File.open(lock_path, File::RDONLY) do |lock|
        lock.flock(File::LOCK_EX)
        assert_raises(DevSession::SessionBusyError) { runner.service_action(slug, as_is: true, action: 'list') }
      end
      files = services_files(runner, slug)
      before = File.binread(files[:registry])
      launches = manager.calls.count { |args| args.first == 'systemd-run' }
      assert_raises(DevSession::Error) { launch_service(runner, workspace, slug, argv: ['/bin/server', 'x' * WorkspaceSessionServices::MAX_BYTES]) }
      assert_equal before, File.binread(files[:registry])
      assert_equal launches, manager.calls.count { |args| args.first == 'systemd-run' }
    end
  end

  def test_session_services_readiness_detects_lost_ready_registry_without_native_effects
    with_services_fixture(archive: true) do |runner, manager, workspace, slug|
      launch_service(runner, workspace, slug)
      File.unlink(services_files(runner, slug)[:registry])
      calls = manager.calls.length
      result = runner.archive_check(slug, as_is: true)
      refute result['ready']
      assert result['blockers'].any? { |row| row['message'].include?('service registry is missing') }
      assert_equal calls, manager.calls.length
      assert File.directory?(File.join(workspace, 'work', slug))
    end
  end

  def test_session_services_automatic_scan_persists_launch_dimension_and_resumes_its_prelude
    with_services_fixture do |runner, manager, workspace, slug|
      portal = archive_fixture_env(workspace)
      runner.instance_variable_get(:@env).merge!(portal)
      runner.instance_variable_set(:@portal_command, Shellwords.split(portal.fetch(DevSession::ENV_PORTAL_COMMAND)))
      runner.instance_variable_set(:@codex_socket, '/run/test/codex.sock')
      runner.instance_variable_set(:@authority_dir, portal.fetch(DevSession::ENV_AUTHORITY_DIR))
      runner.instance_variable_get(:@env)['DEV_WORKSPACE_DEFER_TRACKING'] = '1'
      commit_archive_fixture(workspace, slug, lifecycle: 'active')
      configure_workspace_origin(workspace)
      runner.auto_archive_configure(true)
      predecessor_path = File.expand_path('../fixtures/auto-archive-before-session-services.rb', __dir__)
      predecessor = Module.new
      predecessor.module_eval(File.read(predecessor_path), predecessor_path)
      legacy = predecessor.const_get(:WorkspaceAutoArchive)
      old_store = legacy::Store.new(workspace:, state_root: runner.instance_variable_get(:@workspace_state_root))
      automatic_scan(runner)
      untouched = runner.auto_archive_store.session(slug)
      native_snapshot = runner.send(:auto_archive_snapshot, slug)
      assert_equal native_snapshot['activity_token'], untouched.dig('dimensions', 'activity')
      assert_equal untouched, old_store.session(slug)
      automatic_scan(runner)
      unchanged = runner.auto_archive_store.session(slug)
      assert_equal untouched['fingerprint'], unchanged['fingerprint']
      assert_equal untouched['idle_since'], unchanged['idle_since']
      age_automatic_session(runner, slug, 2 * 86_400)
      aged = runner.auto_archive_store.session(slug)['idle_since']
      launch_service(runner, workspace, slug)
      automatic_scan(runner)
      first = runner.auto_archive_store.session(slug)
      expected = Digest::SHA256.hexdigest(JSON.generate(['session-services-activity-v1', native_snapshot['activity_token'], runner.session_services(slug).activity_token]))
      assert_equal expected, first.dig('dimensions', 'activity')
      refute_equal aged, first['idle_since']
      assert_equal first, old_store.session(slug)
      automatic_scan(runner)
      assert_equal first['dimensions'], runner.auto_archive_store.session(slug)['dimensions']
      service_id = runner.service_action(slug, as_is: true, action: 'list')['services'].first['id']
      runner.service_action(slug, as_is: true, action: 'stop', id: service_id)
      automatic_scan(runner)
      retired = runner.auto_archive_store.session(slug)
      assert_equal first['idle_since'], retired['idle_since']
      assert_equal first['fingerprint'], retired['fingerprint']
      assert_equal retired, old_store.session(slug)
      # The exact predecessor can read the final record and naturally starts
      # fresh grace when its ordinary native token replaces the composite.
      later = Time.now.utc + 10
      old_result = legacy::Policy.observe(retired, native_snapshot, old_store.policy, later)
      assert_equal later.iso8601, old_result['idle_since']
      refute old_result['eligible']
      token_before_native_activity = runner.session_services(slug).activity_token
      age_automatic_session(runner, slug, 2 * 86_400)
      observer = Shellwords.split(portal.fetch(DevSession::ENV_PORTAL_COMMAND)).last
      File.write(observer, File.read(observer).sub('verified-threadless', 'verified-threadless-next'))
      automatic_scan(runner)
      native_changed = runner.auto_archive_store.session(slug)
      refute_equal expected, native_changed.dig('dimensions', 'activity')
      assert_equal token_before_native_activity, runner.session_services(slug).activity_token
      refute_equal first['activity_token'], native_changed['activity_token']
      expected = native_changed.dig('dimensions', 'activity')
      # Unknown registered state preserves the last composite rather than
      # replacing it with the native token while eligibility is blocked.
      registry_path = services_files(runner, slug)[:registry]
      registry_bytes = File.binread(registry_path)
      File.unlink(registry_path)
      automatic_scan(runner)
      unknown = runner.auto_archive_store.session(slug)
      assert_equal expected, unknown.dig('dimensions', 'activity')
      refute unknown['eligible']
      assert_includes unknown['unknown_dimensions'], 'services'
      File.write(registry_path, registry_bytes)
      File.chmod(0o600, registry_path)
      launch_service(runner, workspace, slug, label: 'next candidate')
      automatic_scan(runner)
      refute_equal expected, runner.auto_archive_store.session(slug).dig('dimensions', 'activity')
      age_automatic_session(runner, slug, 15 * 86_400)
      manager.residual = true
      automatic_scan(runner)
      state = runner.auto_archive_store.session(slug)
      assert state['operation'], state.inspect
      prelude = runner.session_services(slug).prelude
      assert_equal true, prelude['automatic']
      assert_equal state.dig('operation', 'id'), prelude['operation_id']
      manager.residual = false
      result = automatic_scan(runner).find { |row| row['slug'] == slug }
      assert_equal 'archived', result['result'], result.inspect
      assert File.directory?(File.join(workspace, 'archive', slug))
    end
  end

  def test_session_services_unsettled_native_job_blocks_retirement_until_retry
    with_services_fixture do |runner, manager, workspace, slug|
      result = launch_service(runner, workspace, slug)
      row = result['services'].first
      unit = manager.units.keys.first
      manager.units[unit]['Job'] = '41'
      assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'stop', id: row['id']) }
      assert manager.units.key?(unit)
      assert_equal 1, runner.service_action(slug, as_is: true, action: 'list')['count']
      manager.units[unit]['Job'] = ''
      assert_equal 0, runner.service_action(slug, as_is: true, action: 'stop', id: row['id'])['count']
    end
  end

  def test_session_services_explicit_zero_job_remains_compatible
    with_services_fixture do |runner, manager, workspace, slug|
      row = launch_service(runner, workspace, slug)['services'].first
      unit = manager.units.keys.fetch(0)
      manager.units[unit]['Job'] = '0'
      assert_equal 'running', runner.service_action(slug, as_is: true, action: 'list')['services'].first['state']
      assert_equal 0, runner.service_action(slug, as_is: true, action: 'stop', id: row['id'])['count']
    end
  end

  def test_session_services_missing_or_malformed_job_keeps_registration_for_retry
    [nil, "Job\n", "Job=null\n", "Job=unavailable\n", "Job= \n", "Job= 0\n", "Job=0 \n", "Job=01\n", "Job=41 pending\n"].each do |job_line|
      with_services_fixture do |runner, manager, workspace, slug|
        row = launch_service(runner, workspace, slug)['services'].first
        unit = manager.units.keys.fetch(0)
        capture = manager.method(:capture)
        manager.define_singleton_method(:capture) do |argv, **options|
          stdout, stderr, status = capture.call(argv, **options)
          if argv.include?('show') && argv.include?(unit)
            stdout = stdout.lines.reject { |line| line.start_with?('Job=') }.join + job_line.to_s
          end
          [stdout, stderr, status]
        end

        error = assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'stop', id: row['id']) }
        assert_instance_of WorkspaceSessionServices::Error, error.cause
        refute_nil error.cause.cause
        refute manager.calls.any? { |argv| argv.include?('stop') }
        retained = JSON.parse(File.read(services_files(runner, slug)[:registry])).fetch('resources').first
        assert_equal row['id'], retained['id']
        assert_equal 'running', retained['phase']
        assert_equal 'active', manager.units.fetch(unit)['ActiveState']

        job_line = "Job=\n"
        assert_equal 0, runner.service_action(slug, as_is: true, action: 'stop', id: row['id'])['count']
      end
    end
  end

  def test_session_services_unloaded_unit_still_requires_known_settled_job
    with_services_fixture do |runner, manager, workspace, slug|
      row = launch_service(runner, workspace, slug)['services'].first
      unit = manager.units.keys.fetch(0)
      group = manager.units[unit]['ControlGroup']
      manager.units[unit]['LoadState'] = 'not-found'
      FileUtils.rm_r(File.join(workspace, '.test-cgroups', group))

      ['41', nil].each do |job|
        if job
          manager.units[unit]['Job'] = job
        else
          manager.units[unit].delete('Job')
        end
        assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'stop', id: row['id']) }
        assert_equal 1, runner.service_action(slug, as_is: true, action: 'list')['count']
        refute manager.calls.any? { |argv| %w[stop reset-failed].include?(argv[2]) }
        retained = JSON.parse(File.read(services_files(runner, slug)[:registry])).fetch('resources').first
        assert_equal 'running', retained['phase']
      end

      manager.units[unit]['Job'] = ''
      assert_equal 0, runner.service_action(slug, as_is: true, action: 'stop', id: row['id'])['count']
    end
  end

  def test_session_services_cli_lists_untouched_session_and_reports_busy_without_json
    with_services_fixture do |runner, _manager, workspace, slug|
      env = { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state') }
      args = ['--workspace', workspace, 'service', 'list', slug, '--as-is', '--json']
      output, errors = StringIO.new, StringIO.new
      assert_equal 0, DevSession::CLI.new(args, out: output, err: errors, env:).run, errors.string
      assert_equal 0, JSON.parse(output.string)['count']
      launch_service(runner, workspace, slug)
      lock_path = runner.send(:session_lock_file, slug)
      File.open(lock_path, File::RDONLY) do |lock|
        lock.flock(File::LOCK_EX)
        output, errors = StringIO.new, StringIO.new
        assert_equal 75, DevSession::CLI.new(args, out: output, err: errors, env:).run
        assert_empty output.string
      end
    end
  end

  def test_session_services_requirements_refuses_unreadable_or_nondirectory_inventory_without_writes
    with_services_fixture do |runner, _manager, workspace, slug|
      root = runner.instance_variable_get(:@workspace_state_root)
      assert_equal({ required: false, pending: [] }, WorkspaceSessionServices.requirements(workspace:, state_root: root))
      inventory = File.dirname(services_files(runner, slug)[:registry])
      FileUtils.mkdir_p(File.dirname(inventory))
      File.write(inventory, 'ordinary configuration mistake')
      error = assert_raises(WorkspaceSessionServices::Error) { WorkspaceSessionServices.exists?(File.join(inventory, 'missing.json')) }
      assert_instance_of Errno::ENOTDIR, error.cause
      error = assert_raises(DevSession::Error) { runner.service_action(slug, as_is: true, action: 'list') }
      assert_instance_of WorkspaceSessionServices::Error, error.cause
      assert_instance_of Errno::ENOTDIR, error.cause.cause
      assert_raises(WorkspaceSessionServices::Error) { WorkspaceSessionServices.requirements(workspace:, state_root: root) }
      assert_equal 'ordinary configuration mistake', File.read(inventory)
      File.unlink(inventory)
      FileUtils.mkdir_p(inventory, mode: 0o700)
      File.chmod(0o000, inventory)
      unless File.readable?(inventory)
        assert_raises(WorkspaceSessionServices::Error) { WorkspaceSessionServices.requirements(workspace:, state_root: root) }
        assert_equal 0o000, File.stat(inventory).mode & 0o777
      end
      File.chmod(0o700, inventory)
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(File.dirname(locks))
      File.write(locks, 'invalid lock inventory')
      assert_raises(WorkspaceSessionServices::Error) { WorkspaceSessionServices.requirements(workspace:, state_root: root) }
      assert_equal 'invalid lock inventory', File.read(locks)
    ensure
      File.chmod(0o700, inventory) if inventory && File.directory?(inventory)
    end
  end

  def test_session_services_empty_registration_retains_metadata_capability_requirement
    with_services_fixture do |runner, manager, workspace, slug|
      store = runner.session_services(slug)
      state_root = runner.instance_variable_get(:@workspace_state_root)
      store.send(:initialize_registry!)
      assert_equal true, WorkspaceSessionServices.requirements(workspace:, state_root:)[:required]
      assert_raises(DevSession::Error) { runner.require_service_root_creation_allowed!(slug) }
      store.send(:finish_initialization_without_launch!)
      assert_equal 0, store.public_status['count']
      assert_equal true, WorkspaceSessionServices.requirements(workspace:, state_root:)[:required]
      assert_raises(DevSession::Error) { runner.require_service_root_creation_allowed!(slug) }
      assert_empty manager.calls
    end
  end

  def test_session_services_first_conversation_refuses_live_and_retired_registration_before_mutation
    [false, true].each do |retired|
      with_services_fixture(archive: true) do |runner, manager, workspace, slug|
        service = launch_service(runner, workspace, slug)['services'].first
        runner.service_action(slug, as_is: true, action: 'stop', id: service['id']) if retired
        files = services_files(runner, slug).values_at(:marker, :registry) +
          %w[plan.md state.md portal.yml].map { |name| File.join(workspace, 'work', slug, name) }
        before = files.to_h { |path| [path, File.binread(path)] }
        effects = []
        guarded_steps = %i[select_sensitivity! prepare_revive_runtime_start! prepare_portal_creation_receipt!
          prepare_creation_journal prepare_provider_selection! create_portal_thread]
        guarded_steps.each do |name|
          runner.define_singleton_method(name) do |*args, **options|
            effects << name
            raise Minitest::Assertion, "first-root refusal reached #{name}"
          end
        end
        native_calls = manager.calls.length
        error = assert_raises(DevSession::Error) do
          runner.start(slug, as_is: true, new: false, attach: false, run_codex: true,
            goal_text: 'Create the first conversation.')
        end
        assert_includes error.message, 'cannot add a Codex conversation'
        assert_empty effects
        assert_equal before, files.to_h { |path| [path, File.binread(path)] }
        %w[creation start fork].each do |kind|
          refute File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.#{kind}.json"))
        end
        assert_equal native_calls, manager.calls.length
        guarded_steps.each { |name| runner.singleton_class.send(:remove_method, name) }
        assert_equal retired ? 0 : 1, runner.service_action(slug, as_is: true, action: 'list')['count']
        runner.service_action(slug, as_is: true, action: 'stop', id: service['id']) unless retired
        runner.archive(slug, as_is: true, explicit: true)
        runner.revive(slug, as_is: true)
        assert_nil runner.service_session_identity(slug)['retained_thread_id']
        assert_equal 0, runner.service_action(slug, as_is: true, action: 'list')['count']
        runner.delete(slug, as_is: true, force: true)
        assert_empty manager.units
      end
    end
  end

  def test_session_services_headless_start_and_absent_registration_retain_admission
    [false, true].each do |registered|
      with_services_fixture do |runner, _manager, workspace, slug|
        launch_service(runner, workspace, slug) if registered
        runner.define_singleton_method(:select_sensitivity!) do |*_args, **_options|
          raise DevSession::Error, 'fixture reached admitted start'
        end
        error = assert_raises(DevSession::Error) do
          runner.start(slug, as_is: true, new: false, attach: false, run_codex: !registered,
            goal_text: registered ? nil : 'Create the first conversation.')
        end
        assert_equal 'fixture reached admitted start', error.message
      end
    end
  end

  def test_session_services_same_retained_root_still_starts_without_rebinding_registration
    with_workspace do |workspace|
      slug = '2026-06-06-rooted-service-registration'
      runner, calls, out = ordinary_retained_runner_fixture(workspace, slug)
      store = runner.session_services(slug)
      store.send(:initialize_registry!)
      store.send(:finish_initialization_without_launch!)
      before = store.files.values_at(:marker, :registry).to_h { |path| [path, File.binread(path)] }
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: true, json: true)
      assert_equal 'retained-root', JSON.parse(out.string)['threadId']
      assert_equal before, before.keys.to_h { |path| [path, File.binread(path)] }
      native = File.readlines(calls).map { |line| JSON.parse(line) }
      resume = native.find { |args| args[0, 2] == ['thread', 'create'] }
      assert_equal 'retained-root', resume.fetch(resume.index('--thread-id') + 1)
    end
  end

  def test_session_services_fork_refuses_registered_rootless_destination_before_initial_mutations
    with_services_fixture do |runner, manager, workspace, slug|
      launch_service(runner, workspace, slug)
      files = services_files(runner, slug).values_at(:marker, :registry) + [File.join(workspace, 'work', slug, 'portal.yml')]
      before = files.to_h { |path| [path, File.binread(path)] }
      %i[select_sensitivity! prepare_portal_creation_receipt! prepare_fork_journal!
         prepare_provider_selection! create_portal_fork].each do |name|
        runner.define_singleton_method(name) { |*_args, **_options| raise Minitest::Assertion, "fork refusal reached #{name}" }
      end
      calls = manager.calls.length
      error = assert_raises(DevSession::Error) { runner.fork('source', slug, as_is: true, json: true) }
      assert_includes error.message, 'cannot add a Codex conversation'
      assert_equal before, files.to_h { |path| [path, File.binread(path)] }
      refute File.exist?(runner.send(:fork_journal_file, slug))
      assert_equal calls, manager.calls.length
    end
  end

  def test_session_services_fork_rechecks_destination_after_admission_without_gating_source
    with_workspace do |workspace|
      source, destination = '2026-06-06-source-with-services', '2026-06-06-new-fork'
      runner, calls, = ordinary_retained_runner_fixture(workspace, source)
      store = runner.session_services(source)
      store.send(:initialize_registry!)
      store.send(:finish_initialization_without_launch!)
      source_bytes = store.files.values_at(:marker, :registry).to_h { |path| [path, File.binread(path)] }
      checked = []
      guard = runner.method(:require_service_root_creation_allowed!)
      runner.define_singleton_method(:require_service_root_creation_allowed!) do |slug|
        checked << slug
        guard.call(slug)
      end
      destination_bytes = nil
      runner.define_singleton_method(:resolve_portal_fork_settings) do |*_args, **_options|
        # Another admitted operation can establish tracking/registration while
        # fork's destination locks are released for source checks.
        ensure_tracking_files(destination)
        send(:ensure_portal_manifest, destination, tracking_only: true)
        registered = session_services(destination)
        registered.send(:initialize_registry!)
        registered.send(:finish_initialization_without_launch!)
        destination_bytes = registered.files.values_at(:marker, :registry).to_h { |path| [path, File.binread(path)] }
        [nil, nil]
      end
      %i[prepare_fork_journal! prepare_provider_selection! create_portal_fork].each do |name|
        runner.define_singleton_method(name) { |*_args, **_options| raise Minitest::Assertion, "fork recheck reached #{name}" }
      end
      error = assert_raises(DevSession::Error) { runner.fork(source, destination, as_is: true, json: true) }
      assert_includes error.message, 'cannot add a Codex conversation'
      assert_equal [destination, destination], checked
      assert_equal source_bytes, source_bytes.keys.to_h { |path| [path, File.binread(path)] }
      assert destination_bytes
      assert_equal destination_bytes, destination_bytes.keys.to_h { |path| [path, File.binread(path)] }
      refute File.exist?(runner.send(:fork_journal_file, destination))
      native = File.exist?(calls) ? File.readlines(calls).map { |line| JSON.parse(line) } : []
      refute native.any? { |args| args[0, 2] == ['thread', 'fork'] }
    end
  end
end
