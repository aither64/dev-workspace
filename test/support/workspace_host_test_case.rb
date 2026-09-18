# frozen_string_literal: true

require 'fileutils'
require 'minitest/autorun'
require 'stringio'
require 'tmpdir'

load File.expand_path('../../libexec/workspace-host', __dir__)

class WorkspaceHostTest < Minitest::Test

  private

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
    File.symlink(File.expand_path('../..', __dir__), profile)
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
        { 'id' => provider_id, 'label' => provider_id.capitalize, 'command' => RbConfig.ruby }
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
