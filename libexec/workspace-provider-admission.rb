# frozen_string_literal: true

require 'json'
require 'fileutils'
require_relative 'workspace-provider-bindings'

module WorkspaceProviderAdmission
  # Marked readers use the same strict provider codec. Paths identify the
  # existing registered workspace; they do not grant session/caller authority.
  def self.binding_state(environment, workspace, socket)
    root, name = environment.values_at('DEV_WORKSPACES_STATE', 'DEV_WORKSPACE_NAME')
    raise 'binding provider identity is unavailable' unless root.is_a?(String) && root.start_with?('/') &&
      name.is_a?(String) && DevWorkspaceHost::ProviderBindings::ID.match?(name)
    path = File.join(root, 'providers', name, 'state.json')
    stat = File.lstat(path)
    raise 'binding provider state is not private' unless stat.file? && stat.uid == Process.euid && (stat.mode & 0o777) == 0o600 && stat.size <= 1024 * 1024
    catalog_path = environment.fetch('DEV_WORKSPACES_CODEX_PROVIDERS', '/etc/dev-workspaces/codex-providers.json')
    catalog_stat = File.stat(catalog_path)
    raise 'binding provider catalog exceeds bounds' unless catalog_stat.file? && catalog_stat.size <= 64 * 1024
    catalog = JSON.parse(File.binread(catalog_path), object_class: DevWorkspaceHost::ProviderBindings::UniqueObject)
    DevWorkspaceHost::ProviderBindings.decode_binding_state(File.binread(path), workspace: name, root: workspace, socket: socket, catalog: catalog)
  end

  def self.binding_evidence(environment, operation)
    codec = DevWorkspaceHost::ProviderBindings
    raise 'invalid binding evidence operation' unless operation.is_a?(String) && codec::OPERATION.match?(operation)
    root, name = environment.values_at('DEV_WORKSPACES_STATE', 'DEV_WORKSPACE_NAME')
    raise 'binding provider identity is unavailable' unless codec.path?(root) && codec.id?(name)
    path = File.join(root, 'providers', name, "#{operation}.snapshot.json.evidence")
    stat = File.lstat(path)
    raise 'binding evidence is not private' unless stat.file? && stat.uid == Process.euid && (stat.mode & 0o777) == 0o600 && stat.size <= codec::EVIDENCE_LIMIT
    bytes = File.binread(path, codec::EVIDENCE_LIMIT + 1)
    raise 'binding evidence exceeds bounds' if bytes.bytesize > codec::EVIDENCE_LIMIT
    bytes
  end

  def self.with_creation(environment)
    root, name = environment.values_at('DEV_WORKSPACES_STATE', 'DEV_WORKSPACE_NAME')
    return yield unless root && name
    raise 'invalid provider workspace identity' unless name.match?(/\A[a-z0-9][a-z0-9_-]{0,62}\z/) && root.start_with?('/')
    directory = File.join(root, 'providers', name)
    FileUtils.mkdir_p(directory, mode: 0o700)
    File.open(File.join(directory, 'creation-admission.lock'), File::RDWR | File::CREAT, 0o600) do |lock|
      lock.flock(File::LOCK_SH)
      yield
    end
  end

  def self.record_terminal!(environment, workspace, session, tmux_socket)
    path = environment['DEV_WORKSPACE_PROVIDER_TERMINAL_SNAPSHOT']
    return unless path
    root = environment.fetch('DEV_WORKSPACES_STATE')
    name = environment.fetch('DEV_WORKSPACE_NAME')
    state_path = File.join(root, 'providers', name, 'state.json')
    data = JSON.parse(File.binread(state_path, 1024 * 1024 + 1))
    operation = data.fetch('operation')
    expected = File.join(root, 'providers', name, "#{operation.fetch('id')}.snapshot.json.terminals")
    unless operation['phase'] == 'applying' && path == expected && data['root'] == workspace &&
           data['workspace'] == name && data['socket'] == session.codex_socket_path &&
           session.workspace == workspace && session.managed? && session.socket_path == tmux_socket
      raise 'provider terminal snapshot has a different ownership identity'
    end
    stat = File.lstat(path)
    raise 'provider terminal snapshot is not private' unless stat.file? && (stat.mode & 0o777) == 0o600 && stat.uid == Process.euid && stat.size <= 1024 * 1024
    records = JSON.parse(File.binread(path, 1024 * 1024 + 1))
    unless records['schema'] == 1 && records['root'] == workspace && records['workspace'] == name && records['socket'] == data['socket'] && records['terminals'].is_a?(Array)
      raise 'invalid provider terminal snapshot'
    end
    socket_stat = File.stat(tmux_socket)
    record = {'session' => session.to_h.transform_keys(&:to_s), 'socketIdentity' => [socket_stat.dev, socket_stat.ino]}
    old = records['terminals'].find { |item| item.fetch('session').fetch('slug') == session.slug }
    raise 'provider terminal identity changed' if old && old != record
    return if old
    records['terminals'] << record
    temporary = "#{path}.#{$$}.tmp"
    begin
      File.open(temporary, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
        file.write(JSON.generate(records)); file.flush; file.fsync
      end
      File.rename(temporary, path)
      File.open(File.dirname(path), File::RDONLY) { |directory| directory.fsync }
    ensure
      File.unlink(temporary) if File.exist?(temporary)
    end
  end

  def self.require!(environment, workspace, socket, slug: nil)
    root = environment['DEV_WORKSPACES_STATE']
    name = environment['DEV_WORKSPACE_NAME']
    return unless root && name
    raise 'invalid provider workspace identity' unless name.match?(/\A[a-z0-9][a-z0-9-]{0,62}\z/) && root.start_with?('/')
    path = File.join(root, 'providers', name, 'state.json')
    return unless File.exist?(path)
    stat = File.lstat(path)
    raise 'invalid workspace provider state' unless stat.file? && stat.uid == Process.euid && (stat.mode & 0o777) == 0o600 && stat.size <= 1024 * 1024
    data = JSON.parse(File.binread(path, 1024 * 1024 + 1))
    data = binding_state(environment, workspace, socket) if data['schema'] == 3 || data.key?('bindings') || data.key?('bindingRevision') || data.dig('operation', 'strategy') == 'fork-v1'
    unless [1, 2, 3].include?(data['schema']) && data['workspace'] == name && data['root'] == workspace && data['socket'] == socket
      raise 'workspace provider state has a different ownership identity'
    end
    operation = data['operation']
    return unless operation
    raise 'invalid workspace provider operation' unless %w[waiting applying applied canceled failed recovery-required].include?(operation['phase'])
    if %w[applying recovery-required].include?(operation['phase'])
      if operation['strategy'] == 'scoped-v1'
        return unless slug
        if operation['session']
          return if operation['session'] != slug
        elsif (selection = data.fetch('sessions', {})[slug]) && selection['rootThreadId']
          require 'yaml'
          manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
          manifest = YAML.safe_load(File.binread(manifest_path, 1024 * 1024 + 1), permitted_classes: [], aliases: false)
          return if selection['rootThreadId'] == manifest.dig('codex', 'thread_id')
        end
      end
      raise 'workspace provider transition is reserved; wait for completion or recover it'
    end
  end

  def self.scoped_pause?(environment, workspace, slug, socket)
    path = environment['DEV_WORKSPACE_PROVIDER_TERMINAL_SNAPSHOT']
    return false unless path
    root = environment.fetch('DEV_WORKSPACES_STATE')
    name = environment.fetch('DEV_WORKSPACE_NAME')
    data = JSON.parse(File.binread(File.join(root, 'providers', name, 'state.json'), 1024 * 1024 + 1))
    operation = data.fetch('operation')
    expected = File.join(root, 'providers', name, "#{operation.fetch('id')}.snapshot.json.terminals")
    unless data['root'] == workspace && data['socket'] == socket && path == expected && operation['phase'] == 'applying'
      raise 'provider pause has a different operation identity'
    end
    operation['strategy'] == 'scoped-v1' && operation.fetch('sessions').include?(slug)
  end
end
