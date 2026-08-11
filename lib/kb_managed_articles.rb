# frozen_string_literal: true

require 'digest'
require 'open3'
require 'yaml'
require_relative 'kb_contract_files'

module KbManagedArticles
  class Error < StandardError; end

  module_function

  def registry(code_root)
    path = KbContractFiles.path_within(code_root, 'contract/articles.yml')
    data = YAML.safe_load_file(path)
    raise Error, 'article contract schema must be 1' unless data.fetch('schema') == 1

    data
  end

  def validate_base!(code_root, base)
    _output, error, status = Open3.capture3(
      'git', '-C', code_root, 'rev-parse', '--verify', "#{base}^{commit}"
    )
    raise Error, "invalid article base #{base.inspect}: #{error.strip}" unless status.success?
  end

  def base_source(code_root, base, relative)
    output, _error, status = Open3.capture3(
      'git', '-C', code_root, 'show', "#{base}:#{relative}"
    )
    status.success? ? output.force_encoding(Encoding::UTF_8) : nil
  end

  def reconcile(code_root:, base:, article_id:, wiki_pages:, bootstrap: {})
    validate_base!(code_root, base)
    article = registry(code_root).fetch('articles').fetch(article_id) do
      raise Error, "unknown managed article #{article_id.inspect}"
    end

    article.fetch('pages').map do |language, page|
      page_id = KbContractFiles.validate_page_id!(page.fetch('id'))
      relative = page.fetch('source')
      current_path = KbContractFiles.path_within(code_root, relative)
      raise Error, "#{article_id}: #{language}: canonical source is missing" unless File.file?(current_path)

      current = File.read(current_path, encoding: Encoding::UTF_8)
      wiki = wiki_pages.fetch([language, page_id]) do
        raise Error, "#{article_id}: #{language}: fetched wiki page #{page_id} is missing"
      end
      base_content = base_source(code_root, base, relative)

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
        path: current_path,
        status: status,
        base: base_content,
        current: current,
        wiki: wiki
      }
    end
  end
end
