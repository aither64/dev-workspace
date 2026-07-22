# frozen_string_literal: true

require 'digest'
require 'minitest/autorun'
require 'stringio'
require 'tmpdir'
require 'yaml'

require_relative '../lib/kb_stage'
load File.expand_path('../bin/kb-page', __dir__)
require_relative '../lib/kb_release'

class KbStageTest < Minitest::Test
  class FakeClient
    def initialize
      @expected = []
    end

    def expect(method, params = nil, result:)
      @expected << [method, params, result]
    end

    def call(method, params = nil)
      expected = @expected.shift
      raise "unexpected call #{[method, params].inspect}" unless expected

      exp_method, exp_params, result = expected
      unless [exp_method, exp_params] == [method, params]
        raise "expected #{[exp_method, exp_params].inspect}, got #{[method, params].inspect}"
      end

      raise result if result.is_a?(Exception)

      result
    end

    def done?
      @expected.empty?
    end
  end

  def test_claim_serializes_owners_and_release_refuses_pending_bundle
    with_state do
      stub_kb_stage(:current_slug, 'session-one') do
        KbStage.claim!
        assert_equal('session-one', KbStage.owner.fetch('slug'))
        KbStage.claim!
        KbStage.write_json(KbStage.pending_release_path, 'sha256' => 'test')
        assert_raises(KbStage::Error) { KbStage.release! }
        KbStage.release!(discard_pending: true)
        assert_nil(KbStage.owner)
      end
    end
  end

  def test_claim_refuses_a_different_session
    with_state do
      stub_kb_stage(:current_slug, 'session-one') { KbStage.claim! }
      error = stub_kb_stage(:current_slug, 'session-two') do
        assert_raises(KbStage::Error) { KbStage.claim! }
      end
      assert_match(/session-one/, error.message)
    end
  end

  def test_staging_mutation_invalidates_pending_release
    with_state do
      stub_kb_stage(:current_slug, 'session-one') do
        KbStage.claim!
        KbStage.write_json(KbStage.pending_release_path, 'sha256' => 'test')
        KbStage.with_staging_mutation { true }
        refute_path_exists(KbStage.pending_release_path)
      end
    end
  end

  def test_credentials_are_generated_outside_the_repository
    with_state do |state, codex|
      KbStage.ensure_credentials!
      %w[cz org].each do |site|
        password = File.join(codex, "codex-kb-staging-#{site}-aither-password")
        users = File.join(state, 'credentials', "#{site}.users.auth.php")
        assert(File.size?(password))
        assert_match(/\Aaither:\$6\$/, File.read(users))
        assert_equal(0o600, File.stat(password).mode & 0o777)
        assert_equal(0o644, File.stat(users).mode & 0o777)
      end
    end
  end

  def test_container_status_uses_lowercase_nixos_container_output
    successful = Object.new
    successful.define_singleton_method(:success?) { true }
    failed = Object.new
    failed.define_singleton_method(:success?) { false }

    assert(KbStage.container_running?(runner: ->(*_args) { ["up\n", successful] }))
    refute(KbStage.container_running?(runner: ->(*_args) { ["down\n", successful] }))
    refute(KbStage.container_running?(runner: ->(*_args) { ["UP\n", successful] }))
    refute(KbStage.container_running?(runner: ->(*_args) { ["up\n", failed] }))
  end

  def test_language_links_warm_english_first_and_verify_both_pages
    calls = []
    pages = {
      'informace:novacci' => "<page>information:new_members</page>\ntext\n",
      'navody:server:ssh' => "unpaired\n"
    }
    czech = 'http://kb-cs.aitherdev.int.vpsfree.cz/informace/novacci'
    english = 'http://kb-en.aitherdev.int.vpsfree.cz/information/new_members'
    html = %(<a href="#{czech}">cs</a><a href="#{english}">en</a>)
    links = KbStage::LanguageLinks.new(
      fetcher: ->(url) { calls << url; html },
      out: StringIO.new
    )

    assert_equal(1, links.warm_and_verify(pages))
    assert_equal([english, czech, english, czech], calls)
  end

  def test_language_links_reject_an_incomplete_pair
    pages = { 'informace:novacci' => '<page>information:new_members</page>' }
    links = KbStage::LanguageLinks.new(fetcher: ->(_url) { '<a href="only-one">cs</a>' })

    error = assert_raises(KbStage::Error) { links.warm_and_verify(pages) }
    assert_match(/informace:novacci/, error.message)
  end

  def test_language_links_can_verify_explicit_pairs_for_english_releases
    calls = []
    czech = 'http://kb-cs.aitherdev.int.vpsfree.cz/navody/vps/zalohy'
    english = 'http://kb-en.aitherdev.int.vpsfree.cz/manuals/vps/backups'
    html = %(<a href="#{czech}">cs</a><a href="#{english}">en</a>)
    links = KbStage::LanguageLinks.new(
      fetcher: ->(url) { calls << url; html },
      out: StringIO.new
    )

    assert_equal(1, links.warm_and_verify_pairs([['navody:vps:zalohy', 'manuals:vps:backups']]))
    assert_equal([english, czech, english, czech], calls)
  end

  def test_english_release_manifest_requires_language_counterparts
    Dir.mktmpdir do |release_dir|
      File.write(File.join(release_dir, 'page.txt'), "candidate\n")
      manifest_path = File.join(release_dir, 'release.yml')
      File.write(
        manifest_path,
        YAML.dump(
          'schema' => 1,
          'wiki' => 'org',
          'pages' => [
            {
              'id' => 'manuals:vps:backups',
              'source_revision' => 123,
              'source_sha256' => Digest::SHA256.hexdigest("source\n"),
              'file' => 'page.txt',
              'sha256' => Digest::SHA256.hexdigest("candidate\n")
            }
          ],
          'media' => []
        )
      )

      error = assert_raises(KbRelease::Error) { KbRelease::Manifest.new(manifest_path) }
      assert_match(/language_counterpart/, error.message)
    end
  end

  def test_release_manifest_rejects_multiline_production_summary
    Dir.mktmpdir do |release_dir|
      manifest_path = File.join(release_dir, 'release.yml')
      File.write(
        manifest_path,
        YAML.dump(
          'schema' => 1,
          'wiki' => 'cz',
          'production_summary' => "first line\nsecond line",
          'pages' => [],
          'media' => []
        )
      )

      error = assert_raises(KbRelease::Error) { KbRelease::Manifest.new(manifest_path) }
      assert_match(/single line/, error.message)
    end
  end

  def test_schema_two_release_manifest_requires_production_summary
    Dir.mktmpdir do |release_dir|
      manifest_path = File.join(release_dir, 'release.yml')
      File.write(
        manifest_path,
        YAML.dump('schema' => 2, 'wiki' => 'org', 'pages' => [], 'media' => [])
      )

      error = assert_raises(KbRelease::Error) { KbRelease::Manifest.new(manifest_path) }
      assert_match(/production_summary/, error.message)
    end
  end

  def test_release_manifest_rejects_blank_production_summary
    Dir.mktmpdir do |release_dir|
      manifest_path = File.join(release_dir, 'release.yml')
      File.write(
        manifest_path,
        YAML.dump(
          'schema' => 2,
          'wiki' => 'org',
          'production_summary' => '   ',
          'pages' => [],
          'media' => []
        )
      )

      error = assert_raises(KbRelease::Error) { KbRelease::Manifest.new(manifest_path) }
      assert_match(/non-empty single line/, error.message)
    end
  end

  def test_english_release_verifies_every_explicit_counterpart_pair
    Dir.mktmpdir do |release_dir|
      pages = {
        'manuals:vps:backups' => 'navody:vps:zalohy',
        'manuals:vps:console' => 'navody:vps:konzole'
      }.map do |id, counterpart|
        file = "#{id.tr(':', '-')}.txt"
        content = "candidate for #{id}\n"
        File.write(File.join(release_dir, file), content)
        {
          'id' => id,
          'language_counterpart' => counterpart,
          'source_revision' => 123,
          'source_sha256' => Digest::SHA256.hexdigest("source for #{id}\n"),
          'file' => file,
          'sha256' => Digest::SHA256.hexdigest(content)
        }
      end
      manifest_path = File.join(release_dir, 'release.yml')
      File.write(
        manifest_path,
        YAML.dump('schema' => 1, 'wiki' => 'org', 'pages' => pages, 'media' => [])
      )
      received = nil
      language_links = Object.new
      language_links.define_singleton_method(:warm_and_verify_pairs) do |pairs|
        received = pairs
      end
      runner = KbRelease::Runner.new(
        manifest: KbRelease::Manifest.new(manifest_path),
        client_factory: ->(_name) { raise 'must not connect' },
        language_links:,
        out: StringIO.new
      )

      runner.send(:verify_language_links!)

      assert_equal(
        [
          ['navody:vps:zalohy', 'manuals:vps:backups'],
          ['navody:vps:konzole', 'manuals:vps:console']
        ],
        received
      )
    end
  end

  def test_release_stages_only_when_production_and_staging_match_source
    with_state do
      Dir.mktmpdir do |release_dir|
        source = "old\n"
        candidate = "new\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 1,
            'wiki' => 'cz',
            'pages' => [
              {
                'id' => 'page',
                'source_revision' => 123,
                'source_sha256' => Digest::SHA256.hexdigest(source),
                'file' => 'page.txt',
                'sha256' => Digest::SHA256.hexdigest(candidate)
              }
            ],
            'media' => []
          )
        )

        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'page' }, result: { 'revision' => 123 })
        production.expect('core.getPage', { page: 'page' }, result: source)
        staging = FakeClient.new
        staging.expect('core.getPage', { page: 'page' }, result: source)
        staging.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        staging.expect('core.aclCheck', { page: 'page' }, result: 255)
        staging.expect(
          'core.savePage',
          {
            page: 'page', text: candidate, summary: 'Stage reviewed KB release', isminor: false
          },
          result: true
        )
        staging.expect('core.getPage', { page: 'page' }, result: candidate)

        clients = { 'cz' => production, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          out: StringIO.new
        )

        stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          stub_kb_stage(:current_slug, 'session-one') { runner.stage! }
        end

        assert(production.done?)
        assert(staging.done?)
        assert_equal('session-one', JSON.parse(File.read(KbStage.pending_release_path)).fetch('slug'))
      end
    end
  end

  def test_release_stage_can_retry_when_candidate_pages_were_already_saved
    with_state do
      Dir.mktmpdir do |release_dir|
        source = "old\n"
        candidate = "new\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 1,
            'wiki' => 'cz',
            'pages' => [
              {
                'id' => 'page',
                'source_revision' => 123,
                'source_sha256' => Digest::SHA256.hexdigest(source),
                'file' => 'page.txt',
                'sha256' => Digest::SHA256.hexdigest(candidate)
              }
            ],
            'media' => []
          )
        )

        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'page' }, result: { 'revision' => 123 })
        production.expect('core.getPage', { page: 'page' }, result: source)
        staging = FakeClient.new
        staging.expect('core.getPage', { page: 'page' }, result: candidate)
        staging.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        staging.expect('core.aclCheck', { page: 'page' }, result: 255)
        staging.expect(
          'core.savePage',
          {
            page: 'page', text: candidate, summary: 'Stage reviewed KB release', isminor: false
          },
          result: true
        )
        staging.expect('core.getPage', { page: 'page' }, result: candidate)

        clients = { 'cz' => production, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          out: StringIO.new
        )

        stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          stub_kb_stage(:current_slug, 'session-one') { runner.stage! }
        end

        assert(production.done?)
        assert(staging.done?)
      end
    end
  end

  def test_release_stages_a_guarded_create_only_page
    with_state do
      Dir.mktmpdir do |release_dir|
        candidate = "<page>manuals:notifications</page>\n====== Notifikace ======\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 3,
            'wiki' => 'cz',
            'production_summary' => 'Přidat návod k notifikacím',
            'pages' => [
              {
                'id' => 'navody:notifikace',
                'policy' => 'create',
                'source_sha256' => Digest::SHA256.hexdigest(''),
                'file' => 'page.txt',
                'sha256' => Digest::SHA256.hexdigest(candidate)
              }
            ],
            'media' => []
          )
        )
        missing = KbPage::RpcError.new(221, 'The requested page does not exist')
        production = FakeClient.new
        production.expect(
          'core.getPageInfo',
          { page: 'navody:notifikace' },
          result: missing
        )
        staging = FakeClient.new
        staging.expect(
          'core.getPageInfo',
          { page: 'navody:notifikace' },
          result: missing
        )
        staging.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        staging.expect('core.aclCheck', { page: 'navody:notifikace' }, result: 255)
        staging.expect(
          'core.savePage',
          {
            page: 'navody:notifikace',
            text: candidate,
            summary: 'Stage reviewed KB release',
            isminor: false
          },
          result: true
        )
        staging.expect('core.getPage', { page: 'navody:notifikace' }, result: candidate)
        clients = { 'cz' => production, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          out: StringIO.new
        )

        stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          stub_kb_stage(:current_slug, 'session-one') { runner.stage! }
        end

        assert(production.done?)
        assert(staging.done?)
      end
    end
  end

  def test_release_refuses_create_only_page_that_appeared_in_production
    with_state do
      Dir.mktmpdir do |release_dir|
        candidate = "new page\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 3,
            'wiki' => 'cz',
            'production_summary' => 'Přidat nový návod',
            'pages' => [
              {
                'id' => 'navody:new',
                'policy' => 'create',
                'source_sha256' => Digest::SHA256.hexdigest(''),
                'file' => 'page.txt',
                'sha256' => Digest::SHA256.hexdigest(candidate)
              }
            ],
            'media' => []
          )
        )
        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'navody:new' }, result: { 'revision' => 999 })
        production.expect('core.getPage', { page: 'navody:new' }, result: "concurrent page\n")
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { name == 'cz' ? production : raise('must not stage') },
          out: StringIO.new
        )

        error = stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          assert_raises(KbRelease::Error) { runner.stage! }
        end
        assert_match(/create-only page already exists/, error.message)
        assert(production.done?)
      end
    end
  end

  def test_release_stages_create_only_media
    with_state do
      Dir.mktmpdir do |release_dir|
        candidate = "\x89PNG\r\n\x1a\nfixture".b
        File.binwrite(File.join(release_dir, 'capture.png'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 3,
            'wiki' => 'cz',
            'production_summary' => 'Přidat snímek notifikací',
            'pages' => [],
            'media' => [
              {
                'id' => 'cs:screenshots:vpsadmin:notifications:routes.png',
                'policy' => 'create',
                'file' => 'capture.png',
                'sha256' => Digest::SHA256.hexdigest(candidate)
              }
            ]
          )
        )
        missing = KbPage::RpcError.new(221, 'The requested media does not exist')
        staging = FakeClient.new
        staging.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        staging.expect(
          'core.getMediaInfo',
          { media: 'cs:screenshots:vpsadmin:notifications:routes.png' },
          result: missing
        )
        staging.expect(
          'core.aclCheck',
          { page: 'cs:screenshots:vpsadmin:notifications:routes.png' },
          result: 8
        )
        staging.expect(
          'core.getMediaInfo',
          { media: 'cs:screenshots:vpsadmin:notifications:routes.png' },
          result: missing
        )
        staging.expect(
          'core.saveMedia',
          {
            media: 'cs:screenshots:vpsadmin:notifications:routes.png',
            base64: Base64.strict_encode64(candidate),
            overwrite: false
          },
          result: true
        )
        staging.expect(
          'core.getMedia',
          { media: 'cs:screenshots:vpsadmin:notifications:routes.png' },
          result: Base64.strict_encode64(candidate)
        )
        clients = { 'cz' => FakeClient.new, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          out: StringIO.new
        )

        stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          stub_kb_stage(:current_slug, 'session-one') { runner.stage! }
        end

        assert(staging.done?)
      end
    end
  end

  def test_release_library_refuses_promotion_without_explicit_approval
    runner = KbRelease::Runner.new(
      manifest: Object.new,
      client_factory: ->(_name) { raise 'must not connect' },
      out: StringIO.new
    )

    error = assert_raises(KbRelease::Error) { runner.promote! }
    assert_match(/explicit approval/, error.message)
  end

  def test_promotion_refuses_staging_content_changed_after_review
    with_state do
      Dir.mktmpdir do |release_dir|
        candidate = "reviewed\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 1,
            'wiki' => 'cz',
            'pages' => [
              {
                'id' => 'page',
                'source_revision' => 123,
                'source_sha256' => Digest::SHA256.hexdigest("old\n"),
                'file' => 'page.txt',
                'sha256' => Digest::SHA256.hexdigest(candidate)
              }
            ],
            'media' => []
          )
        )
        manifest = KbRelease::Manifest.new(manifest_path)
        KbStage.write_json(
          KbStage.pending_release_path,
          'sha256' => manifest.digest,
          'slug' => 'session-one'
        )
        staging = FakeClient.new
        staging.expect('core.getPage', { page: 'page' }, result: "changed\n")
        runner = KbRelease::Runner.new(
          manifest:,
          client_factory: ->(name) { name == 'cz-staging' ? staging : raise('must not use production') },
          out: StringIO.new
        )

        error = stub_kb_stage(:with_owned_lock, ->(&block) { block.call }) do
          stub_kb_stage(:current_slug, 'session-one') do
            assert_raises(KbRelease::Error) do
              runner.promote!(approved_production: true)
            end
          end
        end
        assert_match(/page verification failed/, error.message)
        assert(staging.done?)
      end
    end
  end

  def test_promotion_retries_pages_already_saved_by_a_partial_attempt
    with_state do
      Dir.mktmpdir do |release_dir|
        sources = { 'one' => "old one\n", 'two' => "old two\n" }
        candidates = { 'one' => "new one\n", 'two' => "new two\n" }
        pages = candidates.map do |id, content|
          file = "#{id}.txt"
          File.write(File.join(release_dir, file), content)
          {
            'id' => id,
            'source_revision' => id == 'one' ? 101 : 102,
            'source_sha256' => Digest::SHA256.hexdigest(sources.fetch(id)),
            'file' => file,
            'sha256' => Digest::SHA256.hexdigest(content)
          }
        end
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 1,
            'wiki' => 'cz',
            'production_summary' => 'Aktualizovat snímky obrazovky',
            'pages' => pages,
            'media' => []
          )
        )
        manifest = KbRelease::Manifest.new(manifest_path)
        KbStage.write_json(
          KbStage.pending_release_path,
          'sha256' => manifest.digest,
          'slug' => 'session-one'
        )

        staging = FakeClient.new
        candidates.each do |id, content|
          staging.expect('core.getPage', { page: id }, result: content)
        end
        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'one' }, result: { 'revision' => 999 })
        production.expect('core.getPage', { page: 'one' }, result: candidates.fetch('one'))
        production.expect('core.getPageInfo', { page: 'two' }, result: { 'revision' => 102 })
        production.expect('core.getPage', { page: 'two' }, result: sources.fetch('two'))
        production.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        production.expect('core.aclCheck', { page: 'one' }, result: 255)
        production.expect('core.aclCheck', { page: 'two' }, result: 255)
        production.expect('core.getPageInfo', { page: 'two' }, result: { 'revision' => 102 })
        production.expect('core.getPage', { page: 'two' }, result: sources.fetch('two'))
        production.expect(
          'core.savePage',
          {
            page: 'two',
            text: candidates.fetch('two'),
            summary: 'Aktualizovat snímky obrazovky',
            isminor: false
          },
          result: true
        )
        candidates.each do |id, content|
          production.expect('core.getPage', { page: id }, result: content)
        end
        clients = { 'cz-staging' => staging, 'cz' => production }
        runner = KbRelease::Runner.new(
          manifest:,
          client_factory: ->(name) { clients.fetch(name) },
          out: StringIO.new
        )

        stub_kb_stage(:with_owned_lock, ->(&block) { block.call }) do
          stub_kb_stage(:current_slug, 'session-one') do
            runner.promote!(approved_production: true)
          end
        end
        assert(staging.done?)
        assert(production.done?)
        refute_path_exists(KbStage.pending_release_path)
      end
    end
  end

  def test_update_media_refuses_unrecorded_source_content
    with_state do
      Dir.mktmpdir do |release_dir|
        candidate = "candidate media"
        File.binwrite(File.join(release_dir, 'media.bin'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 1,
            'wiki' => 'cz',
            'pages' => [],
            'media' => [
              {
                'id' => 'media.bin',
                'file' => 'media.bin',
                'sha256' => Digest::SHA256.hexdigest(candidate),
                'policy' => 'update',
                'source_sha256' => Digest::SHA256.hexdigest('expected source')
              }
            ]
          )
        )
        staging = FakeClient.new
        staging.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        staging.expect('core.getMediaInfo', { media: 'media.bin' }, result: {})
        staging.expect('core.aclCheck', { page: 'media.bin' }, result: 255)
        staging.expect('core.getMediaInfo', { media: 'media.bin' }, result: {})
        staging.expect(
          'core.getMedia',
          { media: 'media.bin' },
          result: Base64.strict_encode64('unexpected source')
        )
        clients = { 'cz' => FakeClient.new, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          out: StringIO.new
        )

        error = stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          assert_raises(KbRelease::Error) { runner.stage! }
        end
        assert_match(/media source drift/, error.message)
        assert(staging.done?)
      end
    end
  end

  private

  def stub_kb_stage(name, value)
    original = KbStage.method(name)
    KbStage.define_singleton_method(name) do |*args, &block|
      value.respond_to?(:call) ? value.call(*args, &block) : value
    end
    yield
  ensure
    KbStage.define_singleton_method(name, original)
  end

  def with_state
    Dir.mktmpdir do |dir|
      state = File.join(dir, 'state')
      codex = File.join(dir, 'codex')
      old_state = ENV['KB_STAGE_STATE_DIR']
      old_codex = ENV['KB_STAGE_CODEX_DIR']
      ENV['KB_STAGE_STATE_DIR'] = state
      ENV['KB_STAGE_CODEX_DIR'] = codex
      yield state, codex
    ensure
      ENV['KB_STAGE_STATE_DIR'] = old_state
      ENV['KB_STAGE_CODEX_DIR'] = old_codex
    end
  end
end
