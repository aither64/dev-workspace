# frozen_string_literal: true

require_relative 'suspension_test'

class WorkspaceHostTest < Minitest::Test
  def test_profile_preparation_keeps_actual_compatibility_diagnostics_out_of_json
    with_transition_host do |host, paths|
      host.executing_package = host.candidate
      host.send(:root_codex, paths[:old_codex], paths[:current_root])
      command = File.join(host.candidate, 'bin/workspace-host')
      File.write(command, "#!/bin/sh\necho 'contract passed'\necho 'probe diagnostic' >&2\n")
      entry = host.send(:registry).entries.first
      engine = WorkspaceCoordination::Engine.new(state: host.instance_variable_get(:@state),
        package: host.candidate, profile: host.instance_variable_get(:@profile),
        workspace: entry['root'], entry:, fixtures: true)
      host.define_singleton_method(:coordination_engine) { |_entry| engine }
      Dir.chdir(entry['root']) do
        assert_equal 0, host.run('workspace-host', ['prepare', '--source', paths[:source], '--json'])
        result = JSON.parse(host.instance_variable_get(:@out).string)
        assert_equal host.candidate, result['candidatePackage']
        assert_equal 1, result['schema']
        diagnostics = host.instance_variable_get(:@err).string
        assert_equal 2, diagnostics.scan('contract passed').length
        assert_equal 2, diagnostics.scan('probe diagnostic').length
        refute File.symlink?(host.instance_variable_get(:@profile))

        host.instance_variable_get(:@out).truncate(0)
        host.instance_variable_get(:@out).rewind
        File.write(command, "#!/bin/sh\necho 'incompatible contract'\nexit 1\n")
        assert_equal 1, host.run('workspace-host', ['prepare', '--source', paths[:source], '--json'])
        assert_empty host.instance_variable_get(:@out).string
        assert_includes host.instance_variable_get(:@err).string, 'incompatible contract'
        refute File.symlink?(host.instance_variable_get(:@profile))
      end
    end
  end

  def with_ready_coordination_session
    with_transition_host do |host, paths|
      assert_equal 0, host.run('workspace-host', ['switch', '--source', paths[:source]])
      entry = host.send(:registry).entries.first
      slug = 'normally-created'
      runtime = host.send(:instance_runtime, entry)
      directory = File.join(entry['root'], 'work', slug)
      FileUtils.mkdir_p(directory)
      manifest = {'schema' => 2, 'slug' => slug, 'repositories' => [],
        'codex' => {'thread_id' => SecureRandom.uuid, 'socket_path' => runtime[:codex]},
        'creation' => {'state' => 'ready', 'goal_sha256' => 'c' * 64}}
      manifest_path = File.join(directory, 'portal.yml')
      File.write(manifest_path, YAML.dump(manifest))
      File.write(File.join(directory, 'state.md'), "---\nlifecycle: active\n---\n")
      journal = {'schema' => 2, 'slug' => slug, 'goal_sha256' => 'c' * 64,
        'run_codex' => true, 'state' => 'ready', 'model' => 'gpt-6-sol', 'effort' => 'xhigh',
        'agent_team_binding' => "v1.fixture.#{'a' * 64}", 'agent_team_binding_digest' => 'a' * 64}
      path = File.join(entry['root'], 'worktrees', '.locks', "#{slug}.creation.json")
      WorkspaceCoordination.atomic(path, journal)
      catalog = {'schema' => 1, 'repositories' => {}, 'adapters' => {}, 'machines' => {}, 'profileMachine' => nil}
      File.write(File.join(host.candidate, 'share/dev-workspace/coordination.json'), JSON.generate(catalog))
      engine = WorkspaceCoordination::Engine.new(state: host.instance_variable_get(:@state),
        package: host.candidate, profile: host.instance_variable_get(:@profile), workspace: entry['root'],
        entry: entry.merge('socket' => runtime[:codex], 'portalUrl' => 'https://workspace.example.test'), fixtures: true)
      yield host, entry, slug, path, journal, manifest_path, engine
    end
  end

  def test_coordination_accepts_and_preserves_completed_managed_creation_receipt
    with_ready_coordination_session do |host, entry, slug, path, _journal, _manifest_path, engine|
      before = File.binread(path)
      admitted = host.send(:with_coordination_session_admission, entry, slug) { engine.owner(slug) }
      assert_equal slug, admitted['slug']
      assert_equal before, File.binread(path)
      host.define_singleton_method(:coordination_engine) { |_entry| engine }
      claimed = []
      engine.define_singleton_method(:claim) { |owner_id:| claimed << owner_id; [] }
      engine.define_singleton_method(:deliver_notifications) { |owner_id:| throw :scheduler_tick_completed }
      catch(:scheduler_tick_completed) { host.send(:run_coordinator, []) }
      assert_equal [admitted['id']], claimed
      assert_equal before, File.binread(path)
      refute_match(/coordinator.*creation/, host.instance_variable_get(:@err).string)
    end
  end

  def test_coordination_rejects_unfinished_malformed_and_mismatched_creation_receipts
    with_ready_coordination_session do |host, entry, slug, path, journal, manifest_path, _engine|
      invalid = [journal.merge('state' => 'creating', 'tmux_identity' => 'd' * 64),
        journal.merge('slug' => 'another-session'), journal.merge('goal_sha256' => 'e' * 64),
        journal.merge('agent_team_binding_digest' => 'f' * 64)]
      invalid.each do |record|
        WorkspaceCoordination.atomic(path, record)
        assert_raises(DevWorkspaceHost::Error) do
          host.send(:with_coordination_session_admission, entry, slug) { flunk 'invalid receipt was admitted' }
        end
        assert_equal record, JSON.parse(File.read(path))
      end
      WorkspaceCoordination.atomic(path, journal)
      manifest = YAML.safe_load(File.read(manifest_path))
      manifest['creation']['state'] = 'creating'
      File.write(manifest_path, YAML.dump(manifest))
      assert_raises(WorkspaceCoordination::Error) do
        host.send(:with_coordination_session_admission, entry, slug) { flunk 'unfinished manifest was admitted' }
      end
      File.write(path, '{"schema":2,"schema":2}')
      File.write(manifest_path, YAML.dump(manifest.merge('creation' => {'state' => 'ready', 'goal_sha256' => 'c' * 64})))
      assert_raises(DevWorkspaceHost::Error) do
        host.send(:with_coordination_session_admission, entry, slug) { flunk 'malformed journal was admitted' }
      end
    end
  end

  def test_interleaved_switch_completion_cannot_clear_or_restore_new_generation
    with_transition_host do |host, paths|
      assert_equal 0, host.run('workspace-host', ['switch', '--source', paths[:source]])
      original = host.method(:wait_for_codex_sockets)
      nested = false
      second = make_package(paths[:root], 'package-two')
      third = make_package(paths[:root], 'package-three')
      host.candidate = second
      host.define_singleton_method(:wait_for_codex_sockets) do
        unless nested
          nested = true
          self.candidate = third
          raise 'nested switch failed' unless run('workspace-host', ['switch', '--source', paths[:source]]) == 0
          send(:write_pending_codex_update, third, send(:active_codex), send(:registration_plans, package: third))
        end
        original.call
      end
      assert_equal 1, host.run('workspace-host', ['switch', '--source', paths[:source]])
      assert_equal third, File.realpath(host.instance_variable_get(:@profile))
      assert_equal third, host.send(:pending_codex_update)['package_root']
      assert_match(/superseded/, host.instance_variable_get(:@err).string)
    end
  end

  def test_suspended_ledger_resumes_without_discarding_queued_state
    with_transition_host do |host, paths|
      assert_equal 0, host.run('workspace-host', ['switch', '--source', paths[:source]])
      store = host.send(:coordination_store)
      contract = File.join(host.candidate, 'share/dev-workspace/coordination-contract.json')
      File.write(contract, JSON.generate(WorkspaceCoordination::CONTRACT))
      store.transaction { |data| data['suspended'] = true }
      before = store.read.reject { |key, _| %w[suspended revision].include?(key) }
      assert_equal 0, host.run('workspace-host', ['resume'])
      refute store.read['suspended']
      assert_equal before, store.read.reject { |key, _| %w[suspended revision].include?(key) }
    end
  end

  def test_selected_registration_uses_real_activation_path_with_machine_catalog
    with_transition_host do |host, paths|
      assert_equal 0, host.run('workspace-host', ['switch', '--source', paths[:source]])
      catalog = {'schema' => 1, 'repositories' => {'configuration' => {'remote' => 'git@github.com:example/configuration.git'}}, 'profileMachine' => 'example:host',
        'adapters' => {'confctl' => {'protocol' => 1, 'command' => RbConfig.ruby}},
        'machines' => {'example:host' => {'id' => 'example:host', 'aliases' => [], 'adapter' => 'confctl', 'configurationRepository' => 'configuration', 'selector' => 'fixture'}}}
      File.write(File.join(host.candidate, 'share/dev-workspace/coordination.json'), JSON.generate(catalog))
      host.instance_variable_set(:@coordination_catalog, WorkspaceCoordination::Catalog.new(host.candidate, fixtures: true))
      host.define_singleton_method(:activate_installed) do |command|
        DevWorkspaceHost::Host.instance_method(:activate_installed).bind(self).call(command)
      end
      host.define_singleton_method(:system_env!) do |environment, command, *argv|
        raise 'wrong activation command' unless argv == ['_activate']
        previous = @env
        @env = environment
        send(:activate, [])
      ensure
        @env = previous
      end
      entry = host.send(:registry).entries.first
      assert_equal 0, host.run('workspace-host', ['register', entry['name'], entry['root'], '--hostname', 'replacement.example.test', '--replace']), host.instance_variable_get(:@err).string
      assert_equal 'replacement.example.test', host.send(:registry).entries.first['hostname']
      File.write(File.join(host.candidate, 'share/dev-workspace/coordination-contract.json'), JSON.generate(WorkspaceCoordination::CONTRACT))
      before_registry = File.binread(host.send(:registry).path)
      owner_id = SecureRandom.uuid
      store = host.send(:coordination_store)
      store.transaction do |data|
        data['owners'][owner_id] = {'id' => owner_id, 'identity' => 'sha256:' + ('a' * 64), 'workspace' => entry['root'], 'workspaceName' => entry['name'],
          'slug' => 'review-owner', 'thread' => SecureRandom.uuid, 'socket' => '/fixture/socket', 'portalUrl' => 'https://workspace.example.test/review-owner/', 'bootstrap' => false, 'fence' => nil}
        id = SecureRandom.uuid
        data['reservations'][id] = {'id' => id, 'ownerId' => owner_id, 'machine' => 'example:host', 'target' => 'example:host',
          'state' => 'held', 'revision' => 1, 'sequence' => 1, 'mode' => 'deploy_first', 'approvalId' => SecureRandom.uuid, 'integrationObligations' => [], 'history' => []}
      end
      assert_equal 4, host.run('workspace-host', ['register', entry['name'], entry['root'], '--hostname', 'blocked.example.test', '--replace'])
      assert_equal before_registry, File.binread(host.send(:registry).path)
      # Old selection helpers still enter _activate without reconciliation authority.
      host.instance_variable_get(:@env)['DEV_WORKSPACE_ACTIVATION'] = '1'
      assert_equal 4, host.run('workspace-host', ['_activate'])
    end
  end

  def test_suspend_compensates_service_failure_before_teardown_finishes
    with_transition_host do |host, paths|
      assert_equal 0, host.run('workspace-host', ['switch', '--source', paths[:source]])
      store = host.send(:coordination_store)
      store.transaction { |data| data['suspended'] = true }
      store.transaction { |data| data['suspended'] = false }
      original = host.method(:stop_auto_archive_services)
      failed = false
      host.define_singleton_method(:stop_auto_archive_services) do |*args|
        unless failed
          failed = true
          raise DevWorkspaceHost::Error, 'injected early service stop failure'
        end
        original.call(*args)
      end
      host.events.clear
      assert_equal 1, host.run('workspace-host', ['suspend'])
      refute store.read['suspended']
      assert_includes host.events, [:configured]
    end
  end

  def test_installed_coordination_guide_is_available_from_an_unchanged_workspace
    with_transition_host do |host, paths|
      assert_equal 0, host.run('workspace-host', ['switch', '--source', paths[:source]])
      %w[coordination.md coordination-operations.md].each do |name|
        FileUtils.cp(File.expand_path("../../docs/#{name}", __dir__), File.join(host.candidate, 'share/dev-workspace', name))
      end
      assert_equal 0, host.run('workspace-host', ['coordination', 'guide'])
      output = host.instance_variable_get(:@out).string
      assert_includes output, '# Coordinated workspace operations'
      assert_includes output, '# Coordination operations and recovery'
      assert_includes output, host.candidate
      refute File.exist?(File.join(host.send(:registry).entries.first['root'], 'docs/coordination.md'))
    end
  end

  def test_busy_switch_preserves_consumers_and_launch_marker_across_catalog_changes
    [nil, 'b' * 64].each do |digest|
      with_transition_host do |host, paths|
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        entry = host.send(:registry).entries.fetch(0)
        marker = host.send(:load_registration_marker, entry)
        host.events.clear
        host.busy = ['lead and member are active']
        host.registration_digest = digest
        host.candidate = make_package(paths.fetch(:root), 'package-two')

        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        assert_equal(host.candidate, File.realpath(host.instance_variable_get(:@profile)))
        refute(host.events.any? { |event| %i[sessions_quiesced sessions_restored consumers_restarted].include?(event.first) })
        assert_includes(host.events, [:configured])
        assert_equal(marker, host.send(:load_registration_marker, entry))
        assert_equal(host.candidate, host.send(:load_registration_inventory, entry).fetch('package_root'))
        assert_nil(host.send(:pending_codex_update))
      end
    end
  end

  def test_busy_switch_refuses_missing_malformed_or_incompatible_launch_evidence
    %i[missing malformed argv capacity legacy].each do |mismatch|
      with_transition_host do |host, paths|
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        entry = host.send(:registry).entries.fetch(0)
        marker_path = host.send(:registration_marker_path, entry)
        case mismatch
        when :missing then File.unlink(marker_path)
        when :malformed then File.write(marker_path, '{}')
        when :argv then host.registration_argv = []
        when :capacity then host.registration_capacity = 2
        when :legacy
          contract = File.join(host.candidate, 'share/workspace-portal/runtime-contract.json')
          data = JSON.parse(File.read(contract))
          data.delete('livePackageSwitchPolicy')
          File.write(contract, JSON.generate(data))
        end
        predecessor = host.candidate
        host.candidate = make_package(paths.fetch(:root), 'package-two')
        host.busy = ['active member']
        host.events.clear

        assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]), mismatch.to_s)
        assert_equal(predecessor, File.realpath(host.instance_variable_get(:@profile)))
        refute(host.events.any? { |event| event.first == :profile_set })
        assert_equal(host.candidate, host.send(:pending_codex_update).fetch('package_root'))
        host.busy = []
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        assert_includes(host.events, [:consumers_restarted])
      end
    end
  end

  def test_switch_checks_and_adopts_the_candidates_binary
    with_transition_host do |host, paths|
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.instance_variable_get(:@env).delete('DEV_WORKSPACES_SYSTEM_CODEX')
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      codex = make_codex(paths.fetch(:root), 'codex-candidate')
      bundled = File.join(host.candidate, 'libexec/codex/bin/codex')
      FileUtils.mkdir_p(File.dirname(bundled))
      File.symlink(codex, bundled)
      host.events.clear
      host.busy = ['active lead']

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(File.realpath(codex), host.send(:pending_codex_update).fetch('codex_path'))
      assert(host.events.any? { |event| event == [:registration_probed, File.realpath(codex), 'a' * 64] })
      host.busy = []
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(File.realpath(codex), File.realpath(host.send(:active_codex)))
    end
  end

  def test_failed_live_activation_retains_forward_target_and_retries_while_busy
    with_transition_host do |host, paths|
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.busy = ['active lead']
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.fail_activation = true
      host.events.clear

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(host.candidate, File.realpath(host.instance_variable_get(:@profile)))
      assert_equal(host.candidate, host.send(:pending_codex_update).fetch('package_root'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(host.events.any? { |event| %i[sessions_quiesced sessions_restored consumers_restarted].include?(event.first) })
      assert_nil(host.send(:pending_codex_update))
    end
  end
end
