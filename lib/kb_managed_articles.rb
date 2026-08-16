# frozen_string_literal: true

require 'digest'
require 'open3'
require 'yaml'
require_relative 'kb_contract_files'

module KbManagedArticles
  class Error < StandardError; end

  COMMIT_OID = /\A[0-9a-f]{40}\z/
  REGISTRY_PATH = 'contract/articles.yml'

  module_function

  def registry(code_root, content: nil)
    path = KbContractFiles.path_within(code_root, REGISTRY_PATH)
    data = content ? YAML.safe_load(content) : YAML.safe_load_file(path)
    raise Error, 'article contract schema must be 1' unless data.fetch('schema') == 1
    raise Error, 'article contract must declare its repository' unless data.fetch('repository').is_a?(String)
    raise Error, 'article contract articles must be a mapping' unless data.fetch('articles').is_a?(Hash)

    data
  rescue KeyError => e
    raise Error, "incomplete article contract: #{e.message}"
  end

  def resolve_base!(code_root, base)
    unless base.is_a?(String) && base.match?(COMMIT_OID)
      raise Error, 'article base must be a full 40-character commit OID'
    end

    output, error, status = Open3.capture3(
      'git', '-C', code_root, 'rev-parse', '--verify', "#{base}^{commit}"
    )
    raise Error, "invalid article base #{base.inspect}: #{error.strip}" unless status.success?

    resolved = output.strip
    raise Error, "article base resolved unexpectedly to #{resolved}" unless resolved == base

    resolved
  end

  def resolve_head!(code_root)
    output, error, status = Open3.capture3(
      'git', '-C', code_root, 'rev-parse', '--verify', 'HEAD^{commit}'
    )
    raise Error, "unable to resolve article contract HEAD: #{error.strip}" unless status.success?

    resolved = output.strip
    raise Error, "invalid article contract HEAD #{resolved.inspect}" unless resolved.match?(COMMIT_OID)

    resolved
  end

  def git_source(code_root, commit, relative)
    output, _error, status = Open3.capture3(
      'git', '-C', code_root, 'show', "#{commit}:#{relative}"
    )
    status.success? ? output.force_encoding(Encoding::UTF_8) : nil
  end

  def contract_provenance(code_root:, base:)
    base_commit = resolve_base!(code_root, base)
    head_commit = resolve_head!(code_root)
    registry_path = KbContractFiles.path_within(code_root, REGISTRY_PATH)
    current_registry = File.read(registry_path, encoding: Encoding::UTF_8)
    committed_registry = git_source(code_root, head_commit, REGISTRY_PATH)
    raise Error, 'article registry is absent from contract HEAD' unless committed_registry
    unless current_registry == committed_registry
      raise Error, 'article registry differs from contract HEAD; commit it before building'
    end
    registry_data = registry(code_root, content: current_registry)

    {
      registry: registry_data,
      repository: registry_data.fetch('repository'),
      base_commit: base_commit,
      head_commit: head_commit,
      registry_sha256: Digest::SHA256.hexdigest(current_registry)
    }
  end

  def registered_pages(code_root:, contract:, require_committed:)
    pages = contract.fetch(:registry).fetch('articles').flat_map do |article_id, article|
      KbContractFiles.validate_semantic_id!(article_id)
      article.fetch('pages').map do |language, page|
        unless %w[cs en].include?(language)
          raise Error, "#{article_id}: unknown managed page language #{language.inspect}"
        end

        page_id = KbContractFiles.validate_page_id!(page.fetch('id'))
        relative = page.fetch('source')
        current_path = KbContractFiles.path_within(code_root, relative)
        raise Error, "#{article_id}: #{language}: canonical source is missing" unless File.file?(current_path)

        current = File.read(current_path, encoding: Encoding::UTF_8)
        if require_committed
          committed = git_source(code_root, contract.fetch(:head_commit), relative)
          raise Error, "#{article_id}: #{language}: canonical source is absent from contract HEAD" unless committed
          unless current == committed
            raise Error, "#{article_id}: #{language}: canonical source differs from contract HEAD; " \
                         'commit it before building'
          end
        end

        {
          article: article_id,
          language: language,
          page: page_id,
          source: relative,
          path: current_path,
          current: current,
          sha256: Digest::SHA256.hexdigest(current)
        }
      end
    end

    keys = pages.map { |page| page.values_at(:language, :page) }
    raise Error, 'duplicate managed page IDs in article registry' unless keys.uniq.length == keys.length

    pages
  rescue KeyError => e
    raise Error, "incomplete article contract: #{e.message}"
  end

  def registered_tests(code_root:, contract:, require_committed:)
    tests = contract.fetch(:registry).fetch('articles').map do |article_id, article|
      KbContractFiles.validate_semantic_id!(article_id)
      test = article.fetch('test')
      suite = test.fetch('suite')
      components = suite.split('/', -1) if suite.is_a?(String)
      unless components&.all? do |component|
               component.match?(/\A[a-z0-9][a-z0-9_.-]*\z/) && !%w[. ..].include?(component)
             end
        raise Error, "#{article_id}: invalid managed test suite #{suite.inspect}"
      end

      relative = test.fetch('source')
      expected_relative = "tests/suite/#{suite}.nix"
      unless relative == expected_relative
        raise Error,
              "#{article_id}: managed test source must be #{expected_relative}, got #{relative.inspect}"
      end
      current_path = KbContractFiles.path_within(code_root, relative)
      raise Error, "#{article_id}: managed test source is missing" unless File.file?(current_path)

      current = File.binread(current_path)
      if require_committed
        committed = git_source(code_root, contract.fetch(:head_commit), relative)
        raise Error, "#{article_id}: managed test source is absent from contract HEAD" unless committed
        unless current == committed
          raise Error, "#{article_id}: managed test source differs from contract HEAD; " \
                       'commit it before building'
        end
      end

      {
        article: article_id,
        pattern: "#{suite}#*",
        source: relative,
        path: current_path,
        sha256: Digest::SHA256.hexdigest(current)
      }
    end

    patterns = tests.map { |test| test.fetch(:pattern) }
    raise Error, 'duplicate managed test patterns in article registry' unless patterns.uniq.length == patterns.length

    sources = tests.map { |test| test.fetch(:source) }
    raise Error, 'duplicate managed test sources in article registry' unless sources.uniq.length == sources.length

    tests
  rescue KeyError => e
    raise Error, "incomplete article contract: #{e.message}"
  end

  def reconcile(
    code_root:, base:, article_id:, wiki_pages:, bootstrap: {},
    contract: nil, require_committed: false
  )
    contract ||= contract_provenance(code_root: code_root, base: base)
    base_commit = contract.fetch(:base_commit)
    article = contract.fetch(:registry).fetch('articles').fetch(article_id) do
      raise Error, "unknown managed article #{article_id.inspect}"
    end
    registered = registered_pages(
      code_root: code_root,
      contract: contract,
      require_committed: require_committed
    ).select { |page| page.fetch(:article) == article_id }

    registered.map do |registered_page|
      language = registered_page.fetch(:language)
      page_id = registered_page.fetch(:page)
      relative = registered_page.fetch(:source)
      current = registered_page.fetch(:current)
      wiki = wiki_pages.fetch([language, page_id]) do
        raise Error, "#{article_id}: #{language}: fetched wiki page #{page_id} is missing"
      end
      base_content = git_source(code_root, base_commit, relative)

      status = if base_content.nil?
                 expected = bootstrap[language]
                 unless expected&.match?(/\A[0-9a-f]{64}\z/)
                   raise Error, "#{article_id}: #{language}: canonical source is absent at the base; " \
                                'a bootstrap SHA-256 is required'
                 end
                 actual = Digest::SHA256.hexdigest(wiki)
                 unless actual == expected
                   raise Error, "#{article_id}: #{language}: fetched bootstrap page differs: " \
                                "expected #{expected}, got #{actual}"
                 end
                 :bootstrap
               elsif wiki == base_content
                 current == base_content ? :in_sync : :git_only
               elsif wiki == current
                 :reconciled
               elsif current == base_content
                 :wiki_only
               else
                 :conflict
               end

      {
        article: article_id,
        language: language,
        page: page_id,
        source: relative,
        path: registered_page.fetch(:path),
        status: status,
        base_commit: base_commit,
        head_commit: contract.fetch(:head_commit),
        canonical_sha256: registered_page.fetch(:sha256),
        base: base_content,
        current: current,
        wiki: wiki
      }
    end
  rescue KeyError => e
    raise Error, "incomplete article contract: #{e.message}"
  end
end
