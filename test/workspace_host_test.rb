# frozen_string_literal: true

require 'fileutils'
require 'minitest/autorun'
require 'stringio'
require 'tmpdir'

load File.expand_path('../libexec/workspace-host', __dir__)

class WorkspaceHostTest < Minitest::Test
  def test_lifecycle_journals_are_projected_from_the_shared_runtime_contract
    contract = JSON.parse(
      File.read(File.expand_path('../portal/internal/session/runtime-contract.json', __dir__))
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
    end
  end

  def test_core_source_catalog_contains_no_organization_extensions
    catalog = DevWorkspaceHost::ExtensionCatalog.load(File.expand_path('..', __dir__))

    assert_empty(catalog.commands)
    assert_empty(catalog.skills)
    assert_empty(catalog.cluster_providers)
  end

  def test_link_install_reconciles_extension_links_across_full_core_and_legacy_rollback
    Dir.mktmpdir('workspace-host-link-test') do |directory|
      full = make_package(directory, 'package-full')
      core = make_package(directory, 'package-core')
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
        skills: { skill_name => skill_source }
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
      command_link = File.join(directory, 'bin', command)
      skill_link = File.join(directory, '.codex/skills', skill_name)

      full_host.send(:install_links)
      assert_equal(command_source, File.readlink(command_link))
      assert_equal(skill_source, File.readlink(skill_link))

      core_host.send(:install_links)
      refute(File.exist?(command_link))
      refute(File.exist?(skill_link))

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

  def test_run_portal_applies_workspace_display_and_provider_configuration
    Dir.mktmpdir('workspace-host-portal-config-test') do |directory|
      root = make_workspace(directory, 'workspace')
      File.write(File.join(root, '.dev-workspace.json'), JSON.generate(
        'schema' => 2, 'displayLabel' => 'example organization development',
        'hostLabel' => 'build-host', 'sshHost' => 'build-host.int.example.cz',
        'developmentClusterProviders' => %w[alpha],
        'portal' => { 'hostname' => 'workspace.example.test', 'aliases' => [] }
      ))
      config = File.join(directory, 'config/registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'workspace.example.test', aliases: [], replace: false
      )
      package = make_package(directory, 'package')
      profile = File.join(directory, 'state/profile')
      FileUtils.mkdir_p(File.dirname(profile))
      File.symlink(package, profile)
      host = PortalArgumentHost.new(
        package_root: package, env: host_environment(directory, config:),
        out: StringIO.new, err: StringIO.new
      )
      host.send(:run_portal, ['example-workspace'])
      arguments = host.execution.drop(2)
      assert_equal('example organization development', arguments[arguments.index('--display-label') + 1])
      assert_equal('build-host', arguments[arguments.index('--host-label') + 1])
      assert_equal('build-host.int.example.cz', arguments[arguments.index('--ssh-host') + 1])
      assert_equal(
        File.join(directory, 'state'),
        arguments[arguments.index('--user-state-root') + 1]
      )
      provider_index = arguments.index('--cluster-provider')
      refute_nil(provider_index)
      assert_match(/\Aalpha=Alpha=/, arguments.fetch(provider_index + 1))
      refute(arguments.any? { |value| value.match?(/\Abeta=Beta=/) })
    end
  end

  def test_unregister_stops_instance_services_and_removes_the_registry_entry
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      runtime = File.join(directory, 'runtime')
      authority = File.join(runtime, 'example-workspace', 'authority')
      FileUtils.mkdir_p(authority)
      File.write(File.join(authority, 'old.json'), "old authority\n")
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = UnregisterHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => runtime
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(0, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_empty(DevWorkspaceHost::Registry.new(config).entries)
      disable = host.commands.find do |command|
        command[0, 4] == ['systemctl', '--user', 'disable', '--now']
      end
      refute_nil(disable)
      assert_includes(disable, 'workspace-portal@example-workspace.service')
      assert_includes(disable, 'workspace-codex@example-workspace.service')
      assert_includes(disable, 'workspace-tmux@example-workspace.service')
      refute(File.exist?(File.join(runtime, 'example-workspace')))

      replacement = make_workspace(directory, 'replacement')
      registered = DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root: replacement,
        hostname: 'replacement.workspace.example.test', aliases: [], replace: false
      )
      assert_equal(replacement, registered.fetch('root'))
    end
  end

  def test_unregister_recovers_registration_after_failed_first_switch
    Dir.mktmpdir('workspace-host-bootstrap-test') do |directory|
      original = make_workspace(directory, 'original')
      replacement = make_workspace(directory, 'replacement')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      environment = host_environment(directory, config:).merge(
        'DEV_WORKSPACES_STATE' => state
      )
      host = BootstrappingHost.new(
        env: environment, out: StringIO.new, err: StringIO.new
      )

      assert_equal(0, host.run('workspace-host', [
        'register', 'example-workspace', original,
        '--hostname', 'example-workspace.workspace.example.test'
      ]))
      assert_equal(1, host.run('workspace-host', [
        'switch', '--source', File.join(directory, 'missing-source')
      ]))
      refute(File.exist?(File.join(state, 'profile')))

      assert_equal(0, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_equal(0, host.run('workspace-host', [
        'register', 'example-workspace', replacement,
        '--hostname', 'replacement.workspace.example.test'
      ]))
      assert_equal(
        replacement,
        DevWorkspaceHost::Registry.new(config).find('example-workspace').fetch('root')
      )
    end
  end

  def test_unregister_restores_units_and_clients_after_a_partial_disable_failure
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = FailedUnregisterHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      refute_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      enable = host.commands.find do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end
      refute_nil(enable)
      assert_equal([:quiesced], host.restored)
      error_output = host.instance_variable_get(:@err).string
      primary = error_output.index('injected partial disable failure')
      units = error_output.index('failed to re-enable workspace services: injected unit recovery failure')
      clients = error_output.index('failed to restore terminal clients: injected terminal recovery failure')
      refute_nil(primary)
      refute_nil(units)
      refute_nil(clients)
      assert_operator(primary, :<, units)
      assert_operator(units, :<, clients)
    end
  end

  def test_unregister_reconciles_persisted_registration_after_a_post_commit_failure
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      runtime = File.join(directory, 'runtime')
      state = File.join(directory, 'state')
      entry = PostCommitFailureRegistry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      runtime_root = File.join(runtime, 'example-workspace')
      FileUtils.mkdir_p(runtime_root)
      host = PostCommitFailureUnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:, runtime:).merge(
          'DEV_WORKSPACES_STATE' => state
        )),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_equal(entry, DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      assert(File.directory?(runtime_root))
      assert(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_equal(
        1,
        host.commands.count do |command|
          command == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
        end
      )
      assert_equal([:quiesced], host.restored)
      assert_includes(
        host.instance_variable_get(:@err).string,
        'injected failure after the registry replacement'
      )
    end
  end

  def test_unregister_skips_dependent_recovery_when_prerequisites_cannot_be_restored
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      replacement = make_workspace(directory, 'replacement')
      config = File.join(directory, 'config', 'registry.json')
      runtime = File.join(directory, 'runtime')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(File.join(runtime, 'example-workspace'))
      host = FailedLateUnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:, runtime:)),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      refute(File.exist?(File.join(runtime, 'example-workspace')))
      assert_equal(1, Dir[File.join(runtime, '.retired-example-workspace-*')].length)
      refute(host.runtime_restore_attempted)
      refute(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_equal(
        1,
        host.commands.count do |command|
          command == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
        end
      )
      assert_nil(host.restored)
      error_output = host.instance_variable_get(:@err).string
      primary = error_output.index('injected router failure')
      registration = error_output.index(
        'failed to restore workspace registration: injected registration recovery failure'
      )
      refute_nil(primary)
      refute_nil(registration)
      assert_operator(primary, :<, registration)

      registered = DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root: replacement,
        hostname: 'replacement.workspace.example.test', aliases: [], replace: false
      )
      assert_equal(replacement, registered.fetch('root'))
      refute(File.exist?(File.join(runtime, 'example-workspace')))
      assert_equal(1, Dir[File.join(runtime, '.retired-example-workspace-*')].length)
    end
  end

  def test_unregister_keeps_old_runtime_quarantined_for_a_replacement_registration
    Dir.mktmpdir('workspace-host-test') do |directory|
      original = make_workspace(directory, 'original')
      replacement = make_workspace(directory, 'replacement')
      config = File.join(directory, 'config', 'registry.json')
      runtime = File.join(directory, 'runtime')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root: original,
        hostname: 'example-workspace.workspace.example.test', aliases: [], replace: false
      )
      runtime_root = File.join(runtime, 'example-workspace')
      FileUtils.mkdir_p(runtime_root)
      File.write(File.join(runtime_root, 'original-authority'), "original\n")
      host = ReplacementDuringUnregisterHost.new(
        replacement:,
        env: install_source_profile(host_environment(directory, config:, runtime:)),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      registered = DevWorkspaceHost::Registry.new(config).find('example-workspace')
      assert_equal(replacement, registered.fetch('root'))
      refute(File.exist?(runtime_root))
      retired = Dir[File.join(runtime, '.retired-example-workspace-*')]
      assert_equal(1, retired.length)
      assert(File.file?(File.join(retired.fetch(0), 'original-authority')))
      refute(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_equal(
        1,
        host.commands.count do |command|
          command == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
        end
      )
      assert_includes(
        host.instance_variable_get(:@err).string,
        'workspace registration changed during unregister recovery: example-workspace'
      )
    end
  end

  def test_unregister_keeps_runtime_quarantined_after_an_ambiguous_recovery_write
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      runtime = File.join(directory, 'runtime')
      entry = PostCommitFailureRegistry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      runtime_root = File.join(runtime, 'example-workspace')
      FileUtils.mkdir_p(runtime_root)
      File.write(File.join(runtime_root, 'original-authority'), "original\n")
      host = AmbiguousRecoveryWriteUnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:, runtime:)),
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_equal(entry, DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      refute(File.exist?(runtime_root))
      retired = Dir[File.join(runtime, '.retired-example-workspace-*')]
      assert_equal(1, retired.length)
      assert(File.file?(File.join(retired.fetch(0), 'original-authority')))
      refute(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_nil(host.restored)
      error_output = host.instance_variable_get(:@err).string
      primary = error_output.index('injected failure after the registry replacement')
      recovery = error_output.index('injected failure after the recovery replacement')
      refute_nil(primary)
      refute_nil(recovery)
      assert_operator(primary, :<, recovery)
    end
  end

  def test_unregister_does_not_compensate_after_committed_success_output_failure
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = UnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:)),
        out: FailedOutput.new,
        err: StringIO.new
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      assert_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      refute(host.commands.any? do |command|
        command[0, 4] == ['systemctl', '--user', 'enable', '--now']
      end)
      assert_includes(host.instance_variable_get(:@err).string, 'Broken pipe')
    end
  end

  def test_unregister_refuses_workspace_with_development_cluster_state
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      cluster = File.join(
        root, '.dev-clusters', 'alpha', 'clusters',
        '2026-09-07-active-cluster'
      )
      FileUtils.mkdir_p(cluster)
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      error_output = StringIO.new
      host = UnregisterHost.new(
        env: install_source_profile(host_environment(directory, config:)),
        out: StringIO.new,
        err: error_output
      )

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      refute_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      assert_includes(error_output.string, 'workspace unregister is blocked')
      assert_includes(error_output.string, 'example-workspace/alpha/2026-09-07-active-cluster')
      assert_empty(host.commands)
    end
  end

  def test_register_and_unregister_refuse_unfinished_lifecycle_operations
    Dir.mktmpdir('workspace-host-test') do |directory|
      config = File.join(directory, 'config', 'registry.json')
      root = make_workspace(directory, 'workspace')
      locks = File.join(root, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-07-pending.archive.json'), "{}\n")
      error_output = StringIO.new
      host = DevWorkspaceHost::Host.new(
        env: install_source_profile(host_environment(directory, config:)),
        out: StringIO.new, err: error_output
      )

      assert_equal(1, host.run('workspace-host', [
        'register', 'example-workspace', root,
        '--hostname', 'example-workspace.workspace.example.test'
      ]))
      assert_empty(DevWorkspaceHost::Registry.new(config).entries)
      assert_includes(error_output.string, 'workspace register is blocked')

      File.unlink(File.join(locks, '2026-09-07-pending.archive.json'))
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      File.write(File.join(locks, '2026-09-07-pending.revive.json'), "{}\n")
      error_output.truncate(0)
      error_output.rewind

      assert_equal(1, host.run('workspace-host', ['unregister', 'example-workspace']))
      refute_nil(DevWorkspaceHost::Registry.new(config).find('example-workspace'))
      assert_includes(error_output.string, 'workspace unregister is blocked')
    end
  end

  def test_quiesce_ignores_a_manifest_from_another_codex_runtime
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      runtime = File.join(directory, 'runtime')
      slug = '2026-09-06-old-runtime'
      manifest_dir = File.join(root, 'work', slug)
      FileUtils.mkdir_p(manifest_dir)
      File.write(
        File.join(manifest_dir, 'portal.yml'),
        portal_manifest('thread-old', '/run/old/app-server.sock', 'ready')
      )
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = QuiesceHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => runtime
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_empty(host.send(:quiesce_sessions))
      assert_empty(host.commands)
    end
  end

  def test_dev_session_dispatch_binds_the_registered_workspace_runtime
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      runtime = File.join(directory, 'runtime')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: install_source_profile({
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => runtime,
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        }),
        out: StringIO.new,
        err: StringIO.new
      )

      Dir.chdir(directory) do
        assert_equal(0, host.run('dev-session', ['--workspace', 'example-workspace', 'list']))
      end
      environment, command, arguments = host.captured
      assert_equal('example-workspace', environment.fetch('DEV_WORKSPACE_NAME'))
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, root)
      assert_includes(arguments, File.join(runtime, 'example-workspace', 'app-server.sock'))
      lock_index = arguments.index('--transition-lock')
      assert_operator(lock_index, :<, arguments.index('--'))
      assert_equal(File.join(state, 'transition.lock'), arguments.fetch(lock_index + 1))
      token_index = arguments.index('--expected-host-profile-token')
      assert_operator(token_index, :<, arguments.index('--'))
      assert_match(/\A[0-9a-f]{64}\z/, arguments.fetch(token_index + 1))
      assert_equal('list', arguments.last)
    end
  end

  def test_cluster_commands_hold_the_shared_host_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      profile = File.join(directory, 'profile')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      File.symlink(File.expand_path('..', __dir__), profile)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_PROFILE' => profile,
          'DEV_WORKSPACES_EXTENSION_CATALOG' => source_extension_catalog(directory)
        },
        out: StringIO.new,
        err: StringIO.new
      )
      result = Thread.new do
        host.run('alpha-devcluster', ['--workspace', 'example-workspace', 'status', '2026-09-06-test'])
      end
      sleep 0.05
      assert_nil(host.captured)
      owner.flock(File::LOCK_UN)

      assert_equal(0, result.value)
      refute_nil(host.captured)
    ensure
      owner&.close unless owner&.closed?
    end
  end

  def test_waiting_cluster_command_rejects_a_changed_package_generation
    %w[successful-switch compensated-switch].each do |scenario|
      Dir.mktmpdir("workspace-host-#{scenario}") do |directory|
        root = make_workspace(directory, 'workspace')
        config = File.join(directory, 'config', 'registry.json')
        state = File.join(directory, 'state')
        profile = File.join(directory, 'profile')
        expected = File.join(directory, 'old')
        selected = File.join(directory, 'new')
        FileUtils.mkdir_p(state)
        FileUtils.mkdir_p(expected)
        FileUtils.mkdir_p(selected)
        File.symlink(expected, profile)
        DevWorkspaceHost::Registry.new(config).register(
          name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
          aliases: [], replace: false
        )
        lock_path = File.join(state, 'transition.lock')
        owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
        owner.flock(File::LOCK_EX)
        error_output = StringIO.new
        host = GenerationClusterHost.new(
          package_root: expected,
          env: {
            'HOME' => directory,
            'PATH' => ENV.fetch('PATH'),
            'DEV_WORKSPACES_CONFIG' => config,
            'DEV_WORKSPACES_STATE' => state,
            'DEV_WORKSPACES_PROFILE' => profile
          },
          out: StringIO.new,
          err: error_output
        )
        result = Thread.new do
          host.run('alpha-devcluster', [
            '--workspace', 'example-workspace', 'status', '2026-09-06-test'
          ])
        end
        sleep 0.05
        File.unlink(profile)
        File.symlink(selected, profile)
        if scenario == 'compensated-switch'
          File.unlink(profile)
          File.symlink(expected, profile)
        end
        owner.flock(File::LOCK_UN)

        assert_equal(1, result.value)
        assert_nil(host.captured)
        assert_includes(error_output.string, 'package transition completed')
      ensure
        owner&.flock(File::LOCK_UN)
        owner&.close
        result&.join
      end
    end
  end

  def test_waiting_host_mutation_rechecks_successful_and_compensated_switches
    %w[successful compensated].each do |scenario|
      Dir.mktmpdir("workspace-host-mutation-#{scenario}") do |directory|
        state = File.join(directory, 'state')
        profile = File.join(directory, 'profile')
        expected = File.join(directory, 'expected')
        candidate = File.join(directory, 'candidate')
        FileUtils.mkdir_p([state, expected, candidate])
        File.symlink(expected, profile)
        lock_path = File.join(state, 'transition.lock')
        owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
        owner.flock(File::LOCK_EX)
        error_output = StringIO.new
        host = GenerationMutationHost.new(
          package_root: expected,
          env: {
            'HOME' => directory,
            'PATH' => ENV.fetch('PATH'),
            'DEV_WORKSPACES_STATE' => state,
            'DEV_WORKSPACES_PROFILE' => profile
          },
          out: StringIO.new,
          err: error_output
        )
        result = Thread.new { host.run('workspace-host', ['suspend']) }
        sleep 0.05
        assert_nil(host.captured)

        File.unlink(profile)
        File.symlink(candidate, profile)
        if scenario == 'compensated'
          File.unlink(profile)
          File.symlink(expected, profile)
        end
        owner.flock(File::LOCK_UN)

        assert_equal(1, result.value)
        assert_nil(host.captured)
        assert_includes(error_output.string, 'package transition completed')
      ensure
        owner&.flock(File::LOCK_UN)
        owner&.close
        result&.join
      end
    end
  end

  def test_cluster_command_ignores_spoofed_transition_and_lifecycle_environment
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      profile = File.join(directory, 'profile')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      File.symlink(File.expand_path('..', __dir__), profile)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_PROFILE' => profile,
          'DEV_WORKSPACES_EXTENSION_CATALOG' => source_extension_catalog(directory),
          'DEV_WORKSPACE_TRANSITION_HELD' => '1',
          'DEV_SESSION_LIFECYCLE_OPERATION' => 'archive'
        },
        out: StringIO.new,
        err: StringIO.new
      )

      result = Thread.new do
        host.run(
          'alpha-devcluster',
          ['--workspace', 'example-workspace', 'reset', '2026-09-06-test']
        )
      end
      sleep 0.05
      assert_nil(host.captured)
      owner.flock(File::LOCK_UN)

      assert_equal(0, result.value)
      refute_nil(host.captured)
      environment, = host.captured
      refute(environment.key?('DEV_WORKSPACE_TRANSITION_HELD'))
      refute(environment.key?('DEV_SESSION_LIFECYCLE_OPERATION'))
    ensure
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_public_delete_executes_the_cli_with_its_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => File.join(directory, 'state'),
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(
        0,
        host.run(
          'dev-session',
          ['--workspace', 'example-workspace', 'delete', '2026-09-06-test', '--as-is']
        )
      )
      environment, command, arguments = host.captured
      refute(environment.key?('DEV_WORKSPACE_TRANSITION_HELD'))
      assert_equal('dev-session', File.basename(command))
      lock_index = arguments.index('--transition-lock')
      assert_operator(lock_index, :<, arguments.index('--'))
      assert_equal(File.join(directory, 'state', 'transition.lock'), arguments.fetch(lock_index + 1))
      assert_equal(
        ['delete', '2026-09-06-test', '--as-is'],
        arguments.last(3)
      )
    end
  end

  def test_portal_lifecycle_reuses_a_verified_inherited_transition_lock
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(state)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      error_output = StringIO.new
      host = ClusterHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex,
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => owner.fileno.to_s
        },
        out: StringIO.new,
        err: error_output
      )

      assert_equal(0, host.run(
        'dev-session',
        ['--workspace', 'example-workspace', 'delete', '2026-09-06-test', '--as-is']
      ), error_output.string)
      environment, command, arguments = host.captured
      assert_equal(owner.fileno.to_s, environment.fetch('DEV_WORKSPACE_TRANSITION_LOCK_FD'))
      assert_equal('dev-session', File.basename(command))
      refute_includes(arguments, '--transition-lock')
    ensure
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_inherited_transition_lock_rejects_an_unlocked_descriptor_for_the_same_inode
    Dir.mktmpdir('workspace-host-test') do |directory|
      state = File.join(directory, 'state')
      FileUtils.mkdir_p(state)
      lock_path = File.join(state, 'transition.lock')
      owner = File.open(lock_path, File::RDWR | File::CREAT, 0o600)
      owner.flock(File::LOCK_EX)
      impostor = File.open(lock_path, File::RDWR)
      host = DevWorkspaceHost::Host.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => impostor.fileno.to_s
        },
        out: StringIO.new,
        err: StringIO.new
      )

      refute(host.send(:inherited_exclusive_transition_lock?))

      host = DevWorkspaceHost::Host.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACE_TRANSITION_LOCK_FD' => owner.fileno.to_s
        },
        out: StringIO.new,
        err: StringIO.new
      )
      assert(host.send(:inherited_exclusive_transition_lock?))
    ensure
      impostor&.close
      owner&.flock(File::LOCK_UN)
      owner&.close
    end
  end

  def test_public_archive_delegates_cluster_cleanup_to_the_session_command
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )

      assert_equal(
        0,
        host.run(
          'dev-session',
          ['--workspace', 'example-workspace', 'archive', '2026-09-06-test', '--as-is']
        )
      )
      _environment, command, arguments = host.captured
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, '--transition-lock')
      assert_equal(['archive', '2026-09-06-test', '--as-is'], arguments.last(3))
    end
  end

  def test_public_archive_passes_options_to_the_private_cli
    Dir.mktmpdir('workspace-host-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      state = File.join(directory, 'state')
      codex = File.join(directory, 'codex')
      File.write(codex, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
      File.chmod(0o755, codex)
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      host = CapturingHost.new(
        env: {
          'HOME' => directory,
          'PATH' => ENV.fetch('PATH'),
          'DEV_WORKSPACES_CONFIG' => config,
          'DEV_WORKSPACES_STATE' => state,
          'DEV_WORKSPACES_RUNTIME_DIR' => File.join(directory, 'runtime'),
          'DEV_WORKSPACES_SYSTEM_CODEX' => codex
        },
        out: StringIO.new,
        err: StringIO.new
      )
      argv = [
        '--workspace', 'example-workspace', 'archive', '--abandoned',
        '2026-09-06-Foo_bar', '--as-is'
      ]

      assert_equal(0, host.run('dev-session', argv))
      _environment, command, arguments = host.captured
      assert_equal('dev-session', File.basename(command))
      assert_includes(arguments, '--transition-lock')
      assert_equal(['archive', '--abandoned', '2026-09-06-Foo_bar', '--as-is'], arguments.last(4))
    end
  end

  def test_switch_retains_codex_with_the_profile_generation_and_restarts_as_one_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(1, host.send(:profile_generation))
      assert_equal(
        File.realpath(paths.fetch(:system_codex)),
        File.realpath(host.send(:generation_codex, 1))
      )
      assert_equal(File.realpath(paths.fetch(:system_codex)), File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:configured])
      assert_includes(host.events, [:consumers_restarted])
    end
  end

  def test_switch_rejects_a_nonactivating_profile_change
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))

      assert_equal(
        1,
        host.run('workspace-host', ['switch', '--source', paths.fetch(:source), '--no-start'])
      )
      refute(File.exist?(host.instance_variable_get(:@profile)))
    end
  end

  def test_switch_refuses_unfinished_session_lifecycle_operations
    DevWorkspaceHost::LIFECYCLE_JOURNALS.each do |journal|
      kind = journal.fetch('name')
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        workspace = host.send(:registry).entries.fetch(0).fetch('root')
        locks = File.join(workspace, 'worktrees', '.locks')
        FileUtils.mkdir_p(locks)
        File.write(File.join(locks, "2026-09-07-pending.#{kind}.json"), "{}\n")

        assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        refute(File.exist?(host.instance_variable_get(:@profile)))
      end
    end
  end

  def test_switch_refuses_an_unfinished_session_creation
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-pending.creation.json')
      File.write(
        journal,
        JSON.generate(
          'schema' => 1,
          'slug' => '2026-09-07-pending',
          'state' => 'creating',
          'tmux_identity' => 'a' * 64
        ) + "\n"
      )
      File.chmod(0o600, journal)

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, '(creation)')
    end
  end

  def test_switch_allows_a_completed_session_creation_journal
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-ready.creation.json')
      File.write(
        journal,
        JSON.generate(
          'schema' => 1,
          'slug' => '2026-09-07-ready',
          'state' => 'ready'
        ) + "\n"
      )
      File.chmod(0o600, journal)

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
    end
  end

  def test_switch_allows_a_legacy_journal_only_creation_for_safe_retry
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-legacy.creation.json')
      File.write(
        journal,
        JSON.generate(
          'schema' => 1,
          'slug' => '2026-09-07-legacy',
          'state' => 'creating'
        ) + "\n"
      )
      File.chmod(0o600, journal)

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
    end
  end

  def test_switch_refuses_an_unfinished_session_fork
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-07-fork.fork.json'), "{}\n")

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, '(fork)')
    end
  end

  def test_switch_refuses_an_unfinished_session_start
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-07-restart.start.json'), "{}\n")

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(host.instance_variable_get(:@err).string, '(start)')
    end
  end

  def test_switch_fails_closed_on_a_malformed_creation_journal
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      journal = File.join(locks, '2026-09-07-broken.creation.json')
      File.write(journal, "{\n")
      File.chmod(0o600, journal)

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'cannot inspect session operation state'
      )
    end
  end

  def test_switch_refuses_incompatible_cluster_helpers_while_cluster_state_exists
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'beta', 'clusters',
          '2026-09-07-active-cluster'
        )
      )
      incompatible = make_package(paths.fetch(:root), 'package-incompatible', cluster_contract: false)
      host.candidate = incompatible

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'target package has no compatible cluster-state contract'
      )
    end
  end

  def test_rollback_refuses_a_cluster_contract_with_the_old_tracking_limit
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      old_contract = File.join(
        host.send(:profile_generation_path, 1),
        'share/workspace-portal/runtime-contract.json'
      )
      File.write(old_contract, JSON.generate(
        'developmentClusterStateSchema' => 1,
        'developmentClusterTransitionPolicy' =>
          DevWorkspaceHost::RUNTIME_CONTRACT.fetch(
            'developmentClusterTransitionPolicy'
          ),
        'trackingMaxBytes' => 1024 * 1024
      ))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(File.join(
        workspace, '.dev-clusters', 'beta', 'clusters',
        '2026-09-07-active-cluster'
      ))

      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(2, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'target package has no compatible cluster-state contract'
      )
    end
  end

  def test_rollback_refuses_a_package_without_the_identity_authority_contract
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      old_contract = File.join(
        host.send(:profile_generation_path, 1),
        'share/workspace-portal/runtime-contract.json'
      )
      contract = JSON.parse(File.read(old_contract))
      contract.delete('runtimeAuthorityIdentityPolicy')
      File.write(old_contract, JSON.generate(contract))
      runtime = host.send(:instance_runtime, host.send(:registry).entries.fetch(0))
      FileUtils.mkdir_p(runtime.fetch(:authority), mode: 0o700)
      File.write(
        File.join(runtime.fetch(:authority), '2026-09-07-active.json'),
        JSON.generate('tmux_identity' => 'a' * 64),
        mode: 'w'
      )
      File.chmod(0o600, File.join(runtime.fetch(:authority), '2026-09-07-active.json'))

      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(2, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'target package cannot validate their tmux identities'
      )
    end
  end

  def test_switch_accepts_a_stricter_cluster_transition_policy
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      cluster = File.join(
        workspace, '.dev-clusters', 'beta', 'clusters',
        '2026-09-07-active-cluster'
      )
      FileUtils.mkdir_p(cluster)
      File.write(File.join(cluster, 'socket-dir'), "/tmp/workspace-scoped-socket\n")
      host.candidate = make_package(
        paths.fetch(:root),
        'package-stricter-policy',
        transition_policy: DevWorkspaceHost::RUNTIME_CONTRACT.fetch(
          'developmentClusterTransitionPolicy'
        ) + 1
      )

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
    end
  end

  def test_rollback_refuses_the_permissive_cluster_transition_policy
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      old_contract = File.join(
        host.send(:profile_generation_path, 1),
        'share/workspace-portal/runtime-contract.json'
      )
      File.write(old_contract, JSON.generate(
        'developmentClusterStateSchema' =>
          DevWorkspaceHost::RUNTIME_CONTRACT.fetch('developmentClusterStateSchema'),
        'developmentClusterTransitionPolicy' => 1,
        'trackingMaxBytes' =>
          DevWorkspaceHost::RUNTIME_CONTRACT.fetch('trackingMaxBytes')
      ))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      cluster = File.join(
        workspace, '.dev-clusters', 'alpha', 'clusters',
        '2026-09-07-active-cluster'
      )
      FileUtils.mkdir_p(cluster)
      File.write(File.join(cluster, 'socket-dir'), "/tmp/workspace-scoped-socket\n")

      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(2, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'target package has no compatible cluster-state contract'
      )
    end
  end

  def test_switch_refuses_unadoptable_precontract_cluster_state
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'alpha', 'clusters',
          '2026-09-07-active-cluster'
        )
      )

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'pre-contract cluster cannot be adopted'
      )
    end
  end

  def test_switch_refuses_the_password_cluster_without_recorded_socket_identity
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'alpha', 'clusters',
          '2026-08-18-alpha-password-reset'
        )
      )

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.instance_variable_get(:@profile)))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'pre-contract cluster cannot be adopted'
      )
    end
  end

  def test_switch_uses_the_installed_predecessor_contract_not_the_invoking_package
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      host.candidate = make_package(paths.fetch(:root), 'package-precontract', cluster_contract: false)
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      FileUtils.mkdir_p(
        File.join(
          workspace, '.dev-clusters', 'alpha', 'clusters',
          '2026-09-07-unadoptable'
        )
      )
      host.candidate = make_package(paths.fetch(:root), 'package-contract')

      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(1, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'pre-contract cluster cannot be adopted'
      )
    end
  end

  def test_switch_validates_cluster_state_owned_by_a_contract_predecessor
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      cluster = File.join(
        workspace, '.dev-clusters', 'alpha', 'clusters',
        '2026-09-07-contract-state'
      )
      FileUtils.mkdir_p(cluster)
      File.write(File.join(cluster, 'socket-dir'), "/tmp/workspace-scoped-socket\n")
      host.candidate = make_package(paths.fetch(:root), 'package-next-contract')

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(2, host.send(:profile_generation))
    end
  end

  def test_candidate_activation_rechecks_precontract_cluster_adoption
    Dir.mktmpdir('workspace-host-activation-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      FileUtils.mkdir_p(
        File.join(
          root, '.dev-clusters', 'beta', 'clusters',
          '2026-09-07-precontract'
        )
      )
      package = make_package(directory, 'candidate')
      error_output = StringIO.new
      environment = host_environment(directory, config:).merge(
        'DEV_WORKSPACE_ACTIVATION' => '1'
      )
      host = ActivationGuardHost.new(
        package:, env: environment, out: StringIO.new, err: error_output
      )

      assert_equal(1, host.run('workspace-host', ['_activate']))
      refute(host.configured)
      assert_includes(error_output.string, 'pre-contract cluster cannot be adopted')
    end
  end

  def test_busy_switch_keeps_the_old_codex_and_retries_only_the_pending_update
    with_transition_host(busy: ['example-workspace/active']) do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))

      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(File.realpath(paths.fetch(:old_codex)), File.realpath(host.send(:active_codex)))
      assert(host.send(:pending_codex_update?, File.realpath(paths.fetch(:system_codex))))
      refute_includes(host.events, [:consumers_restarted])

      host.busy = []
      assert_equal(0, host.run('workspace-host', ['reconcile-codex', '--pending-only']))
      assert_equal(File.realpath(paths.fetch(:system_codex)), File.realpath(host.send(:active_codex)))
      refute(host.send(:pending_codex_update?))
      assert_includes(host.events, [:consumers_restarted])

      checks = host.events.count { |event| event.first == :codex_checked }
      assert_equal(0, host.run('workspace-host', ['reconcile-codex', '--pending-only']))
      assert_equal(checks, host.events.count { |event| event.first == :codex_checked })
    end
  end

  def test_rollback_selects_the_retained_codex_for_the_previous_profile_generation
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      second_package = make_package(paths.fetch(:root), 'package-two')
      second_codex = make_codex(paths.fetch(:root), 'codex-two')
      host.candidate = second_package
      host.instance_variable_set(:@system_codex, second_codex)
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(2, host.send(:profile_generation))

      assert_equal(0, host.run('workspace-host', ['rollback']))
      assert_equal(1, host.send(:profile_generation))
      assert_equal(File.realpath(paths.fetch(:system_codex)), File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:router_restarted])
      assert_includes(host.events, [:consumers_restarted])
      assert_includes(
        host.events,
        [:sessions_restored, File.realpath(host.send(:profile_generation_path, 1))]
      )
    end
  end

  def test_rollback_keeps_the_selected_generation_when_terminal_restoration_fails
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      second_package = make_package(paths.fetch(:root), 'package-two')
      host.candidate = second_package
      host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.fail_restore = true

      assert_equal(1, host.run('workspace-host', ['rollback']))
      assert_equal(1, host.send(:profile_generation))
      assert_includes(
        host.instance_variable_get(:@err).string,
        'workspace package rollback completed, but terminal clients need dev-session sync'
      )
    end
  end

  def test_terminal_restoration_attempts_every_session_with_one_package_generation
    Dir.mktmpdir('workspace-host-restoration-test') do |directory|
      first = {
        'name' => 'first', 'root' => make_workspace(directory, 'first'),
        'hostname' => 'first.workspace.example.test'
      }
      second = {
        'name' => 'second', 'root' => make_workspace(directory, 'second'),
        'hostname' => 'second.workspace.example.test'
      }
      config = File.join(directory, 'config', 'registry.json')
      target = make_package(directory, 'target-package')
      environment = host_environment(directory, config:)
      profile = environment.fetch('DEV_WORKSPACES_PROFILE')
      FileUtils.mkdir_p(File.dirname(profile))
      File.symlink(target, profile)
      host = RestorationHost.new(
        fail_slug: 'broken', env: environment,
        out: StringIO.new, err: StringIO.new
      )

      error = assert_raises(DevWorkspaceHost::Error) do
        host.send(
          :restore_quiesced_sessions,
          [[first, 'broken'], [second, 'restored']],
          package: target
        )
      end

      assert_includes(error.message, 'first/broken: injected sync failure')
      assert_equal(2, host.invocations.length)
      host.invocations.each do |_environment, command, arguments|
        assert_equal(File.join(target, 'libexec/workspace-portal/dev-session'), command)
        assert_equal(target, arguments.fetch(arguments.index('--expected-host-generation') + 1))
        assert_equal(
          File.join(target, 'bin/workspace-portal'),
          arguments.fetch(arguments.index('--portal-command') + 1)
        )
        configured = arguments.each_index.filter_map do |index|
          arguments[index + 1] if arguments[index] == '--cluster-provider'
        end
        assert_equal(
          [
            "alpha=#{File.join(target, 'libexec/workspace-portal/alpha-devcluster')}",
            "beta=#{File.join(target, 'libexec/workspace-portal/beta-devcluster')}"
          ],
          configured
        )
        assert_equal(
          DevWorkspaceProfileIdentity.token(profile),
          arguments.fetch(arguments.index('--expected-host-profile-token') + 1)
        )
      end
    end
  end

  def test_terminal_restoration_preserves_the_primary_failure
    Dir.mktmpdir('workspace-host-restoration-error-test') do |directory|
      entry = {
        'name' => 'first', 'root' => make_workspace(directory, 'first'),
        'hostname' => 'first.workspace.example.test'
      }
      config = File.join(directory, 'config', 'registry.json')
      target = make_package(directory, 'target-package')
      host = RestorationHost.new(
        fail_slug: 'broken', env: host_environment(directory, config:),
        out: StringIO.new, err: StringIO.new
      )
      primary = DevWorkspaceHost::Error.new('injected primary failure')

      error = assert_raises(DevWorkspaceHost::Error) do
        host.send(
          :reraise_after_terminal_restoration,
          primary,
          [[entry, 'broken']],
          package: target
        )
      end

      assert_equal(
        'injected primary failure; unable to restore terminal clients: ' \
        'first/broken: injected sync failure',
        error.message
      )
    end
  end

  def test_rollback_refuses_unfinished_session_lifecycle_operations
    DevWorkspaceHost::LIFECYCLE_JOURNALS.each do |journal|
      kind = journal.fetch('name')
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        host.candidate = make_package(paths.fetch(:root), 'package-two')
        host.instance_variable_set(:@system_codex, make_codex(paths.fetch(:root), 'codex-two'))
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        workspace = host.send(:registry).entries.fetch(0).fetch('root')
        locks = File.join(workspace, 'worktrees', '.locks')
        FileUtils.mkdir_p(locks)
        File.write(File.join(locks, "2026-09-07-pending.#{kind}.json"), "{}\n")

        assert_equal(1, host.run('workspace-host', ['rollback']))
        assert_equal(2, host.send(:profile_generation))
      end
    end
  end

  def test_rollback_refuses_canonical_and_legacy_development_cluster_state
    [
      ['beta', '2026-09-07-canonical-cluster'],
      ['alpha', '2026-08-18-alpha-password-reset']
    ].each do |kind, slug|
      with_transition_host do |host, paths|
        host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        File.unlink(
          File.join(
            host.send(:profile_generation_path, 1),
            'share/workspace-portal/runtime-contract.json'
          )
        )
        host.candidate = make_package(paths.fetch(:root), 'package-two')
        host.instance_variable_set(
          :@system_codex,
          make_codex(paths.fetch(:root), 'codex-two')
        )
        assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
        workspace = host.send(:registry).entries.fetch(0).fetch('root')
        FileUtils.mkdir_p(File.join(workspace, '.dev-clusters', kind, 'clusters', slug))

        assert_equal(1, host.run('workspace-host', ['rollback']))
        assert_equal(2, host.send(:profile_generation))
        error_output = host.instance_variable_get(:@err).string
        assert_includes(error_output, "example-workspace/#{kind}/#{slug}")
        assert_includes(error_output, 'reset these clusters first')
      end
    end
  end

  def test_failed_switch_restores_the_previous_profile_and_codex_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      first_codex = File.realpath(host.send(:active_codex))

      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.fail_activation = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(1, host.send(:profile_generation))
      assert_equal(first_codex, File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:profile_selected, 1])
      assert_includes(host.events, [:consumers_restarted])
    end
  end

  def test_failed_link_install_restores_the_previous_profile_and_codex_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      first_codex = File.realpath(host.send(:active_codex))

      host.candidate = make_package(paths.fetch(:root), 'package-two')
      host.fail_links = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      assert_equal(1, host.send(:profile_generation))
      assert_equal(first_codex, File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:profile_selected, 1])
      assert_includes(host.events, [:consumers_restarted])
    end
  end

  def test_failed_switch_generation_is_not_eligible_for_later_rollback
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))

      host.candidate = make_package(paths.fetch(:root), 'failed-package')
      host.fail_activation = true
      assert_equal(1, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      refute(File.exist?(host.send(:profile_generation_path, 2)))
      assert_nil(host.send(:generation_codex, 2))

      host.candidate = make_package(paths.fetch(:root), 'working-package')
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      assert_equal(2, host.send(:profile_generation))

      assert_equal(0, host.run('workspace-host', ['rollback']))
      assert_equal(1, host.send(:profile_generation))
      assert_equal(File.realpath(paths.fetch(:system_codex)), File.realpath(host.send(:active_codex)))
    end
  end

  def test_failed_codex_adoption_restores_the_previous_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      previous = File.realpath(host.send(:active_codex))
      replacement = make_codex(paths.fetch(:root), 'codex-replacement')
      host.instance_variable_set(:@system_codex, replacement)
      host.fail_restart = true

      assert_equal(1, host.run('workspace-host', ['reconcile-codex']))

      assert_equal(previous, File.realpath(host.send(:active_codex)))
      assert_equal(previous, File.realpath(host.send(:generation_codex, 1)))
      assert_operator(host.events.count { |event| event == [:consumers_restarted] }, :>=, 2)
    end
  end

  def test_failed_rollback_restores_the_original_generation_pair
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.candidate = make_package(paths.fetch(:root), 'package-two')
      second_codex = make_codex(paths.fetch(:root), 'codex-two')
      host.instance_variable_set(:@system_codex, second_codex)
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.fail_restart = true
      host.fail_restore = true

      assert_equal(1, host.run('workspace-host', ['rollback']))

      assert_equal(2, host.send(:profile_generation))
      assert_equal(File.realpath(second_codex), File.realpath(host.send(:active_codex)))
      assert_includes(host.events, [:profile_selected, 2])
      assert_includes(
        host.events,
        [:sessions_restored, File.realpath(host.send(:profile_generation_path, 2))]
      )
      error_output = host.instance_variable_get(:@err).string
      assert_includes(error_output, 'injected consumer restart failure')
      assert_includes(error_output, 'injected restoration failure')
    end
  end

  def test_suspend_quiesces_sessions_before_disabling_the_user_services
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      host.events.clear

      assert_equal(0, host.run('workspace-host', ['suspend']))

      assert_equal(:sessions_quiesced, host.events.fetch(0).first)
      disable = host.events.find do |event|
        event[0, 4] == [:command, 'systemctl', '--user', 'disable']
      end
      refute_nil(disable)
      assert_includes(disable, '--now')
      assert_includes(disable, 'workspace-codex@example-workspace.service')
      assert_includes(disable, 'workspace-tmux@example-workspace.service')
    end
  end

  def test_suspend_and_codex_reconciliation_refuse_unfinished_lifecycle_operations
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      workspace = host.send(:registry).entries.fetch(0).fetch('root')
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      File.write(File.join(locks, '2026-09-07-pending.removal.json'), "{}\n")
      host.events.clear

      assert_equal(1, host.run('workspace-host', ['suspend']))
      assert_equal(1, host.run('workspace-host', ['reconcile-codex']))
      refute(host.events.any? { |event| event.first == :sessions_quiesced })
      refute_includes(host.events, [:consumers_restarted])
    end
  end

  private

  class CapturingHost < DevWorkspaceHost::Host
    attr_reader :captured

    private

    def exec_with_workspace(entry, command, *arguments)
      @captured = [@env.to_h.merge('DEV_WORKSPACE_NAME' => entry.fetch('name')), command, arguments]
    end
  end

  class ClusterHost < DevWorkspaceHost::Host
    attr_reader :captured

    private

    def system_env!(environment, command, *arguments)
      @captured = [environment, command, arguments]
    end
  end

  class GenerationClusterHost < ClusterHost
    def initialize(package_root:, **options)
      @test_package_root = package_root
      super(**options)
    end

    private

    def package_root
      @test_package_root
    end
  end

  class GenerationMutationHost < DevWorkspaceHost::Host
    attr_reader :captured

    def initialize(package_root:, **options)
      @test_package_root = package_root
      super(**options)
    end

    private

    def package_root
      @test_package_root
    end

    def suspend(argv)
      raise DevWorkspaceHost::Error, 'unexpected arguments' unless argv.empty?

      @captured = :suspended
    end
  end

  class CompatibilityLinkHost < DevWorkspaceHost::Host
    def initialize(package_root:, **options)
      @test_package_root = package_root
      super(**options)
    end

    private

    def package_root
      @test_package_root
    end
  end

  class PortalArgumentHost < CompatibilityLinkHost
    attr_reader :execution

    private

    def exec(*arguments)
      @execution = arguments
    end

    def active_codex
      '/nix/store/codex/bin/codex'
    end

    def codex_version(_command)
      '0.152.1'
    end

    def find_command(name)
      "/usr/bin/#{name}"
    end
  end

  class FinalizeHost < DevWorkspaceHost::Host
    attr_reader :commands
    attr_accessor :cluster_active

    def initialize(**options)
      super
      @commands = []
      @cluster_active = true
    end

    private

    def capture_env!(_environment, command, *arguments)
      if File.basename(command).include?('dev-session')
        separator = arguments.index('--')
        request = arguments.drop(separator + 2)
        slug = request.find { |argument| !argument.start_with?('-') }
        "https://example-workspace.workspace.example.test/#{slug}/\n"
      else
        found = cluster_active && File.basename(command) == 'alpha-devcluster'
        JSON.generate('schema' => 1, 'found' => found)
      end
    end

    def system_env!(_environment, command, *arguments)
      @commands << [command, arguments]
    end
  end

  class UnregisterHost < DevWorkspaceHost::Host
    attr_reader :commands

    def initialize(**options)
      super
      @commands = []
    end

    private

    def system!(*argv)
      @commands << argv
    end

    def quiesce_sessions(_entries)
      []
    end
  end

  class BootstrappingHost < UnregisterHost
    private

    def capture!(*argv)
      if argv[0, 2] == ['nix', 'build']
        raise DevWorkspaceHost::Error, 'injected first switch failure'
      end

      super
    end
  end

  class QuiesceHost < DevWorkspaceHost::Host
    attr_reader :commands

    def initialize(**options)
      super
      @commands = []
    end

    private

    def capture_env!(environment, command, *arguments)
      @commands << [environment, command, arguments]
      "quiesced terminal: #{arguments[-2]}\n"
    end
  end

  class FailedUnregisterHost < DevWorkspaceHost::Host
    attr_reader :commands, :restored

    def initialize(**options)
      super
      @commands = []
    end

    private

    def quiesce_sessions(_entries)
      [:quiesced]
    end

    def restore_quiesced_sessions(sessions, **)
      @restored = sessions
      raise DevWorkspaceHost::Error, 'injected terminal recovery failure'
    end

    def system!(*argv)
      @commands << argv
      if argv[0, 4] == ['systemctl', '--user', 'disable', '--now']
        raise DevWorkspaceHost::Error, 'injected partial disable failure'
      elsif argv[0, 4] == ['systemctl', '--user', 'enable', '--now']
        raise DevWorkspaceHost::Error, 'injected unit recovery failure'
      end
    end
  end

  class PostCommitFailureRegistry < DevWorkspaceHost::Registry
    def unregister(name)
      existing = find(name)
      raise DevWorkspaceHost::Error, "workspace is not registered: #{name}" unless existing

      # Model a failure after the replacement became visible but before the
      # Registry instance updated its cached entries.
      send(:write, entries.reject { |entry| entry.fetch('name') == name })
      raise DevWorkspaceHost::Error,
            'injected failure after the registry replacement'
    end
  end

  class PostCommitFailureUnregisterHost < UnregisterHost
    attr_reader :restored

    private

    def registry
      @injected_registry ||= PostCommitFailureRegistry.new(@config)
    end

    def quiesce_sessions(_entries)
      [:quiesced]
    end

    def restore_quiesced_sessions(sessions, **)
      @restored = sessions
    end
  end

  class FailedLateUnregisterHost < DevWorkspaceHost::Host
    attr_reader :commands, :restored, :runtime_restore_attempted

    def initialize(**options)
      super
      @commands = []
    end

    private

    def quiesce_sessions(_entries)
      [:quiesced]
    end

    def system!(*argv)
      @commands << argv
      if argv == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
        raise DevWorkspaceHost::Error, 'injected router failure'
      end
    end

    def restore_workspace_registration(_registry_path, _entry)
      {
        state: :absent,
        error: DevWorkspaceHost::Error.new('injected registration recovery failure')
      }
    end

    def restore_instance_runtime(_retired, _entry)
      @runtime_restore_attempted = true
      raise DevWorkspaceHost::Error, 'injected runtime recovery failure'
    end

    def restore_quiesced_sessions(sessions, **)
      @restored = sessions
    end
  end

  class AmbiguousRecoveryWriteRegistry < DevWorkspaceHost::Registry
    def register(**options)
      super
      raise DevWorkspaceHost::Error,
            'injected failure after the recovery replacement'
    end
  end

  class AmbiguousRecoveryWriteUnregisterHost < PostCommitFailureUnregisterHost
    private

    def open_registry(path)
      @registry_open_count ||= 0
      @registry_open_count += 1
      if @registry_open_count == 1
        AmbiguousRecoveryWriteRegistry.new(path)
      else
        DevWorkspaceHost::Registry.new(path)
      end
    end
  end

  class ReplacementDuringUnregisterHost < UnregisterHost
    def initialize(replacement:, **options)
      super(**options)
      @replacement = replacement
      @router_failed = false
    end

    private

    def system!(*argv)
      @commands << argv
      return unless argv == ['systemctl', '--user', 'try-restart', 'workspace-router.service']
      return if @router_failed

      @router_failed = true
      DevWorkspaceHost::Registry.new(@config).register(
        name: 'example-workspace', root: @replacement,
        hostname: 'replacement.workspace.example.test', aliases: [], replace: false
      )
      raise DevWorkspaceHost::Error, 'injected router failure after replacement'
    end
  end

  class FailedOutput < StringIO
    def puts(*)
      raise Errno::EPIPE
    end
  end

  class TransitionHost < DevWorkspaceHost::Host
    attr_accessor :busy, :candidate, :fail_activation, :fail_links, :fail_restart, :fail_restore
    attr_reader :events

    def initialize(candidate:, busy:, **options)
      super(**options)
      @candidate = candidate
      @busy = busy
      @events = []
    end

    private

    # A real stable command invocation comes from the selected profile. Tests
    # reuse this host object across invocations, so follow the selected profile
    # to model the package that a new process would execute.
    def package_root
      File.symlink?(@profile) ? File.realpath(@profile) : super
    end

    def capture!(*argv)
      return "#{candidate}\n" if argv[0, 2] == ['nix', 'build']
      super
    end

    def system!(*argv)
      if argv[0, 3] == ['nix-env', '--profile', @profile]
        if argv[3] == '--set'
          generations = Dir["#{@profile}-*-link"].filter_map do |path|
            File.basename(path)[/-(\d+)-link\z/, 1]&.to_i
          end
          current = generations.max.to_i + 1
          generation = profile_generation_path(current)
          File.symlink(argv.fetch(4), generation)
          File.unlink(@profile) if File.symlink?(@profile)
          File.symlink(File.basename(generation), @profile)
          @events << [:profile_set, current]
          return
        elsif argv[3] == '--rollback'
          target = previous_profile_generation
          File.unlink(@profile)
          File.symlink(File.basename(profile_generation_path(target)), @profile)
          @events << [:profile_rolled_back, target]
          return
        elsif argv[3] == '--switch-generation'
          target = Integer(argv.fetch(4), 10)
          File.unlink(@profile)
          File.symlink(File.basename(profile_generation_path(target)), @profile)
          @events << [:profile_selected, target]
          return
        elsif argv[3] == '--delete-generations'
          target = Integer(argv.fetch(4), 10)
          File.unlink(profile_generation_path(target))
          @events << [:profile_deleted, target]
          return
        end
      end
      if argv[0, 4] == ['systemctl', '--user', 'restart', 'workspace-router.service']
        @events << [:router_restarted]
      else
        @events << [:command, *argv]
      end
    end

    def root_codex(command, root)
      package = File.dirname(File.dirname(File.realpath(command)))
      FileUtils.mkdir_p(File.dirname(root))
      File.unlink(root) if File.symlink?(root)
      File.symlink(package, root)
    end

    def install_links(**)
      @events << [:links_installed]
      if fail_links
        self.fail_links = false
        raise DevWorkspaceHost::Error, 'injected link installation failure'
      end
    end

    def configure_user_services
      @events << [:configured]
    end

    def activate_installed(_command)
      configure_user_services
      if fail_activation
        self.fail_activation = false
        raise DevWorkspaceHost::Error, 'injected activation failure'
      end
      reconcile_codex_update(defer_busy: true)
    end

    def quiesce_sessions
      raise DevWorkspaceHost::Error, busy.join(', ') unless busy.empty?
      @events << [:sessions_quiesced]
      []
    end

    def wait_for_codex_sockets
      @events << [:codex_ready]
    end

    def restore_quiesced_sessions(_sessions, package: nil)
      @events << [:sessions_restored, package && File.realpath(package)]
      if fail_restore
        self.fail_restore = false
        raise DevWorkspaceHost::Error, 'injected restoration failure'
      end
    end

    def check_codex(command)
      @events << [:codex_checked, File.realpath(command)]
    end

    def busy_codex_sessions
      busy
    end

    def restart_codex_consumers
      @events << [:consumers_restarted]
      if fail_restart
        self.fail_restart = false
        raise DevWorkspaceHost::Error, 'injected consumer restart failure'
      end
    end
  end

  class RestorationHost < DevWorkspaceHost::Host
    attr_reader :invocations

    def initialize(fail_slug:, **options)
      super(**options)
      @fail_slug = fail_slug
      @invocations = []
    end

    private

    def codex_version(_command)
      '0.153.4'
    end

    def system_env!(environment, command, *arguments)
      @invocations << [environment, command, arguments]
      slug = arguments.fetch(-2)
      raise DevWorkspaceHost::Error, 'injected sync failure' if slug == @fail_slug
    end
  end

  class ActivationGuardHost < DevWorkspaceHost::Host
    attr_reader :configured

    def initialize(package:, **options)
      super(**options)
      @package = package
      @configured = false
    end

    private

    def package_root
      @package
    end

    def configure_user_services
      @configured = true
    end

    def reconcile_codex_update(defer_busy:)
      defer_busy
    end
  end

  def registry_at(directory)
    DevWorkspaceHost::Registry.new(File.join(directory, 'config', 'registry.json'))
  end

  def make_workspace(parent, name)
    root = File.join(parent, name)
    %w[repos work worktrees].each { |item| FileUtils.mkdir_p(File.join(root, item)) }
    root
  end

  def make_codex(parent, name)
    package = File.join(parent, name)
    command = File.join(package, 'bin', 'codex')
    FileUtils.mkdir_p(File.dirname(command))
    File.write(command, "#!/bin/sh\necho 'codex-cli 1.2.3'\n")
    File.chmod(0o755, command)
    command
  end

  def install_source_profile(environment)
    profile = environment.fetch(
      'DEV_WORKSPACES_PROFILE',
      File.join(environment.fetch('DEV_WORKSPACES_STATE'), 'profile')
    )
    FileUtils.mkdir_p(File.dirname(profile))
    File.symlink(File.expand_path('..', __dir__), profile)
    catalog = source_extension_catalog(File.dirname(profile))
    environment.merge(
      'DEV_WORKSPACES_PROFILE' => profile,
      'DEV_WORKSPACES_EXTENSION_CATALOG' => catalog
    )
  end

  def source_extension_catalog(directory)
    catalog = File.join(directory, 'source-extensions.json')
    File.write(catalog, JSON.generate(
      'schema' => 1,
      'commands' => [],
      'skills' => [],
      'clusterProviders' => %w[alpha beta].map do |provider_id|
        { 'id' => provider_id, 'label' => provider_id.capitalize, 'command' => '/bin/true' }
      end
    ))
    catalog
  end

  def make_package(
    parent,
    name,
    cluster_contract: true,
    tracking_max: DevWorkspaceHost::RUNTIME_CONTRACT.fetch('trackingMaxBytes'),
    transition_policy: DevWorkspaceHost::RUNTIME_CONTRACT.fetch(
      'developmentClusterTransitionPolicy'
    ),
    authority_policy: DevWorkspaceHost::RUNTIME_CONTRACT.fetch(
      'runtimeAuthorityIdentityPolicy'
    )
  )
    package = File.join(parent, name)
    command = File.join(package, 'bin', 'workspace-host')
    FileUtils.mkdir_p(File.dirname(command))
    File.write(command, "#!/bin/sh\nexit 0\n")
    File.chmod(0o755, command)
    if cluster_contract
      contract = File.join(package, 'share/workspace-portal/runtime-contract.json')
      FileUtils.mkdir_p(File.dirname(contract))
      contract_data = {
        'developmentClusterStateSchema' => DevWorkspaceHost::RUNTIME_CONTRACT.fetch(
          'developmentClusterStateSchema'
        ),
        'developmentClusterTransitionPolicy' => transition_policy,
        'trackingMaxBytes' => tracking_max
      }
      contract_data['runtimeAuthorityIdentityPolicy'] = authority_policy if authority_policy
      File.write(contract, JSON.generate(contract_data))
      %w[alpha beta].each do |kind|
        helper = File.join(package, 'libexec/workspace-portal', "#{kind}-devcluster")
        FileUtils.mkdir_p(File.dirname(helper))
        File.write(helper, <<~SH)
          #!/bin/sh
          if [ "$1" = transition-adopt ] &&
             [ -f "$DEVCLUSTER_WORKSPACE/.dev-clusters/#{kind}/clusters/$2/socket-dir" ]; then
            exit 0
          fi
          echo 'pre-contract cluster cannot be adopted' >&2
          exit 1
        SH
        File.chmod(0o755, helper)
      end
    end
    %w[alpha beta].each do |provider_id|
      public_helper = File.join(package, 'bin', "#{provider_id}-devcluster")
      File.write(public_helper, "#!/bin/sh\nexit 0\n")
      File.chmod(0o755, public_helper)
    end
    write_extension_catalog(package)
    package
  end

  def write_extension_catalog(
    package,
    commands: { },
    skills: { }
  )
    catalog = File.join(package, 'share/dev-workspace/extensions.json')
    FileUtils.mkdir_p(File.dirname(catalog))
    providers = %w[alpha beta].map do |provider_id|
      {
        'id' => provider_id,
        'label' => provider_id.capitalize,
        'command' => File.join(
          package, 'libexec/workspace-portal', "#{provider_id}-devcluster"
        )
      }
    end
    File.write(catalog, JSON.generate(
      'schema' => 1,
      'commands' => commands.map { |name, path| { 'name' => name, 'path' => path } },
      'skills' => skills.map { |name, path| { 'name' => name, 'path' => path } },
      'clusterProviders' => providers
    ))
  end

  def host_environment(directory, config:, runtime: File.join(directory, 'runtime'), system_codex: nil)
    system_codex ||= make_codex(directory, 'codex-system')
    {
      'HOME' => directory,
      'PATH' => ENV.fetch('PATH'),
      'DEV_WORKSPACES_CONFIG' => config,
      'DEV_WORKSPACES_STATE' => File.join(directory, 'state'),
      'DEV_WORKSPACES_RUNTIME_DIR' => runtime,
      'DEV_WORKSPACES_PROFILE' => File.join(directory, 'state', 'profile'),
      'DEV_WORKSPACES_SYSTEM_CODEX' => system_codex
    }
  end

  def with_transition_host(busy: [])
    Dir.mktmpdir('workspace-host-transition-test') do |directory|
      root = make_workspace(directory, 'workspace')
      config = File.join(directory, 'config', 'registry.json')
      DevWorkspaceHost::Registry.new(config).register(
        name: 'example-workspace', root:, hostname: 'example-workspace.workspace.example.test',
        aliases: [], replace: false
      )
      source = File.join(directory, 'source')
      FileUtils.mkdir_p(source)
      old_codex = make_codex(directory, 'codex-old')
      system_codex = make_codex(directory, 'codex-system')
      candidate = make_package(directory, 'package-one')
      environment = host_environment(directory, config:, system_codex:).merge(
        'DEV_WORKSPACES_EXTENSION_CATALOG' => source_extension_catalog(directory)
      )
      host = TransitionHost.new(
        candidate:, busy:, env: environment, out: StringIO.new, err: StringIO.new
      )
      yield host, {
        root: directory,
        source:,
        old_codex:,
        system_codex:,
        current_root: File.join(directory, 'state', 'codex', 'current')
      }
    end
  end

  def portal_manifest(thread_id, socket, state)
    YAML.dump(
      'schema' => state == 'creating' ? 2 : 1,
      'slug' => 'ignored-by-host',
      'codex' => { 'thread_id' => thread_id, 'socket_path' => socket },
      'creation' => { 'state' => state }
    )
  end
end
