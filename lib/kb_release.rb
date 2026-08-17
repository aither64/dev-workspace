# frozen_string_literal: true

require 'base64'
require 'cgi'
require 'digest'
require 'json'
require 'net/http'
require 'time'
require 'uri'
require 'yaml'

require_relative 'kb_stage'

module KbRelease
  class Error < StandardError; end
  MANAGED_REPOSITORY = 'vpsfreecz/vpsfree-kb-contracts'

  class Manifest
    LEGACY_PAGE_KEY_FIELD = 'article'

    attr_reader :path, :data

    def initialize(path)
      @path = File.expand_path(path)
      @data = YAML.safe_load_file(@path)
      validate!
    rescue Psych::SyntaxError => e
      raise Error, "invalid release manifest: #{e.message}"
    end

    def digest
      Digest::SHA256.file(path).hexdigest
    end

    def root
      File.dirname(path)
    end

    def wiki
      data.fetch('wiki')
    end

    def staging_wiki
      "#{wiki}-staging"
    end

    def pages
      data.fetch('pages')
    end

    def media
      data.fetch('media')
    end

    def deletions
      data.fetch('deletions', [])
    end

    def contract
      data['contract']
    end

    def managed_contract?
      contract.is_a?(Hash) && !contract.fetch('pages').empty?
    end

    def managed_ref
      contract&.fetch('head_commit')
    end

    def managed_sources
      return [] unless managed_contract?

      contract.fetch('pages') + contract.fetch('tests', [])
    end

    def contract_page_key(entry)
      field = data.fetch('schema') >= 5 ? 'page_key' : LEGACY_PAGE_KEY_FIELD
      entry.fetch(field)
    end

    def per_page_summaries?
      data.fetch('schema') >= 4
    end

    def production_summary
      return data.fetch('production_summary') if data.key?('production_summary')
      return 'Publish reviewed KB release' if data.fetch('schema') == 1

      data.fetch('production_summary')
    end

    def page_summary(entry)
      return entry.fetch('summary') if per_page_summaries?

      production_summary
    end

    def staging_summary(entry)
      return entry.fetch('summary') if per_page_summaries?

      'Stage reviewed KB release'
    end

    def page_policy(entry)
      entry.fetch('policy', 'update')
    end

    def read(entry)
      file = File.expand_path(entry.fetch('file'), root)
      unless file.start_with?("#{root}/")
        raise Error, "release file escapes the manifest directory: #{entry.fetch('file')}"
      end

      content = File.binread(file)
      actual = Digest::SHA256.hexdigest(content)
      expected = entry.fetch('sha256')
      raise Error, "checksum mismatch for #{entry.fetch('file')}" unless actual == expected

      content
    rescue Errno::ENOENT
      raise Error, "release file not found: #{entry.fetch('file')}"
    end

    private

    def validate!
      unless [1, 2, 3, 4, 5].include?(data['schema'])
        raise Error, 'release manifest schema must be 1, 2, 3, 4 or 5'
      end
      raise Error, 'release wiki must be cz or org' unless %w[cz org].include?(data['wiki'])

      if data['schema'] >= 4
        raise Error, "schema #{data['schema']} uses per-page summaries" if data.key?('production_summary')
      elsif data['schema'] >= 2 || data.key?('production_summary')
        summary = data.fetch('production_summary')
        validate_summary!(summary, 'production summary')
      end

      %w[pages media].each do |kind|
        entries = data.fetch(kind)
        raise Error, "#{kind} must be a list" unless entries.is_a?(Array)
        ids = entries.map { |entry| entry.fetch('id') }
        raise Error, "duplicate #{kind} IDs" unless ids.uniq.length == ids.length
      end
      pages.each do |entry|
        %w[id file sha256 source_sha256].each { |key| entry.fetch(key) }
        validate_summary!(entry.fetch('summary'), "page summary for #{entry.fetch('id')}") if per_page_summaries?
        policy = page_policy(entry)
        raise Error, "invalid page policy #{policy}" unless %w[create update].include?(policy)
        entry.fetch('source_revision') if policy == 'update'
        if policy == 'create' && entry.fetch('source_sha256') != Digest::SHA256.hexdigest('')
          raise Error, "create page source must be empty: #{entry.fetch('id')}"
        end
        entry.fetch('language_counterpart') if wiki == 'org'
      end
      if per_page_summaries?
        entries = data.fetch('deletions')
        raise Error, 'deletions must be a list' unless entries.is_a?(Array)
        ids = entries.map { |entry| entry.fetch('id') }
        raise Error, 'duplicate deletion IDs' unless ids.uniq.length == ids.length
        raise Error, 'page and deletion IDs overlap' unless (pages.map { |entry| entry.fetch('id') } & ids).empty?
        entries.each do |entry|
          %w[id source_revision source_sha256 summary].each { |key| entry.fetch(key) }
          validate_digest!(entry.fetch('source_sha256'), "deletion #{entry.fetch('id')}")
          validate_summary!(entry.fetch('summary'), "deletion summary for #{entry.fetch('id')}")
        end
      elsif data.key?('deletions') && !data.fetch('deletions').empty?
        raise Error, 'page deletions require release manifest schema 4 or 5'
      end
      validate_contract! if data.key?('contract')
      media.each do |entry|
        %w[id file sha256].each { |key| entry.fetch(key) }
        policy = entry.fetch('policy', 'create')
        raise Error, "invalid media policy #{policy}" unless %w[create update].include?(policy)
        entry.fetch('source_sha256') if policy == 'update'
      end
    rescue KeyError => e
      raise Error, "incomplete release manifest: #{e.message}"
    end

    def validate_summary!(value, description)
      return if value.is_a?(String) && !value.strip.empty? && !value.match?(/[\r\n]/)

      raise Error, "#{description} must be a non-empty single line"
    end

    def validate_digest!(value, description)
      return if value.is_a?(String) && value.match?(/\A[0-9a-f]{64}\z/)

      raise Error, "invalid SHA-256 for #{description}"
    end

    def validate_contract!
      contract = data.fetch('contract')
      raise Error, 'release contract must be a mapping' unless contract.is_a?(Hash)

      repository = contract.fetch('repository')
      unless repository == MANAGED_REPOSITORY
        raise Error,
              "release contract repository must be #{MANAGED_REPOSITORY}, got #{repository.inspect}"
      end
      %w[base_commit head_commit].each do |key|
        value = contract.fetch(key)
        unless value.is_a?(String) && value.match?(/\A[0-9a-f]{40}\z/)
          raise Error, "invalid release contract #{key}"
        end
      end
      registry_sha256 = contract.fetch('registry_sha256')
      unless registry_sha256.is_a?(String) && registry_sha256.match?(/\A[0-9a-f]{64}\z/)
        raise Error, 'invalid release contract registry_sha256'
      end

      contract_pages = contract.fetch('pages')
      raise Error, 'release contract pages must be a list' unless contract_pages.is_a?(Array)
      ids = contract_pages.map { |page| page.fetch('id') }
      raise Error, 'duplicate release contract page IDs' unless ids.uniq.length == ids.length
      release_pages = pages.to_h { |page| [page.fetch('id'), page] }
      contract_pages.each do |page|
        page_key = contract_page_key(page)
        unless page_key.is_a?(String) && page_key.match?(/\A[a-z0-9][a-z0-9-]*\z/)
          raise Error, "invalid release contract page key #{page_key.inspect}"
        end
        validate_relative_path!(page.fetch('source'), 'release contract source')
        sha256 = page.fetch('sha256')
        unless sha256.is_a?(String) && sha256.match?(/\A[0-9a-f]{64}\z/)
          raise Error, "invalid release contract page SHA-256 for #{page.fetch('id')}"
        end
        release_page = release_pages.fetch(page.fetch('id')) do
          raise Error, "release contract page is absent from manifest: #{page.fetch('id')}"
        end
        unless release_page.fetch('sha256') == sha256
          raise Error, "release contract checksum differs for #{page.fetch('id')}"
        end
      end

      return unless contract.key?('tests')

      contract_tests = contract.fetch('tests')
      raise Error, 'release contract tests must be a list' unless contract_tests.is_a?(Array)
      test_page_keys = contract_tests.map do |test|
        page_key = contract_page_key(test)
        unless page_key.is_a?(String) && page_key.match?(/\A[a-z0-9][a-z0-9-]*\z/)
          raise Error, "invalid release contract test page key #{page_key.inspect}"
        end
        pattern = test.fetch('pattern')
        suite = pattern.delete_suffix('#*') if pattern.is_a?(String) && pattern.end_with?('#*')
        unless suite&.split('/', -1)&.all? do |component|
                 component.match?(/\A[a-z0-9][a-z0-9_.-]*\z/) && !%w[. ..].include?(component)
               end
          raise Error, "invalid release contract test pattern #{pattern.inspect}"
        end
        source = validate_relative_path!(test.fetch('source'), 'release contract test source')
        expected_source = "tests/suite/#{suite}.nix"
        unless source == expected_source
          raise Error,
                "release contract test source must be #{expected_source}, got #{source.inspect}"
        end
        validate_digest!(test.fetch('sha256'), "release contract test #{page_key}")
        page_key
      end
      unless test_page_keys.uniq.length == test_page_keys.length
        raise Error, 'duplicate release contract test page keys'
      end
      patterns = contract_tests.map { |test| test.fetch('pattern') }
      raise Error, 'duplicate release contract test patterns' unless patterns.uniq.length == patterns.length
      sources = contract_tests.map { |test| test.fetch('source') }
      raise Error, 'duplicate release contract test sources' unless sources.uniq.length == sources.length
      covered_page_keys = contract_pages.map { |page| contract_page_key(page) }.uniq
      unless covered_page_keys.sort == test_page_keys.sort
        raise Error, 'release contract tests must cover every managed page exactly once'
      end
    end

    def validate_relative_path!(value, description)
      components = value.split('/', -1) if value.is_a?(String) && !value.start_with?('/')
      unless components&.all? do |component|
               component.match?(/\A[a-zA-Z0-9_.-]+\z/) && !%w[. ..].include?(component)
             end
        raise Error, "invalid #{description} #{value.inspect}"
      end

      value
    end
  end

  class ManagedRepository
    def initialize(fetcher: nil, out: $stdout)
      @fetcher = fetcher || method(:fetch)
      @out = out
    end

    def verify!(manifest, ref:)
      return [] unless manifest.managed_contract?

      KbStage.validate_managed_ref!(ref)
      repository = manifest.contract.fetch('repository')
      rows = manifest.managed_sources.map do |entry|
        source = entry.fetch('source')
        content = @fetcher.call(raw_url(repository, ref, source))
        actual = Digest::SHA256.hexdigest(content)
        expected = entry.fetch('sha256')
        unless actual == expected
          raise Error,
                "managed repository content differs at #{repository}@#{ref}:#{source}: " \
                "expected #{expected}, got #{actual}"
        end

        [manifest.contract_page_key(entry), entry['pattern'], source_url(repository, ref, source)]
      end
      @out.puts("verified managed repository #{repository}@#{ref}")
      rows.each do |page_key, pattern, url|
        label = pattern ? "test #{pattern}" : "page #{page_key}"
        @out.puts("  #{label}: #{url}")
      end
      rows
    rescue KbStage::Error => e
      raise Error, e.message
    end

    private

    def escaped_path(path)
      path.split('/').map { |part| URI.encode_uri_component(part) }.join('/')
    end

    def raw_url(repository, ref, source)
      "https://raw.githubusercontent.com/#{repository}/#{ref}/#{escaped_path(source)}"
    end

    def source_url(repository, ref, source)
      "https://github.com/#{repository}/blob/#{ref}/#{escaped_path(source)}"
    end

    def fetch(url)
      uri = URI(url)
      response = Net::HTTP.start(
        uri.hostname,
        uri.port,
        use_ssl: true,
        open_timeout: 10,
        read_timeout: 30
      ) do |http|
        http.get(uri.request_uri)
      end
      return response.body if response.is_a?(Net::HTTPSuccess)

      raise Error, "unable to read managed repository source #{url}: HTTP #{response.code}"
    rescue SystemCallError, Timeout::Error => e
      raise Error, "unable to read managed repository source #{url}: #{e.message}"
    end
  end

  class RevisionHistory
    SUMMARY_PATTERN = %r{<span\s+class=["']sum["']>(.*?)</span>}m

    def initialize(site_url:, fetcher: nil, headers: nil)
      @site_url = site_url
      @fetcher = fetcher || method(:fetch)
      @headers = headers || ->(_wiki) { {} }
    end

    def verify!(wiki, id, expected)
      actual = latest_summary(wiki, id)
      return history_url(wiki, id) if actual == expected

      raise Error,
            "revision summary differs for #{id}: expected #{expected.inspect}, got #{actual.inspect}"
    end

    def latest_summary(wiki, id)
      html = @fetcher.call(history_url(wiki, id), @headers.call(wiki))
      match = html.match(SUMMARY_PATTERN)
      raise Error, "revision history has no entry for #{id}" unless match

      summary_html = match[1].dup.force_encoding(Encoding::UTF_8)
      raise Error, "revision history summary is not valid UTF-8 for #{id}" unless summary_html.valid_encoding?

      CGI.unescapeHTML(summary_html.gsub(%r{<[^>]*>}, ''))
         .sub(/\A\s*[–-]\s*/, '')
         .strip
    end

    def history_url(wiki, id)
      query = URI.encode_www_form('id' => id, 'do' => 'revisions')
      "#{@site_url.call(wiki)}/doku.php?#{query}"
    end

    private

    def fetch(url, headers)
      uri = URI(url)
      request = Net::HTTP::Get.new(uri)
      headers.each { |name, value| request[name] = value }
      response = Net::HTTP.start(
        uri.hostname,
        uri.port,
        use_ssl: uri.scheme == 'https'
      ) do |http|
        http.request(request)
      end
      return response.body if response.is_a?(Net::HTTPSuccess)

      raise Error, "unable to read revision history #{url}: HTTP #{response.code}"
    rescue SystemCallError, Timeout::Error => e
      raise Error, "unable to read revision history #{url}: #{e.message}"
    end
  end

  class Runner
    ACL_EDIT = 2
    ACL_CREATE = 4
    ACL_UPLOAD = 8
    ACL_DELETE = 16

    def initialize(
      manifest:,
      client_factory:,
      out: $stdout,
      language_links: KbStage::LanguageLinks.new(out: out),
      revision_history: nil,
      managed_repository: ManagedRepository.new(out: out)
    )
      @manifest = manifest
      @client_factory = client_factory
      @out = out
      @language_links = language_links
      @revision_history = revision_history
      @managed_repository = managed_repository
    end

    def stage!
      KbStage.with_staging_mutation do
        verify_managed_repository!(@manifest.managed_ref)
        KbStage.write_managed_ref!(@manifest.managed_ref || 'master')
        check_production_baseline!
        staging = @client_factory.call(@manifest.staging_wiki)
        states = verify_staging_baseline!(staging)
        verify_write_access!(staging, states:)
        save_media!(staging)
        save_pages!(staging, states: page_states_for_stage(states), staging: true)
        delete_pages!(staging, states: states.fetch(:deletions), production: false)
        verify_client!(staging)
        verify_revision_summaries!(@manifest.staging_wiki)
        verify_language_links! unless @manifest.pages.any? { |entry| page_create?(entry) }
        KbStage.write_json(
          KbStage.pending_release_path,
          'manifest' => @manifest.path,
          'sha256' => @manifest.digest,
          'staged_at' => Time.now.utc.iso8601,
          'slug' => KbStage.current_slug,
          'managed_ref' => @manifest.managed_ref || 'master'
        )
      end
      @out.puts(
        "staged #{@manifest.pages.length} pages, #{@manifest.deletions.length} deletions " \
        "and #{@manifest.media.length} media objects"
      )
    end

    def verify!(environment)
      KbStage.verify_current_owner! if environment == :staging
      if environment == :staging
        verify_active_managed_ref!
        verify_managed_repository!(@manifest.managed_ref)
      else
        verify_managed_repository!('master')
      end
      name = environment == :staging ? @manifest.staging_wiki : @manifest.wiki
      verify_client!(@client_factory.call(name))
      verify_revision_summaries!(name, report: true)
      verify_language_links! if environment == :staging
      @out.puts("verified #{name}")
    end

    def promote!(approved_production: false)
      raise Error, 'production promotion requires explicit approval' unless approved_production

      KbStage.with_owned_lock do
        verify_pending!
        verify_active_managed_ref!
        verify_client!(@client_factory.call(@manifest.staging_wiki))
        verify_revision_summaries!(@manifest.staging_wiki)
        verify_managed_repository!('master')
        states = check_production_baseline!(allow_candidate: true)
        production = @client_factory.call(@manifest.wiki)
        verify_write_access!(production, states:)
        save_media!(production)
        save_pages!(production, states: states.fetch(:pages), staging: false)
        delete_pages!(production, states: states.fetch(:deletions), production: true)
        verify_client!(production)
        verify_revision_summaries!(@manifest.wiki)
        File.delete(KbStage.pending_release_path)
      end
      @out.puts(
        "promoted #{@manifest.pages.length} pages, #{@manifest.deletions.length} deletions " \
        "and #{@manifest.media.length} media objects"
      )
    end

    private

    def verify_managed_repository!(ref)
      return unless @manifest.managed_contract?

      @managed_repository.verify!(@manifest, ref:)
    end

    def verify_active_managed_ref!
      return unless @manifest.managed_contract?

      expected = @manifest.managed_ref || 'master'
      actual = KbStage.read_managed_ref
      return if actual == expected

      raise Error,
            "staging managed repository ref differs: expected #{expected}, got #{actual.inspect}"
    end

    def check_production_baseline!(allow_candidate: false)
      production = @client_factory.call(@manifest.wiki)
      page_states = @manifest.pages.to_h do |entry|
        id = entry.fetch('id')
        if page_create?(entry)
          info = page_info(production, id)
          if info.nil?
            next [id, :source]
          end

          content = production.call('core.getPage', page: id)
          if allow_candidate && Digest::SHA256.hexdigest(content) == entry.fetch('sha256')
            verify_entry_summary!(@manifest.wiki, entry) if @manifest.per_page_summaries?
            next [id, :candidate]
          end

          raise Error, "create-only page already exists: #{id}"
        end

        info = production.call('core.getPageInfo', page: id)
        revision = info['rev'] || info['lastModified'] || info['revision']
        content = production.call('core.getPage', page: id)
        hash = Digest::SHA256.hexdigest(content)

        if hash == entry.fetch('source_sha256')
          unless revision.to_s == entry.fetch('source_revision').to_s
            raise Error, "production revision drift for #{id}: #{revision.inspect}"
          end
          [id, :source]
        elsif allow_candidate && hash == entry.fetch('sha256')
          verify_entry_summary!(@manifest.wiki, entry) if @manifest.per_page_summaries?
          [id, :candidate]
        else
          raise Error, "production content drift for #{id}"
        end
      end

      deletion_states = @manifest.deletions.to_h do |entry|
        id = entry.fetch('id')
        info = page_info(production, id)
        unless info
          if allow_candidate
            verify_entry_summary!(@manifest.wiki, entry)
            next [id, :candidate]
          end

          raise Error, "production page selected for deletion does not exist: #{id}"
        end

        revision = page_revision(info)
        content = production.call('core.getPage', page: id)
        unless Digest::SHA256.hexdigest(content) == entry.fetch('source_sha256')
          raise Error, "production deletion source drift for #{id}"
        end
        unless revision.to_s == entry.fetch('source_revision').to_s
          raise Error, "production deletion revision drift for #{id}: #{revision.inspect}"
        end

        [id, :source]
      end

      check_production_media_baseline!(production, allow_candidate:)
      { pages: page_states, deletions: deletion_states }
    end

    def check_production_media_baseline!(production, allow_candidate:)
      @manifest.media.each do |entry|
        id = entry.fetch('id')
        exists = media_exists?(production, id)
        policy = entry.fetch('policy', 'create')

        unless exists
          raise Error, "update-only production media does not exist: #{id}" if policy == 'update'

          next
        end

        encoded = production.call('core.getMedia', media: id)
        current_hash = Digest::SHA256.hexdigest(Base64.strict_decode64(encoded))
        next if policy == 'update' && current_hash == entry.fetch('source_sha256')
        next if allow_candidate && current_hash == entry.fetch('sha256')

        if policy == 'create'
          raise Error, "create-only production media already exists: #{id}"
        end

        raise Error, "production media drift for #{id}"
      end
    end

    def verify_staging_baseline!(client)
      page_states = @manifest.pages.to_h do |entry|
        id = entry.fetch('id')
        if page_create?(entry)
          info = page_info(client, id)
          next [id, :source] if info.nil?

          content = client.call('core.getPage', page: id)
          if Digest::SHA256.hexdigest(content) == entry.fetch('sha256')
            verify_entry_summary!(@manifest.staging_wiki, entry) if @manifest.per_page_summaries?
            next [id, :candidate]
          end

          raise Error, "staging create-only page has unexpected content at #{id}"
        end

        content = client.call('core.getPage', page: id)
        hash = Digest::SHA256.hexdigest(content)
        if hash == entry.fetch('source_sha256')
          [id, :source]
        elsif hash == entry.fetch('sha256')
          verify_entry_summary!(@manifest.staging_wiki, entry) if @manifest.per_page_summaries?
          [id, :candidate]
        else
          raise Error, "staging is not a clean production mirror at #{id}"
        end
      end

      deletion_states = @manifest.deletions.to_h do |entry|
        id = entry.fetch('id')
        info = page_info(client, id)
        unless info
          verify_entry_summary!(@manifest.staging_wiki, entry)
          next [id, :candidate]
        end

        content = client.call('core.getPage', page: id)
        unless Digest::SHA256.hexdigest(content) == entry.fetch('source_sha256')
          raise Error, "staging deletion source drift for #{id}"
        end

        [id, :source]
      end

      { pages: page_states, deletions: deletion_states }
    end

    def page_states_for_stage(states)
      return nil unless @manifest.per_page_summaries?

      states.fetch(:pages)
    end

    def save_pages!(client, states:, staging:)
      @manifest.pages.each do |entry|
        next if states && states.fetch(entry.fetch('id')) == :candidate
        verify_source_page!(client, entry) if states && !staging

        result = client.call(
          'core.savePage',
          page: entry.fetch('id'),
          text: @manifest.read(entry).force_encoding(Encoding::UTF_8),
          summary: staging ? @manifest.staging_summary(entry) : @manifest.page_summary(entry),
          isminor: false
        )
        raise Error, "failed to save page #{entry.fetch('id')}" unless result == true
      end
    end

    def delete_pages!(client, states:, production:)
      @manifest.deletions.each do |entry|
        next if states.fetch(entry.fetch('id')) == :candidate
        verify_source_deletion!(client, entry) if production

        result = client.call(
          'core.savePage',
          page: entry.fetch('id'),
          text: '',
          summary: @manifest.page_summary(entry),
          isminor: false
        )
        raise Error, "failed to delete page #{entry.fetch('id')}" unless result == true
      end
    end

    def verify_source_page!(client, entry)
      id = entry.fetch('id')
      if page_create?(entry)
        return unless page_info(client, id)

        raise Error, "create-only page appeared before save: #{id}"
      end

      info = client.call('core.getPageInfo', page: id)
      revision = page_revision(info)
      unless revision.to_s == entry.fetch('source_revision').to_s
        raise Error, "production revision drift before save for #{id}: #{revision.inspect}"
      end

      content = client.call('core.getPage', page: id)
      return if Digest::SHA256.hexdigest(content) == entry.fetch('source_sha256')

      raise Error, "production content drift before save for #{id}"
    end

    def verify_source_deletion!(client, entry)
      id = entry.fetch('id')
      info = page_info(client, id)
      raise Error, "page selected for deletion disappeared before save: #{id}" unless info

      revision = page_revision(info)
      unless revision.to_s == entry.fetch('source_revision').to_s
        raise Error, "production deletion revision drift before save for #{id}: #{revision.inspect}"
      end

      content = client.call('core.getPage', page: id)
      return if Digest::SHA256.hexdigest(content) == entry.fetch('source_sha256')

      raise Error, "production deletion source drift before save for #{id}"
    end

    def verify_write_access!(client, states: nil)
      identity = client.call('core.whoAmI')
      login = identity.is_a?(Hash) ? identity['login'] : nil
      raise Error, 'DokuWiki API identity is anonymous' if login.nil? || login.empty?

      @manifest.pages.each do |entry|
        create = page_create?(entry)
        required = create ? ACL_CREATE : ACL_EDIT
        verify_acl!(client, entry.fetch('id'), required, create ? 'create page' : 'edit page')
      end
      @manifest.media.each do |entry|
        required = media_exists?(client, entry.fetch('id')) ? ACL_DELETE : ACL_UPLOAD
        verify_acl!(client, entry.fetch('id'), required, 'write media')
      end
      @manifest.deletions.each do |entry|
        next if states && states.fetch(:deletions).fetch(entry.fetch('id')) == :candidate

        verify_acl!(client, entry.fetch('id'), ACL_DELETE, 'delete page')
      end
    end

    def verify_acl!(client, id, required, action)
      actual = client.call('core.aclCheck', page: id).to_i
      return if actual >= required

      raise Error, "insufficient ACL to #{action} #{id}: #{actual} < #{required}"
    end

    def save_media!(client)
      @manifest.media.each do |entry|
        exists = media_exists?(client, entry.fetch('id'))
        policy = entry.fetch('policy', 'create')
        if exists
          current = Base64.strict_decode64(client.call('core.getMedia', media: entry.fetch('id')))
          current_hash = Digest::SHA256.hexdigest(current)
          next if current_hash == entry.fetch('sha256')

          if policy == 'create'
            raise Error, "create-only media already exists with different content: #{entry.fetch('id')}"
          elsif current_hash != entry.fetch('source_sha256')
            raise Error, "update media source drift: #{entry.fetch('id')}"
          end
        elsif policy == 'update' && !exists
          raise Error, "update-only media does not exist: #{entry.fetch('id')}"
        end

        result = client.call(
          'core.saveMedia',
          media: entry.fetch('id'),
          base64: Base64.strict_encode64(@manifest.read(entry)),
          overwrite: exists
        )
        raise Error, "failed to save media #{entry.fetch('id')}" unless result == true
      end
    end

    def verify_client!(client)
      @manifest.pages.each do |entry|
        actual = client.call('core.getPage', page: entry.fetch('id'))
        expected = @manifest.read(entry).force_encoding(Encoding::UTF_8)
        raise Error, "page verification failed: #{entry.fetch('id')}" unless actual == expected
      end
      @manifest.media.each do |entry|
        encoded = client.call('core.getMedia', media: entry.fetch('id'))
        actual = Digest::SHA256.hexdigest(Base64.strict_decode64(encoded))
        raise Error, "media verification failed: #{entry.fetch('id')}" unless actual == entry.fetch('sha256')
      end
      @manifest.deletions.each do |entry|
        next unless page_info(client, entry.fetch('id'))

        raise Error, "deleted page verification failed: #{entry.fetch('id')}"
      end
    end

    def verify_revision_summaries!(wiki, report: false)
      return unless @manifest.per_page_summaries?

      rows = @manifest.pages.map { |entry| ['write', entry] } +
             @manifest.deletions.map { |entry| ['delete', entry] }
      verified = rows.map do |action, entry|
        url = verify_entry_summary!(wiki, entry)
        [action, entry.fetch('id'), entry.fetch('summary'), url]
      end
      if report
        @out.puts('verified revision summaries:')
        verified.each do |action, id, summary, url|
          @out.puts("  #{action} #{id}: #{summary}")
          @out.puts("    history: #{url}")
        end
      end
      verified
    end

    def verify_entry_summary!(wiki, entry)
      raise Error, 'revision history reader is required for schema 4 or 5' unless @revision_history

      @revision_history.verify!(wiki, entry.fetch('id'), entry.fetch('summary'))
    rescue Error => e
      if wiki.end_with?('-staging')
        raise Error, "#{e.message}; reset staging and stage this manifest again"
      end

      raise
    end

    def media_exists?(client, id)
      client.call('core.getMediaInfo', media: id)
      true
    rescue KbPage::RpcError => e
      raise unless e.rpc_message =~ /(does not exist|not exist|not found|doesn't exist)/i

      false
    end

    def page_create?(entry)
      @manifest.page_policy(entry) == 'create'
    end

    def page_info(client, id)
      client.call('core.getPageInfo', page: id)
    rescue KbPage::RpcError => e
      raise unless e.rpc_message =~ /(does not exist|not exist|not found|doesn't exist)/i

      nil
    end

    def page_revision(info)
      info['rev'] || info['lastModified'] || info['revision']
    end

    def verify_language_links!
      if @manifest.wiki == 'cz'
        pages = @manifest.pages.to_h do |entry|
          [entry.fetch('id'), @manifest.read(entry).force_encoding(Encoding::UTF_8)]
        end
        @language_links.warm_and_verify(pages)
      else
        pairs = @manifest.pages.map do |entry|
          [entry.fetch('language_counterpart'), entry.fetch('id')]
        end
        @language_links.warm_and_verify_pairs(pairs)
      end
    end

    def verify_pending!
      pending = JSON.parse(File.read(KbStage.pending_release_path))
      unless pending.fetch('sha256') == @manifest.digest && pending.fetch('slug') == KbStage.current_slug
        raise Error, 'pending release does not match this manifest and session'
      end
      expected_ref = @manifest.managed_ref || 'master'
      unless pending.fetch('managed_ref', 'master') == expected_ref
        raise Error, 'pending release uses a different managed repository ref'
      end
    rescue Errno::ENOENT
      raise Error, 'this manifest has not been staged'
    end
  end
end
