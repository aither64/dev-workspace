# frozen_string_literal: true

require 'base64'
require 'minitest/autorun'
require 'stringio'
require 'tempfile'

load File.expand_path('../bin/kb-page', __dir__)
require_relative '../lib/kb_cleanup'

class KbCleanupTest < Minitest::Test
  class FakeClient
    attr_reader :calls

    def initialize(pages: {}, media: {}, media_store: nil, acl: 16, login: 'aither')
      @pages = pages.dup
      @media = media_store || media.dup
      @acl = acl
      @login = login
      @calls = []
    end

    def call(method, params = nil)
      @calls << [method, params]
      case method
      when 'core.whoAmI' then { 'login' => @login }
      when 'core.aclCheck' then @acl
      when 'core.getPageInfo'
        missing! unless @pages.key?(params.fetch(:page))
        { 'id' => params.fetch(:page) }
      when 'core.getPage'
        @pages.fetch(params.fetch(:page)) { missing! }
      when 'core.savePage'
        @pages.delete(params.fetch(:page))
        true
      when 'core.getMediaInfo'
        missing! unless @media.key?(params.fetch(:media))
        { 'id' => params.fetch(:media) }
      when 'core.getMedia'
        Base64.strict_encode64(@media.fetch(params.fetch(:media)) { missing! })
      when 'core.deleteMedia'
        @media.delete(params.fetch(:media))
        true
      else
        raise "unsupported fake call #{method}"
      end
    end

    private

    def missing!
      raise KbPage::RpcError.new(-1, 'not found')
    end
  end

  def test_deletes_matching_pages_and_shared_media_once
    page = 'draft page'
    media = "\x89PNG\r\n\x1a\nmedia".b
    manifest = manifest_for(page:, media:)
    shared_media = { 'drafts:test:image.png' => media }
    cz = FakeClient.new(pages: { 'drafts:test:page' => page }, media_store: shared_media)
    org = FakeClient.new(media_store: shared_media)

    out = StringIO.new
    KbCleanup::Runner.new(
      manifest:,
      client_factory: ->(name) { { 'cz' => cz, 'org' => org }.fetch(name) },
      out:
    ).cleanup!

    assert_includes out.string, 'deleted 1 pages and 1 shared media objects'
    assert_equal 1, cz.calls.count { |method, _params| method == 'core.deleteMedia' }
    assert_equal 0, org.calls.count { |method, _params| method == 'core.deleteMedia' }
  end

  def test_schema_two_uses_the_page_specific_deletion_summary
    page = 'draft page'
    manifest = manifest_with_entries(
      pages: { 'drafts:test:page' => page },
      media: {},
      schema: 2,
      summary: 'Remove superseded test draft'
    )
    cz = FakeClient.new(pages: { 'drafts:test:page' => page })
    org = FakeClient.new

    KbCleanup::Runner.new(
      manifest:,
      client_factory: ->(name) { { 'cz' => cz, 'org' => org }.fetch(name) },
      out: StringIO.new
    ).cleanup!

    save = cz.calls.find { |method, _params| method == 'core.savePage' }
    assert_equal('Remove superseded test draft', save.last.fetch(:summary))
  end

  def test_schema_two_requires_a_single_line_page_summary
    error = assert_raises(KbCleanup::Error) do
      manifest_with_entries(
        pages: { 'drafts:test:page' => 'draft page' },
        media: {},
        schema: 2,
        summary: "first\nsecond"
      )
    end

    assert_match(/non-empty single line/, error.message)
  end

  def test_is_idempotent_when_everything_is_absent
    manifest = manifest_for(page: 'draft page', media: 'media')
    clients = { 'cz' => FakeClient.new, 'org' => FakeClient.new }
    out = StringIO.new

    KbCleanup::Runner.new(
      manifest:,
      client_factory: ->(name) { clients.fetch(name) },
      out:
    ).cleanup!

    assert_includes out.string, '0 pages and 0 shared media objects'
  end

  def test_aborts_before_deletion_when_a_page_changed
    manifest = manifest_for(page: 'expected', media: 'media')
    cz = FakeClient.new(
      pages: { 'drafts:test:page' => 'changed' },
      media: { 'drafts:test:image.png' => 'media' }
    )
    org = FakeClient.new(media: { 'drafts:test:image.png' => 'media' })

    error = assert_raises(KbCleanup::Error) do
      KbCleanup::Runner.new(
        manifest:,
        client_factory: ->(name) { { 'cz' => cz, 'org' => org }.fetch(name) }
      ).cleanup!
    end

    assert_match(/draft page content changed/, error.message)
    refute cz.calls.any? { |method, _params| %w[core.savePage core.deleteMedia].include?(method) }
  end

  def test_aborts_when_shared_media_visibility_differs
    manifest = manifest_for(page: 'page', media: 'media')
    cz = FakeClient.new(media: { 'drafts:test:image.png' => 'media' })
    org = FakeClient.new

    error = assert_raises(KbCleanup::Error) do
      KbCleanup::Runner.new(
        manifest:,
        client_factory: ->(name) { { 'cz' => cz, 'org' => org }.fetch(name) }
      ).cleanup!
    end

    assert_match(/visibility differs/, error.message)
  end

  def test_aborts_before_deletion_when_page_exists_on_non_owning_wiki
    manifest = manifest_for(page: 'page', media: 'media')
    cz = FakeClient.new(pages: { 'drafts:test:page' => 'page' })
    org = FakeClient.new(pages: { 'drafts:test:page' => 'unexpected' })

    error = assert_raises(KbCleanup::Error) do
      KbCleanup::Runner.new(
        manifest:,
        client_factory: ->(name) { { 'cz' => cz, 'org' => org }.fetch(name) }
      ).cleanup!
    end

    assert_match(/unexpected page exists on non-owning wiki org/, error.message)
    refute cz.calls.any? { |method, _params| %w[core.savePage core.deleteMedia].include?(method) }
  end

  def test_resumes_from_mixed_partial_state
    shared_media = { 'drafts:test:image-2.png' => 'media 2' }
    clients = {
      'cz' => FakeClient.new(
        pages: { 'drafts:test:page-2' => 'page 2' },
        media_store: shared_media
      ),
      'org' => FakeClient.new(media_store: shared_media)
    }
    manifest = manifest_with_entries(
      pages: {
        'drafts:test:page-1' => 'page 1',
        'drafts:test:page-2' => 'page 2'
      },
      media: {
        'drafts:test:image-1.png' => 'media 1',
        'drafts:test:image-2.png' => 'media 2'
      }
    )
    out = StringIO.new
    runner = KbCleanup::Runner.new(
      manifest:,
      client_factory: ->(name) { clients.fetch(name) },
      out:
    )

    runner.cleanup!
    assert_includes out.string, 'deleted 1 pages and 1 shared media objects'
    runner.cleanup!
    assert_includes out.string, 'deleted 0 pages and 0 shared media objects'
  end

  def test_aborts_before_deletion_when_shared_media_content_differs
    manifest = manifest_for(page: 'page', media: 'expected')
    cz = FakeClient.new(
      pages: { 'drafts:test:page' => 'page' },
      media: { 'drafts:test:image.png' => 'expected' }
    )
    org = FakeClient.new(media: { 'drafts:test:image.png' => 'changed' })

    error = assert_raises(KbCleanup::Error) do
      KbCleanup::Runner.new(
        manifest:,
        client_factory: ->(name) { { 'cz' => cz, 'org' => org }.fetch(name) }
      ).cleanup!
    end

    assert_match(/draft media content changed/, error.message)
    refute cz.calls.any? { |method, _params| %w[core.savePage core.deleteMedia].include?(method) }
  end

  def test_check_only_reads_matching_objects
    page = 'page'
    media = 'media'
    shared_media = { 'drafts:test:image.png' => media }
    clients = {
      'cz' => FakeClient.new(
        pages: { 'drafts:test:page' => page },
        media_store: shared_media
      ),
      'org' => FakeClient.new(media_store: shared_media)
    }
    out = StringIO.new

    KbCleanup::Runner.new(
      manifest: manifest_for(page:, media:),
      client_factory: ->(name) { clients.fetch(name) },
      out:
    ).check!

    assert_includes out.string, 'verified 1/1 pages and 1/1 shared media objects'
    clients.each_value do |client|
      refute client.calls.any? { |method, _params| %w[core.savePage core.deleteMedia].include?(method) }
    end
  end

  private

  def manifest_for(page:, media:)
    manifest_with_entries(
      pages: { 'drafts:test:page' => page },
      media: { 'drafts:test:image.png' => media }
    )
  end

  def manifest_with_entries(
    pages:,
    media:,
    schema: 1,
    summary: 'Remove obsolete localization review draft'
  )
    file = Tempfile.new(['cleanup', '.yml'])
    file.write(
      YAML.dump(
        'schema' => schema,
        'delete_via' => 'cz',
        'verify_wikis' => %w[cz org],
        'pages' => pages.map do |id, content|
          entry = {
            'wiki' => 'cz',
            'id' => id,
            'sha256' => Digest::SHA256.hexdigest(content)
          }
          entry['summary'] = summary if schema == 2
          entry
        end,
        'shared_media' => media.map do |id, content|
          {
            'id' => id,
            'sha256' => Digest::SHA256.hexdigest(content)
          }
        end
      )
    )
    file.close
    KbCleanup::Manifest.new(file.path)
  end
end
