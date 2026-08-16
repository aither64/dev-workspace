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

  class FakeRevisionHistory
    attr_reader :calls

    def initialize(summaries)
      @summaries = summaries
      @calls = []
    end

    def verify!(wiki, id, expected)
      @calls << [wiki, id, expected]
      actual = @summaries.fetch([wiki, id])
      unless actual == expected
        raise KbRelease::Error,
              "revision summary differs for #{id}: expected #{expected.inspect}, got #{actual.inspect}"
      end

      "https://history.example.test/#{wiki}/#{id.tr(':', '/')}"
    end
  end

  class NoopLanguageLinks
    def warm_and_verify(_pages); end

    def warm_and_verify_pairs(_pairs); end
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

  def test_managed_repository_ref_is_public_validated_and_preserves_pending_release
    with_state do |state, _codex|
      KbStage.ensure_credentials!
      assert_equal('master', KbStage.write_managed_ref!('master'))
      assert_equal('master', KbStage.read_managed_ref)
      assert_equal(0o644, File.stat(KbStage.managed_ref_path).mode & 0o777)

      commit = 'a' * 40
      KbStage.write_json(KbStage.pending_release_path, 'managed_ref' => commit)
      assert_equal(commit, KbStage.prepare_managed_ref!)
      assert_equal(commit, File.read(KbStage.managed_ref_path).strip)

      FileUtils.rm_f(KbStage.pending_release_path)
      assert_equal('master', KbStage.prepare_managed_ref!)
      assert_raises(KbStage::Error) { KbStage.write_managed_ref!('feature-branch') }
      assert_raises(KbStage::Error) { KbStage.write_managed_ref!('../master') }
      File.write(KbStage.managed_ref_path, " master\n")
      assert_raises(KbStage::Error) { KbStage.read_managed_ref }
      assert_path_exists(File.join(state, 'credentials', 'managed-repository.ref'))
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

  def test_schema_four_manifest_requires_per_page_summaries_and_guarded_deletions
    Dir.mktmpdir do |release_dir|
      candidate = "candidate\n"
      File.write(File.join(release_dir, 'page.txt'), candidate)
      manifest = schema_four_manifest(candidate:)
      manifest_path = File.join(release_dir, 'release.yml')
      File.write(manifest_path, YAML.dump(manifest))

      parsed = KbRelease::Manifest.new(manifest_path)
      assert_equal('Aktualizace návodu', parsed.page_summary(parsed.pages.first))
      assert_equal('Odstranění starého návodu', parsed.page_summary(parsed.deletions.first))

      manifest['production_summary'] = 'Generic summary'
      File.write(manifest_path, YAML.dump(manifest))
      assert_match(/per-page summaries/, assert_raises(KbRelease::Error) {
        KbRelease::Manifest.new(manifest_path)
      }.message)
      manifest.delete('production_summary')

      manifest.fetch('pages').first['summary'] = "first\nsecond"
      File.write(manifest_path, YAML.dump(manifest))
      assert_match(/non-empty single line/, assert_raises(KbRelease::Error) {
        KbRelease::Manifest.new(manifest_path)
      }.message)
      manifest.fetch('pages').first['summary'] = 'Aktualizace návodu'

      manifest.fetch('deletions').first['id'] = 'page'
      File.write(manifest_path, YAML.dump(manifest))
      assert_match(/overlap/, assert_raises(KbRelease::Error) {
        KbRelease::Manifest.new(manifest_path)
      }.message)
    end
  end

  def test_revision_history_reads_the_latest_html_summary
    requested = nil
    requested_site = nil
    requested_headers = nil
    history = KbRelease::RevisionHistory.new(
      site_url: lambda do |wiki|
        requested_site = wiki
        "https://#{wiki}.example.test"
      end,
      headers: ->(wiki) { { 'X-Test-Wiki' => wiki } },
      fetcher: lambda do |url, headers|
        requested = url
        requested_headers = headers
        '<span class="sum"> – Update SSH &amp; firewall</span>'.b
      end
    )

    url = history.verify!('org', 'manuals:server:ssh', 'Update SSH & firewall')

    assert_equal(requested, url)
    assert_equal('org', requested_site)
    assert_equal({ 'X-Test-Wiki' => 'org' }, requested_headers)
    assert_includes(url, 'https://org.example.test/doku.php')
    assert_includes(url, 'id=manuals%3Aserver%3Assh')
    assert_includes(url, 'do=revisions')
  end

  def test_release_manifest_binds_managed_contract_page_checksums
    Dir.mktmpdir do |release_dir|
      content = "candidate\n"
      sha256 = Digest::SHA256.hexdigest(content)
      File.write(File.join(release_dir, 'page.txt'), content)
      manifest_path = File.join(release_dir, 'release.yml')
      manifest = {
        'schema' => 3,
        'wiki' => 'cz',
        'production_summary' => 'Publish managed guide',
        'pages' => [{
          'id' => 'navody:vps:kvm',
          'source_revision' => 123,
          'source_sha256' => Digest::SHA256.hexdigest("source\n"),
          'file' => 'page.txt',
          'sha256' => sha256
        }],
        'media' => [],
        'contract' => {
          'repository' => 'vpsfreecz/vpsfree-kb-contracts',
          'base_commit' => '1' * 40,
          'head_commit' => '2' * 40,
          'registry_sha256' => '3' * 64,
          'pages' => [{
            'id' => 'navody:vps:kvm',
            'article' => 'kvm',
            'source' => 'contract/pages/navody-vps-kvm.txt',
            'sha256' => '4' * 64
          }]
        }
      }
      File.write(manifest_path, YAML.dump(manifest))

      error = assert_raises(KbRelease::Error) { KbRelease::Manifest.new(manifest_path) }
      assert_match(/contract checksum differs/, error.message)

      manifest.fetch('contract').fetch('pages').first['sha256'] = sha256
      manifest.fetch('contract')['tests'] = [{
        'article' => 'kvm',
        'pattern' => 'kb/kvm#*',
        'source' => 'tests/suite/kb/kvm.nix',
        'sha256' => '5' * 64
      }]
      File.write(manifest_path, YAML.dump(manifest))
      parsed = KbRelease::Manifest.new(manifest_path)
      assert_instance_of(KbRelease::Manifest, parsed)
      assert(parsed.managed_contract?)
      assert_equal('2' * 40, parsed.managed_ref)
      assert_equal(2, parsed.managed_sources.length)

      manifest.fetch('contract').fetch('tests').first['pattern'] = 'kb/kvm'
      File.write(manifest_path, YAML.dump(manifest))
      assert_match(/test pattern/, assert_raises(KbRelease::Error) {
        KbRelease::Manifest.new(manifest_path)
      }.message)

      test = manifest.fetch('contract').fetch('tests').first
      test['pattern'] = 'kb/kvm#*'
      test['source'] = 'tests/suite/kb/gre.nix'
      File.write(manifest_path, YAML.dump(manifest))
      assert_match(/test source must be tests\/suite\/kb\/kvm\.nix/, assert_raises(KbRelease::Error) {
        KbRelease::Manifest.new(manifest_path)
      }.message)
    end
  end

  def test_managed_repository_verifies_exact_ref_content_and_reports_links
    Dir.mktmpdir do |release_dir|
      content = "candidate\n"
      test_source = "{ ... }: { }\n"
      File.write(File.join(release_dir, 'page.txt'), content)
      manifest_path = File.join(release_dir, 'release.yml')
      ref = '2' * 40
      File.write(
        manifest_path,
        YAML.dump(
          'schema' => 3,
          'wiki' => 'cz',
          'production_summary' => 'Publish managed guide',
          'pages' => [{
            'id' => 'navody:vps:kvm',
            'source_revision' => 123,
            'source_sha256' => Digest::SHA256.hexdigest("source\n"),
            'file' => 'page.txt',
            'sha256' => Digest::SHA256.hexdigest(content)
          }],
          'media' => [],
          'contract' => {
            'repository' => 'vpsfreecz/vpsfree-kb-contracts',
            'base_commit' => '1' * 40,
            'head_commit' => ref,
            'registry_sha256' => '3' * 64,
            'pages' => [{
              'id' => 'navody:vps:kvm',
              'article' => 'kvm',
              'source' => 'contract/pages/navody-vps-kvm.txt',
              'sha256' => Digest::SHA256.hexdigest(content)
            }],
            'tests' => [{
              'article' => 'kvm',
              'pattern' => 'kb/kvm#*',
              'source' => 'tests/suite/kb/kvm.nix',
              'sha256' => Digest::SHA256.hexdigest(test_source)
            }]
          }
        )
      )
      requested = []
      output = StringIO.new
      repository = KbRelease::ManagedRepository.new(
        fetcher: lambda do |url|
          requested << url
          url.end_with?('/tests/suite/kb/kvm.nix') ? test_source : content
        end,
        out: output
      )

      rows = repository.verify!(KbRelease::Manifest.new(manifest_path), ref:)

      assert_equal(2, rows.length)
      assert_equal(2, requested.length)
      assert(requested.all? { |url| url.include?("/#{ref}/") })
      assert_includes(output.string, "test kb/kvm#*")
      assert_includes(output.string, "github.com/vpsfreecz/vpsfree-kb-contracts/blob/#{ref}")
    end
  end

  def test_managed_release_checks_remote_commit_before_any_staging_access
    with_state do
      Dir.mktmpdir do |release_dir|
        manifest_path = write_managed_release_manifest(release_dir)
        client_requested = false
        repository = Object.new
        repository.define_singleton_method(:verify!) do |_manifest, ref:|
          raise KbRelease::Error, "managed commit is unpublished: #{ref}"
        end
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: lambda do |_name|
            client_requested = true
            raise 'must not connect'
          end,
          managed_repository: repository,
          out: StringIO.new
        )

        error = stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          assert_raises(KbRelease::Error) { runner.stage! }
        end

        assert_match(/unpublished/, error.message)
        refute(client_requested)
        assert_nil(KbStage.read_managed_ref)
        refute_path_exists(KbStage.pending_release_path)
      end
    end
  end

  def test_managed_promotion_checks_remote_master_before_production_access
    with_state do
      Dir.mktmpdir do |release_dir|
        manifest = KbRelease::Manifest.new(write_managed_release_manifest(release_dir))
        KbStage.write_managed_ref!(manifest.managed_ref)
        KbStage.write_json(
          KbStage.pending_release_path,
          'sha256' => manifest.digest,
          'slug' => 'session-one',
          'managed_ref' => manifest.managed_ref
        )
        staging = FakeClient.new
        staging.expect('core.getPage', { page: 'navody:vps:kvm' }, result: "candidate\n")
        production_requested = false
        repository = Object.new
        repository.define_singleton_method(:verify!) do |_manifest, ref:|
          raise KbRelease::Error, 'managed master is not ready' if ref == 'master'
        end
        runner = KbRelease::Runner.new(
          manifest:,
          client_factory: lambda do |name|
            if name == 'cz-staging'
              staging
            else
              production_requested = true
              raise 'must not connect to production'
            end
          end,
          managed_repository: repository,
          out: StringIO.new
        )

        error = stub_kb_stage(:with_owned_lock, ->(&block) { block.call }) do
          stub_kb_stage(:current_slug, 'session-one') do
            assert_raises(KbRelease::Error) do
              runner.promote!(approved_production: true)
            end
          end
        end

        assert_match(/master is not ready/, error.message)
        assert(staging.done?)
        refute(production_requested)
        assert_path_exists(KbStage.pending_release_path)
      end
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
        staging.expect('core.aclCheck', { page: 'navody:notifikace' }, result: 4)
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

  def test_release_requires_create_acl_for_create_only_pages
    manifest_class = Struct.new(:pages, :media) do
      def page_policy(entry)
        entry.fetch('policy', 'update')
      end
    end
    manifest = manifest_class.new(
      [{ 'id' => 'navody:notifikace', 'policy' => 'create' }],
      []
    )
    client = FakeClient.new
    client.expect('core.whoAmI', nil, result: { 'login' => 'editor' })
    client.expect('core.aclCheck', { page: 'navody:notifikace' }, result: 2)
    runner = KbRelease::Runner.new(
      manifest:,
      client_factory: ->(_name) { client },
      out: StringIO.new
    )

    error = assert_raises(KbRelease::Error) do
      runner.send(:verify_write_access!, client)
    end

    assert_match(/insufficient ACL to create page/, error.message)
    assert(client.done?)
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
        production = FakeClient.new
        production.expect(
          'core.getMediaInfo',
          { media: 'cs:screenshots:vpsadmin:notifications:routes.png' },
          result: missing
        )
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

  def test_schema_four_stages_writes_and_deletions_with_exact_summaries
    with_state do
      Dir.mktmpdir do |release_dir|
        source = "old\n"
        candidate = "new\n"
        obsolete = "obsolete\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(schema_four_manifest(candidate:, source:, obsolete:))
        )

        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'page' }, result: { 'revision' => 123 })
        production.expect('core.getPage', { page: 'page' }, result: source)
        production.expect('core.getPageInfo', { page: 'obsolete' }, result: { 'revision' => 456 })
        production.expect('core.getPage', { page: 'obsolete' }, result: obsolete)
        staging = FakeClient.new
        staging.expect('core.getPage', { page: 'page' }, result: source)
        staging.expect('core.getPageInfo', { page: 'obsolete' }, result: { 'revision' => 999 })
        staging.expect('core.getPage', { page: 'obsolete' }, result: obsolete)
        staging.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        staging.expect('core.aclCheck', { page: 'page' }, result: 2)
        staging.expect('core.aclCheck', { page: 'obsolete' }, result: 16)
        staging.expect(
          'core.savePage',
          {
            page: 'page', text: candidate, summary: 'Aktualizace návodu', isminor: false
          },
          result: true
        )
        staging.expect(
          'core.savePage',
          {
            page: 'obsolete', text: '', summary: 'Odstranění starého návodu', isminor: false
          },
          result: true
        )
        staging.expect('core.getPage', { page: 'page' }, result: candidate)
        staging.expect(
          'core.getPageInfo',
          { page: 'obsolete' },
          result: KbPage::RpcError.new(221, 'page does not exist')
        )
        history = FakeRevisionHistory.new(
          ['cz-staging', 'page'] => 'Aktualizace návodu',
          ['cz-staging', 'obsolete'] => 'Odstranění starého návodu'
        )
        clients = { 'cz' => production, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          language_links: NoopLanguageLinks.new,
          revision_history: history,
          out: StringIO.new
        )

        stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          stub_kb_stage(:current_slug, 'session-one') { runner.stage! }
        end

        assert(production.done?)
        assert(staging.done?)
        assert_equal(2, history.calls.length)
        assert_path_exists(KbStage.pending_release_path)
      end
    end
  end

  def test_schema_four_retry_rejects_a_staged_page_with_another_summary
    with_state do
      Dir.mktmpdir do |release_dir|
        source = "old\n"
        candidate = "new\n"
        obsolete = "obsolete\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(manifest_path, YAML.dump(schema_four_manifest(candidate:, source:, obsolete:)))

        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'page' }, result: { 'revision' => 123 })
        production.expect('core.getPage', { page: 'page' }, result: source)
        production.expect('core.getPageInfo', { page: 'obsolete' }, result: { 'revision' => 456 })
        production.expect('core.getPage', { page: 'obsolete' }, result: obsolete)
        staging = FakeClient.new
        staging.expect('core.getPage', { page: 'page' }, result: candidate)
        history = FakeRevisionHistory.new(
          ['cz-staging', 'page'] => 'Unrelated summary'
        )
        clients = { 'cz' => production, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          revision_history: history,
          out: StringIO.new
        )

        error = stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          assert_raises(KbRelease::Error) { runner.stage! }
        end

        assert_match(/reset staging and stage this manifest again/, error.message)
        assert(production.done?)
        assert(staging.done?)
      end
    end
  end

  def test_schema_four_refuses_deletion_source_drift_before_staging
    with_state do
      Dir.mktmpdir do |release_dir|
        manifest = schema_four_manifest(candidate: "unused\n")
        manifest['pages'] = []
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(manifest_path, YAML.dump(manifest))
        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'obsolete' }, result: { 'revision' => 456 })
        production.expect('core.getPage', { page: 'obsolete' }, result: "changed\n")
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { name == 'cz' ? production : raise('must not stage') },
          out: StringIO.new
        )

        error = stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          assert_raises(KbRelease::Error) { runner.stage! }
        end

        assert_match(/deletion source drift/, error.message)
        assert(production.done?)
      end
    end
  end

  def test_schema_four_requires_delete_acl_before_staging
    with_state do
      Dir.mktmpdir do |release_dir|
        obsolete = "obsolete\n"
        manifest = schema_four_manifest(candidate: "unused\n", obsolete:)
        manifest['pages'] = []
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(manifest_path, YAML.dump(manifest))
        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'obsolete' }, result: { 'revision' => 456 })
        production.expect('core.getPage', { page: 'obsolete' }, result: obsolete)
        staging = FakeClient.new
        staging.expect('core.getPageInfo', { page: 'obsolete' }, result: { 'revision' => 999 })
        staging.expect('core.getPage', { page: 'obsolete' }, result: obsolete)
        staging.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        staging.expect('core.aclCheck', { page: 'obsolete' }, result: 2)
        clients = { 'cz' => production, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          out: StringIO.new
        )

        error = stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          assert_raises(KbRelease::Error) { runner.stage! }
        end

        assert_match(/insufficient ACL to delete page obsolete/, error.message)
        assert(production.done?)
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

  def test_schema_four_promotion_accepts_matching_partial_write_and_deletion
    with_state do
      Dir.mktmpdir do |release_dir|
        candidate = "new\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(manifest_path, YAML.dump(schema_four_manifest(candidate:)))
        manifest = KbRelease::Manifest.new(manifest_path)
        KbStage.write_json(
          KbStage.pending_release_path,
          'sha256' => manifest.digest,
          'slug' => 'session-one'
        )
        missing = KbPage::RpcError.new(221, 'page does not exist')
        staging = FakeClient.new
        staging.expect('core.getPage', { page: 'page' }, result: candidate)
        staging.expect('core.getPageInfo', { page: 'obsolete' }, result: missing)
        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'page' }, result: { 'revision' => 999 })
        production.expect('core.getPage', { page: 'page' }, result: candidate)
        production.expect('core.getPageInfo', { page: 'obsolete' }, result: missing)
        production.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
        production.expect('core.aclCheck', { page: 'page' }, result: 2)
        production.expect('core.getPage', { page: 'page' }, result: candidate)
        production.expect('core.getPageInfo', { page: 'obsolete' }, result: missing)
        history = FakeRevisionHistory.new(
          ['cz-staging', 'page'] => 'Aktualizace návodu',
          ['cz-staging', 'obsolete'] => 'Odstranění starého návodu',
          ['cz', 'page'] => 'Aktualizace návodu',
          ['cz', 'obsolete'] => 'Odstranění starého návodu'
        )
        clients = { 'cz-staging' => staging, 'cz' => production }
        runner = KbRelease::Runner.new(
          manifest:,
          client_factory: ->(name) { clients.fetch(name) },
          revision_history: history,
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

  def test_schema_four_promotion_rejects_a_partial_write_with_another_summary
    with_state do
      Dir.mktmpdir do |release_dir|
        candidate = "new\n"
        File.write(File.join(release_dir, 'page.txt'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(manifest_path, YAML.dump(schema_four_manifest(candidate:)))
        manifest = KbRelease::Manifest.new(manifest_path)
        KbStage.write_json(
          KbStage.pending_release_path,
          'sha256' => manifest.digest,
          'slug' => 'session-one'
        )
        missing = KbPage::RpcError.new(221, 'page does not exist')
        staging = FakeClient.new
        staging.expect('core.getPage', { page: 'page' }, result: candidate)
        staging.expect('core.getPageInfo', { page: 'obsolete' }, result: missing)
        production = FakeClient.new
        production.expect('core.getPageInfo', { page: 'page' }, result: { 'revision' => 999 })
        production.expect('core.getPage', { page: 'page' }, result: candidate)
        history = FakeRevisionHistory.new(
          ['cz-staging', 'page'] => 'Aktualizace návodu',
          ['cz-staging', 'obsolete'] => 'Odstranění starého návodu',
          ['cz', 'page'] => 'Another summary'
        )
        clients = { 'cz-staging' => staging, 'cz' => production }
        runner = KbRelease::Runner.new(
          manifest:,
          client_factory: ->(name) { clients.fetch(name) },
          revision_history: history,
          out: StringIO.new
        )

        error = stub_kb_stage(:with_owned_lock, ->(&block) { block.call }) do
          stub_kb_stage(:current_slug, 'session-one') do
            assert_raises(KbRelease::Error) do
              runner.promote!(approved_production: true)
            end
          end
        end

        assert_match(/revision summary differs/, error.message)
        assert(staging.done?)
        assert(production.done?)
        assert_path_exists(KbStage.pending_release_path)
      end
    end
  end

  def test_schema_four_verify_prints_summaries_and_history_links
    Dir.mktmpdir do |release_dir|
      candidate = "new\n"
      File.write(File.join(release_dir, 'page.txt'), candidate)
      manifest_path = File.join(release_dir, 'release.yml')
      File.write(manifest_path, YAML.dump(schema_four_manifest(candidate:)))
      missing = KbPage::RpcError.new(221, 'page does not exist')
      production = FakeClient.new
      production.expect('core.getPage', { page: 'page' }, result: candidate)
      production.expect('core.getPageInfo', { page: 'obsolete' }, result: missing)
      history = FakeRevisionHistory.new(
        ['cz', 'page'] => 'Aktualizace návodu',
        ['cz', 'obsolete'] => 'Odstranění starého návodu'
      )
      out = StringIO.new
      runner = KbRelease::Runner.new(
        manifest: KbRelease::Manifest.new(manifest_path),
        client_factory: ->(_name) { production },
        revision_history: history,
        out:
      )

      runner.verify!(:production)

      assert_includes(out.string, 'write page: Aktualizace návodu')
      assert_includes(out.string, 'delete obsolete: Odstranění starého návodu')
      assert_includes(out.string, 'https://history.example.test/cz/page')
      assert(production.done?)
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
        production = FakeClient.new
        production.expect('core.getMediaInfo', { media: 'media.bin' }, result: {})
        production.expect(
          'core.getMedia',
          { media: 'media.bin' },
          result: Base64.strict_encode64('expected source')
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
        clients = { 'cz' => production, 'cz-staging' => staging }
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { clients.fetch(name) },
          out: StringIO.new
        )

        error = stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          assert_raises(KbRelease::Error) { runner.stage! }
        end
        assert_match(/media source drift/, error.message)
        assert(production.done?)
        assert(staging.done?)
      end
    end
  end

  def test_update_media_refuses_production_drift_before_staging
    with_state do
      Dir.mktmpdir do |release_dir|
        source = 'expected source'
        candidate = 'candidate media'
        File.binwrite(File.join(release_dir, 'media.bin'), candidate)
        manifest_path = File.join(release_dir, 'release.yml')
        File.write(
          manifest_path,
          YAML.dump(
            'schema' => 3,
            'wiki' => 'cz',
            'production_summary' => 'Aktualizovat snímek',
            'pages' => [],
            'media' => [
              {
                'id' => 'media.bin',
                'file' => 'media.bin',
                'sha256' => Digest::SHA256.hexdigest(candidate),
                'policy' => 'update',
                'source_sha256' => Digest::SHA256.hexdigest(source)
              }
            ]
          )
        )
        production = FakeClient.new
        production.expect('core.getMediaInfo', { media: 'media.bin' }, result: {})
        production.expect(
          'core.getMedia',
          { media: 'media.bin' },
          result: Base64.strict_encode64('concurrent production change')
        )
        runner = KbRelease::Runner.new(
          manifest: KbRelease::Manifest.new(manifest_path),
          client_factory: ->(name) { name == 'cz' ? production : raise('must not stage') },
          out: StringIO.new
        )

        error = stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
          assert_raises(KbRelease::Error) { runner.stage! }
        end

        assert_match(/production media drift/, error.message)
        assert(production.done?)
      end
    end
  end

  def test_stage_refuses_candidate_media_already_in_production
    %w[create update].each do |policy|
      with_state do
        Dir.mktmpdir do |release_dir|
          source = 'source media'
          candidate = 'candidate media'
          File.binwrite(File.join(release_dir, 'media.bin'), candidate)
          media = {
            'id' => 'media.bin',
            'file' => 'media.bin',
            'sha256' => Digest::SHA256.hexdigest(candidate),
            'policy' => policy
          }
          media['source_sha256'] = Digest::SHA256.hexdigest(source) if policy == 'update'
          manifest_path = File.join(release_dir, 'release.yml')
          File.write(
            manifest_path,
            YAML.dump(
              'schema' => 3,
              'wiki' => 'cz',
              'production_summary' => 'Aktualizovat snímek',
              'pages' => [],
              'media' => [media]
            )
          )
          production = FakeClient.new
          production.expect('core.getMediaInfo', { media: 'media.bin' }, result: {})
          production.expect(
            'core.getMedia',
            { media: 'media.bin' },
            result: Base64.strict_encode64(candidate)
          )
          runner = KbRelease::Runner.new(
            manifest: KbRelease::Manifest.new(manifest_path),
            client_factory: ->(name) { name == 'cz' ? production : raise('must not stage') },
            out: StringIO.new
          )

          error = stub_kb_stage(:with_staging_mutation, ->(&block) { block.call }) do
            assert_raises(KbRelease::Error) { runner.stage! }
          end

          assert_match(/production media|create-only production media/, error.message)
          assert(production.done?)
        end
      end
    end
  end

  def test_promotion_retries_media_already_saved_by_a_partial_attempt
    %w[create update].each do |policy|
      with_state do
        Dir.mktmpdir do |release_dir|
          source = 'source media'
          candidate = 'candidate media'
          File.binwrite(File.join(release_dir, 'media.bin'), candidate)
          media = {
            'id' => 'media.bin',
            'file' => 'media.bin',
            'sha256' => Digest::SHA256.hexdigest(candidate),
            'policy' => policy
          }
          media['source_sha256'] = Digest::SHA256.hexdigest(source) if policy == 'update'
          manifest_path = File.join(release_dir, 'release.yml')
          File.write(
            manifest_path,
            YAML.dump(
              'schema' => 3,
              'wiki' => 'cz',
              'production_summary' => 'Aktualizovat snímek',
              'pages' => [],
              'media' => [media]
            )
          )
          manifest = KbRelease::Manifest.new(manifest_path)
          KbStage.write_json(
            KbStage.pending_release_path,
            'sha256' => manifest.digest,
            'slug' => 'session-one'
          )

          staging = FakeClient.new
          staging.expect(
            'core.getMedia',
            { media: 'media.bin' },
            result: Base64.strict_encode64(candidate)
          )
          production = FakeClient.new
          production.expect('core.getMediaInfo', { media: 'media.bin' }, result: {})
          production.expect(
            'core.getMedia',
            { media: 'media.bin' },
            result: Base64.strict_encode64(candidate)
          )
          production.expect('core.whoAmI', nil, result: { 'login' => 'aither' })
          production.expect('core.getMediaInfo', { media: 'media.bin' }, result: {})
          production.expect('core.aclCheck', { page: 'media.bin' }, result: 255)
          production.expect('core.getMediaInfo', { media: 'media.bin' }, result: {})
          production.expect(
            'core.getMedia',
            { media: 'media.bin' },
            result: Base64.strict_encode64(candidate)
          )
          production.expect(
            'core.getMedia',
            { media: 'media.bin' },
            result: Base64.strict_encode64(candidate)
          )
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
  end

  private

  def write_managed_release_manifest(release_dir)
    candidate = "candidate\n"
    File.write(File.join(release_dir, 'page.txt'), candidate)
    manifest_path = File.join(release_dir, 'release.yml')
    File.write(
      manifest_path,
      YAML.dump(
        'schema' => 3,
        'wiki' => 'cz',
        'production_summary' => 'Publish managed guide',
        'pages' => [{
          'id' => 'navody:vps:kvm',
          'source_revision' => 123,
          'source_sha256' => Digest::SHA256.hexdigest("source\n"),
          'file' => 'page.txt',
          'sha256' => Digest::SHA256.hexdigest(candidate)
        }],
        'media' => [],
        'contract' => {
          'repository' => 'vpsfreecz/vpsfree-kb-contracts',
          'base_commit' => '1' * 40,
          'head_commit' => '2' * 40,
          'registry_sha256' => '3' * 64,
          'pages' => [{
            'id' => 'navody:vps:kvm',
            'article' => 'kvm',
            'source' => 'contract/pages/navody-vps-kvm.txt',
            'sha256' => Digest::SHA256.hexdigest(candidate)
          }],
          'tests' => [{
            'article' => 'kvm',
            'pattern' => 'kb/kvm#*',
            'source' => 'tests/suite/kb/kvm.nix',
            'sha256' => '4' * 64
          }]
        }
      )
    )
    manifest_path
  end

  def schema_four_manifest(candidate:, source: "old\n", obsolete: "obsolete\n")
    {
      'schema' => 4,
      'wiki' => 'cz',
      'pages' => [{
        'id' => 'page',
        'source_revision' => 123,
        'source_sha256' => Digest::SHA256.hexdigest(source),
        'file' => 'page.txt',
        'sha256' => Digest::SHA256.hexdigest(candidate),
        'summary' => 'Aktualizace návodu'
      }],
      'deletions' => [{
        'id' => 'obsolete',
        'source_revision' => 456,
        'source_sha256' => Digest::SHA256.hexdigest(obsolete),
        'summary' => 'Odstranění starého návodu'
      }],
      'media' => []
    }
  end

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
