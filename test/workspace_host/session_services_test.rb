# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'
require 'securerandom'

class WorkspaceHostTest < Minitest::Test
  def test_session_services_absence_needs_no_new_capability
    with_service_host do |host, paths|
      candidate = make_package(paths.fetch(:root), 'service-predecessor')
      host.send(:require_compatible_session_services!, candidate)
    end
  end

  def test_session_services_retired_empty_and_archived_records_block_incapable_predecessor_dispatch
    [{ phase: 'retired' }, { empty: true }, { phase: 'retired', archived: true }].each do |state|
      with_service_host do |host, paths|
        candidate = make_package(paths.fetch(:root), 'service-predecessor')
        files = host_service_fixture(host, **state)
        original = files.values.to_h { |path| [path, File.binread(path)] }
        profile = host.instance_variable_get(:@profile)
        refute(File.exist?(profile))
        dispatched = File.join(paths.fetch(:root), 'predecessor-dispatched')
        command = File.join(candidate, 'bin', 'workspace-host')
        File.write(command, "#!/bin/sh\ntouch #{dispatched}\nexit 99\n")
        source = File.join(paths.fetch(:root), 'source')
        FileUtils.mkdir_p(source)
        host.define_singleton_method(:coordination_source_identity) { |_path| 'fixture-source' }
        host.define_singleton_method(:coordination_build) { |_path| candidate + "\n" }
        profile_admitted = false
        host.define_singleton_method(:require_coordination_profile_claim!) do |_package|
          profile_admitted = true
          raise 'incapable predecessor reached activation admission'
        end

        error = assert_raises(DevWorkspaceHost::Error) do
          host.send(:prepare_profile, ['--source', source])
        end
        assert_includes(error.message, 'registered temporary services')
        error = assert_raises(DevWorkspaceHost::Error) do
          host.send(:activate_candidate, candidate, prepared: {})
        end
        assert_includes(error.message, 'registered temporary services')
        refute(profile_admitted, 'capability check must precede activation admission')
        refute(File.exist?(dispatched), 'incapable predecessor must not be dispatched')
        refute(File.exist?(profile), 'incapable package must not be selected')
        original.each { |path, bytes| assert_equal(bytes, File.binread(path)) }

        contract_path = File.join(candidate, 'share/workspace-portal/runtime-contract.json')
        contract = JSON.parse(File.read(contract_path)).merge(
          'sessionServicesStateSchema' => 1, 'sessionServicesLifecyclePolicy' => 1
        )
        File.write(contract_path, JSON.generate(contract))
        host.send(:require_compatible_session_services!, candidate)
      end
    end
  end

  def test_session_services_live_and_initializing_state_requires_both_exact_capabilities
    with_service_host do |host, paths|
      candidate = make_package(paths.fetch(:root), 'service-candidate')
      files = host_service_fixture(host)
      contract_path = File.join(candidate, 'share/workspace-portal/runtime-contract.json')
      contract = JSON.parse(File.read(contract_path)).merge('sessionServicesStateSchema' => 1, 'sessionServicesLifecyclePolicy' => 1)
      %w[sessionServicesStateSchema sessionServicesLifecyclePolicy].each do |field|
        [nil, false, true, 0, 2, '1', 1.0].each do |value|
          File.write(contract_path, JSON.generate(contract.merge(field => value)))
          error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_services!, candidate) }
          assert_includes(error.message, 'registered temporary services')
        end
      end
      File.write(contract_path, JSON.generate(contract))
      host.send(:require_compatible_session_services!, candidate)
      marker = JSON.parse(File.read(files.fetch(:marker))).merge('state' => 'initializing')
      File.write(files.fetch(:marker), JSON.generate(marker))
      File.unlink(files.fetch(:registry))
      host.send(:require_compatible_session_services!, candidate)
      File.write(contract_path, JSON.generate(contract.reject { |key, _| key == 'sessionServicesStateSchema' }))
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_services!, candidate) }
    end
  end

  def test_session_services_pending_cleanup_blocks_selection_and_candidate_activation
    with_service_host do |host, paths|
      candidate = make_package(paths.fetch(:root), 'service-pending')
      files = host_service_fixture(host, phase: 'retired', pending: true)
      before = files.values.to_h { |path| [path, File.binread(path)] }
      error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_services!, candidate) }
      assert_includes(error.message, 'unfinished service cleanup')
      error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_no_unfinished_lifecycle_operations!) }
      assert_includes(error.message, 'archive service cleanup')
      assert_raises(DevWorkspaceHost::Error) { host.send(:activate_candidate, candidate, prepared: {}) }
      before.each { |path, bytes| assert_equal(bytes, File.binread(path)) }
    end
  end

  def test_session_services_known_loss_or_corruption_never_becomes_absence
    with_service_host do |host, paths|
      candidate = make_package(paths.fetch(:root), 'service-loss')
      files = host_service_fixture(host)
      marker, registry = files.values_at(:marker, :registry)
      original = File.binread(registry)
      File.unlink(registry)
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_services!, candidate) }
      File.write(registry, original, mode: 'w', perm: 0o600)
      marker_bytes = File.binread(marker)
      File.unlink(marker)
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_compatible_session_services!, candidate) }
      File.write(marker, marker_bytes, mode: 'w', perm: 0o600)
      File.write(registry, '{broken')
      assert_raises(DevWorkspaceHost::Error) { host.send(:require_no_unfinished_lifecycle_operations!) }
      assert_equal('{broken', File.binread(registry))
    end
  end

  private

  def with_service_host
    Dir.mktmpdir('workspace-host-session-services') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'registry.json')
      registry_at_path = DevWorkspaceHost::Registry.new(config)
      registry_at_path.register(name: 'example', root: root, hostname: 'example.test', aliases: [], replace: false)
      environment = host_environment(directory, config: config).merge('DEV_WORKSPACES_EXTENSION_CATALOG' => source_extension_catalog(directory))
      host = DevWorkspaceHost::Host.new(env: environment, out: StringIO.new, err: StringIO.new)
      yield host, { root: directory }
    end
  end

  def host_service_fixture(host, phase: 'running', pending: false, empty: false, archived: false)
    workspace = host.send(:registry).entries.fetch(0).fetch('root')
    slug = 'service-fixture'
    tracking = File.join(workspace, 'work', slug)
    FileUtils.mkdir_p(tracking)
    File.write(File.join(tracking, 'portal.yml'), "schema: 1\nslug: #{slug}\n")
    stat = File.stat(tracking)
    now = Time.now.utc.iso8601
    owner = { 'schema' => 1, 'workspace' => workspace, 'slug' => slug,
              'registry_id' => SecureRandom.uuid, 'retained_thread_id' => nil,
              'tracking_identity' => { 'dev' => stat.dev, 'ino' => stat.ino } }
    marker = owner.merge('state' => 'ready', 'created_at' => now)
    id = SecureRandom.uuid
    row = { 'id' => id, 'label' => 'Fixture preview', 'unit' => "workspace-session-#{id}.service",
            'description' => "Workspace session service #{owner.fetch('registry_id')} #{id}",
            'argv' => ['/bin/true'], 'cwd' => tracking,
            'request_sha256' => Digest::SHA256.hexdigest(JSON.generate(['Fixture preview', tracking, ['/bin/true']])),
            'phase' => phase, 'created_at' => now, 'updated_at' => now, 'boot_id' => SecureRandom.uuid,
            'manager_uid' => Process.euid, 'manager_control_group' => '/fixture-manager',
            'expected_control_group' => "/fixture-manager/app.slice/workspace-session-#{id}.service", 'invocation_id' => nil, 'control_group' => nil,
            'stopped_at' => phase == 'retired' ? now : nil, 'retired_at' => phase == 'retired' ? now : nil, 'error' => nil }
    registry = owner.merge('revision' => 1, 'activity_token' => SecureRandom.uuid, 'resources' => empty ? [] : [row])
    prelude = owner.merge('operation' => 'archive', 'operation_id' => 'a' * 64, 'mode' => 'archived',
                          'force' => nil, 'automatic' => false, 'resource_ids' => [id], 'phase' => 'services_stopped',
                          'created_at' => now, 'updated_at' => now)
    files = WorkspaceSessionServices.paths(workspace: workspace, state_root: host.instance_variable_get(:@state), slug: slug)
    values = { marker: marker, registry: registry }
    values[:prelude] = prelude if pending
    values.each do |key, value|
      path = files.fetch(key)
      FileUtils.mkdir_p(File.dirname(path), mode: 0o700)
      File.write(path, JSON.generate(value), mode: 'w', perm: 0o600)
    end
    if archived
      archive = File.join(workspace, 'archive', slug)
      FileUtils.mkdir_p(File.dirname(archive))
      FileUtils.mv(tracking, archive)
    end
    files.slice(*values.keys)
  end
end
