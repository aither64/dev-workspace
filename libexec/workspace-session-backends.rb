# frozen_string_literal: true

require 'digest'
require 'json'
require 'pathname'

# Shared read-only guards. These do not create backend state or accept Pi
# creation formats; schema-4 support must arrive with the complete writer.
module WorkspaceSessionBackends
  class Error < StandardError; end
  class StrictObject < Hash
    def []=(key, value)
      raise Error, "duplicate backend evidence key: #{key}" if key?(key)
      super
    end
  end

  FIELDS = %w[sessionBackendBindingSchema sessionBackendBindingPolicy runtimeAuthoritySchema piCreationSchema].zip([1, 1, 2, 4]).to_h.freeze
  BINDING_KEYS = %w[schema backend backend_contract workspace slug session_incarnation creation_receipt_id revision state conversations].freeze
  HEX = /\A[0-9a-f]{64}\z/
  SLUG = /\A[A-Za-z0-9][A-Za-z0-9_-]*\z/
  MEMBER = /\A[a-z][a-z0-9]{0,31}[0-9]+\z/

  module_function

  def present?(path)
    File.lstat(path)
    true
  rescue Errno::ENOENT
    false
  end

  def private_directory!(path)
    stat = File.lstat(path)
    unless stat.directory? && stat.uid == Process.euid && (stat.mode & 0o777) == 0o700
      raise Error, "backend evidence directory is not private and owned: #{path}"
    end
  end

  def read!(path, maximum: 64 * 1024, private_state: true)
    stat = File.lstat(path)
    unless stat.file? && stat.size <= maximum &&
      (!private_state || stat.uid == Process.euid && (stat.mode & 0o777) == 0o600)
      raise Error, "backend evidence is not a private bounded regular file: #{path}"
    end
    data = File.binread(path, maximum + 1).force_encoding(Encoding::UTF_8)
    raise Error, "invalid backend evidence encoding or size: #{path}" unless data.valid_encoding? && data.bytesize <= maximum
    value = JSON.parse(data, object_class: StrictObject)
    raise Error, "backend evidence is not an object: #{path}" unless value.is_a?(Hash)
    value
  rescue JSON::ParserError, SystemCallError => e
    raise Error, "cannot read backend evidence #{path}: #{e.message}"
  end

  def scalar?(value)
    value.is_a?(String) && value.valid_encoding? && value.bytesize.between?(1, 4096) && !value.include?("\0")
  end

  def hex?(value) = value.is_a?(String) && HEX.match?(value)
  def member?(value) = scalar?(value) && (value == 'lead' || MEMBER.match?(value))

  def binding_directory(root, workspace)
    File.join(root, 'session-backends', Digest::SHA256.hexdigest(workspace))
  end

  def binding(root, workspace, slug)
    raise Error, 'invalid backend scope' unless scalar?(slug) && SLUG.match?(slug) &&
      scalar?(workspace) && Pathname.new(workspace).absolute? && File.expand_path(workspace) == workspace
    directory = binding_directory(root, workspace)
    return nil unless present?(directory)
    private_directory!(directory)
    path = File.join(directory, "#{slug}.json")
    return nil unless present?(path)
    value = read!(path)
    unless value.keys.sort == BINDING_KEYS.sort && value['schema'].is_a?(Integer) && value['schema'] == 1 &&
      value['backend'] == 'pi' && value['backend_contract'].is_a?(Integer) && value['backend_contract'] == 1 &&
      value['workspace'] == workspace && value['slug'] == slug && hex?(value['session_incarnation']) &&
      scalar?(value['creation_receipt_id']) && value['revision'].is_a?(Integer) && value['revision'].between?(1, 2**64 - 1) &&
      %w[creating ready].include?(value['state']) && value['conversations'].is_a?(Array) && !value['conversations'].empty?
      raise Error, "unsupported or invalid session backend binding: #{path}"
    end
    addresses, native_ids = [], []
    value['conversations'].each do |conversation|
      unless conversation.is_a?(Hash) &&
        (conversation.keys.sort == %w[address incarnation state].sort || conversation.keys.sort == %w[address incarnation state native_id].sort) &&
        member?(conversation['address']) && hex?(conversation['incarnation']) && %w[creating ready].include?(conversation['state']) &&
        (!conversation.key?('native_id') || scalar?(conversation['native_id'])) &&
        (conversation['state'] != 'ready' || scalar?(conversation['native_id']))
        raise Error, "invalid backend conversation: #{path}"
      end
      addresses << conversation['address']
      native_ids << conversation['native_id'] if conversation['state'] == 'ready'
    end
    lead = value['conversations'].find { |conversation| conversation['address'] == 'lead' }
    unless lead && addresses.uniq == addresses && native_ids.uniq == native_ids && (value['state'] != 'ready' || lead['state'] == 'ready')
      raise Error, "conflicting backend conversations: #{path}"
    end
    value
  end

  def binding_slugs(root, workspace)
    directory = binding_directory(root, workspace)
    return [] unless present?(directory)
    private_directory!(directory)
    Dir.children(directory).sort.filter_map do |name|
      next unless name.end_with?('.json')
      slug = name.delete_suffix('.json')
      binding(root, workspace, slug)
      slug
    end
  rescue SystemCallError => e
    raise Error, "cannot inspect session backend state: #{e.message}"
  end

  def creation_paths(root, workspace, slug)
    identifier = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
    [File.join(root, 'portal', identifier, 'creations', "#{slug}.json"),
     File.join(workspace, 'worktrees', '.locks', "#{slug}.creation.json")]
  end

  def refuse_pi_creation!(path)
    return unless present?(path)
    record = read!(path, maximum: 12 * 20_000 + 16_384)
    schema = record['schema']
    unless schema.is_a?(Integer) && [1, 2, 3].include?(schema) && !record.key?('backend') &&
      !(record['request'].is_a?(Hash) && record['request'].key?('backend'))
      raise Error, "Pi/unsupported creation evidence requires full schema-4 validation, unavailable in this package: #{path}"
    end
  end

  def require_codex!(root, workspace, slug)
    selected = binding(root, workspace, slug)
    paths = creation_paths(root, workspace, slug)
    paths.each { |path| refuse_pi_creation!(path) }
    return unless selected
    if paths.none? { |path| present?(path) }
      raise Error, "session backend conflict: Pi binding has no retained creation fence: #{slug}"
    end
    raise Error, "session backend conflict: Pi binding cannot use a legacy Codex writer: #{slug}"
  rescue SystemCallError => e
    raise Error, "cannot inspect session backend state: #{e.message}"
  end

  def require_contract!(package)
    contract = read!(File.join(package, 'share/workspace-portal/runtime-contract.json'), private_state: false)
    unless FIELDS.all? { |field, expected| contract[field].is_a?(Integer) && contract[field] == expected }
      raise Error, 'target package cannot preserve Pi backend bindings and creation evidence'
    end
  end
end
