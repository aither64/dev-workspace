# frozen_string_literal: true

require 'base64'
require 'digest'
require 'json'
require 'time'
require 'yaml'

require_relative 'kb_stage'

module KbRelease
  class Error < StandardError; end

  class Manifest
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
      raise Error, 'release manifest schema must be 1' unless data['schema'] == 1
      raise Error, 'release wiki must be cz or org' unless %w[cz org].include?(data['wiki'])

      %w[pages media].each do |kind|
        entries = data.fetch(kind)
        raise Error, "#{kind} must be a list" unless entries.is_a?(Array)
        ids = entries.map { |entry| entry.fetch('id') }
        raise Error, "duplicate #{kind} IDs" unless ids.uniq.length == ids.length
      end
      pages.each do |entry|
        %w[id file sha256 source_revision source_sha256].each { |key| entry.fetch(key) }
      end
      media.each do |entry|
        %w[id file sha256].each { |key| entry.fetch(key) }
        policy = entry.fetch('policy', 'create')
        raise Error, "invalid media policy #{policy}" unless %w[create update].include?(policy)
        entry.fetch('source_sha256') if policy == 'update'
      end
    rescue KeyError => e
      raise Error, "incomplete release manifest: #{e.message}"
    end
  end

  class Runner
    ACL_EDIT = 2
    ACL_UPLOAD = 8
    ACL_DELETE = 16

    def initialize(manifest:, client_factory:, out: $stdout)
      @manifest = manifest
      @client_factory = client_factory
      @out = out
    end

    def stage!
      KbStage.with_staging_mutation do
        check_production_baseline!
        staging = @client_factory.call(@manifest.staging_wiki)
        verify_staging_baseline!(staging)
        verify_write_access!(staging)
        save_media!(staging)
        save_pages!(staging, summary: 'Stage reviewed KB release')
        verify_client!(staging)
        verify_language_links!
        KbStage.write_json(
          KbStage.pending_release_path,
          'manifest' => @manifest.path,
          'sha256' => @manifest.digest,
          'staged_at' => Time.now.utc.iso8601,
          'slug' => KbStage.current_slug
        )
      end
      @out.puts("staged #{@manifest.pages.length} pages and #{@manifest.media.length} media objects")
    end

    def verify!(environment)
      KbStage.verify_current_owner! if environment == :staging
      name = environment == :staging ? @manifest.staging_wiki : @manifest.wiki
      verify_client!(@client_factory.call(name))
      verify_language_links! if environment == :staging
      @out.puts("verified #{name}")
    end

    def promote!(approved_production: false)
      raise Error, 'production promotion requires explicit approval' unless approved_production

      KbStage.with_owned_lock do
        verify_pending!
        verify_client!(@client_factory.call(@manifest.staging_wiki))
        states = check_production_baseline!(allow_candidate: true)
        production = @client_factory.call(@manifest.wiki)
        verify_write_access!(production)
        save_media!(production)
        save_pages!(production, summary: 'Publish reviewed KB release', states:)
        verify_client!(production)
        File.delete(KbStage.pending_release_path)
      end
      @out.puts("promoted #{@manifest.pages.length} pages and #{@manifest.media.length} media objects")
    end

    private

    def check_production_baseline!(allow_candidate: false)
      production = @client_factory.call(@manifest.wiki)
      @manifest.pages.to_h do |entry|
        id = entry.fetch('id')
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
          [id, :candidate]
        else
          raise Error, "production content drift for #{id}"
        end
      end
    end

    def verify_staging_baseline!(client)
      @manifest.pages.each do |entry|
        content = client.call('core.getPage', page: entry.fetch('id'))
        hash = Digest::SHA256.hexdigest(content)
        unless [entry.fetch('source_sha256'), entry.fetch('sha256')].include?(hash)
          raise Error, "staging is not a clean production mirror at #{entry.fetch('id')}"
        end
      end
    end

    def save_pages!(client, summary:, states: nil)
      @manifest.pages.each do |entry|
        next if states && states.fetch(entry.fetch('id')) == :candidate
        verify_source_page!(client, entry) if states

        result = client.call(
          'core.savePage',
          page: entry.fetch('id'),
          text: @manifest.read(entry).force_encoding(Encoding::UTF_8),
          summary:,
          isminor: false
        )
        raise Error, "failed to save page #{entry.fetch('id')}" unless result == true
      end
    end

    def verify_source_page!(client, entry)
      id = entry.fetch('id')
      info = client.call('core.getPageInfo', page: id)
      revision = info['rev'] || info['lastModified'] || info['revision']
      unless revision.to_s == entry.fetch('source_revision').to_s
        raise Error, "production revision drift before save for #{id}: #{revision.inspect}"
      end

      content = client.call('core.getPage', page: id)
      return if Digest::SHA256.hexdigest(content) == entry.fetch('source_sha256')

      raise Error, "production content drift before save for #{id}"
    end

    def verify_write_access!(client)
      identity = client.call('core.whoAmI')
      login = identity.is_a?(Hash) ? identity['login'] : nil
      raise Error, 'DokuWiki API identity is anonymous' if login.nil? || login.empty?

      @manifest.pages.each do |entry|
        verify_acl!(client, entry.fetch('id'), ACL_EDIT, 'edit page')
      end
      @manifest.media.each do |entry|
        required = media_exists?(client, entry.fetch('id')) ? ACL_DELETE : ACL_UPLOAD
        verify_acl!(client, entry.fetch('id'), required, 'write media')
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
    end

    def media_exists?(client, id)
      client.call('core.getMediaInfo', media: id)
      true
    rescue KbPage::RpcError => e
      raise unless e.rpc_message =~ /(does not exist|not exist|not found|doesn't exist)/i

      false
    end

    def verify_language_links!
      return unless @manifest.wiki == 'cz'

      pages = @manifest.pages.to_h do |entry|
        [entry.fetch('id'), @manifest.read(entry).force_encoding(Encoding::UTF_8)]
      end
      KbStage::LanguageLinks.new(out: @out).warm_and_verify(pages)
    end

    def verify_pending!
      pending = JSON.parse(File.read(KbStage.pending_release_path))
      unless pending.fetch('sha256') == @manifest.digest && pending.fetch('slug') == KbStage.current_slug
        raise Error, 'pending release does not match this manifest and session'
      end
    rescue Errno::ENOENT
      raise Error, 'this manifest has not been staged'
    end
  end
end
