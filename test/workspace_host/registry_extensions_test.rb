# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'

class WorkspaceHostTest < Minitest::Test
  def test_auto_archive_units_are_enabled_and_removed_on_rollback
    Dir.mktmpdir do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example', root:, hostname: 'example.test', aliases: [], replace: false
      )
      current = make_package(directory, 'current')
      previous = make_package(directory, 'previous')
      unit_root = File.join(current, 'share/systemd/user')
      FileUtils.mkdir_p(unit_root)
      %w[timer service].each do |suffix|
        File.write(File.join(unit_root, "workspace-auto-archive@.#{suffix}"), "[Unit]\n")
      end
      env = host_environment(directory, config:)
      profile = env.fetch('DEV_WORKSPACES_PROFILE')
      FileUtils.mkdir_p(File.dirname(profile))
      File.symlink(current, profile)
      host = UnregisterHost.new(env:, out: StringIO.new, err: StringIO.new)
      host.send(:install_links, package: current)
      refute_includes(host.commands, ['systemctl', '--user', 'enable', '--now', 'workspace-auto-archive@example.timer'])
      host.send(:configure_auto_archive_services, package: current)
      assert_includes(host.commands, ['systemctl', '--user', 'enable', '--now', 'workspace-auto-archive@example.timer'])
      host.send(:stop_auto_archive_services)
      File.unlink(profile)
      File.symlink(previous, profile)
      host.send(:install_links, package: previous)
      assert_includes(host.commands, ['systemctl', '--user', 'disable', '--now', 'workspace-auto-archive@example.timer'])
      assert_includes(host.commands, ['systemctl', '--user', 'stop', 'workspace-auto-archive@example.service'])
      refute(File.symlink?(File.join(directory, '.config/systemd/user/workspace-auto-archive@.timer')))
      refute(File.symlink?(File.join(directory, '.config/systemd/user/workspace-auto-archive@.service')))
    end
  end

  def test_candidate_cleans_auto_archive_units_when_old_initiator_cannot
    %w[configure timer].each do |failure|
      Dir.mktmpdir do |directory|
        root = make_workspace(directory, 'workspace')
        config = File.join(directory, 'registry.json')
        DevWorkspaceHost::Registry.new(config).register(
          name: 'example', root:, hostname: 'example.test', aliases: [], replace: false
        )
        candidate = make_package(directory, 'candidate')
        unit_root = File.join(candidate, 'share/systemd/user')
        FileUtils.mkdir_p(unit_root)
        %w[timer service].each do |suffix|
          File.write(File.join(unit_root, "workspace-auto-archive@.#{suffix}"), "[Unit]\n")
        end
        env = host_environment(directory, config:).merge('DEV_WORKSPACE_ACTIVATION' => '1')
        profile = env.fetch('DEV_WORKSPACES_PROFILE')
        FileUtils.mkdir_p(File.dirname(profile))
        File.symlink(candidate, profile)
        # Model the old initiator's wildcard unit linking, with no timer cleanup.
        links = File.join(directory, '.config/systemd/user')
        FileUtils.mkdir_p(links)
        Dir[File.join(unit_root, '*')].each do |unit|
          File.symlink(File.join(profile, 'share/systemd/user', File.basename(unit)), File.join(links, File.basename(unit)))
        end
        host = UnregisterHost.new(env:, out: StringIO.new, err: StringIO.new)
        host.define_singleton_method(:package_root) { candidate }
        host.define_singleton_method(:reconcile_codex_update) { |**| nil }
        host.define_singleton_method(:configure_user_services) do
          raise DevWorkspaceHost::Error, 'injected configure failure' if failure == 'configure'
        end
        original = host.method(:system!)
        host.define_singleton_method(:system!) do |*argv|
          original.call(*argv)
          if failure == 'timer' && argv == ['systemctl', '--user', 'enable', '--now', 'workspace-auto-archive@example.timer']
            raise DevWorkspaceHost::Error, 'injected partial timer enable failure'
          end
        end
        assert_equal(1, host.run('workspace-host', ['_activate']))
        refute(File.symlink?(File.join(links, 'workspace-auto-archive@.timer')))
        refute(File.symlink?(File.join(links, 'workspace-auto-archive@.service')))
        assert_includes(host.commands, ['systemctl', '--user', 'stop', 'workspace-auto-archive@example.service'])
        if failure == 'timer'
          enable = host.commands.index(['systemctl', '--user', 'enable', '--now', 'workspace-auto-archive@example.timer'])
          assert_includes(host.commands.drop(enable + 1), ['systemctl', '--user', 'disable', '--now', 'workspace-auto-archive@example.timer'])
        end
      end
    end
  end

  def test_unregister_restores_clients_and_timer_after_timer_stop_failure
    Dir.mktmpdir do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example', root:, hostname: 'example.test', aliases: [], replace: false
      )
      restored = []
      host = UnregisterHost.new(env: host_environment(directory, config:), out: StringIO.new, err: StringIO.new)
      host.define_singleton_method(:quiesce_sessions) { |*| [:client] }
      host.define_singleton_method(:stop_auto_archive_services) { |*| raise DevWorkspaceHost::Error, 'injected timer stop failure' }
      host.define_singleton_method(:configure_auto_archive_services) { |*| restored << :timer }
      host.define_singleton_method(:restore_quiesced_sessions) { |sessions, **| restored.concat(sessions) }
      assert_equal(1, host.run('workspace-host', ['unregister', 'example']))
      assert_equal([:timer, :client], restored)
      assert(DevWorkspaceHost::Registry.new(config).find('example'))
    end
  end

  def test_rollback_refuses_before_touching_timer_or_terminal_clients
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.events.clear

      assert_equal(1, host.run('workspace-host', ['rollback']))

      assert_equal(1, host.send(:profile_generation))
      refute(host.events.any? { |event| event.first == :sessions_restored })
      refute(host.events.any? { |event| event == [:timer_restored] })
      assert_includes(host.instance_variable_get(:@err).string, 'forward-only')
    end
  end

  def test_lifecycle_journals_are_projected_from_the_shared_runtime_contract
    contract = JSON.parse(
      File.read(File.expand_path('../../portal/internal/session/runtime-contract.json', __dir__))
    )

    assert_equal(
      contract.fetch('lifecycleJournals'),
      DevWorkspaceHost::LIFECYCLE_JOURNALS
    )
  end

  def test_registry_selects_the_longest_matching_root_and_requires_a_name_outside_multiple_roots
    Dir.mktmpdir('workspace-host-test') do |directory|
      first = make_workspace(directory, 'first')
      nested = make_workspace(first, 'nested')
      registry = registry_at(directory)
      registry.register(
        name: 'first', root: first, hostname: 'first.workspace.example.test',
        aliases: [], replace: false
      )
      registry.register(
        name: 'nested', root: nested, hostname: 'nested.workspace.example.test',
        aliases: [], replace: false
      )

      assert_equal('nested', registry.select(cwd: nested).fetch('name'))
      error = assert_raises(DevWorkspaceHost::Error) do
        registry.select(cwd: directory)
      end
      assert_includes(error.message, '--workspace NAME')
      assert_equal('first', registry.select(name: 'first', cwd: directory).fetch('name'))
    end
  end

  def test_registry_is_private_and_rejects_duplicate_hosts
    Dir.mktmpdir('workspace-host-test') do |directory|
      first = make_workspace(directory, 'first')
      second = make_workspace(directory, 'second')
      registry = registry_at(directory)
      registry.register(
        name: 'first', root: first, hostname: 'first.workspace.example.test',
        aliases: ['old.example.test'], replace: false
      )

      assert_equal(0o600, File.stat(registry.path).mode & 0o777)
      error = assert_raises(DevWorkspaceHost::Error) do
        registry.register(
          name: 'second', root: second, hostname: 'old.example.test',
          aliases: [], replace: false
        )
      end
      assert_includes(error.message, 'duplicate workspace hostname')

      File.chmod(0o644, registry.path)
      assert_raises(DevWorkspaceHost::Error) do
        DevWorkspaceHost::Registry.new(registry.path)
      end
    end
  end

  def test_registry_replace_keeps_the_workspace_root_immutable
    Dir.mktmpdir('workspace-host-test') do |directory|
      first = make_workspace(directory, 'first')
      second = make_workspace(directory, 'second')
      registry = registry_at(directory)
      registry.register(
        name: 'example-workspace', root: first, hostname: 'old.workspace.example.test',
        aliases: [], replace: false
      )

      error = assert_raises(DevWorkspaceHost::Error) do
        registry.register(
          name: 'example-workspace', root: second, hostname: 'new.workspace.example.test',
          aliases: [], replace: true
        )
      end

      assert_includes(error.message, 'unregister example-workspace first')
      assert_equal(first, registry.find('example-workspace').fetch('root'))
      updated = registry.register(
        name: 'example-workspace', root: first, hostname: 'new.workspace.example.test',
        aliases: ['old.workspace.example.test'], replace: true
      )
      assert_equal(first, updated.fetch('root'))
      assert_equal('new.workspace.example.test', updated.fetch('hostname'))
    end
  end

  def test_register_reads_portal_identity_from_workspace_configuration
    Dir.mktmpdir('workspace-host-register-config-test') do |directory|
      root = make_workspace(directory, 'workspace')
      File.write(File.join(root, '.dev-workspace.json'), JSON.generate(
        'schema' => 2,
        'displayLabel' => 'Example development',
        'hostLabel' => 'build-host',
        'sshHost' => '',
        'portal' => {
          'hostname' => 'workspace.example.test',
          'aliases' => ['legacy-workspace.example.test']
        },
        'developmentClusterProviders' => []
      ))
      config = File.join(directory, 'config/registry.json')
      host = DevWorkspaceHost::Host.new(
        env: host_environment(directory, config:),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(0, host.run('workspace-host', ['register', 'example', root]))
      entry = DevWorkspaceHost::Registry.new(config).find('example')
      assert_equal('workspace.example.test', entry.fetch('hostname'))
      assert_equal(['legacy-workspace.example.test'], entry.fetch('aliases'))
    end
  end

  def test_extension_catalog_rejects_duplicate_and_relative_entries
    Dir.mktmpdir('workspace-host-extension-catalog-test') do |directory|
      catalog = File.join(directory, 'catalog.json')
      payload = {
        'schema' => 1,
        'commands' => [
          { 'name' => 'tool', 'path' => '/bin/true' },
          { 'name' => 'tool', 'path' => '/bin/false' }
        ],
        'skills' => [],
        'clusterProviders' => []
      }
      File.write(catalog, JSON.generate(payload))
      assert_raises(DevWorkspaceHost::Error) do
        DevWorkspaceHost::ExtensionCatalog.new(payload, path: catalog)
      end

      payload['commands'] = [{ 'name' => 'tool', 'path' => 'relative' }]
      assert_raises(DevWorkspaceHost::Error) do
        DevWorkspaceHost::ExtensionCatalog.new(payload, path: catalog)
      end

      payload['commands'] = [{ 'name' => 'tool', 'path' => directory }]
      assert_raises(DevWorkspaceHost::Error) do
        DevWorkspaceHost::ExtensionCatalog.new(payload, path: catalog)
      end

      payload['commands'] = [{ 'name' => 'workspace-portal', 'path' => RbConfig.ruby }]
      assert_raises(DevWorkspaceHost::Error) do
        DevWorkspaceHost::ExtensionCatalog.new(payload, path: catalog)
      end
    end
  end

  def test_core_source_catalog_contains_no_organization_extensions
    catalog = DevWorkspaceHost::ExtensionCatalog.load(File.expand_path('../..', __dir__))

    assert_empty(catalog.commands)
    assert_empty(catalog.skills)
    assert_empty(catalog.cluster_providers)
  end

  def test_link_install_reconciles_extension_links_across_full_core_and_legacy_rollback
    Dir.mktmpdir('workspace-host-link-test') do |directory|
      full = make_package(directory, 'package-full')
      core = make_package(directory, 'package-core')
      previous = make_package(directory, 'package-previous')
      documentation = 'dev-session-documentation'
      documentation_sources = [full, core].to_h do |package|
        source = File.join(package, 'share/codex/skills', documentation)
        FileUtils.mkdir_p(source)
        File.write(File.join(source, 'SKILL.md'), "# Documentation\n")
        [package, source]
      end
      write_extension_catalog(core, skills: { documentation => documentation_sources.fetch(core) })
      command = 'kb-page'
      command_source = File.join(full, 'bin', command)
      File.write(command_source, "#!/bin/sh\nexit 0\n")
      File.chmod(0o755, command_source)
      skill_name = 'mandatory-change-review'
      skill_source = File.join(full, 'share/codex/skills', skill_name)
      FileUtils.mkdir_p(skill_source)
      File.write(File.join(skill_source, 'SKILL.md'), "# Example\n")
      write_extension_catalog(
        full,
        commands: { command => command_source },
        skills: { skill_name => skill_source, documentation => documentation_sources.fetch(full) }
      )

      state = File.join(directory, 'state')
      profile = File.join(state, 'profile')
      FileUtils.mkdir_p(state)
      File.symlink(core, profile)
      File.symlink(full, "#{profile}-1-link")
      environment = {
        'HOME' => directory, 'PATH' => ENV.fetch('PATH'),
        'DEV_WORKSPACES_STATE' => state, 'DEV_WORKSPACES_PROFILE' => profile,
        'DEV_WORKSPACES_SYSTEM_CODEX' => make_codex(directory, 'codex-system')
      }
      full_host = CompatibilityLinkHost.new(
        package_root: full, env: environment, out: StringIO.new, err: StringIO.new
      )
      core_host = CompatibilityLinkHost.new(
        package_root: core, env: environment, out: StringIO.new, err: StringIO.new
      )
      previous_host = CompatibilityLinkHost.new(
        package_root: previous, env: environment, out: StringIO.new, err: StringIO.new
      )
      command_link = File.join(directory, 'bin', command)
      skill_link = File.join(directory, '.codex/skills', skill_name)
      documentation_link = File.join(directory, '.codex/skills', documentation)
      portal_target = File.join(directory, 'unrelated-workspace-portal')
      portal_link = File.join(directory, 'bin', 'workspace-portal')
      File.write(portal_target, "#!/bin/sh\nexit 0\n")
      FileUtils.mkdir_p(File.dirname(portal_link))
      File.symlink(portal_target, portal_link)

      full_host.send(:install_links)
      assert_equal(
        File.join(profile, 'bin', 'workspace-host'),
        File.readlink(File.join(directory, 'bin', 'workspace-host'))
      )
      assert_equal(
        File.join(profile, 'bin', 'dev-session'),
        File.readlink(File.join(directory, 'bin', 'dev-session'))
      )
      assert_equal(portal_target, File.readlink(portal_link))
      assert_equal(command_source, File.readlink(command_link))
      assert_equal(skill_source, File.readlink(skill_link))
      assert_equal(documentation_sources.fetch(full), File.readlink(documentation_link))

      core_host.send(:install_links)
      refute(File.exist?(command_link))
      refute(File.exist?(skill_link))
      assert_equal(documentation_sources.fetch(core), File.readlink(documentation_link))

      previous_host.send(:install_links)
      refute(File.symlink?(documentation_link))
      assert(File.file?(File.join(documentation_sources.fetch(core), 'SKILL.md')))
      core_host.send(:install_links)
      assert_equal(documentation_sources.fetch(core), File.readlink(documentation_link))

      unrelated = File.join(directory, 'unrelated-command')
      File.write(unrelated, "#!/bin/sh\nexit 0\n")
      File.symlink(unrelated, command_link)
      core_host.send(:install_links)
      assert_equal(unrelated, File.readlink(command_link))
      File.unlink(command_link)

      # A retained pre-inventory generation recreates its static links during
      # rollback. The next core activation must remove them without journal
      # state from that older package.
      File.symlink(command_source, command_link)
      File.symlink(skill_source, skill_link)
      core_host.send(:install_links)
      refute(File.exist?(command_link))
      refute(File.exist?(skill_link))
    end
  end

  def test_link_install_removes_only_the_predecessor_portal_home_link
    Dir.mktmpdir('workspace-host-legacy-portal-link-test') do |directory|
      package = make_package(directory, 'package')
      state = File.join(directory, 'state')
      profile = File.join(state, 'profile')
      FileUtils.mkdir_p(state)
      File.symlink(package, profile)
      environment = {
        'HOME' => directory, 'PATH' => ENV.fetch('PATH'),
        'DEV_WORKSPACES_STATE' => state, 'DEV_WORKSPACES_PROFILE' => profile,
        'DEV_WORKSPACES_SYSTEM_CODEX' => make_codex(directory, 'codex-system')
      }
      host = CompatibilityLinkHost.new(
        package_root: package, env: environment, out: StringIO.new, err: StringIO.new
      )
      link = File.join(directory, 'bin', 'workspace-portal')
      FileUtils.mkdir_p(File.dirname(link))
      File.symlink(File.join(profile, 'bin', 'workspace-portal'), link)

      host.send(:install_links)
      refute(File.symlink?(link))

      unrelated = File.join(directory, 'unrelated-workspace-portal')
      File.write(unrelated, "#!/bin/sh\nexit 0\n")
      File.symlink(unrelated, link)
      host.send(:install_links)
      assert_equal(unrelated, File.readlink(link))
    end
  end

  def test_candidate_activation_links_the_candidate_extensions
    Dir.mktmpdir('workspace-host-candidate-links-test') do |directory|
      candidate = make_package(directory, 'package-candidate')
      command = 'kb-release'
      command_source = File.join(candidate, 'bin', command)
      File.write(command_source, "#!/bin/sh\nexit 0\n")
      File.chmod(0o755, command_source)
      skill_name = 'dev-session-handoff'
      skill_source = File.join(candidate, 'share/codex/skills', skill_name)
      FileUtils.mkdir_p(skill_source)
      File.write(File.join(skill_source, 'SKILL.md'), "# Example\n")
      write_extension_catalog(
        candidate,
        commands: { command => command_source },
        skills: { skill_name => skill_source }
      )
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config)
      state = File.join(directory, 'state')
      profile = File.join(state, 'profile')
      FileUtils.mkdir_p(state)
      File.symlink(candidate, profile)
      host = ActivationGuardHost.new(
        package: candidate,
        env: host_environment(directory, config:).merge(
          'DEV_WORKSPACES_PROFILE' => profile,
          'DEV_WORKSPACE_ACTIVATION' => '1'
        ),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(0, host.run('workspace-host', ['_activate']))
      assert(host.configured)
      assert_equal(command_source, File.readlink(File.join(directory, 'bin', command)))
      assert_equal(
        skill_source,
        File.readlink(File.join(directory, '.codex/skills', skill_name))
      )
    end
  end

  def test_workspace_configuration_defaults_and_validates_provider_selection
    Dir.mktmpdir('workspace-host-config-test') do |directory|
      package = make_package(directory, 'package')
      environment = {
        'HOME' => directory, 'PATH' => ENV.fetch('PATH'),
        'DEV_WORKSPACES_STATE' => File.join(directory, 'state'),
        'DEV_WORKSPACES_PROFILE' => File.join(directory, 'state/profile'),
        'DEV_WORKSPACES_SYSTEM_CODEX' => make_codex(directory, 'codex-system')
      }
      host = CompatibilityLinkHost.new(
        package_root: package, env: environment, out: StringIO.new, err: StringIO.new
      )
      entry = {
        'root' => directory, 'hostname' => 'workspace.example.test', 'aliases' => []
      }
      assert_equal([], host.send(:workspace_configuration, entry).fetch('developmentClusterProviders'))
      config = File.join(directory, '.dev-workspace.json')
      File.write(config, JSON.generate(
        'schema' => 2, 'displayLabel' => 'example organization development',
        'hostLabel' => 'build-host', 'sshHost' => 'build-host.int.example.cz',
        'developmentClusterProviders' => %w[alpha beta],
        'portal' => { 'hostname' => 'workspace.example.test', 'aliases' => [] }
      ))
      parsed = host.send(:workspace_configuration, entry)
      assert_equal('build-host', parsed.fetch('hostLabel'))
      File.write(config, JSON.generate(parsed.merge('developmentClusterProviders' => ['unknown'])))
      assert_raises(DevWorkspaceHost::Error) { host.send(:workspace_configuration, entry) }
      File.write(config, "{\"schema\":2}" + (' ' * (64 * 1024)))
      assert_raises(DevWorkspaceHost::Error) { host.send(:workspace_configuration, entry) }
      File.unlink(config)
      File.symlink('/dev/null', config)
      assert_raises(DevWorkspaceHost::Error) { host.send(:workspace_configuration, entry) }
    end
  end

  def test_user_namespace_selects_compatible_default_paths
    Dir.mktmpdir('workspace-host-user-namespace-test') do |directory|
      host = DevWorkspaceHost::Host.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'XDG_RUNTIME_DIR' => File.join(directory, 'run'),
          'DEV_WORKSPACES_NAMESPACE' => 'previous-workspaces'
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(
        File.join(directory, '.config/previous-workspaces/registry.json'),
        host.instance_variable_get(:@config)
      )
      assert_equal(
        File.join(directory, '.local/state/previous-workspaces'),
        host.instance_variable_get(:@state)
      )
      assert_equal(
        File.join(directory, 'run/previous-workspaces'),
        host.instance_variable_get(:@runtime)
      )
    end
  end

  def test_user_namespace_rejects_unsafe_names
    error = assert_raises(DevWorkspaceHost::Error) do
      DevWorkspaceHost::Host.new(
        env: { 'HOME' => Dir.pwd, 'DEV_WORKSPACES_NAMESPACE' => '../state' },
        out: StringIO.new,
        err: StringIO.new
      )
    end

    assert_includes(error.message, 'invalid workspace user namespace')
  end

  def test_private_activation_accepts_a_packaged_legacy_alias
    host = DevWorkspaceHost::Host.new(
      env: {
        'HOME' => Dir.pwd,
        'DEV_WORKSPACE_ACTIVATION_ALIASES' => 'PREVIOUS_WORKSPACE_ACTIVATION',
        'PREVIOUS_WORKSPACE_ACTIVATION' => '1'
      },
      out: StringIO.new,
      err: StringIO.new
    )

    assert(host.send(:private_activation?))
  end

  def test_private_activation_rejects_invalid_aliases
    host = DevWorkspaceHost::Host.new(
      env: {
        'HOME' => Dir.pwd,
        'DEV_WORKSPACE_ACTIVATION_ALIASES' => 'unsafe-name',
        'unsafe-name' => '1'
      },
      out: StringIO.new,
      err: StringIO.new
    )

    error = assert_raises(DevWorkspaceHost::Error) { host.send(:private_activation?) }
    assert_includes(error.message, 'activation aliases are invalid')
  end

end
