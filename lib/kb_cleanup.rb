# frozen_string_literal: true

require 'base64'
require 'digest'
require 'yaml'

module KbCleanup
  class Error < StandardError; end

  class Manifest
    attr_reader :data, :path

    def initialize(path)
      @path = File.expand_path(path)
      @data = YAML.safe_load_file(@path)
      validate!
    rescue Psych::SyntaxError => e
      raise Error, "invalid cleanup manifest: #{e.message}"
    end

    def delete_via
      data.fetch('delete_via')
    end

    def verify_wikis
      data.fetch('verify_wikis')
    end

    def pages
      data.fetch('pages')
    end

    def media
      data.fetch('shared_media')
    end

    def page_summary(entry)
      return entry.fetch('summary') if data.fetch('schema') == 2

      'Remove obsolete localization review draft'
    end

    private

    def validate!
      unless [1, 2].include?(data['schema'])
        raise Error, 'cleanup manifest schema must be 1 or 2'
      end
      raise Error, 'delete_via must be cz or org' unless %w[cz org].include?(delete_via)
      unless verify_wikis.is_a?(Array) && verify_wikis.sort == %w[cz org]
        raise Error, 'verify_wikis must contain cz and org exactly once'
      end

      pages.each do |entry|
        %w[wiki id sha256].each { |key| entry.fetch(key) }
        raise Error, "invalid page wiki #{entry['wiki']}" unless verify_wikis.include?(entry['wiki'])
        validate_digest!(entry.fetch('sha256'), "page #{entry.fetch('id')}")
        validate_summary!(entry.fetch('summary'), entry.fetch('id')) if data.fetch('schema') == 2
      end
      media.each do |entry|
        %w[id sha256].each { |key| entry.fetch(key) }
        validate_digest!(entry.fetch('sha256'), "media #{entry.fetch('id')}")
      end

      page_ids = pages.map { |entry| [entry.fetch('wiki'), entry.fetch('id')] }
      raise Error, 'duplicate page IDs in cleanup manifest' unless page_ids.uniq.length == page_ids.length
      media_ids = media.map { |entry| entry.fetch('id') }
      raise Error, 'duplicate media IDs in cleanup manifest' unless media_ids.uniq.length == media_ids.length
    rescue KeyError => e
      raise Error, "incomplete cleanup manifest: #{e.message}"
    end

    def validate_digest!(value, description)
      return if value.match?(/\A[0-9a-f]{64}\z/)

      raise Error, "invalid SHA-256 for #{description}"
    end

    def validate_summary!(value, id)
      return if value.is_a?(String) && !value.strip.empty? && !value.match?(/[\r\n]/)

      raise Error, "page summary for #{id} must be a non-empty single line"
    end
  end

  class Runner
    ACL_DELETE = 16

    def initialize(manifest:, client_factory:, out: $stdout)
      @manifest = manifest
      @client_factory = client_factory
      @out = out
      @clients = {}
    end

    def cleanup!
      states = preflight!
      deleted_pages = delete_pages!(states.fetch(:pages))
      deleted_media = delete_media!(states.fetch(:media))
      verify_absent!
      @out.puts(
        "deleted #{deleted_pages} pages and #{deleted_media} shared media objects " \
        "(#{@manifest.pages.length - deleted_pages} pages and " \
        "#{@manifest.media.length - deleted_media} media already absent)"
      )
    end

    def check!
      states = preflight!
      present_pages = states.fetch(:pages).values.count(true)
      present_media = states.fetch(:media).values.count(true)
      @out.puts(
        "verified #{present_pages}/#{@manifest.pages.length} pages and " \
        "#{present_media}/#{@manifest.media.length} shared media objects"
      )
    end

    private

    def preflight!
      verify_identities!
      page_states = @manifest.pages.to_h do |entry|
        client = client(entry.fetch('wiki'))
        exists = page_exists?(client, entry.fetch('id'))
        if exists
          verify_page_hash!(client, entry)
          verify_acl!(client, entry.fetch('id'), 'delete page')
        end
        (@manifest.verify_wikis - [entry.fetch('wiki')]).each do |wiki|
          next unless page_exists?(client(wiki), entry.fetch('id'))

          raise Error, "unexpected page exists on non-owning wiki #{wiki}: #{entry.fetch('id')}"
        end
        [[entry.fetch('wiki'), entry.fetch('id')], exists]
      end

      media_states = @manifest.media.to_h do |entry|
        states = @manifest.verify_wikis.to_h do |wiki|
          wiki_client = client(wiki)
          exists = media_exists?(wiki_client, entry.fetch('id'))
          verify_media_hash!(wiki_client, entry) if exists
          [wiki, exists]
        end
        unless states.values.uniq.length == 1
          raise Error, "shared media visibility differs between wikis: #{entry.fetch('id')}"
        end
        verify_acl!(client(@manifest.delete_via), entry.fetch('id'), 'delete media') if states.values.first
        [entry.fetch('id'), states.values.first]
      end

      { pages: page_states, media: media_states }
    end

    def verify_identities!
      mutation_wikis = (@manifest.pages.map { |entry| entry.fetch('wiki') } + [@manifest.delete_via]).uniq
      mutation_wikis.each do |wiki|
        identity = client(wiki).call('core.whoAmI')
        login = identity.is_a?(Hash) ? identity['login'] : nil
        raise Error, "DokuWiki API identity is anonymous on #{wiki}" if login.nil? || login.empty?
      end
    end

    def delete_pages!(states)
      @manifest.pages.count do |entry|
        key = [entry.fetch('wiki'), entry.fetch('id')]
        next false unless states.fetch(key)

        result = client(entry.fetch('wiki')).call(
          'core.savePage',
          page: entry.fetch('id'),
          text: '',
          summary: @manifest.page_summary(entry),
          isminor: false
        )
        raise Error, "failed to delete page #{entry.fetch('id')}" unless result == true

        true
      end
    end

    def delete_media!(states)
      @manifest.media.count do |entry|
        next false unless states.fetch(entry.fetch('id'))

        result = client(@manifest.delete_via).call('core.deleteMedia', media: entry.fetch('id'))
        raise Error, "failed to delete media #{entry.fetch('id')}" unless result == true

        true
      end
    end

    def verify_absent!
      @manifest.pages.each do |entry|
        @manifest.verify_wikis.each do |wiki|
          next unless page_exists?(client(wiki), entry.fetch('id'))

          raise Error, "page still exists on #{wiki}: #{entry.fetch('id')}"
        end
      end
      @manifest.media.each do |entry|
        @manifest.verify_wikis.each do |wiki|
          next unless media_exists?(client(wiki), entry.fetch('id'))

          raise Error, "media still exists on #{wiki}: #{entry.fetch('id')}"
        end
      end
    end

    def verify_page_hash!(client, entry)
      content = client.call('core.getPage', page: entry.fetch('id'))
      actual = Digest::SHA256.hexdigest(content)
      return if actual == entry.fetch('sha256')

      raise Error, "draft page content changed: #{entry.fetch('id')}"
    end

    def verify_media_hash!(client, entry)
      encoded = client.call('core.getMedia', media: entry.fetch('id'))
      actual = Digest::SHA256.hexdigest(Base64.strict_decode64(encoded))
      return if actual == entry.fetch('sha256')

      raise Error, "draft media content changed: #{entry.fetch('id')}"
    end

    def verify_acl!(client, id, action)
      actual = client.call('core.aclCheck', page: id).to_i
      return if actual >= ACL_DELETE

      raise Error, "insufficient ACL to #{action} #{id}: #{actual} < #{ACL_DELETE}"
    end

    def client(wiki)
      @clients[wiki] ||= @client_factory.call(wiki)
    end

    def page_exists?(client, id)
      client.call('core.getPageInfo', page: id)
      true
    rescue KbPage::RpcError => e
      raise unless missing?(e)

      false
    end

    def media_exists?(client, id)
      client.call('core.getMediaInfo', media: id)
      true
    rescue KbPage::RpcError => e
      raise unless missing?(e)

      false
    end

    def missing?(error)
      error.rpc_message =~ /(does not exist|not exist|not found|doesn't exist)/i
    end
  end
end
